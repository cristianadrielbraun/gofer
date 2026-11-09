package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Saved outbound proxies are managed on the Mail security page, next to the
// exceptions: by an administrator, or by the owner in personal and open mode.
// People only pick one of them for their accounts.

func outboundProxyView(p storage.OutboundProxy) models.OutboundProxyView {
	return models.OutboundProxyView{ID: p.ID, Name: p.Name, Type: p.Type, Host: p.Host, Port: p.Port,
		Username: p.Username, HasPassword: len(p.EncryptedPassword) > 0, IsDefault: p.IsDefault, Everyone: p.Everyone}
}

// addOutboundProxyData fills in the administrator's proxies of the Mail
// security page whose forms post under base, with their forms' CSRF tokens.
// On a managed installation's admin page it adds who each proxy is for and
// whether people may add their own.
func (h *Handler) addOutboundProxyData(ctx context.Context, data *models.MailSecurityAdminData, base string) error {
	if h.accountStore == nil {
		return nil
	}
	saved, err := h.accountStore.ListOutboundProxies(ctx)
	if err != nil {
		return err
	}
	data.ProxyPeople = h.ownedMailbox != nil && base == "/admin/security"
	var users []models.AdminWebmailUserOption
	if data.ProxyPeople {
		if data.PeopleMayAddProxies, err = h.accountStore.PeopleMayAddOutboundProxies(ctx); err != nil {
			return err
		}
		scope, err := h.adminWebmailScope(ctx, "")
		if err != nil {
			return err
		}
		users = scope.Users
		for _, action := range []string{base + "/proxies/policy"} {
			data.CSRFTokens[action] = auth.CSRFToken(ctx, http.MethodPost, action)
		}
	}
	for _, p := range saved {
		view := outboundProxyView(p)
		if data.ProxyPeople && !p.Everyone {
			chosen, err := h.db.OutboundProxyUsers(ctx, p.ID)
			if err != nil {
				return err
			}
			for _, user := range users {
				if _, ok := slicesIndex(chosen, user.ID); ok {
					view.People = append(view.People, user)
				} else {
					view.Candidates = append(view.Candidates, user)
				}
			}
		}
		data.Proxies = append(data.Proxies, view)
	}
	for _, action := range outboundProxyActions(base, data.Proxies, data.ProxyPeople) {
		data.CSRFTokens[action] = auth.CSRFToken(ctx, http.MethodPost, action)
	}
	return nil
}

func outboundProxyActions(base string, proxies []models.OutboundProxyView, people bool) []string {
	actions := []string{base + "/proxies"}
	for _, p := range proxies {
		actions = append(actions, base+"/proxies/"+p.ID+"/delete")
		if people {
			actions = append(actions, base+"/proxies/"+p.ID+"/everyone", base+"/proxies/"+p.ID+"/people")
		}
	}
	return actions
}

// handleSetOutboundProxyEveryone makes a proxy for everyone, or only for
// the people chosen for it.
func (h *Handler) handleSetOutboundProxyEveryone(w http.ResponseWriter, r *http.Request) {
	// Only a managed installation has several people to choose from.
	if h.ownedMailbox == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := h.ownedAdminDiagnosticContext(r.Context())
	defer cancel()
	r = r.WithContext(ctx)
	if !h.prepareMailSecurityChange(w, r) {
		return
	}
	if err := h.db.SetOutboundProxyEveryone(ctx, r.PathValue("id"), r.FormValue("everyone") == "true"); err != nil {
		h.redirectOutboundProxyError(w, r, err)
		return
	}
	h.reconnectAllAccounts(ctx)
	h.redirectAdminSecurity(w, r, i18n.T(ctx, "admin.proxies.people_saved"), "")
}

// handleSetOutboundProxyPerson chooses a person for a proxy, or takes them
// off it.
func (h *Handler) handleSetOutboundProxyPerson(w http.ResponseWriter, r *http.Request) {
	// Only a managed installation has several people to choose from.
	if h.ownedMailbox == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := h.ownedAdminDiagnosticContext(r.Context())
	defer cancel()
	r = r.WithContext(ctx)
	if !h.prepareMailSecurityChange(w, r) {
		return
	}
	userID := strings.TrimSpace(r.FormValue("user_id"))
	if userID == "" {
		h.redirectAdminSecurity(w, r, "", i18n.T(ctx, "auth.admin.form_invalid"))
		return
	}
	if err := h.db.SetOutboundProxyUser(ctx, r.PathValue("id"), userID, r.FormValue("chosen") == "true"); err != nil {
		h.redirectOutboundProxyError(w, r, err)
		return
	}
	h.reconnectAllAccounts(ctx)
	h.redirectAdminSecurity(w, r, i18n.T(ctx, "admin.proxies.people_saved"), "")
}

// handleSetOwnOutboundProxiesPolicy lets people add proxies of their own, or
// stops them.
func (h *Handler) handleSetOwnOutboundProxiesPolicy(w http.ResponseWriter, r *http.Request) {
	// Only a managed installation has several people to choose from.
	if h.ownedMailbox == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := h.ownedAdminDiagnosticContext(r.Context())
	defer cancel()
	r = r.WithContext(ctx)
	if !h.prepareMailSecurityChange(w, r) {
		return
	}
	allowed := r.FormValue("people_add") == "yes"
	if err := h.db.SetPeopleMayAddOutboundProxies(ctx, allowed); err != nil {
		log.Printf("mail security: set own proxies policy: %v", err)
		h.redirectAdminSecurity(w, r, "", i18n.T(ctx, "admin.proxies.policy_failed"))
		return
	}
	if !allowed {
		// Accounts that used their owner's proxies go back to the default.
		h.reconnectAllAccounts(ctx)
	}
	notice := "admin.proxies.policy_off"
	if allowed {
		notice = "admin.proxies.policy_on"
	}
	h.redirectAdminSecurity(w, r, i18n.T(ctx, notice), "")
}

func (h *Handler) redirectOutboundProxyError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, storage.ErrOutboundProxyNotFound) {
		h.redirectAdminSecurity(w, r, "", i18n.T(r.Context(), "admin.proxies.not_found"))
		return
	}
	log.Printf("mail security: change proxy: %v", err)
	h.redirectAdminSecurity(w, r, "", i18n.T(r.Context(), "admin.proxies.people_failed"))
}

// handleSaveOutboundProxy adds a proxy, or changes the one named by the
// form's id. A password left empty keeps the saved one.
func (h *Handler) handleSaveOutboundProxy(w http.ResponseWriter, r *http.Request) {
	if h.ownedMailbox != nil {
		ctx, cancel := h.ownedAdminDiagnosticContext(r.Context())
		defer cancel()
		r = r.WithContext(ctx)
	}
	if !h.prepareMailSecurityChange(w, r) {
		return
	}
	input := outboundProxyInputFromForm(r)
	if _, err := h.accountStore.SaveOutboundProxy(r.Context(), input, h.userID(r.Context())); err != nil {
		switch {
		case errors.Is(err, storage.ErrOutboundProxyNameTaken):
			h.redirectAdminSecurity(w, r, "", i18n.T(r.Context(), "admin.proxies.name_taken"))
		case errors.Is(err, storage.ErrOutboundProxyNotFound):
			h.redirectAdminSecurity(w, r, "", i18n.T(r.Context(), "admin.proxies.not_found"))
		default:
			h.redirectAdminSecurity(w, r, "", i18n.T(r.Context(), "admin.proxies.invalid", "reason", err.Error()))
		}
		return
	}
	h.reconnectAllAccounts(r.Context())
	h.redirectAdminSecurity(w, r, i18n.T(r.Context(), "admin.proxies.saved", "name", strings.TrimSpace(input.Name)), "")
}

func (h *Handler) handleDeleteOutboundProxy(w http.ResponseWriter, r *http.Request) {
	if h.ownedMailbox != nil {
		ctx, cancel := h.ownedAdminDiagnosticContext(r.Context())
		defer cancel()
		r = r.WithContext(ctx)
	}
	if !h.prepareMailSecurityChange(w, r) {
		return
	}
	if err := h.accountStore.DeleteOutboundProxy(r.Context(), "", r.PathValue("id")); err != nil {
		if errors.Is(err, storage.ErrOutboundProxyNotFound) {
			h.redirectAdminSecurity(w, r, "", i18n.T(r.Context(), "admin.proxies.not_found"))
			return
		}
		log.Printf("mail security: delete proxy: %v", err)
		h.redirectAdminSecurity(w, r, "", i18n.T(r.Context(), "admin.proxies.delete_failed"))
		return
	}
	h.reconnectAllAccounts(r.Context())
	h.redirectAdminSecurity(w, r, i18n.T(r.Context(), "admin.proxies.deleted"), "")
}

// prepareMailSecurityChange runs the checks every change on the Mail
// security page runs first, and answers the request when one fails.
func (h *Handler) prepareMailSecurityChange(w http.ResponseWriter, r *http.Request) bool {
	if !h.requireRecentAdminSecurityStepUp(w, r) {
		return false
	}
	if err := r.ParseForm(); err != nil {
		h.redirectAdminSecurity(w, r, "", i18n.T(r.Context(), "auth.admin.form_invalid"))
		return false
	}
	if err := h.validateMailSecurityAdministrator(r.Context()); err != nil {
		h.redirectAdminSecurity(w, r, "", i18n.T(r.Context(), "auth.admin.access_lost"))
		return false
	}
	if h.accountStore == nil {
		h.redirectAdminSecurity(w, r, "", i18n.T(r.Context(), "admin.proxies.unavailable"))
		return false
	}
	return true
}

// reconnectAllAccounts reconnects every account after a proxy changed, so
// long-lived connections, such as IMAP IDLE, don't keep the old route. New
// connections take the new one anyway.
func (h *Handler) reconnectAllAccounts(ctx context.Context) {
	if owned := h.ownedMailbox; owned != nil {
		if owned.userIMAP == nil || owned.userStorage == nil {
			return
		}
		rows, err := owned.userStorage.System().Read().QueryContext(ctx, `SELECT account_id FROM gofer_account_directory WHERE state = 'active'`)
		if err != nil {
			log.Printf("mail security: list accounts to reconnect: %v", err)
			return
		}
		var ids []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			if err := owned.userIMAP.RestartAccount(ctx, id); err != nil && !errors.Is(err, storage.ErrAccountRoute) {
				log.Printf("mail security: reconnect account %s: %v", id, err)
			}
		}
		return
	}
	if h.syncer == nil {
		return
	}
	rows, err := h.db.Read().QueryContext(ctx, `SELECT id FROM accounts WHERE COALESCE(is_deleting, 0) = 0`)
	if err != nil {
		log.Printf("mail security: list accounts to reconnect: %v", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		h.reconnectAccount(id)
	}
}

// reconnectAccount drops the account's open connections in open and personal
// mode, so its next ones take its current proxy.
func (h *Handler) reconnectAccount(id string) {
	if h.syncer == nil {
		return
	}
	h.closeBodyClient(id)
	h.syncer.RestartAccount(id)
}

// outboundProxyInputFromForm reads a proxy's fields from its form. A
// password left empty keeps the saved one, unless clear_password is ticked.
func outboundProxyInputFromForm(r *http.Request) config.OutboundProxyInput {
	port, err := strconv.Atoi(strings.TrimSpace(r.FormValue("port")))
	if err != nil {
		port = 0
	}
	input := config.OutboundProxyInput{
		ID:        strings.TrimSpace(r.FormValue("id")),
		Name:      r.FormValue("name"),
		Type:      r.FormValue("type"),
		Host:      r.FormValue("host"),
		Port:      port,
		Username:  r.FormValue("username"),
		IsDefault: r.FormValue("default") == "yes",
	}
	if password := r.FormValue("password"); password != "" {
		input.Password = &password
	} else if r.FormValue("clear_password") == "yes" || input.ID == "" {
		empty := ""
		input.Password = &empty
	}
	return input
}
