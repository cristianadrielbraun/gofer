package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/a-h/templ"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func accountRequestFromForm(r *http.Request) models.CreateAccountRequest {
	return models.CreateAccountRequest{
		Provider: r.FormValue("provider"), EmailAddress: r.FormValue("email_address"), DisplayName: r.FormValue("display_name"),
		IMAPHost: r.FormValue("imap_host"), IMAPPort: atoiDefault(r.FormValue("imap_port"), 993), IMAPTLSMode: r.FormValue("imap_tls_mode"),
		SMTPHost: r.FormValue("smtp_host"), SMTPPort: atoiDefault(r.FormValue("smtp_port"), 465), SMTPTLSMode: r.FormValue("smtp_tls_mode"),
		Username: r.FormValue("username"), Password: r.FormValue("password"), AuthMethod: r.FormValue("auth_method"),
		SmtpUsername: r.FormValue("smtp_username"), SmtpPassword: r.FormValue("smtp_password"),
	}
}

func (h *Handler) handleUserAccounts(w http.ResponseWriter, r *http.Request) {
	var accounts []models.Account
	err := h.withUserDB(r.Context(), h.userID(r.Context()), func(db *storage.DB) error {
		var err error
		accounts, err = db.GetAccounts(r.Context(), h.userID(r.Context()))
		return err
	})
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if accounts == nil {
		accounts = []models.Account{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(accounts)
}

func (h *Handler) handleUserAccountSettings(w http.ResponseWriter, r *http.Request) {
	h.handleUserSyncSettingsView(w, r, "accounts")
}

func (h *Handler) handleUserSyncSettingsView(w http.ResponseWriter, r *http.Request, tab string) {
	ctx := r.Context()
	statuses := make(map[string]map[string]mail.IDLEFolderRuntimeStatus)
	if h.userIMAP != nil {
		snapshot, err := h.userIMAP.IdleStatusesForUser(ctx, h.userID(ctx))
		if err != nil {
			userAccountError(w, r, err)
			return
		}
		for _, status := range snapshot {
			if statuses[status.AccountID] == nil {
				statuses[status.AccountID] = make(map[string]mail.IDLEFolderRuntimeStatus)
			}
			statuses[status.AccountID][status.FolderID] = status
		}
	}
	h.renderMailboxView(w, r, &ctx, func(local *Handler) (templ.Component, error) {
		local.userIdleStatuses = statuses
		userID := local.userID(ctx)
		accounts, err := local.db.GetAccounts(ctx, userID)
		if err != nil {
			return nil, err
		}
		display, err := local.db.GetAccountsIncludingDeleting(ctx, userID)
		if err != nil {
			return nil, err
		}
		settings := local.db.GetUISettings(ctx, userID)
		syncSettings := local.buildSyncSettings(ctx, accounts)
		signatures := local.buildAccountSignatureData(ctx, accounts)
		if r.Header.Get("HX-Request") == "true" {
			return views.SettingsPartial(display, syncSettings, tab, settings, signatures), nil
		}
		return views.SettingsLayout(display, syncSettings, tab, settings, signatures), nil
	})
}

func (h *Handler) userEditData(ctx context.Context, owner, id string) (*models.EditAccountData, error) {
	var data *models.EditAccountData
	err := h.userAccounts.WithAccountForUser(ctx, owner, id, func(local *config.AccountStore, _ *storage.DB) error {
		var err error
		data, err = local.GetEditData(ctx, id)
		return err
	})
	return data, err
}

func (h *Handler) handleUserEditAccount(w http.ResponseWriter, r *http.Request) {
	data, err := h.userEditData(r.Context(), h.userID(r.Context()), r.PathValue("id"))
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.EditAccountDialog(*data).Render(r.Context(), w)
}

func (h *Handler) handleUserCreateAccount(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	req := accountRequestFromForm(r)
	if req.EmailAddress == "" || req.IMAPHost == "" || req.SMTPHost == "" || req.Username == "" || req.Password == "" {
		http.Error(w, "all required fields must be filled in", http.StatusBadRequest)
		return
	}
	// OAuth callbacks still depend on the shared credential/account integration.
	// Do not accept an apparently working mailbox through that unconverted path.
	if (req.Provider != "" && req.Provider != "imap") || (req.AuthMethod != "" && req.AuthMethod != "plain") {
		http.Error(w, "this account connection method is not available yet", http.StatusNotImplemented)
		return
	}
	account, err := h.userAccounts.CreateAccount(r.Context(), h.userID(r.Context()), &req)
	if err != nil {
		var pending *config.PendingAccountCreationError
		if errors.As(err, &pending) {
			w.Header().Set("X-Gofer-Account-ID", pending.AccountID)
		}
		userAccountError(w, r, err)
		return
	}
	w.Header().Set("X-Gofer-Account-ID", account.ID)
	if err := h.userStorage.WithAccountActivityForUser(r.Context(), h.userID(r.Context()), account.ID, func() error {
		return h.userAccountHooks.Created(r.Context(), account.ID)
	}); err != nil {
		log.Printf("routed account %s created but worker start failed: %v", account.ID, err)
		http.Error(w, "account saved; synchronization could not be started", http.StatusServiceUnavailable)
		return
	}
	data, err := h.userEditData(r.Context(), h.userID(r.Context()), account.ID)
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.AddAccountPostCreateStep(*data).Render(r.Context(), w)
}

func (h *Handler) handleUserUpdateAccount(w http.ResponseWriter, r *http.Request) {
	owner, id := h.userID(r.Context()), r.PathValue("id")
	existing, err := h.userEditData(r.Context(), owner, id)
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	req := accountRequestFromForm(r)
	if req.Provider == "" {
		req.Provider = existing.Provider
	}
	if req.AuthMethod == "" {
		req.AuthMethod = existing.AuthMethod
	}
	if req.Provider != "imap" || req.Provider != existing.Provider || req.AuthMethod != "plain" || existing.AuthMethod != "plain" {
		http.Error(w, "this account connection method is not available yet", http.StatusNotImplemented)
		return
	}
	if req.EmailAddress == "" || req.IMAPHost == "" || req.SMTPHost == "" || req.Username == "" {
		http.Error(w, "all required fields must be filled in", http.StatusBadRequest)
		return
	}
	if err := h.userAccounts.UpdateAccount(r.Context(), owner, id, &req); err != nil {
		userAccountError(w, r, err)
		return
	}
	w.Header().Set("X-Gofer-Account-ID", id)
	if err := h.userStorage.WithAccountActivityForUser(r.Context(), owner, id, func() error {
		return h.userAccountHooks.Updated(r.Context(), id)
	}); err != nil {
		log.Printf("routed account %s updated but worker restart failed: %v", id, err)
		http.Error(w, "account saved; synchronization could not be restarted", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.WizardStepSuccess("Account updated", id, "edit").Render(r.Context(), w)
}

func (h *Handler) handleUserAccountColor(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	color, ok := normalizeAccountColor(r.FormValue("color"))
	if !ok {
		http.Error(w, "choose a valid hex color", http.StatusBadRequest)
		return
	}
	owner, id := h.userID(r.Context()), r.PathValue("id")
	if err := h.userAccounts.WithAccountForUser(r.Context(), owner, id, func(local *config.AccountStore, _ *storage.DB) error {
		return local.UpdateAccountColor(r.Context(), owner, id, color)
	}); err != nil {
		userAccountError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"color": color})
}

func (h *Handler) handleUserAccountDeletionStatus(w http.ResponseWriter, r *http.Request) {
	state, err := h.userStorage.AccountStateForUser(r.Context(), h.userID(r.Context()), r.PathValue("id"))
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": string(state)})
}

type userAccountDeletionJob struct {
	ready     chan struct{}
	intentErr error
}

func (h *Handler) handleUserDeleteAccount(w http.ResponseWriter, r *http.Request) {
	owner, id := h.userID(r.Context()), r.PathValue("id")
	state, err := h.userStorage.AccountStateForUser(r.Context(), owner, id)
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	accepted := func() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "account_id": id})
	}
	if state == storage.AccountDeleted {
		accepted()
		return
	}
	h.accountDeleteMu.Lock()
	if running := h.userDeletions[id]; running != nil {
		h.accountDeleteMu.Unlock()
		select {
		case <-running.ready:
			if running.intentErr != nil {
				userAccountError(w, r, running.intentErr)
				return
			}
			accepted()
		case <-r.Context().Done():
			userAccountError(w, r, r.Context().Err())
		}
		return
	}
	// Bound active drains/cleanup without opening or pinning queued databases.
	if len(h.userDeletions) >= 4 {
		h.accountDeleteMu.Unlock()
		w.Header().Set("Retry-After", "1")
		http.Error(w, "account cleanup busy; retry deletion", http.StatusServiceUnavailable)
		return
	}
	job := &userAccountDeletionJob{ready: make(chan struct{})}
	h.userDeletions[id] = job
	h.accountDeleteMu.Unlock()
	release := func() { h.accountDeleteMu.Lock(); delete(h.userDeletions, id); h.accountDeleteMu.Unlock() }
	// Persist the intent before returning 202. Draining and external cleanup run
	// with application cancellation, not the short-lived HTTP request context.
	if err := h.userStorage.RequestAccountDeletion(r.Context(), owner, id); err != nil {
		job.intentErr = err
		close(job.ready)
		release()
		userAccountError(w, r, err)
		return
	}
	close(job.ready)
	go func() {
		defer release()
		ctx, cancel := context.WithTimeout(h.userStorageContext, 30*time.Minute)
		defer cancel()
		if err := h.userAccounts.DeleteAccount(ctx, owner, id, h.userAccountHooks.Cleanup); err != nil {
			log.Printf("routed account %s deletion remains pending: %v", id, err)
		}
	}()
	accepted()
}

func userAccountError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, storage.ErrAccountRoute) || errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, config.ErrMailboxExists) {
		http.Error(w, "mailbox already exists for this user", http.StatusConflict)
		return
	}
	log.Printf("routed account operation failed: %v", err)
	http.Error(w, "account operation unavailable", http.StatusServiceUnavailable)
}
