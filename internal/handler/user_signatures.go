package handler

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Only these six local signature operations use the adapter. Provider services
// and network clients are absent from the request-local handler.
func (h *Handler) registerUserSignatures(private func(string, http.HandlerFunc)) {
	for _, route := range []struct {
		pattern string
		handler func(*Handler) http.HandlerFunc
	}{
		{"GET /api/accounts/{id}/signatures", func(h *Handler) http.HandlerFunc { return h.handleAccountSignatures }},
		{"GET /api/accounts/{id}/signatures/manage", func(h *Handler) http.HandlerFunc { return h.handleManageAccountSignatures }},
		{"GET /api/settings/signatures/manage", func(h *Handler) http.HandlerFunc { return h.handleManageSignaturesSettings }},
		{"POST /api/signatures", func(h *Handler) http.HandlerFunc { return h.handleSaveSignature }},
		{"DELETE /api/signatures/{id}", func(h *Handler) http.HandlerFunc { return h.handleDeleteSignature }},
		{"POST /api/accounts/{id}/signature-settings", func(h *Handler) http.HandlerFunc { return h.handleSaveAccountSignatureSettings }},
	} {
		private(route.pattern, h.userSignatureHandler(route.handler))
	}
}

func (h *Handler) userSignatureHandler(selectHandler func(*Handler) http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		stop := context.AfterFunc(h.userStorageContext, cancel)
		defer stop()
		r = r.WithContext(ctx)
		if r.Method == http.MethodPost {
			// Parse before leasing storage, using the same bound as compose.
			r.Body = http.MaxBytesReader(w, r.Body, 2*composeAttachmentMaxBytes+1<<20)
			if err := r.ParseForm(); err != nil {
				http.Error(w, "invalid form data", http.StatusBadRequest)
				return
			}
		}
		owner := h.userID(ctx)
		account := ""
		if strings.HasPrefix(r.URL.Path, "/api/accounts/") {
			account = r.PathValue("id")
		}
		guard := func(tx *sql.Tx) error {
			if err := h.userStorage.ValidateUser(ctx, owner); err != nil {
				return err
			}
			if account != "" {
				state, err := h.userStorage.AccountStateForUser(ctx, owner, account)
				if err != nil {
					return err
				}
				if state != storage.AccountActive {
					return storage.ErrAccountRoute
				}
				if tx != nil {
					var active bool
					if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=? AND user_id=? AND COALESCE(is_deleting,0)=0)`, account, owner).Scan(&active); err != nil {
						return err
					}
					if !active {
						return storage.ErrAccountRoute
					}
				}
			}
			return ctx.Err()
		}
		response := &userMutationResponse{header: make(http.Header)}
		prepare := func(accounts *config.AccountStore, db *storage.DB) error {
			local := &Handler{db: db, accountStore: accounts, auth: h.auth, signatureGuard: guard}
			selectHandler(local)(response, r)
			return guard(nil)
		}
		var err error
		if account != "" {
			err = h.userAccounts.WithAccountForUser(ctx, owner, account, prepare)
		} else {
			err = h.userAccounts.WithUser(ctx, owner, prepare)
		}
		if err != nil {
			userAccountError(w, r, err)
			return
		}
		// No database/account lease remains while a browser consumes the response.
		for key, values := range response.header {
			w.Header()[key] = append([]string(nil), values...)
		}
		if response.status == 0 {
			response.status = http.StatusOK
		}
		w.WriteHeader(response.status)
		_, _ = w.Write(response.body.Bytes())
	}
}
