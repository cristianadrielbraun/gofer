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
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) reconnectAccounts(ctx context.Context, ids []string) {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if h.userIMAP != nil {
			if err := h.userIMAP.RestartAccount(ctx, id); err != nil && !errors.Is(err, storage.ErrAccountRoute) {
				log.Printf("account group: reconnect account %s: %v", id, err)
			}
			continue
		}
		h.reconnectAccount(id)
	}
}

func (h *Handler) loadAccountGroupsForView(ctx context.Context, db *storage.DB, owner string) ([]models.AccountGroup, error) {
	groups, err := db.ListAccountGroups(ctx, owner)
	if err != nil || len(groups) == 0 {
		return groups, err
	}
	store := h.proxyStore()
	if store == nil {
		return groups, nil
	}
	byID := make(map[string]models.AccountGroup, len(groups))
	for _, g := range groups {
		byID[g.ID] = g
	}
	direct := i18n.T(ctx, "account_proxy.direct")
	for i := range groups {
		chain := groupProxyChainFromMap(byID, groups[i].ID)
		name, err := store.ResolveProxyChainName(ctx, owner, chain...)
		if err != nil {
			return nil, err
		}
		groups[i].ProxyActive = name != ""
		switch {
		case name != "":
			groups[i].ProxyState = name
		case chainChoosesDirect(chain):
			// Inherited "direct" also resolves to no name, even when a default
			// proxy exists. With no such choice, an empty name only means
			// there is no default, and the card stays quiet.
			groups[i].ProxyState = direct
		default:
			groups[i].ProxyState = ""
		}
	}
	return groups, nil
}

// chainChoosesDirect reports whether a choice in the chain is a direct connection.
func chainChoosesDirect(chain []string) bool {
	for _, choice := range chain {
		if strings.TrimSpace(choice) == storage.AccountProxyDirect {
			return true
		}
	}
	return false
}

func groupProxyChainFromMap(byID map[string]models.AccountGroup, groupID string) []string {
	var chain []string
	seen := make(map[string]bool, len(byID))
	cur := strings.TrimSpace(groupID)
	for cur != "" && !seen[cur] {
		seen[cur] = true
		g, ok := byID[cur]
		if !ok {
			break
		}
		chain = append(chain, g.ProxyID)
		cur = g.ParentID
	}
	return chain
}

func filterEligibleParentGroups(groups []models.AccountGroup, editingID string) []models.AccountGroup {
	if editingID == "" {
		return groups
	}
	blocked := map[string]bool{editingID: true}
	changed := true
	for changed {
		changed = false
		for _, g := range groups {
			if !blocked[g.ID] && blocked[g.ParentID] {
				blocked[g.ID] = true
				changed = true
			}
		}
	}
	out := make([]models.AccountGroup, 0, len(groups))
	for _, g := range groups {
		if !blocked[g.ID] {
			out = append(out, g)
		}
	}
	return out
}

func (h *Handler) renderSettingsAccountsSection(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	var (
		accounts []models.Account
		groups   []models.AccountGroup
	)
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		if accounts, err = db.GetAccountsIncludingDeleting(ctx, owner); err != nil {
			return err
		}
		groups, err = h.loadAccountGroupsForView(ctx, db, owner)
		return err
	})
	if err != nil {
		log.Printf("settings accounts section: %v", err)
		http.Error(w, i18n.T(ctx, "server.error.failed_to_load_webmail_accounts"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	_ = views.SettingsAccountsTab(accounts).Render(views.WithAccountGroups(ctx, groups), w)
}

func (h *Handler) handleNewAccountGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	parentID := strings.TrimSpace(r.URL.Query().Get("parent_id"))
	var groups []models.AccountGroup
	if err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		groups, err = db.ListAccountGroups(ctx, owner)
		return err
	}); err != nil {
		http.Error(w, i18n.T(ctx, "account_group.error.load"), http.StatusInternalServerError)
		return
	}
	if parentID != "" {
		found := false
		for _, g := range groups {
			if g.ID == parentID {
				found = true
				break
			}
		}
		if !found {
			parentID = ""
		}
	}
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	_ = views.AccountGroupDialog(views.AccountGroupDialogData{
		Group:    models.AccountGroup{ParentID: parentID},
		Parents:  groups,
		IsCreate: true,
	}).Render(ctx, w)
}

func (h *Handler) handleCreateAccountGroup(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	owner := h.userID(ctx)
	name := strings.TrimSpace(r.FormValue("name"))
	parentID := strings.TrimSpace(r.FormValue("parent_id"))
	proxyChoice := strings.TrimSpace(r.FormValue("proxy"))

	if store := h.proxyStore(); store != nil {
		if err := store.CheckProxyChoice(ctx, owner, proxyChoice); err != nil {
			h.renderAccountGroupFormError(w, r, models.AccountGroup{Name: name, ParentID: parentID, ProxyID: proxyChoice}, true, err)
			return
		}
	}

	var createErr error
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		_, createErr = db.CreateAccountGroup(ctx, owner, parentID, name, proxyChoice)
		return nil
	})
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if createErr != nil {
		h.renderAccountGroupFormError(w, r, models.AccountGroup{Name: name, ParentID: parentID, ProxyID: proxyChoice}, true, createErr)
		return
	}
	w.Header().Set("HX-Retarget", "#settings-content #settings-accounts-section")
	w.Header().Set("HX-Reswap", "outerHTML")
	w.Header().Set("HX-Trigger", `{"accountGroupSaved":true}`)
	h.renderSettingsAccountsSection(w, r)
}

func (h *Handler) handleEditAccountGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	id := strings.TrimSpace(r.PathValue("id"))
	var (
		group  models.AccountGroup
		groups []models.AccountGroup
	)
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		if group, err = db.GetAccountGroup(ctx, owner, id); err != nil {
			return err
		}
		groups, err = db.ListAccountGroups(ctx, owner)
		return err
	})
	if errors.Is(err, storage.ErrAccountGroupNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	_ = views.AccountGroupDialog(views.AccountGroupDialogData{
		Group:   group,
		Parents: filterEligibleParentGroups(groups, group.ID),
	}).Render(ctx, w)
}

func (h *Handler) handleUpdateAccountGroup(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	owner := h.userID(ctx)
	id := strings.TrimSpace(r.PathValue("id"))
	name := strings.TrimSpace(r.FormValue("name"))
	parentID := strings.TrimSpace(r.FormValue("parent_id"))

	var (
		existing      models.AccountGroup
		parentChanged bool
		updateErr     error
		affectedIDs   []string
	)
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		existing, err = db.GetAccountGroup(ctx, owner, id)
		if err != nil {
			return err
		}
		if _, parentChanged, updateErr = db.UpdateAccountGroup(ctx, owner, id, name, parentID); updateErr != nil {
			return nil
		}
		if parentChanged {
			affectedIDs, err = db.AccountGroupAccountIDs(ctx, owner, id)
			return err
		}
		return nil
	})
	if errors.Is(err, storage.ErrAccountGroupNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if updateErr != nil {
		h.renderAccountGroupFormError(w, r, models.AccountGroup{ID: id, Name: name, ParentID: parentID, ProxyID: existing.ProxyID}, false, updateErr)
		return
	}
	if len(affectedIDs) > 0 {
		h.reconnectAccounts(ctx, affectedIDs)
	}
	w.Header().Set("HX-Retarget", "#settings-content #settings-accounts-section")
	w.Header().Set("HX-Reswap", "outerHTML")
	w.Header().Set("HX-Trigger", `{"accountGroupSaved":true}`)
	h.renderSettingsAccountsSection(w, r)
}

func (h *Handler) handleDeleteAccountGroup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	id := strings.TrimSpace(r.PathValue("id"))
	var affectedIDs []string
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		affectedIDs, err = db.DeleteAccountGroup(ctx, owner, id)
		return err
	})
	if errors.Is(err, storage.ErrAccountGroupNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if len(affectedIDs) > 0 {
		h.reconnectAccounts(ctx, affectedIDs)
	}
	w.Header().Set("HX-Trigger", `{"accountGroupSaved":true}`)
	h.renderSettingsAccountsSection(w, r)
}

func (h *Handler) renderAccountGroupFormError(w http.ResponseWriter, r *http.Request, group models.AccountGroup, isCreate bool, formErr error) {
	ctx := r.Context()
	owner := h.userID(ctx)
	var groups []models.AccountGroup
	_ = h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		groups, err = db.ListAccountGroups(ctx, owner)
		return err
	})
	var msg string
	switch {
	case errors.Is(formErr, storage.ErrAccountGroupNameEmpty):
		msg = i18n.T(ctx, "account_group.error.name_empty")
	case errors.Is(formErr, storage.ErrAccountGroupNameTaken):
		msg = i18n.T(ctx, "account_group.error.duplicate")
	case errors.Is(formErr, storage.ErrAccountGroupCycle):
		msg = i18n.T(ctx, "account_group.error.cycle")
	case errors.Is(formErr, storage.ErrAccountGroupNotFound):
		msg = i18n.T(ctx, "account_group.error.parent_gone")
	case errors.Is(formErr, storage.ErrOutboundProxyNotFound):
		msg = i18n.T(ctx, "account_proxy.gone")
	default:
		log.Printf("account group save: %v", formErr)
		msg = i18n.T(ctx, "account_group.error.save")
	}
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	_ = views.AccountGroupDialogPanel(views.AccountGroupDialogData{
		Group:    group,
		Parents:  filterEligibleParentGroups(groups, group.ID),
		Error:    msg,
		IsCreate: isCreate,
	}).Render(ctx, w)
}

func (h *Handler) accountGroupProxyData(ctx context.Context, groupID string) (*models.AccountProxyData, error) {
	owner := h.userID(ctx)
	var (
		group  *models.AccountProxyData
		rawGrp models.AccountGroup
		groups []models.AccountGroup
	)
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		rawGrp, err = db.GetAccountGroup(ctx, owner, groupID)
		if err != nil {
			return err
		}
		groups, err = db.ListAccountGroups(ctx, owner)
		return err
	})
	if errors.Is(err, storage.ErrAccountGroupNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	group = &models.AccountProxyData{
		GroupID: rawGrp.ID,
		Name:    rawGrp.Name,
		Choice:  rawGrp.ProxyID,
	}
	store := h.proxyStore()
	if store == nil {
		return group, nil
	}
	saved, err := store.OutboundProxiesForUser(ctx, owner)
	if err != nil {
		return nil, err
	}
	for _, p := range saved {
		group.Proxies = append(group.Proxies, models.OutboundProxyView{
			ID: p.ID, Name: p.Name, Type: p.Type, Host: p.Host, Port: p.Port, IsDefault: p.IsDefault,
		})
	}
	if group.Default, err = store.DefaultOutboundProxyName(ctx, owner); err != nil {
		return nil, err
	}
	if rawGrp.ParentID != "" {
		if err := h.populateInheritedGroupProxy(ctx, owner, store, groups, rawGrp.ParentID, group); err != nil {
			return nil, err
		}
	}
	if h.ownerManagesMailSecurity() {
		group.ManageURL = "/settings/security#outbound-proxies"
	} else if own, err := store.PeopleMayAddOutboundProxies(ctx); err != nil {
		return nil, err
	} else if own {
		group.ManageURL = "/settings/security#outbound-proxies"
	}
	return group, nil
}

func (h *Handler) populateInheritedGroupProxy(ctx context.Context, owner string, store *config.AccountStore, groups []models.AccountGroup, startGroupID string, data *models.AccountProxyData) error {
	if store == nil || startGroupID == "" {
		return nil
	}
	byID := make(map[string]models.AccountGroup, len(groups))
	for _, g := range groups {
		byID[g.ID] = g
	}
	seen := make(map[string]bool, len(groups))
	cur := strings.TrimSpace(startGroupID)
	for cur != "" && !seen[cur] {
		seen[cur] = true
		g, ok := byID[cur]
		if !ok {
			break
		}
		choice := strings.TrimSpace(g.ProxyID)
		switch {
		case choice == "":
			cur = g.ParentID
			continue
		case choice == storage.AccountProxyDirect:
			data.InheritedGroup = g.Name
			data.InheritedProxy = ""
			return nil
		default:
			if err := store.CheckProxyChoice(ctx, owner, choice); err == nil {
				name, err := store.ResolveProxyChainName(ctx, owner, choice)
				if err != nil {
					return err
				}
				data.InheritedGroup = g.Name
				data.InheritedProxy = name
				return nil
			}
			cur = g.ParentID
		}
	}
	return nil
}

func (h *Handler) handleManageAccountGroupProxy(w http.ResponseWriter, r *http.Request) {
	data, err := h.accountGroupProxyData(r.Context(), r.PathValue("id"))
	if err != nil {
		log.Printf("account group proxy group=%s: %v", r.PathValue("id"), err)
		http.Error(w, i18n.T(r.Context(), "account_proxy.failed"), http.StatusInternalServerError)
		return
	}
	if data == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	_ = views.AccountProxyDialog(*data).Render(r.Context(), w)
}

func (h *Handler) handleSaveAccountGroupProxy(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	owner := h.userID(ctx)
	id := strings.TrimSpace(r.PathValue("id"))
	choice := strings.TrimSpace(r.FormValue("proxy"))

	data, err := h.accountGroupProxyData(ctx, id)
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if data == nil {
		http.NotFound(w, r)
		return
	}

	var (
		saveErr     error
		affectedIDs []string
	)
	if store := h.proxyStore(); store != nil {
		saveErr = store.CheckProxyChoice(ctx, owner, choice)
	}
	if saveErr == nil {
		err = h.withUserDB(ctx, owner, func(db *storage.DB) error {
			if saveErr = db.SetAccountGroupProxyChoice(ctx, owner, id, choice); saveErr != nil {
				return nil
			}
			var err error
			affectedIDs, err = db.AccountGroupAccountIDs(ctx, owner, id)
			return err
		})
		if err != nil {
			userAccountError(w, r, err)
			return
		}
	}
	if errors.Is(saveErr, storage.ErrAccountGroupNotFound) {
		http.NotFound(w, r)
		return
	}
	if saveErr == nil {
		w.Header().Set("HX-Trigger", `{"accountGroupSaved":true}`)
	}
	h.renderAccountProxySave(w, r, data, saveErr, func(updated *models.AccountProxyData) error {
		next, err := h.accountGroupProxyData(ctx, id)
		if err == nil && next != nil {
			*updated = *next
		}
		return err
	})
	if saveErr == nil && len(affectedIDs) > 0 {
		h.reconnectAccounts(ctx, affectedIDs)
	}
}

func (h *Handler) handleManageAccountGroupMembership(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	accountID := strings.TrimSpace(r.PathValue("id"))
	var (
		account *models.Account
		groups  []models.AccountGroup
	)
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		accounts, err := db.GetAccounts(ctx, owner)
		if err != nil {
			return err
		}
		for i := range accounts {
			if accounts[i].ID == accountID {
				acc := accounts[i]
				account = &acc
				break
			}
		}
		if account == nil || account.Provider == providers.ProviderLocal {
			return nil
		}
		groups, err = db.ListAccountGroups(ctx, owner)
		return err
	})
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if account == nil || account.Provider == providers.ProviderLocal {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	_ = views.MoveAccountGroupDialog(views.MoveAccountGroupDialogData{
		Account: *account,
		Groups:  groups,
	}).Render(ctx, w)
}

func (h *Handler) handleSaveAccountGroupMembership(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	owner := h.userID(ctx)
	accountID := strings.TrimSpace(r.PathValue("id"))
	groupID := strings.TrimSpace(r.FormValue("group_id"))

	var (
		account     models.Account
		groups      []models.AccountGroup
		prevGroupID string
		found       bool
		moveErr     error
	)
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		accounts, err := db.GetAccounts(ctx, owner)
		if err != nil {
			return err
		}
		for _, a := range accounts {
			if a.ID == accountID && a.Provider != providers.ProviderLocal {
				found = true
				account = a
				prevGroupID = a.GroupID
				break
			}
		}
		if !found {
			return nil
		}
		moveErr = db.MoveAccountToGroup(ctx, owner, accountID, groupID)
		if moveErr != nil {
			groups, err = db.ListAccountGroups(ctx, owner)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	if moveErr != nil {
		if r.Header.Get("HX-Request") == "true" {
			msg := i18n.T(ctx, "account_group.error.move")
			if errors.Is(moveErr, storage.ErrAccountGroupNotFound) {
				msg = i18n.T(ctx, "account_group.error.parent_gone")
			} else {
				log.Printf("move account=%s to group=%s: %v", accountID, groupID, moveErr)
			}
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Cache-Control", "no-store")
			_ = views.MoveAccountGroupDialogPanel(views.MoveAccountGroupDialogData{
				Account: account,
				Groups:  groups,
				Error:   msg,
			}).Render(ctx, w)
			return
		}
		if errors.Is(moveErr, storage.ErrAccountGroupNotFound) {
			http.NotFound(w, r)
			return
		}
		log.Printf("move account=%s to group=%s: %v", accountID, groupID, moveErr)
		http.Error(w, i18n.T(ctx, "account_group.error.move"), http.StatusBadRequest)
		return
	}
	if prevGroupID != groupID {
		h.reconnectAccounts(ctx, []string{accountID})
	}
	w.Header().Set("HX-Retarget", "#settings-content #settings-accounts-section")
	w.Header().Set("HX-Reswap", "outerHTML")
	w.Header().Set("HX-Trigger", `{"accountGroupSaved":true}`)
	h.renderSettingsAccountsSection(w, r)
}

func (h *Handler) handleManageAccountGroupAccounts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	id := strings.TrimSpace(r.PathValue("id"))
	var (
		group    models.AccountGroup
		groups   []models.AccountGroup
		accounts []models.Account
	)
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		if group, err = db.GetAccountGroup(ctx, owner, id); err != nil {
			return err
		}
		if accounts, err = db.GetAccounts(ctx, owner); err != nil {
			return err
		}
		groups, err = db.ListAccountGroups(ctx, owner)
		return err
	})
	if errors.Is(err, storage.ErrAccountGroupNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	_ = views.AddExistingAccountsToGroupDialog(views.AddExistingAccountsDialogData{
		Group:    group,
		Groups:   groups,
		Accounts: accounts,
	}).Render(ctx, w)
}

func (h *Handler) handleSaveAccountGroupAccounts(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	owner := h.userID(ctx)
	id := strings.TrimSpace(r.PathValue("id"))

	var accountIDs []string
	selected := make(map[string]bool)
	for _, raw := range r.Form["account_id"] {
		aid := strings.TrimSpace(raw)
		if aid == "" || selected[aid] {
			continue
		}
		selected[aid] = true
		accountIDs = append(accountIDs, aid)
	}

	var (
		group    models.AccountGroup
		groups   []models.AccountGroup
		accounts []models.Account
		movedIDs []string
		formErr  string
	)
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		if group, err = db.GetAccountGroup(ctx, owner, id); err != nil {
			return err
		}
		if len(accountIDs) == 0 {
			formErr = i18n.T(ctx, "account_group.error.select_accounts")
		} else {
			movedIDs, err = db.MoveAccountsToGroup(ctx, owner, id, accountIDs)
			if errors.Is(err, storage.ErrAccountGroupNotFound) {
				return err
			}
			if err != nil {
				log.Printf("move accounts to group=%s: %v", id, err)
				formErr = i18n.T(ctx, "account_group.error.move")
			}
		}
		if formErr != "" {
			if accounts, err = db.GetAccounts(ctx, owner); err != nil {
				return err
			}
			groups, err = db.ListAccountGroups(ctx, owner)
			return err
		}
		return nil
	})
	if errors.Is(err, storage.ErrAccountGroupNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if formErr != "" {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Cache-Control", "no-store")
		_ = views.AddExistingAccountsDialogPanel(views.AddExistingAccountsDialogData{
			Group:    group,
			Groups:   groups,
			Accounts: accounts,
			Selected: selected,
			Error:    formErr,
		}).Render(ctx, w)
		return
	}
	if len(movedIDs) > 0 {
		h.reconnectAccounts(ctx, movedIDs)
	}
	w.Header().Set("HX-Retarget", "#settings-content #settings-accounts-section")
	w.Header().Set("HX-Reswap", "outerHTML")
	w.Header().Set("HX-Trigger", `{"accountGroupSaved":true}`)
	h.renderSettingsAccountsSection(w, r)
}
