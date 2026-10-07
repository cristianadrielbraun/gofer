package handler

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (h *Handler) handleUserRetryMailOperation(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	stop := context.AfterFunc(h.userStorageContext, cancel)
	defer stop()
	owner, id := h.userID(ctx), strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeMailOperationsJSON(w, 400, map[string]string{"error": "operation id is required"})
		return
	}
	// Resolve the account, release this lease, then enter the account lifecycle
	// guard. Never nest store acquisitions with a bounded one-store cache.
	var operation models.MailOperationSummary
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		operation, err = db.GetMailOperationForUser(ctx, owner, id)
		return err
	})
	if err == nil {
		account := operation.AccountID
		err = h.userAccounts.WithAccountForUser(ctx, owner, account, func(_ *config.AccountStore, db *storage.DB) error {
			guard := func(tx *sql.Tx) error {
				if err := h.userStorage.ValidateUser(ctx, owner); err != nil {
					return err
				}
				state, err := h.userStorage.AccountStateForUser(ctx, owner, account)
				if err != nil {
					return err
				}
				if state != storage.AccountActive {
					return storage.ErrAccountRoute
				}
				var active bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id=? AND user_id=? AND COALESCE(is_deleting,0)=0)`, account, owner).Scan(&active); err != nil {
					return err
				}
				if !active {
					return storage.ErrAccountRoute
				}
				return ctx.Err()
			}
			var err error
			operation, err = db.RetryMailOperationForUserGuarded(ctx, owner, id, guard)
			return err
		})
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, storage.ErrAccountRoute) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, storage.ErrMailOperationNotRetryable) {
		writeMailOperationsJSON(w, 409, map[string]string{"error": "this mail operation cannot be retried in its current state"})
		return
	}
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	// The owned receive gate dispatches mutation, label, IMAP/provider draft and
	// sent-copy queues. A failed wake leaves the committed local intent intact.
	if err := h.userIMAP.WakeMutations(ctx, owner, operation.AccountID); err != nil {
		log.Printf("mail operation retry saved; owned wake failed: %v", err)
		w.Header().Set("X-Gofer-Mail-Delivery", "delayed")
	}
	writeMailOperationsJSON(w, http.StatusOK, mailOperationResponseFrom(operation))
}
