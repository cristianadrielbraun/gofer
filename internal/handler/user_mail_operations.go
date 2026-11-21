package handler

import (
	"context"
	"database/sql"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
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
		writeMailOperationsJSON(w, 400, map[string]string{"error": i18n.T(r.Context(), "server.msg.operation_id_is_required")})
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
			var err error
			operation, err = db.RetryMailOperationForUserGuarded(ctx, owner, id, h.mailOperationAccountGuard(ctx, owner, account))
			return err
		})
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, storage.ErrAccountRoute) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, storage.ErrMailOperationNotRetryable) {
		writeMailOperationsJSON(w, 409, map[string]string{"error": i18n.T(r.Context(), "server.msg.this_mail_operation_cannot_be_retried_in")})
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

// mailOperationAccountGuard runs inside a mail operation's write transaction.
// It refuses when the user is no longer valid or the account is no longer
// active, so a change can't land on an account being removed.
func (h *Handler) mailOperationAccountGuard(ctx context.Context, owner, account string) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
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
}

// handleUserUndoMailOperation undoes a parked mail operation in Gofer only.
// Nothing goes to the server, so no queue is woken.
func (h *Handler) handleUserUndoMailOperation(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	stop := context.AfterFunc(h.userStorageContext, cancel)
	defer stop()
	owner, id := h.userID(ctx), strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeMailOperationsJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.T(r.Context(), "server.msg.operation_id_is_required")})
		return
	}
	// As with retry: resolve the account first, then take its guard.
	var operation models.MailOperationSummary
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		operation, err = db.GetMailOperationForUser(ctx, owner, id)
		return err
	})
	if err == nil {
		account := operation.AccountID
		err = h.userAccounts.WithAccountForUser(ctx, owner, account, func(_ *config.AccountStore, db *storage.DB) error {
			var err error
			operation, err = db.UndoMailOperationForUser(ctx, owner, id, h.mailOperationAccountGuard(ctx, owner, account))
			return err
		})
	}
	h.writeMailOperationUndoResult(w, r, operation, err)
}

// handleUserRetryFailedMailOperations retries all of the user's failed work,
// account by account, each inside its account's guard. An account that
// refuses, such as one being removed, is counted as skipped and the others
// still go ahead. Each account's queues are woken once.
func (h *Handler) handleUserRetryFailedMailOperations(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	stop := context.AfterFunc(h.userStorageContext, cancel)
	defer stop()
	owner := h.userID(ctx)
	var operations []models.MailOperationSummary
	if err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		operations, err = db.ListRetryableMailOperationsForUser(ctx, owner)
		return err
	}); err != nil {
		userAccountError(w, r, err)
		return
	}
	accounts, byAccount := groupMailOperationsByAccount(operations)
	result := retryFailedMailOperationsResponse{}
	for _, account := range accounts {
		ids := byAccount[account]
		var retried, skipped int
		err := h.userAccounts.WithAccountForUser(ctx, owner, account, func(_ *config.AccountStore, db *storage.DB) error {
			var err error
			retried, skipped, err = db.RetryMailOperationsForUserGuarded(ctx, owner, ids, h.mailOperationAccountGuard(ctx, owner, account))
			return err
		})
		if err != nil {
			log.Printf("retry failed mail operations: account %s: %v", account, err)
			result.Skipped += len(ids)
			continue
		}
		result.Retried += retried
		result.Skipped += skipped
		if retried > 0 {
			if err := h.userIMAP.WakeMutations(ctx, owner, account); err != nil {
				log.Printf("mail operations retry saved; owned wake failed: %v", err)
				w.Header().Set("X-Gofer-Mail-Delivery", "delayed")
			}
		}
	}
	writeMailOperationsJSON(w, http.StatusOK, result)
}

// handleUserRetryMailAction and handleUserUndoMailAction act on every change
// one action queued, account by account, each inside its account's guard.
// An account that refuses is counted as skipped and the others still go
// ahead.
func (h *Handler) handleUserRetryMailAction(w http.ResponseWriter, r *http.Request) {
	h.changeUserMailAction(w, r, true)
}

func (h *Handler) handleUserUndoMailAction(w http.ResponseWriter, r *http.Request) {
	h.changeUserMailAction(w, r, false)
}

func (h *Handler) changeUserMailAction(w http.ResponseWriter, r *http.Request, retry bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	stop := context.AfterFunc(h.userStorageContext, cancel)
	defer stop()
	owner, actionID := h.userID(ctx), strings.TrimSpace(r.PathValue("id"))
	var accounts []string
	if err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		accounts, err = db.MailActionAccountsForUser(ctx, owner, actionID)
		return err
	}); err != nil {
		userAccountError(w, r, err)
		return
	}
	result := mailActionChangeResponse{}
	for _, account := range accounts {
		var changed int
		err := h.userAccounts.WithAccountForUser(ctx, owner, account, func(_ *config.AccountStore, db *storage.DB) error {
			var err error
			guard := h.mailOperationAccountGuard(ctx, owner, account)
			if retry {
				changed, err = db.RetryMailActionForUser(ctx, owner, actionID, account, guard)
			} else {
				changed, err = db.UndoMailActionForUser(ctx, owner, actionID, account, guard)
			}
			return err
		})
		if err != nil {
			log.Printf("mail action %s: account %s: %v", actionID, account, err)
			result.Skipped++
			continue
		}
		result.Changed += changed
		// Undo stays in Gofer, so only a retry has anything to send.
		if retry && changed > 0 {
			if err := h.userIMAP.WakeMutations(ctx, owner, account); err != nil {
				log.Printf("mail action retry saved; owned wake failed: %v", err)
				w.Header().Set("X-Gofer-Mail-Delivery", "delayed")
			}
		}
	}
	writeMailActionResult(w, r, retry, result)
}
