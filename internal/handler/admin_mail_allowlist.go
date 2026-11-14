package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/admin"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/policy"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// The allowed mail lists are set on the administrator's Mail security page of
// a managed installation. People in personal and open mode connect only
// their own mailboxes, so they have no lists to set. The lists are the
// Default policy's (internal/policy), and an exemption is the exemption
// policy assigned to someone; each change is recorded as a policy change.
const adminMailAllowlistBase = "/admin/security/allowlist"

var adminMailAllowlistActions = []string{
	adminMailAllowlistBase + "/servers",
	adminMailAllowlistBase + "/servers/remove",
	adminMailAllowlistBase + "/domains",
	adminMailAllowlistBase + "/domains/remove",
	adminMailAllowlistBase + "/providers",
	adminMailAllowlistBase + "/exempt",
}

func (h *Handler) registerAdminMailAllowlistRoutes(adminRoute func(string, http.HandlerFunc)) {
	adminRoute("POST "+adminMailAllowlistBase+"/servers", h.handleChangeMailAllowlistEntry(storage.MailAllowlistServer, true))
	adminRoute("POST "+adminMailAllowlistBase+"/servers/remove", h.handleChangeMailAllowlistEntry(storage.MailAllowlistServer, false))
	adminRoute("POST "+adminMailAllowlistBase+"/domains", h.handleChangeMailAllowlistEntry(storage.MailAllowlistDomain, true))
	adminRoute("POST "+adminMailAllowlistBase+"/domains/remove", h.handleChangeMailAllowlistEntry(storage.MailAllowlistDomain, false))
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
	lists, err := h.auth.MailLists(ctx, h.userID(ctx))
	if err != nil {
		return nil, err
	}
	data := &models.MailAllowlistAdminData{Gmail: lists.Gmail, Outlook: lists.Outlook}
	for _, value := range lists.Servers {
		data.Servers = append(data.Servers, models.MailAllowlistAdminEntry{Value: value})
	}
	for _, value := range lists.Domains {
		data.Domains = append(data.Domains, models.MailAllowlistAdminEntry{Value: value})
	}
	for _, action := range adminMailAllowlistActions {
		tokens[action] = auth.CSRFToken(ctx, http.MethodPost, action)
	}
	scope, err := h.adminWebmailScope(ctx, "")
	if err != nil {
		return nil, err
	}
	refused, checkedAt, err := h.currentMailRefusals(ctx)
	if err != nil {
		return nil, err
	}
	data.RefusalsCheckedAt = checkedAt
	if data.Limited, err = policy.MailListsLimitAnyone(ctx, h.db.Read()); err != nil {
		return nil, err
	}
	for _, user := range scope.Users {
		if _, ok := slicesIndex(lists.Exempt, user.ID); ok {
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

// mailAllowlistRefusalReason says, for the administrator, which rule a
// mailbox breaks.
func mailAllowlistRefusalReason(ctx context.Context, err error) string {
	var refusal *storage.MailAllowlistError
	if !errors.As(err, &refusal) {
		return err.Error()
	}
	switch refusal.Rule {
	case storage.MailAllowlistDomain:
		return i18n.T(ctx, "auth.allowlist.refused_domain", "domain", refusal.Value)
	case storage.MailAllowlistProvider:
		return i18n.T(ctx, "auth.allowlist.refused_provider", "provider", refusal.Value)
	default:
		return i18n.T(ctx, "auth.allowlist.refused_server", "server", refusal.Value)
	}
}

// changeMailAllowlist makes one change to the lists, then stops the mailboxes
// it refuses and reconnects the ones it lets in again, so neither waits for
// its next connection.
func (h *Handler) changeMailAllowlist(w http.ResponseWriter, r *http.Request, change func(ctx context.Context, actor admin.Actor) (notice string, err error)) {
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
		h.redirectAdminSecurity(w, r, "", i18n.T(r.Context(), "auth.admin.form_invalid"))
		return
	}
	actor, ok := admin.ActorFromContext(ctx)
	if err := h.validateMailSecurityAdministrator(ctx); err != nil || !ok {
		h.redirectAdminSecurity(w, r, "", i18n.T(r.Context(), "auth.admin.access_lost"))
		return
	}
	notice, err := change(ctx, actor)
	if err != nil {
		h.redirectAdminSecurity(w, r, "", err.Error())
		return
	}
	// The mailboxes the change refuses stop now, and the ones it lets in
	// reconnect; when that can't be told, every mail session ends.
	if _, _, err := h.checkMailRefusals(ctx); err != nil {
		log.Printf("allowed mail lists: check refused mailboxes after a change: %v", err)
	}
	h.redirectAdminSecurity(w, r, notice, "")
}

// mailAllowlistChangeFailed is what the page says about a change the
// service refused: verification or access when that's why, else failed.
func mailAllowlistChangeFailed(ctx context.Context, err error, failed, doing string) error {
	switch admin.CodeOf(err) {
	case admin.CodeVerificationRequired:
		return errors.New(i18n.T(ctx, "auth.admin.verify.exceptions"))
	case admin.CodeForbidden:
		return errors.New(i18n.T(ctx, "auth.admin.access_lost"))
	}
	log.Printf("allowed mail lists: %s: %v", doing, err)
	return errors.New(i18n.T(ctx, failed))
}

// handleChangeMailAllowlistEntry adds a server or domain to the Default
// policy's list, or takes one off.
func (h *Handler) handleChangeMailAllowlistEntry(kind string, add bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.changeMailAllowlist(w, r, func(ctx context.Context, actor admin.Actor) (string, error) {
			value, err := h.auth.ChangeMailList(ctx, auth.MailListChange{
				ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, List: kind, Value: r.FormValue("value"), Add: add,
			})
			switch {
			case errors.Is(err, storage.ErrMailAllowlistEntry) && kind == storage.MailAllowlistDomain:
				return "", errors.New(i18n.T(r.Context(), "auth.allowlist.domain_invalid"))
			case errors.Is(err, storage.ErrMailAllowlistEntry):
				return "", errors.New(i18n.T(r.Context(), "auth.allowlist.server_invalid"))
			case err != nil && add:
				return "", mailAllowlistChangeFailed(r.Context(), err, "auth.allowlist.add_failed", "add "+kind)
			case err != nil:
				return "", mailAllowlistChangeFailed(r.Context(), err, "auth.allowlist.remove_failed", "remove "+kind)
			case !add:
				return i18n.T(r.Context(), "auth.allowlist.removed"), nil
			case kind == storage.MailAllowlistDomain:
				return i18n.T(r.Context(), "auth.allowlist.domain_added", "domain", value), nil
			}
			return i18n.T(r.Context(), "auth.allowlist.server_added", "server", value), nil
		})
	}
}

func (h *Handler) handleSetMailAllowlistProvider(w http.ResponseWriter, r *http.Request) {
	h.changeMailAllowlist(w, r, func(ctx context.Context, actor admin.Actor) (string, error) {
		provider := strings.TrimSpace(r.FormValue("provider"))
		allowed := r.FormValue("allowed") == "true"
		if provider != providers.ProviderGmail && provider != providers.ProviderOutlook {
			return "", errors.New(i18n.T(r.Context(), "auth.allowlist.choose_provider"))
		}
		if _, err := h.auth.ChangeMailList(ctx, auth.MailListChange{
			ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, List: storage.MailAllowlistProvider, Value: provider, Add: allowed,
		}); err != nil {
			return "", mailAllowlistChangeFailed(r.Context(), err, "auth.allowlist.change_failed", "set "+provider)
		}
		name := "Gmail"
		if provider == providers.ProviderOutlook {
			name = "Outlook"
		}
		if allowed {
			return i18n.T(r.Context(), "auth.allowlist.provider_allowed", "provider", name), nil
		}
		return i18n.T(r.Context(), "auth.allowlist.provider_blocked", "provider", name), nil
	})
}

func (h *Handler) handleSetMailAllowlistExempt(w http.ResponseWriter, r *http.Request) {
	h.changeMailAllowlist(w, r, func(ctx context.Context, actor admin.Actor) (string, error) {
		userID := strings.TrimSpace(r.FormValue("user_id"))
		exempt := r.FormValue("exempt") == "true"
		users, err := h.db.ListAdminWebmailUsers(ctx)
		if err != nil {
			log.Printf("allowed mail lists: list users: %v", err)
			return "", errors.New(i18n.T(r.Context(), "auth.allowlist.change_failed"))
		}
		username := ""
		for _, user := range users {
			if user.ID == userID {
				username = user.Username
			}
		}
		if username == "" {
			return "", errors.New(i18n.T(r.Context(), "auth.allowlist.choose_user"))
		}
		_, err = h.auth.SetMailListExemption(ctx, actor.UserID, actor.SessionID, userID, exempt)
		if errors.Is(err, policy.ErrTargetNotFound) {
			return "", errors.New(i18n.T(r.Context(), "auth.allowlist.choose_user"))
		}
		if err != nil {
			return "", mailAllowlistChangeFailed(r.Context(), err, "auth.allowlist.change_failed", "exempt "+userID)
		}
		if exempt {
			return i18n.T(r.Context(), "auth.allowlist.exempt", "user", username), nil
		}
		return i18n.T(r.Context(), "auth.allowlist.unexempt", "user", username), nil
	})
}

// markExceptionsOutsideAllowlist flags the exceptions for servers the allowed
// mail servers leave out. HTTP discovery reaches the domain itself or its
// autoconfig host.
func (h *Handler) markExceptionsOutsideAllowlist(ctx context.Context, exceptions []models.MailSecurityException) error {
	list, err := policy.DefaultMailAllowlist(ctx, h.db.Read())
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
