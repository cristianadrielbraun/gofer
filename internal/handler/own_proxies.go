package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// On a managed installation whose administrator lets people add proxies of
// their own, each person keeps theirs in Settings, Security. They are only
// theirs, can't be the default, and must have a public address.
const ownProxiesBase = "/settings/security"

// proxyStore is the account store whose central proxies this handler
// manages: its own, or, for the handler of people's own stores, the central
// one.
func (h *Handler) proxyStore() *config.AccountStore {
	if h.accountStore != nil {
		return h.accountStore
	}
	if h.userAccounts != nil {
		return h.userAccounts.Proxies()
	}
	return nil
}

func (h *Handler) registerOwnProxiesRoutes(register func(string, http.HandlerFunc)) {
	register("POST "+ownProxiesBase+"/proxies", h.handleSaveOwnProxy)
	register("POST "+ownProxiesBase+"/proxies/{id}/delete", h.handleDeleteOwnProxy)
}

// ownProxiesData loads the signed-in person's own proxies for their security
// settings. It is nil where the owner manages proxies with the exceptions,
// and while people may not add their own.
func (h *Handler) ownProxiesData(r *http.Request) (*models.MailSecurityAdminData, error) {
	store := h.proxyStore()
	if h.ownerManagesMailSecurity() || store == nil {
		return nil, nil
	}
	ctx := r.Context()
	allowed, err := store.PeopleMayAddOutboundProxies(ctx)
	if err != nil || !allowed {
		return nil, err
	}
	own, err := store.ListOwnOutboundProxies(ctx, h.userID(ctx))
	if err != nil {
		return nil, err
	}
	data := &models.MailSecurityAdminData{
		ActionBase:          ownProxiesBase,
		OwnProxies:          true,
		PeopleMayAddProxies: true,
		Notice:              strings.TrimSpace(r.URL.Query().Get("proxy_notice")),
		Error:               strings.TrimSpace(r.URL.Query().Get("proxy_error")),
		CSRFTokens:          map[string]string{},
	}
	for _, p := range own {
		data.Proxies = append(data.Proxies, outboundProxyView(p))
	}
	for _, action := range outboundProxyActions(ownProxiesBase, data.Proxies, false) {
		data.CSRFTokens[action] = auth.CSRFToken(ctx, http.MethodPost, action)
	}
	return data, nil
}

func (h *Handler) handleSaveOwnProxy(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.redirectOwnProxies(w, r, "", i18n.T(r.Context(), "auth.admin.form_invalid"))
		return
	}
	store := h.proxyStore()
	if h.ownerManagesMailSecurity() || store == nil {
		http.NotFound(w, r)
		return
	}
	ctx, owner := r.Context(), h.userID(r.Context())
	input := outboundProxyInputFromForm(r)
	input.OwnerUserID, input.IsDefault = owner, false
	if _, err := store.SaveOutboundProxy(ctx, input, owner); err != nil {
		switch {
		case errors.Is(err, config.ErrOwnOutboundProxiesOff):
			h.redirectOwnProxies(w, r, "", i18n.T(ctx, "own_proxies.off"))
		case errors.Is(err, storage.ErrOutboundProxyNameTaken):
			h.redirectOwnProxies(w, r, "", i18n.T(ctx, "admin.proxies.name_taken"))
		case errors.Is(err, storage.ErrOutboundProxyNotFound):
			h.redirectOwnProxies(w, r, "", i18n.T(ctx, "admin.proxies.not_found"))
		default:
			h.redirectOwnProxies(w, r, "", i18n.T(ctx, "admin.proxies.invalid", "reason", err.Error()))
		}
		return
	}
	h.reconnectUserAccounts(ctx, owner)
	h.redirectOwnProxies(w, r, i18n.T(ctx, "admin.proxies.saved", "name", strings.TrimSpace(input.Name)), "")
}

func (h *Handler) handleDeleteOwnProxy(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.redirectOwnProxies(w, r, "", i18n.T(r.Context(), "auth.admin.form_invalid"))
		return
	}
	store := h.proxyStore()
	if h.ownerManagesMailSecurity() || store == nil {
		http.NotFound(w, r)
		return
	}
	ctx, owner := r.Context(), h.userID(r.Context())
	if err := store.DeleteOutboundProxy(ctx, owner, r.PathValue("id")); err != nil {
		if errors.Is(err, storage.ErrOutboundProxyNotFound) {
			h.redirectOwnProxies(w, r, "", i18n.T(ctx, "admin.proxies.not_found"))
			return
		}
		log.Printf("own proxies: delete: %v", err)
		h.redirectOwnProxies(w, r, "", i18n.T(ctx, "admin.proxies.delete_failed"))
		return
	}
	h.reconnectUserAccounts(ctx, owner)
	h.redirectOwnProxies(w, r, i18n.T(ctx, "admin.proxies.deleted"), "")
}

func (h *Handler) redirectOwnProxies(w http.ResponseWriter, r *http.Request, notice, message string) {
	values := url.Values{}
	if notice != "" {
		values.Set("proxy_notice", notice)
	}
	if message != "" {
		values.Set("proxy_error", message)
	}
	http.Redirect(w, r, "/settings/security?"+values.Encode()+"#outbound-proxies", http.StatusSeeOther)
}

// reconnectUserAccounts reconnects one person's accounts after a proxy of
// theirs changed.
func (h *Handler) reconnectUserAccounts(ctx context.Context, owner string) {
	// The handler of people's own stores has the services itself; the main
	// handler reaches them through ownedMailbox.
	owned := h
	if h.userIMAP == nil && h.ownedMailbox != nil {
		owned = h.ownedMailbox
	}
	if owned.userIMAP == nil || owned.userStorage == nil {
		return
	}
	rows, err := owned.userStorage.System().Read().QueryContext(ctx, `SELECT account_id FROM gofer_account_directory WHERE user_id = ? AND state = 'active'`, owner)
	if err != nil {
		log.Printf("own proxies: list accounts to reconnect: %v", err)
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
			log.Printf("own proxies: reconnect account %s: %v", id, err)
		}
	}
}
