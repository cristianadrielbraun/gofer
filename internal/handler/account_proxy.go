package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/netguard"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// accountProxyContext marks ctx so connections made with it go through the
// proxy of the owner's account id, or, while an account is being added and
// id is empty, the proxy a new account starts with: the default.
//
// Most connections find their account's proxy on their own, from the
// account's configuration or from the credentials they carry. This is for
// work that signs in with what someone typed into a form.
func (h *Handler) accountProxyContext(ctx context.Context, owner, id string) (context.Context, error) {
	var (
		proxy *netguard.Proxy
		err   error
	)
	switch {
	case id == "":
		proxy, err = h.defaultProxyFor(ctx, owner)
	case h.userAccounts != nil:
		proxy, err = h.userAccounts.AccountProxyForUser(ctx, owner, id)
	case h.accountStore != nil:
		proxy, err = h.accountStore.AccountProxy(ctx, id)
	}
	if err != nil {
		return nil, err
	}
	return netguard.WithProxy(ctx, proxy), nil
}

// sharedDefaultProxy is the proxy work shared by everyone goes through, such
// as avatar lookups: the default proxy, whoever it is for, or none.
func (h *Handler) sharedDefaultProxy(ctx context.Context) (*netguard.Proxy, error) {
	return h.defaultProxyFor(ctx, "")
}

// readerDefaultProxy is the default proxy as it applies to the person a
// request or job is for, such as the reader loading a message's remote
// images.
func (h *Handler) readerDefaultProxy(ctx context.Context) (*netguard.Proxy, error) {
	return h.defaultProxyFor(ctx, mailAllowlistOwner(ctx))
}

// defaultProxyFor is the default proxy as it applies to owner, for their work
// that belongs to no account, such as calendar subscriptions: none when the
// default isn't for them. An empty owner means everyone's.
func (h *Handler) defaultProxyFor(ctx context.Context, owner string) (*netguard.Proxy, error) {
	switch {
	case h.accountStore != nil:
		return h.accountStore.ResolveProxyChoice(ctx, owner, storage.AccountProxyDefault)
	case h.userAccounts != nil:
		return h.userAccounts.ResolveProxyChoice(ctx, owner, storage.AccountProxyDefault)
	}
	return nil, nil
}

// accountProxyData loads the proxy dialog of the signed-in person's account.
// It returns nil for an account that isn't theirs, and for Local folders,
// which connect nowhere.
func (h *Handler) accountProxyData(ctx context.Context, accountID string) (*models.AccountProxyData, error) {
	account, err := h.accountStore.GetAccountByIDForUser(ctx, h.userID(ctx), accountID)
	if err != nil || account == nil || account.Provider == providers.ProviderLocal {
		return nil, err
	}
	choice, err := h.db.GetAccountProxyChoice(ctx, accountID)
	if err != nil {
		return nil, err
	}
	owner := h.userID(ctx)
	saved, err := h.accountStore.OutboundProxiesForUser(ctx, owner)
	if err != nil {
		return nil, err
	}
	data := &models.AccountProxyData{AccountID: account.ID, Name: account.Name, Email: account.Email, Color: account.Color, Choice: choice}
	for _, p := range saved {
		data.Proxies = append(data.Proxies, models.OutboundProxyView{ID: p.ID, Name: p.Name, Type: p.Type, Host: p.Host, Port: p.Port, IsDefault: p.IsDefault})
	}
	if data.Default, err = h.accountStore.DefaultOutboundProxyName(ctx, owner); err != nil {
		return nil, err
	}
	// Where the person can add proxies themselves: with the exceptions in
	// personal and open mode, or as their own when a managed installation's
	// administrator lets them.
	if h.ownerManagesMailSecurity() {
		data.ManageURL = "/settings/security#outbound-proxies"
	} else if own, err := h.accountStore.PeopleMayAddOutboundProxies(ctx); err != nil {
		return nil, err
	} else if own {
		data.ManageURL = "/settings/security#outbound-proxies"
	}
	return data, nil
}

func (h *Handler) handleManageAccountProxy(w http.ResponseWriter, r *http.Request) {
	data, err := h.accountProxyData(r.Context(), r.PathValue("id"))
	if err != nil {
		log.Printf("account proxy account=%s: %v", r.PathValue("id"), err)
		http.Error(w, i18n.T(r.Context(), "server.error.failed_to_load_account"), http.StatusInternalServerError)
		return
	}
	if data == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	_ = views.AccountProxyDialog(*data).Render(r.Context(), w)
}

// handleSaveAccountProxy records the account's proxy choice in open and
// personal mode, and reconnects the account so it takes the new route.
func (h *Handler) handleSaveAccountProxy(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	ctx, id := r.Context(), r.PathValue("id")
	data, err := h.accountProxyData(ctx, id)
	if err != nil || data == nil {
		h.handleManageAccountProxy(w, r)
		return
	}
	saveErr := h.accountStore.SetAccountProxy(ctx, h.userID(ctx), id, r.FormValue("proxy"))
	h.renderAccountProxySave(w, r, data, saveErr, func(updated *models.AccountProxyData) error {
		next, err := h.accountProxyData(ctx, id)
		if err == nil && next != nil {
			*updated = *next
		}
		return err
	})
	if saveErr == nil {
		h.reconnectAccount(id)
	}
}

// handleUserSaveAccountProxy is handleSaveAccountProxy for an account in its
// owner's own database.
func (h *Handler) handleUserSaveAccountProxy(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	ctx, owner, id := r.Context(), h.userID(r.Context()), r.PathValue("id")
	var (
		data    *models.AccountProxyData
		saveErr error
	)
	err := h.userAccounts.WithAccountForUser(ctx, owner, id, func(local *config.AccountStore, db *storage.DB) error {
		scoped := &Handler{db: db, accountStore: local, auth: h.auth}
		var err error
		if data, err = scoped.accountProxyData(ctx, id); err != nil || data == nil {
			return err
		}
		if saveErr = local.SetAccountProxy(ctx, owner, id, r.FormValue("proxy")); saveErr != nil {
			return nil
		}
		next, err := scoped.accountProxyData(ctx, id)
		if next != nil {
			data = next
		}
		return err
	})
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if data == nil {
		http.NotFound(w, r)
		return
	}
	h.renderAccountProxySave(w, r, data, saveErr, nil)
	if saveErr == nil && h.userIMAP != nil {
		if err := h.userIMAP.RestartAccount(ctx, id); err != nil && !errors.Is(err, storage.ErrAccountRoute) {
			log.Printf("account proxy: reconnect account %s: %v", id, err)
		}
	}
}

// renderAccountProxySave shows the dialog again after a save, saying how it
// went. reload, when given, reads the saved choice back first.
func (h *Handler) renderAccountProxySave(w http.ResponseWriter, r *http.Request, data *models.AccountProxyData, saveErr error, reload func(*models.AccountProxyData) error) {
	switch {
	case errors.Is(saveErr, storage.ErrOutboundProxyNotFound):
		data.Error = i18n.T(r.Context(), "account_proxy.gone")
	case saveErr != nil:
		log.Printf("account proxy account=%s: save: %v", data.AccountID, saveErr)
		data.Error = i18n.T(r.Context(), "account_proxy.failed")
	default:
		if reload != nil {
			if err := reload(data); err != nil {
				log.Printf("account proxy account=%s: reload: %v", data.AccountID, err)
			}
		}
		data.Saved = true
	}
	w.Header().Set("Content-Type", "text/html")
	if r.FormValue("inline") == "1" {
		// The Connect through field of the account's own dialog.
		_ = views.AccountProxyChoiceStatus(*data).Render(r.Context(), w)
		return
	}
	_ = views.AccountProxyPanel(*data).Render(r.Context(), w)
}

// defaultProxyContext marks ctx so connections made with it go through the
// default proxy as it applies to owner, for their work that belongs to no
// account.
func (h *Handler) defaultProxyContext(ctx context.Context, owner string) (context.Context, error) {
	proxy, err := h.defaultProxyFor(ctx, owner)
	if err != nil {
		return nil, &netguard.ProxyError{Proxy: "default", Err: err}
	}
	return netguard.WithProxy(ctx, proxy), nil
}

// proxyChoiceContext marks ctx with the proxy chosen in a form for an account
// being added, refusing one the owner may not use.
func (h *Handler) proxyChoiceContext(ctx context.Context, owner, choice string) (context.Context, error) {
	store := h.proxyStore()
	if store == nil {
		return netguard.WithProxy(ctx, nil), nil
	}
	proxy, err := store.ProxyChoice(ctx, owner, choice)
	if err != nil {
		return nil, err
	}
	return netguard.WithProxy(ctx, proxy), nil
}

// withProxyChoice runs the rest of a request for an account being added
// through the proxy its form chose, and answers it when the choice is
// refused.
func (h *Handler) withProxyChoice(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	ctx, err := h.proxyChoiceContext(r.Context(), h.userID(r.Context()), r.FormValue("proxy"))
	if err != nil {
		if errors.Is(err, storage.ErrOutboundProxyNotFound) {
			http.Error(w, i18n.T(r.Context(), "account_proxy.gone"), http.StatusBadRequest)
		} else {
			log.Printf("account proxy choice: %v", err)
			http.Error(w, i18n.T(r.Context(), "account_proxy.failed"), http.StatusServiceUnavailable)
		}
		return nil, false
	}
	return r.WithContext(ctx), true
}

// withAccountProxyFor runs the rest of a request about the owner's existing
// account through that account's proxy.
func (h *Handler) withAccountProxyFor(w http.ResponseWriter, r *http.Request, id string) (*http.Request, bool) {
	ctx, err := h.accountProxyContext(r.Context(), h.userID(r.Context()), id)
	if err != nil {
		userAccountError(w, r, err)
		return nil, false
	}
	return r.WithContext(ctx), true
}

// addProxyChoiceToFlow keeps the proxy chosen in an OAuth sign-in form for
// the account it connects. Reconnect forms have no choice, so their account
// keeps its own.
func addProxyChoiceToFlow(r *http.Request, form map[string]string) {
	if _, chosen := r.Form["proxy"]; chosen {
		form["proxy"] = strings.TrimSpace(r.FormValue("proxy"))
	}
}

// oauthFlowProxyContext marks ctx with the proxy an OAuth sign-in goes
// through: the one its form chose, or, reconnecting an account it names, the
// account's own, or else the owner's default.
func (h *Handler) oauthFlowProxyContext(ctx context.Context, owner string, form map[string]string) (context.Context, error) {
	if choice, chosen := form["proxy"]; chosen {
		return h.proxyChoiceContext(ctx, owner, choice)
	}
	if id := form["account_id"]; id != "" {
		return h.accountProxyContext(ctx, owner, id)
	}
	return h.proxyChoiceContext(ctx, owner, storage.AccountProxyDefault)
}

// handleAccountProxyChoice renders the proxy setting of an account dialog:
// the Proxy row of a wizard's sidebar, or the Connect through field of the
// other dialogs. It is for the account named, with its choice, or for one
// being added, with the choice made so far. It is empty when the person has
// no proxy to choose.
func (h *Handler) handleAccountProxyChoice(w http.ResponseWriter, r *http.Request) {
	data, ok := h.newAccountProxyData(w, r)
	if !ok {
		return
	}
	data.Sidebar = r.URL.Query().Get("sidebar") == "1"
	if r.URL.Query().Get("form") == "account-form" {
		// The add wizard's setting sits in its sidebar, outside its form,
		// and goes with it, also when it adds mail to an existing account.
		data.Form = "account-form"
	}
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	if data.Sidebar {
		_ = views.AccountProxySidebarRow(*data).Render(r.Context(), w)
		return
	}
	_ = views.AccountProxyChoiceField(*data).Render(r.Context(), w)
}

// handleAccountProxyAddDialog renders the Proxy dialog for an account being
// added, which hands the choice back to the wizard instead of saving it.
func (h *Handler) handleAccountProxyAddDialog(w http.ResponseWriter, r *http.Request) {
	data, ok := h.newAccountProxyData(w, r)
	if !ok {
		return
	}
	if h.ownerManagesMailSecurity() {
		data.ManageURL = "/settings/security#outbound-proxies"
	} else if store := h.proxyStore(); store != nil {
		if own, err := store.PeopleMayAddOutboundProxies(r.Context()); err == nil && own {
			data.ManageURL = "/settings/security#outbound-proxies"
		}
	}
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	_ = views.AccountProxyDialog(*data).Render(r.Context(), w)
}

// newAccountProxyData loads the proxies an account dialog offers: the
// account named and its choice, or, for one being added, the choice query
// value. It answers the request itself when it can't.
func (h *Handler) newAccountProxyData(w http.ResponseWriter, r *http.Request) (*models.AccountProxyData, bool) {
	ctx, owner := r.Context(), h.userID(r.Context())
	data := &models.AccountProxyData{Choice: strings.TrimSpace(r.URL.Query().Get("choice"))}
	if id := strings.TrimSpace(r.URL.Query().Get("account")); id != "" {
		var err error
		if data, err = h.accountProxyData(ctx, id); err != nil {
			log.Printf("account proxy choice account=%s: %v", id, err)
			http.Error(w, i18n.T(ctx, "server.error.failed_to_load_account"), http.StatusInternalServerError)
			return nil, false
		}
		if data == nil {
			// Nothing to choose for this account, such as Local folders.
			data = &models.AccountProxyData{}
		}
		return data, true
	}
	store := h.proxyStore()
	if store == nil {
		return data, true
	}
	saved, err := store.OutboundProxiesForUser(ctx, owner)
	if err != nil {
		log.Printf("account proxy choice: %v", err)
		http.Error(w, i18n.T(ctx, "account_proxy.failed"), http.StatusInternalServerError)
		return nil, false
	}
	for _, p := range saved {
		data.Proxies = append(data.Proxies, models.OutboundProxyView{ID: p.ID, Name: p.Name, Type: p.Type, Host: p.Host, Port: p.Port, IsDefault: p.IsDefault})
	}
	if data.Default, err = store.DefaultOutboundProxyName(ctx, owner); err != nil {
		log.Printf("account proxy choice: %v", err)
		http.Error(w, i18n.T(ctx, "account_proxy.failed"), http.StatusInternalServerError)
		return nil, false
	}
	return data, true
}
