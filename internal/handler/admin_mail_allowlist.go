package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// The allowed mail lists are set on the administrator's Mail security page of
// a managed installation. People in personal and open mode connect only
// their own mailboxes, so they have no lists to set.
const adminMailAllowlistBase = "/admin/security/allowlist"

var adminMailAllowlistActions = []string{
	adminMailAllowlistBase + "/servers",
	adminMailAllowlistBase + "/domains",
	adminMailAllowlistBase + "/providers",
	adminMailAllowlistBase + "/exempt",
}

func (h *Handler) registerAdminMailAllowlistRoutes(adminRoute func(string, http.HandlerFunc)) {
	adminRoute("POST "+adminMailAllowlistBase+"/servers", h.handleAddMailAllowlistServer)
	adminRoute("POST "+adminMailAllowlistBase+"/domains", h.handleAddMailAllowlistDomain)
	adminRoute("POST "+adminMailAllowlistBase+"/entries/{id}/delete", h.handleDeleteMailAllowlistEntry)
	adminRoute("POST "+adminMailAllowlistBase+"/providers", h.handleSetMailAllowlistProvider)
	adminRoute("POST "+adminMailAllowlistBase+"/exempt", h.handleSetMailAllowlistExempt)
}

// mailAllowlistAdminData loads the lists, the people they don't apply to and
// the mailboxes they stop from connecting, with the tokens for the page's
// forms. It is nil when Gofer doesn't keep people's mail in their own stores.
func (h *Handler) mailAllowlistAdminData(ctx context.Context, tokens map[string]string) (*models.MailAllowlistAdminData, error) {
	if h.ownedMailbox == nil {
		return nil, nil
	}
	entries, err := h.db.ListMailAllowlistEntries(ctx)
	if err != nil {
		return nil, err
	}
	data := &models.MailAllowlistAdminData{}
	for _, entry := range entries {
		switch entry.Kind {
		case storage.MailAllowlistServer:
			data.Servers = append(data.Servers, models.MailAllowlistAdminEntry{ID: entry.ID, Value: entry.Value})
		case storage.MailAllowlistDomain:
			data.Domains = append(data.Domains, models.MailAllowlistAdminEntry{ID: entry.ID, Value: entry.Value})
		case storage.MailAllowlistProvider:
			data.Gmail = data.Gmail || entry.Value == providers.ProviderGmail
			data.Outlook = data.Outlook || entry.Value == providers.ProviderOutlook
		}
	}
	for _, entry := range append(append([]models.MailAllowlistAdminEntry(nil), data.Servers...), data.Domains...) {
		action := adminMailAllowlistBase + "/entries/" + entry.ID + "/delete"
		tokens[action] = auth.CSRFToken(ctx, http.MethodPost, action)
	}
	for _, action := range adminMailAllowlistActions {
		tokens[action] = auth.CSRFToken(ctx, http.MethodPost, action)
	}
	users, refused, err := h.mailAllowlistRefusals(ctx)
	if err != nil {
		return nil, err
	}
	exempt, err := h.db.ListMailAllowlistExemptUsers(ctx)
	if err != nil {
		return nil, err
	}
	for _, user := range users {
		if _, ok := slicesIndex(exempt, user.ID); ok {
			data.Exempt = append(data.Exempt, user)
		} else {
			data.Candidates = append(data.Candidates, user)
		}
	}
	for _, account := range refused {
		data.Refused = append(data.Refused, account.MailAllowlistRefusedAccount)
	}
	return data, nil
}

func slicesIndex(sorted []string, value string) (int, bool) {
	i := sort.SearchStrings(sorted, value)
	return i, i < len(sorted) && sorted[i] == value
}

type mailAllowlistRefusal struct {
	models.MailAllowlistRefusedAccount
	owner string
}

// mailAllowlistRefusals lists every webmail user, and the mailboxes the lists
// stop from connecting, by address.
func (h *Handler) mailAllowlistRefusals(ctx context.Context) ([]models.AdminWebmailUserOption, []mailAllowlistRefusal, error) {
	list, err := h.db.GetMailAllowlist(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !list.Limited() {
		scope, err := h.adminWebmailScope(ctx, "")
		return scope.Users, nil, err
	}
	exempt, err := h.db.ListMailAllowlistExemptUsers(ctx)
	if err != nil {
		return nil, nil, err
	}
	var refused []mailAllowlistRefusal
	scope, err := h.eachUserDiagnostic(ctx, "", storage.UserDiagnosticsTransports, func(_ context.Context, owner models.AdminWebmailUserOption, data storage.UserDiagnostics, _ storage.DiagnosticsActor) error {
		if _, ok := slicesIndex(exempt, owner.ID); ok {
			return nil
		}
		for _, account := range data.Transports {
			if err := list.CheckMailboxTransports(account); err != nil {
				refused = append(refused, mailAllowlistRefusal{owner: owner.ID, MailAllowlistRefusedAccount: models.MailAllowlistRefusedAccount{
					ID: account.ID, Email: account.Email, Username: owner.Username, Reason: mailAllowlistRefusalReason(err)}})
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(refused, func(i, j int) bool {
		return strings.ToLower(refused[i].Email) < strings.ToLower(refused[j].Email)
	})
	return scope.Users, refused, nil
}

// mailAllowlistRefusalReason says, for the administrator, which rule a
// mailbox breaks.
func mailAllowlistRefusalReason(err error) string {
	var refusal *storage.MailAllowlistError
	if !errors.As(err, &refusal) {
		return err.Error()
	}
	switch refusal.Rule {
	case storage.MailAllowlistDomain:
		return "@" + refusal.Value + " isn't an allowed domain"
	case storage.MailAllowlistProvider:
		return refusal.Value + " isn't allowed"
	default:
		return refusal.Value + " isn't an allowed server"
	}
}

// changeMailAllowlist makes one change to the lists, then stops the mailboxes
// it refuses and reconnects the ones it lets in again, so neither waits for
// its next check.
func (h *Handler) changeMailAllowlist(w http.ResponseWriter, r *http.Request, change func(ctx context.Context, admin string) (notice string, err error)) {
	if h.ownedMailbox == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := h.ownedAdminDiagnosticContext(r.Context())
	defer cancel()
	r = r.WithContext(ctx)
	if !h.requireRecentAdminSecurityStepUp(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.redirectAdminSecurity(w, r, "", "Invalid form data.")
		return
	}
	if err := h.validateMailSecurityAdministrator(ctx); err != nil {
		h.redirectAdminSecurity(w, r, "", "Administrator access is no longer available.")
		return
	}
	_, before, err := h.mailAllowlistRefusals(ctx)
	if err != nil {
		log.Printf("allowed mail lists: list refused mailboxes: %v", err)
		h.redirectAdminSecurity(w, r, "", "Could not check the mailboxes the lists apply to.")
		return
	}
	notice, err := change(ctx, h.userID(ctx))
	if err != nil {
		h.redirectAdminSecurity(w, r, "", err.Error())
		return
	}
	_, after, err := h.mailAllowlistRefusals(ctx)
	if err != nil {
		log.Printf("allowed mail lists: list refused mailboxes after a change: %v", err)
		if h.ownedMailbox.userIMAP != nil {
			h.ownedMailbox.userIMAP.StopMailSecuritySessions()
		}
	} else {
		h.applyMailAllowlistChange(ctx, before, after)
	}
	h.redirectAdminSecurity(w, r, notice, "")
}

func (h *Handler) applyMailAllowlistChange(ctx context.Context, before, after []mailAllowlistRefusal) {
	wasRefused := make(map[string]bool, len(before))
	for _, account := range before {
		wasRefused[account.ID] = true
	}
	for _, account := range after {
		if !wasRefused[account.ID] {
			h.stopMailSecurityAccount(account.ID)
		}
		delete(wasRefused, account.ID)
	}
	if h.ownedMailbox.userIMAP == nil {
		return
	}
	for id := range wasRefused {
		if err := h.ownedMailbox.userIMAP.RestartAccount(ctx, id); err != nil {
			log.Printf("allowed mail lists: reconnect account %s: %v", id, err)
		}
	}
}

func (h *Handler) handleAddMailAllowlistServer(w http.ResponseWriter, r *http.Request) {
	h.addMailAllowlistEntry(w, r, storage.MailAllowlistServer)
}

func (h *Handler) handleAddMailAllowlistDomain(w http.ResponseWriter, r *http.Request) {
	h.addMailAllowlistEntry(w, r, storage.MailAllowlistDomain)
}

func (h *Handler) addMailAllowlistEntry(w http.ResponseWriter, r *http.Request, kind string) {
	h.changeMailAllowlist(w, r, func(ctx context.Context, admin string) (string, error) {
		value, err := h.db.AddMailAllowlistEntry(ctx, kind, r.FormValue("value"), admin)
		if errors.Is(err, storage.ErrMailAllowlistEntry) {
			if kind == storage.MailAllowlistDomain {
				return "", errors.New("Enter a domain, such as example.org, or *.example.org for the domains below it.")
			}
			return "", errors.New("Enter a host name or IP address, such as imap.example.org, or *.example.org for the hosts below it.")
		}
		if err != nil {
			log.Printf("allowed mail lists: add %s: %v", kind, err)
			return "", errors.New("Could not add it to the list.")
		}
		if kind == storage.MailAllowlistDomain {
			return "Mailboxes with @" + value + " addresses are now allowed.", nil
		}
		return "Mailboxes can now connect to " + value + ".", nil
	})
}

func (h *Handler) handleDeleteMailAllowlistEntry(w http.ResponseWriter, r *http.Request) {
	h.changeMailAllowlist(w, r, func(ctx context.Context, _ string) (string, error) {
		if err := h.db.DeleteMailAllowlistEntry(ctx, r.PathValue("id")); err != nil {
			log.Printf("allowed mail lists: remove entry: %v", err)
			return "", errors.New("Could not remove it from the list.")
		}
		return "Removed from the list.", nil
	})
}

func (h *Handler) handleSetMailAllowlistProvider(w http.ResponseWriter, r *http.Request) {
	h.changeMailAllowlist(w, r, func(ctx context.Context, admin string) (string, error) {
		provider := strings.TrimSpace(r.FormValue("provider"))
		allowed := r.FormValue("allowed") == "true"
		if provider != providers.ProviderGmail && provider != providers.ProviderOutlook {
			return "", errors.New("Choose Gmail or Outlook.")
		}
		if err := h.db.SetMailAllowlistProvider(ctx, provider, allowed, admin); err != nil {
			log.Printf("allowed mail lists: set %s: %v", provider, err)
			return "", errors.New("Could not change it.")
		}
		name := "Gmail"
		if provider == providers.ProviderOutlook {
			name = "Outlook"
		}
		if allowed {
			return name + " mailboxes are now allowed.", nil
		}
		return name + " mailboxes are no longer allowed while the servers are limited.", nil
	})
}

func (h *Handler) handleSetMailAllowlistExempt(w http.ResponseWriter, r *http.Request) {
	h.changeMailAllowlist(w, r, func(ctx context.Context, admin string) (string, error) {
		userID := strings.TrimSpace(r.FormValue("user_id"))
		exempt := r.FormValue("exempt") == "true"
		users, err := h.db.ListAdminWebmailUsers(ctx)
		if err != nil {
			log.Printf("allowed mail lists: list users: %v", err)
			return "", errors.New("Could not change it.")
		}
		username := ""
		for _, user := range users {
			if user.ID == userID {
				username = user.Username
			}
		}
		if username == "" {
			return "", errors.New("Choose a webmail user.")
		}
		if err := h.db.SetMailAllowlistExempt(ctx, userID, exempt, admin); err != nil {
			log.Printf("allowed mail lists: exempt %s: %v", userID, err)
			return "", errors.New("Could not change it.")
		}
		if exempt {
			return "The lists no longer apply to " + username + ".", nil
		}
		return "The lists apply to " + username + " again.", nil
	})
}

// markExceptionsOutsideAllowlist flags the exceptions for servers the allowed
// mail servers leave out. HTTP discovery reaches the domain itself or its
// autoconfig host.
func (h *Handler) markExceptionsOutsideAllowlist(ctx context.Context, exceptions []models.MailSecurityException) error {
	list, err := h.db.GetMailAllowlist(ctx)
	if err != nil || !list.ServersLimited() {
		return err
	}
	for i := range exceptions {
		item := &exceptions[i]
		if item.Kind == models.MailSecurityExceptionHTTPDiscovery {
			item.OutsideAllowlist = !list.AllowsServer(item.Host) && !list.AllowsServer("autoconfig."+item.Host)
		} else {
			item.OutsideAllowlist = !list.AllowsServer(item.Host)
		}
	}
	return nil
}
