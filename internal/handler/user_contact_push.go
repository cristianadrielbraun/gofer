package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/google/uuid"
)

const userContactClaimTimeout = 5 * time.Minute

// ProcessUserContactSyncOperations consumes a bounded slice of one owner's
// durable queue. Claim just before each job, so earlier network work cannot use
// up the lock lifetime of a batch of jobs that have not started yet.
func (h *Handler) ProcessUserContactSyncOperations(ctx context.Context, owner string, limit int) (int, error) {
	if h.userAccounts == nil || h.userIMAP == nil || h.userStorage == nil {
		return 0, errors.New("owned contact services are unavailable")
	}
	if limit <= 0 || limit > 25 {
		limit = 5
	}
	var processed int
	err := h.userIMAP.RunUserServiceWork(ctx, owner, func(ctx context.Context) error {
		var err error
		processed, err = h.processUserContactSyncOperations(ctx, owner, limit)
		return err
	})
	return processed, err
}

func (h *Handler) processUserContactSyncOperations(ctx context.Context, owner string, limit int) (int, error) {
	var failures []error
	for processed := 0; processed < limit; processed++ {
		claims, err := h.userAccounts.ClaimContactSync(ctx, owner, 1, userContactClaimTimeout)
		if err != nil {
			return processed, errors.Join(append(failures, err)...)
		}
		if len(claims) == 0 {
			return processed, errors.Join(failures...)
		}
		workCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		err = h.processUserContactSyncOperation(workCtx, claims[0])
		cancel()
		if err != nil {
			failures = append(failures, err)
		}
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return processed + 1, errors.Join(failures...)
		}
	}
	return limit, errors.Join(failures...)
}

func (h *Handler) processUserContactSyncOperation(ctx context.Context, claim *config.UserContactSyncClaim) error {
	profile, err := h.userAccounts.SnapshotContactSync(ctx, claim)
	if err == nil && profile == nil {
		return h.userAccounts.CancelContactSync(ctx, claim)
	}
	if err == nil {
		err = h.userAccounts.StartContactSync(ctx, profile)
	}
	if err == nil && profile != nil {
		seen := make(map[string]bool)
		for _, target := range profile.Targets() {
			if seen[target.AccountID] {
				continue
			}
			seen[target.AccountID] = true
			err = h.userIMAP.RunAccountService(ctx, claim.OwnerID(), target.AccountID, mail.AccountServiceContacts, 45*time.Second, func(ctx context.Context) error {
				return h.pushUserContactAccount(ctx, profile, claim.OwnerID(), target.AccountID)
			})
			if err != nil {
				break
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	status, message, retryAt := "done", "", time.Time{}
	if err != nil {
		status, message = "error", sanitizeProviderErrorBody(err.Error())
		if claim.AttemptCount() < 3 {
			status = "pending"
		}
		var hint interface{ RetryAfter() (time.Time, bool) }
		if errors.As(err, &hint) {
			if at, ok := hint.RetryAfter(); ok && at.After(time.Now()) {
				retryAt, status = at, "pending"
			}
		}
	}
	return errors.Join(err, h.userAccounts.FinishContactSync(ctx, claim, status, message, retryAt))
}

func (h *Handler) pushUserContactAccount(ctx context.Context, profile *config.UserContactSyncProfile, owner, account string) error {
	snapshot, err := h.userAccounts.SnapshotServices(ctx, owner, account)
	if err != nil {
		return err
	}
	cfg := snapshot.ContactConfig()
	if !cfg.Enabled {
		return storage.ErrContactPublication
	}
	var books []models.ContactAddressBook
	if cfg.Provider == providers.ProviderCardDAV {
		books = cardDAVTargetBooks(cfg, profile.Contact().SaveTargets)
		if len(books) == 0 {
			// Account-wide DAV membership preserves the existing copies in
			// selected books; an entirely new copy uses the default book.
			var sources []storage.ContactSource
			if err := h.userAccounts.WithAccountForUser(ctx, owner, account, func(_ *config.AccountStore, db *storage.DB) error {
				var err error
				sources, err = db.GetContactSources(ctx, owner, profile.Contact().ID, providers.ProviderCardDAV)
				return err
			}); err != nil {
				return err
			}
			selected := make(map[string]bool)
			for _, source := range sources {
				if source.AccountID != account {
					continue
				}
				for _, book := range cfg.AddressBooks {
					if source.AddressBookID == book.ID || (source.AddressBookID == "" && strings.HasPrefix(source.RemoteID, strings.TrimRight(book.URL, "/")+"/")) {
						selected[book.ID] = true
					}
				}
			}
			for _, book := range cfg.AddressBooks {
				if selected[book.ID] {
					books = append(books, book)
				}
			}
			if len(books) == 0 {
				_, book := cardDAVConfigForSource(cfg, nil)
				if book.URL != "" {
					books = []models.ContactAddressBook{book}
				}
			}
		}
		if len(books) == 0 {
			return errors.New("CardDAV address book is not configured for this account")
		}
	} else {
		books = []models.ContactAddressBook{{}}
	}
	for _, book := range books {
		destination, err := h.userAccounts.SnapshotContactSyncDestination(ctx, profile, snapshot, book.ID)
		if err != nil {
			return err
		}
		client := &userContactPull{h: h, snapshot: snapshot, destination: destination}
		if cfg.Provider == providers.ProviderGmail || cfg.Provider == providers.ProviderOutlook {
			if h.userCredentials == nil || (cfg.Provider == providers.ProviderGmail && !h.userCredentials.HasGoogleOAuth()) || (cfg.Provider == providers.ProviderOutlook && !h.userCredentials.HasMicrosoftOAuth()) {
				return errors.New("contact provider OAuth is not configured")
			}
			client.credentials = h.userCredentials.ContactsAccount(owner, account)
		}
		var remoteID, etag string
		switch cfg.Provider {
		case providers.ProviderGmail:
			remoteID, etag, err = client.pushGoogle(ctx, profile.Contact(), destination.Source())
		case providers.ProviderOutlook:
			remoteID, etag, err = client.pushOutlook(ctx, profile.Contact(), destination.Source())
		case providers.ProviderCardDAV:
			remoteID, etag, err = client.pushDAV(ctx, profile.Contact(), book, destination.Source())
		default:
			err = errors.New("contact sync is not configured for this account")
		}
		if err != nil {
			return err
		}
		if err := h.userAccounts.PublishContactSyncResult(ctx, destination, remoteID, etag); err != nil {
			return err
		}
	}
	return nil
}

func validOwnedGoogleContactID(remote string) bool {
	if !strings.HasPrefix(remote, "people/") || len(remote) <= len("people/") {
		return false
	}
	for _, c := range strings.TrimPrefix(remote, "people/") {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func validOwnedGraphContactID(remote string) bool {
	return strings.TrimSpace(remote) != "" && remote != "." && remote != ".."
}

func (p *userContactPull) pushGoogle(ctx context.Context, contact models.Contact, source *storage.ContactSource) (string, string, error) {
	return p.pushGoogleAttempt(ctx, contact, source, true)
}

func (p *userContactPull) pushGoogleAttempt(ctx context.Context, contact models.Contact, source *storage.ContactSource, rediscover bool) (string, string, error) {
	remoteID, etag := "", ""
	if source != nil {
		remoteID, etag = source.RemoteID, source.Etag
	}
	if remoteID == "" {
		query := url.Values{"query": {contact.Email}, "readMask": {googleContactPersonFields()}, "pageSize": {"10"}}
		var page googleSearchContactsResponse
		if err := p.getJSON(ctx, googlePeopleAPIBaseURL+"/people:searchContacts?"+query.Encode(), &page); err != nil {
			return "", "", err
		}
		var matches []googlePerson
		for _, result := range page.Results {
			for _, email := range result.Person.EmailAddresses {
				if strings.EqualFold(strings.TrimSpace(email.Value), contact.Email) {
					matches = append(matches, result.Person)
					break
				}
			}
		}
		if len(matches) > 1 {
			return "", "", fmt.Errorf("Gmail has multiple contacts with %s; choose the copy to use before enabling Gofer Sync", contact.Email)
		}
		if len(matches) == 1 {
			remoteID, etag = matches[0].ResourceName, matches[0].Etag
		}
	}
	create := func() (string, string, error) {
		var person googlePerson
		err := p.json(ctx, http.MethodPost, googlePeopleAPIBaseURL+"/people:createContact?personFields="+url.QueryEscape(googleContactPersonFields()), googlePersonFromContact(contact, "", ""), &person)
		if err == nil && !validOwnedGoogleContactID(person.ResourceName) {
			err = errors.New("people api did not return a valid contact resource name")
		}
		return person.ResourceName, person.Etag, err
	}
	if remoteID == "" {
		return create()
	}
	if !validOwnedGoogleContactID(remoteID) {
		return "", "", errors.New("invalid People contact resource name")
	}
	if err := p.h.userAccounts.ValidateContactSyncRemote(ctx, p.destination, remoteID); err != nil {
		return "", "", err
	}
	p.remoteAlias = remoteID
	get := func() (googlePerson, error) {
		var person googlePerson
		err := p.getJSON(ctx, googlePeopleAPIBaseURL+"/"+remoteID+"?personFields="+url.QueryEscape(googleContactPersonFields()), &person)
		return person, err
	}
	if etag == "" {
		person, err := get()
		if err != nil {
			var api googleAPIError
			if isGoogleNotFound(err, &api) && rediscover {
				return p.pushGoogleAttempt(ctx, contact, nil, false)
			}
			return "", "", err
		}
		etag = person.Etag
	}
	write := func(etag string) (googlePerson, error) {
		var person googlePerson
		endpoint := googlePeopleAPIBaseURL + "/" + remoteID + ":updateContact?updatePersonFields=names,emailAddresses,phoneNumbers,organizations,biographies&personFields=" + url.QueryEscape(googleContactPersonFields())
		err := p.json(ctx, http.MethodPatch, endpoint, googlePersonFromContact(contact, remoteID, etag), &person)
		return person, err
	}
	person, err := write(etag)
	if err != nil {
		var api googleAPIError
		if isGoogleNotFound(err, &api) && rediscover {
			// A prior replacement may have reached the provider while its local
			// acknowledgement failed. Repeat exact-email discovery once instead
			// of unconditionally creating another remote resource. A stale search
			// result cannot recurse indefinitely.
			return p.pushGoogleAttempt(ctx, contact, nil, false)
		}
		if api.Status == 400 || api.Status == 409 || api.Status == 412 {
			if latest, getErr := get(); getErr == nil {
				person, err = write(latest.Etag)
			} else {
				// Keep a refresh's Retry-After or lifecycle error. Returning the
				// earlier conflict would exhaust a throttled queue at its last
				// attempt even though the provider cooldown is still active.
				err = getErr
			}
		}
		if err != nil {
			return "", "", err
		}
	}
	if person.ResourceName == "" {
		person.ResourceName = remoteID
	}
	if !validOwnedGoogleContactID(person.ResourceName) {
		return "", "", errors.New("invalid returned People contact resource name")
	}
	return person.ResourceName, person.Etag, nil
}

func (p *userContactPull) pushOutlook(ctx context.Context, contact models.Contact, source *storage.ContactSource) (string, string, error) {
	return p.pushOutlookAttempt(ctx, contact, source, true)
}

func (p *userContactPull) pushOutlookAttempt(ctx context.Context, contact models.Contact, source *storage.ContactSource, rediscover bool) (string, string, error) {
	remoteID := ""
	if source != nil {
		remoteID = source.RemoteID
	}
	if remoteID == "" {
		query := outlookContactListQuery()
		query.Set("$top", "25")
		query.Set("$filter", "emailAddresses/any(a:a/address eq '"+escapeODataString(contact.Email)+"')")
		endpoint := outlookGraphBaseURL + "/me/contacts?" + query.Encode()
		seen := map[string]bool{}
		var matches []outlookContact
		for endpoint != "" {
			if err := validUserContactGraphPage(endpoint); err != nil {
				return "", "", err
			}
			if seen[endpoint] {
				return "", "", errors.New("contact provider repeated a page URL")
			}
			seen[endpoint] = true
			var page outlookContactsResponse
			if err := p.getJSON(ctx, endpoint, &page); err != nil {
				return "", "", err
			}
			for _, remote := range page.Contacts {
				if outlookContactHasEmail(remote, contact.Email) {
					matches = append(matches, remote)
				}
			}
			if len(matches) > 1 {
				return "", "", fmt.Errorf("Outlook has multiple contacts with %s; choose the copy to use before enabling Gofer Sync", contact.Email)
			}
			endpoint = page.NextLink
		}
		if len(matches) == 1 {
			remoteID = matches[0].ID
		}
	}
	create := func() (string, string, error) {
		var remote outlookContact
		err := p.json(ctx, http.MethodPost, outlookGraphBaseURL+"/me/contacts", outlookContactPayloadFromContact(contact), &remote)
		if err == nil && !validOwnedGraphContactID(remote.ID) {
			err = errors.New("graph contacts api did not return a contact id")
		}
		return remote.ID, outlookContactVersion(remote), err
	}
	if remoteID == "" {
		return create()
	}
	if !validOwnedGraphContactID(remoteID) {
		return "", "", errors.New("invalid Graph contact id")
	}
	if err := p.h.userAccounts.ValidateContactSyncRemote(ctx, p.destination, remoteID); err != nil {
		return "", "", err
	}
	p.remoteAlias = remoteID
	var remote outlookContact
	err := p.json(ctx, http.MethodPatch, outlookGraphBaseURL+"/me/contacts/"+url.PathEscape(remoteID), outlookContactPayloadFromContact(contact), &remote)
	if err != nil {
		var api outlookAPIError
		if isOutlookNotFound(err, &api) && rediscover {
			return p.pushOutlookAttempt(ctx, contact, nil, false)
		}
		return "", "", err
	}
	if remote.ID == "" {
		remote.ID = remoteID
	}
	if !validOwnedGraphContactID(remote.ID) {
		return "", "", errors.New("invalid returned Graph contact id")
	}
	return remote.ID, outlookContactVersion(remote), nil
}

func (p *userContactPull) pushDAV(ctx context.Context, contact models.Contact, book models.ContactAddressBook, source *storage.ContactSource) (string, string, error) {
	cfg := p.snapshot.ContactConfig()
	cfg.AddressBookURL, cfg.AddressBooks = book.URL, []models.ContactAddressBook{book}
	password, err := p.snapshot.ContactSyncPassword("")
	if err != nil {
		return "", "", err
	}
	// A stable new resource name lets a retry find a PUT whose response was
	// lost, including successful PUT followed by a failed local acknowledgement.
	newRemote := strings.TrimRight(book.URL, "/") + "/" + uuid.NewSHA1(uuid.NameSpaceURL, []byte(p.snapshot.OwnerID()+"\x00"+p.snapshot.AccountID()+"\x00"+book.ID+"\x00"+contact.ID)).String() + ".vcf"
	remoteID, etag := newRemote, ""
	if source != nil && source.RemoteID != "" {
		remoteID, etag = source.RemoteID, source.Etag
	}
	if err := p.h.userAccounts.ValidateContactSyncRemote(ctx, p.destination, remoteID); err != nil {
		return "", "", err
	}
	p.remoteAlias = remoteID
	if err := p.ready(ctx); err != nil {
		return "", "", err
	}
	if etag == "" {
		latest, err := cardDAVFetchContact(ctx, cfg, password, remoteID)
		if err != nil && !isCardDAVStatus(err, 404, 410) {
			return "", "", p.recordRetry(ctx, err)
		}
		if err == nil {
			etag = latest.etag()
		}
	}
	body, err := renderVCard4([]models.Contact{contact})
	if err != nil {
		return "", "", err
	}
	if err := p.ready(ctx); err != nil {
		return "", "", err
	}
	newEtag, err := cardDAVPut(ctx, cfg, password, remoteID, etag, body)
	if isCardDAVStatus(err, 404, 410) && remoteID != newRemote {
		remoteID = newRemote
		p.remoteAlias = remoteID
		if readyErr := p.ready(ctx); readyErr != nil {
			return "", "", readyErr
		}
		// The deterministic replacement may already exist after a successful
		// PUT whose acknowledgement was lost while the old source was retained.
		latest, fetchErr := cardDAVFetchContact(ctx, cfg, password, remoteID)
		replacementEtag := ""
		if fetchErr == nil {
			replacementEtag = latest.etag()
		} else if !isCardDAVStatus(fetchErr, 404, 410) {
			return "", "", p.recordRetry(ctx, fetchErr)
		}
		if readyErr := p.ready(ctx); readyErr != nil {
			return "", "", readyErr
		}
		newEtag, err = cardDAVPut(ctx, cfg, password, remoteID, replacementEtag, body)
	}
	if isCardDAVStatus(err, http.StatusPreconditionFailed) && source != nil {
		if readyErr := p.ready(ctx); readyErr != nil {
			return "", "", readyErr
		}
		latest, fetchErr := cardDAVFetchContact(ctx, cfg, password, remoteID)
		if fetchErr != nil {
			return "", "", p.recordRetry(ctx, fetchErr)
		}
		if publishErr := p.h.userAccounts.PublishContactSyncVersion(ctx, p.destination, remoteID, latest.etag()); publishErr != nil {
			return "", "", publishErr
		}
		return "", "", errors.New("CardDAV contact changed remotely; sync again before saving this contact")
	}
	if err != nil {
		return "", "", p.recordRetry(ctx, err)
	}
	return remoteID, newEtag, nil
}
