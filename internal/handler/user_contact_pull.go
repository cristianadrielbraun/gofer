package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type userContactPull struct {
	h           *Handler
	snapshot    *config.AccountServiceSnapshot
	credentials *mailauth.UserAccountCredentials
	token       string
	destination *config.UserContactSyncDestination
	remoteAlias string
	preview     *config.UserContactPreviewSnapshot
	setup       *config.UserContactEditSnapshot
	deletion    *config.UserContactDeleteSnapshot
}

type userContactDeferred struct{ at time.Time }

func (e userContactDeferred) Error() string {
	return fmt.Sprintf("provider retry is deferred until %s", e.at.Format(time.RFC3339))
}
func (e userContactDeferred) RetryAfter() (time.Time, bool) { return e.at, true }

func (p *userContactPull) validate(ctx context.Context) error {
	if p.deletion != nil {
		return p.h.userAccounts.ValidateContactDelete(ctx, p.deletion)
	}
	if p.preview != nil {
		return p.h.userAccounts.ValidateContactPreview(ctx, p.preview)
	}
	if p.setup != nil {
		return p.h.userAccounts.ValidateContactSetup(ctx, p.setup)
	}
	if p.destination != nil {
		if p.remoteAlias != "" {
			return p.h.userAccounts.ValidateContactSyncRemote(ctx, p.destination, p.remoteAlias)
		}
		return p.h.userAccounts.ValidateContactSyncDestination(ctx, p.destination)
	}
	return p.h.userAccounts.ValidateContactServices(ctx, p.snapshot)
}

// SyncUserContactAccount is the owned request/worker entry point. Only copied
// snapshots survive a lease, including credentials and each DAV book checkpoint.
func (h *Handler) SyncUserContactAccount(ctx context.Context, owner, id string) (imported int, err error) {
	if h.userIMAP == nil || h.userAccounts == nil || h.userStorage == nil {
		return 0, errors.New("owned contact services are unavailable")
	}
	if !h.beginContactSync(id) {
		return 0, errContactSyncAlreadyRunning
	}
	defer h.endContactSync(id)
	err = h.userIMAP.RunAccountService(ctx, owner, id, mail.AccountServiceContacts, 2*time.Minute, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotServices(ctx, owner, id)
		if err != nil {
			return err
		}
		if !snapshot.ContactConfig().Enabled {
			return nil
		}
		snapshot, err = h.userAccounts.StartContactPull(ctx, snapshot)
		if err != nil {
			return err
		}
		pull := &userContactPull{h: h, snapshot: snapshot}
		var pullErr error
		switch snapshot.ContactConfig().Provider {
		case providers.ProviderGmail, providers.ProviderOutlook:
			if h.userCredentials == nil {
				pullErr = errors.New("mailbox OAuth is not configured")
				break
			}
			if snapshot.ContactConfig().Provider == providers.ProviderGmail && !h.userCredentials.HasGoogleOAuth() {
				pullErr = errors.New("Google OAuth is not configured")
				break
			}
			if snapshot.ContactConfig().Provider == providers.ProviderOutlook && !h.userCredentials.HasMicrosoftOAuth() {
				pullErr = errors.New("Microsoft OAuth is not configured")
				break
			}
			pull.credentials = h.userCredentials.ContactsAccount(owner, id)
			if snapshot.ContactConfig().Provider == providers.ProviderGmail {
				imported, pullErr = pull.google(ctx)
			} else {
				imported, pullErr = pull.outlook(ctx)
			}
		case providers.ProviderCardDAV:
			imported, pullErr = pull.dav(ctx)
		default:
			pullErr = errors.New("contact sync is not configured for this account")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.Join(pullErr, h.userAccounts.FinishContactPull(ctx, pull.snapshot, imported, pullErr))
	})
	return imported, err
}

func (p *userContactPull) ready(ctx context.Context) error {
	if err := p.validate(ctx); err != nil {
		return err
	}
	until, err := p.h.userStorage.ProviderRetryUntil(ctx, p.snapshot.OwnerID(), p.snapshot.AccountID())
	if err != nil {
		return err
	}
	if until.After(time.Now()) {
		return userContactDeferred{at: until}
	}
	return nil
}

func (p *userContactPull) recordRetry(ctx context.Context, err error) error {
	var hint interface{ RetryAfter() (time.Time, bool) }
	if errors.As(err, &hint) {
		if at, ok := hint.RetryAfter(); ok && at.After(time.Now()) {
			if changed := p.validate(ctx); changed != nil {
				return errors.Join(err, changed)
			}
			return errors.Join(err, p.h.userStorage.DeferProviderRetry(ctx, p.snapshot.OwnerID(), p.snapshot.AccountID(), at))
		}
	}
	return err
}

func contactAPIUnauthorized(err error) bool {
	var google googleAPIError
	var outlook outlookAPIError
	return (errors.As(err, &google) && google.Status == http.StatusUnauthorized) || (errors.As(err, &outlook) && outlook.Status == http.StatusUnauthorized)
}

func (p *userContactPull) request(ctx context.Context, fn func(string) error) error {
	if err := p.ready(ctx); err != nil {
		return err
	}
	if p.token == "" {
		var err error
		p.token, err = p.credentials.GetOAuthTokenForAccount(ctx, p.snapshot.AccountID())
		if err != nil {
			return p.recordRetry(ctx, err)
		}
	}
	if err := p.validate(ctx); err != nil {
		return err
	}
	err := fn(p.token)
	if !contactAPIUnauthorized(err) {
		return p.recordRetry(ctx, err)
	}
	p.token, err = p.credentials.RefreshOAuthTokenForAccount(ctx, p.snapshot.AccountID())
	if err != nil {
		return p.recordRetry(ctx, err)
	}
	if err := p.ready(ctx); err != nil {
		return err
	}
	return p.recordRetry(ctx, fn(p.token))
}

func (p *userContactPull) getJSON(ctx context.Context, endpoint string, out any) error {
	return p.json(ctx, http.MethodGet, endpoint, nil, out)
}

func (p *userContactPull) json(ctx context.Context, method, endpoint string, payload, out any) error {
	return p.request(ctx, func(token string) error {
		return userContactProviderJSON(ctx, p.snapshot.ContactConfig().Provider, method, endpoint, token, payload, out)
	})
}

// userContactProviderJSON is shared by owned pulls and writes. Credentials and
// lifecycle guards belong to request; this helper retains no repository handle.
func userContactProviderJSON(ctx context.Context, provider, method, endpoint, token string, payload, out any, redirectGuards ...func() error) error {
	if len(redirectGuards) > 1 || (len(redirectGuards) == 1 && redirectGuards[0] == nil) {
		return errors.New("invalid provider redirect guard")
	}
	var body io.Reader
	if payload != nil {
		wire, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(wire))
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if provider == providers.ProviderOutlook {
		req.Header.Set("Prefer", `IdType="ImmutableId"`)
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		// A redirect of POST/PATCH can silently turn an intended write into
		// a GET. A successful response must acknowledge the original write.
		if method != http.MethodGet {
			return errors.New("provider write redirected")
		}
		if len(via) >= 10 {
			return errors.New("provider redirect limit exceeded")
		}
		if len(redirectGuards) == 1 {
			if err := redirectGuards[0](); err != nil {
				return err
			}
		}
		if next.URL.EscapedPath() != req.URL.EscapedPath() || next.URL.Scheme != req.URL.Scheme || next.URL.Host != req.URL.Host || next.URL.Path != req.URL.Path || next.URL.User != nil || next.URL.Fragment != "" {
			return errors.New("provider redirect is outside the configured collection")
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if err != nil {
			return err
		}
		if provider == providers.ProviderGmail {
			return newGoogleAPIError(resp, body)
		}
		return newOutlookAPIError(resp, body)
	}
	const limit = 32 << 20
	wire, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return err
	}
	if len(wire) > limit {
		return errors.New("provider response exceeds 32 MiB")
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(wire, out)
}

func (p *userContactPull) google(ctx context.Context) (int, error) {
	imported := 0
	pageToken := ""
	seen := make(map[string]bool)
	for {
		query := url.Values{"personFields": {googleContactPersonFields()}, "pageSize": {"1000"}}
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		var page googlePeopleResponse
		if err := p.getJSON(ctx, googlePeopleAPIBaseURL+"/people/me/connections?"+query.Encode(), &page); err != nil {
			return imported, err
		}
		inputs := make([]storage.InboundContact, 0, len(page.Connections))
		for _, person := range page.Connections {
			contact := googleContactFromPerson(person)
			if strings.TrimSpace(contact.Email) == "" {
				continue
			}
			inputs = append(inputs, storage.InboundContact{Contact: contact, RemoteID: person.ResourceName, Etag: person.Etag})
		}
		results, err := p.h.userAccounts.PublishInboundContacts(ctx, p.snapshot, inputs)
		if err != nil {
			return imported, err
		}
		imported += len(results)
		p.wakeFanout(ctx, results)
		pageToken = page.NextPageToken
		if pageToken == "" {
			return imported, nil
		}
		if seen[pageToken] {
			return imported, errors.New("provider repeated a page token")
		}
		seen[pageToken] = true
	}
}

func validUserContactGraphPage(endpoint string) error {
	base, err := url.Parse(outlookGraphBaseURL)
	if err != nil {
		return err
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	// Pagination can change only the query of the owned /me/contacts collection.
	if target.User != nil || target.Scheme != base.Scheme || target.Host != base.Host || target.Fragment != "" || target.Path != strings.TrimRight(base.Path, "/")+"/me/contacts" {
		return errors.New("contact page is outside the configured Graph collection")
	}
	return nil
}

func (p *userContactPull) outlook(ctx context.Context) (int, error) {
	endpoint := outlookGraphBaseURL + "/me/contacts?" + outlookContactListQuery().Encode()
	imported := 0
	seen := make(map[string]bool)
	for endpoint != "" {
		if err := validUserContactGraphPage(endpoint); err != nil {
			return imported, err
		}
		if seen[endpoint] {
			return imported, errors.New("provider repeated a page URL")
		}
		seen[endpoint] = true
		var page outlookContactsResponse
		if err := p.getJSON(ctx, endpoint, &page); err != nil {
			return imported, err
		}
		inputs := make([]storage.InboundContact, 0, len(page.Contacts))
		for _, remote := range page.Contacts {
			contact := outlookContactFromGraph(remote)
			if strings.TrimSpace(contact.Email) == "" {
				continue
			}
			var photo string
			err := p.request(ctx, func(token string) error {
				var err error
				photo, err = p.h.fetchOutlookContactPhotoDataURL(ctx, token, remote.ID)
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// Ignore optional image failures inside the HTTP callback only.
				// Lifecycle/token errors from request itself must always propagate.
				var api outlookAPIError
				if errors.As(err, &api) && (api.Status == 401 || !api.RetryAt.IsZero() || api.Status == 429 || api.Status == 503) {
					return err
				}
				return nil
			})
			if err != nil {
				return imported, err
			}
			contact.AvatarURL = photo
			inputs = append(inputs, storage.InboundContact{Contact: contact, RemoteID: remote.ID, Etag: outlookContactVersion(remote)})
		}
		results, err := p.h.userAccounts.PublishInboundContacts(ctx, p.snapshot, inputs)
		if err != nil {
			return imported, err
		}
		imported += len(results)
		p.wakeFanout(ctx, results)
		endpoint = page.NextLink
	}
	return imported, nil
}

func (p *userContactPull) dav(ctx context.Context) (int, error) {
	cfg := p.snapshot.ContactConfig()
	if len(cfg.AddressBooks) == 0 {
		return 0, errors.New("CardDAV address book is not configured for this account")
	}
	password, err := p.snapshot.ContactSyncPassword("")
	if err != nil {
		return 0, err
	}
	imported := 0
	for _, book := range cfg.AddressBooks {
		if err := p.ready(ctx); err != nil {
			return imported, err
		}
		bookCfg := cfg
		bookCfg.AddressBookURL, bookCfg.LastSyncToken = book.URL, book.LastSyncToken
		result, err := ownedCardDAVSync(ctx, bookCfg, password)
		if err != nil {
			return imported, p.recordRetry(ctx, err)
		}
		page := storage.InboundContactBookPage{Full: result.Fallback, SyncToken: result.SyncToken}
		for _, response := range result.Responses {
			remote := absoluteDAVHref(book.URL, response.Href)
			if remote == "" {
				continue
			}
			if response.deleted() {
				page.DeletedRemoteIDs = append(page.DeletedRemoteIDs, remote)
				continue
			}
			// Some DAV servers include the collection itself in a report.
			if strings.TrimRight(remote, "/") == strings.TrimRight(book.URL, "/") {
				continue
			}
			page.SeenRemoteIDs = append(page.SeenRemoteIDs, remote)
			data := strings.TrimSpace(response.addressData())
			if data == "" {
				continue
			}
			contacts, err := parseVCardContacts(strings.NewReader(data), nil)
			if err != nil {
				return imported, err
			}
			for _, contact := range contacts {
				page.Contacts = append(page.Contacts, storage.InboundContact{Contact: contact, RemoteID: remote, Etag: response.etag()})
			}
		}
		results, next, err := p.h.userAccounts.PublishInboundContactBook(ctx, p.snapshot, book.ID, page)
		if err != nil {
			return imported, err
		}
		p.snapshot = next
		imported += len(results)
		p.wakeFanout(ctx, results)
	}
	return imported, nil
}

func ownedCardDAVSync(ctx context.Context, cfg models.ContactSyncConfig, password string) (cardDAVSyncResult, error) {
	if cfg.LastSyncToken != "" {
		result, err := cardDAVSyncCollection(ctx, cfg, password)
		if err == nil {
			return result, nil
		}
		var api cardDAVHTTPError
		if ctx.Err() != nil || (errors.As(err, &api) && (api.Status == 401 || api.Status == 429 || api.Status == 503 || !api.RetryAt.IsZero())) {
			return cardDAVSyncResult{}, err
		}
	}
	responses, token, err := cardDAVAddressBookQuery(ctx, cfg, password)
	return cardDAVSyncResult{Responses: responses, SyncToken: token, Fallback: true}, err
}

func (h *Handler) handleUserSyncAccountContacts(w http.ResponseWriter, r *http.Request) {
	if err := parseUserContactSetupForm(w, r); err != nil {
		htmlStatus(w, http.StatusBadRequest, "Invalid sync request.")
		return
	}
	owner := h.userID(r.Context())
	id := strings.TrimSpace(r.FormValue("account_id"))
	var ids []string
	if id != "" {
		ids = []string{id}
	} else {
		err := h.withUserDB(r.Context(), owner, func(db *storage.DB) error { var err error; ids, err = db.GetAccountIDs(r.Context(), owner); return err })
		if err != nil {
			userContactSetupError(w, r, err)
			return
		}
	}
	var accounts []contactSyncAccount
	for _, id := range ids {
		snapshot, err := h.userAccounts.SnapshotServices(r.Context(), owner, id)
		if err != nil {
			if r.FormValue("account_id") != "" {
				userContactSetupError(w, r, err)
				return
			}
			continue
		}
		if snapshot.ContactConfig().Enabled {
			accounts = append(accounts, contactSyncAccount{ID: id, Email: snapshot.Identity().EmailAddress, Provider: snapshot.ContactConfig().Provider})
		}
	}
	if len(accounts) == 0 {
		htmlStatus(w, http.StatusBadRequest, "Connect an account with contact sync before syncing contacts.")
		return
	}
	total := 0
	var failures []string
	for _, account := range accounts {
		imported, err := h.SyncUserContactAccount(r.Context(), owner, account.ID)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %s", account.Email, err.Error()))
			continue
		}
		total += imported
	}
	if len(failures) == len(accounts) {
		htmlStatus(w, http.StatusBadGateway, "Contact sync failed: "+strings.Join(failures, "; "))
		return
	}
	_ = h.withUserDB(r.Context(), owner, func(db *storage.DB) error {
		return db.LogContactActivity(r.Context(), owner, "provider_contacts_synced", "", "Account contacts synced", total)
	})
	if len(failures) > 0 {
		htmlStatus(w, http.StatusOK, fmt.Sprintf("Contacts partially synced: %d imported or updated. Failed: %s", total, strings.Join(failures, "; ")))
		return
	}
	if len(accounts) == 1 {
		htmlStatus(w, http.StatusOK, fmt.Sprintf("Contacts synced for %s: %d imported or updated.", accounts[0].Email, total))
		return
	}
	htmlStatus(w, http.StatusOK, fmt.Sprintf("Contacts synced across %d accounts: %d imported or updated.", len(accounts), total))
}
