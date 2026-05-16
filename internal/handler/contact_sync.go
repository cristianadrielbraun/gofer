package handler

import (
	"context"
	"errors"
	"log"
	"time"
)

var errContactSyncAlreadyRunning = errors.New("contact sync already running for this account")

func (h *Handler) StartContactSync(ctx context.Context) {
	go func() {
		log.Printf("contacts sync: startup sync started")
		h.SyncContactsForAllAccounts(ctx)
		log.Printf("contacts sync: startup sync finished")

		for {
			interval := h.contactSyncInterval(ctx)
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
				h.SyncContactsForAllAccounts(ctx)
			}
		}
	}()
}

func (h *Handler) contactSyncInterval(ctx context.Context) time.Duration {
	accounts, err := h.db.GetAllAccountIDs(ctx)
	if err == nil {
		for _, accountID := range accounts {
			userID, err := h.db.GetAccountUserID(ctx, accountID)
			if err == nil && userID != "" {
				if interval := h.db.GetSyncInterval(ctx, userID); interval > 0 {
					return time.Duration(interval) * time.Minute
				}
			}
		}
	}
	return 5 * time.Minute
}

func (h *Handler) SyncContactsForAllAccounts(ctx context.Context) {
	accounts, err := h.db.GetAllAccountIDs(ctx)
	if err != nil {
		log.Printf("contacts sync: list accounts: %v", err)
		return
	}
	for _, accountID := range accounts {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if _, err := h.SyncContactAccount(ctx, accountID); err != nil && !errors.Is(err, errContactSyncAlreadyRunning) {
			log.Printf("contacts sync %s: %v", accountID, err)
		}
	}
}

func (h *Handler) SyncContactAccount(ctx context.Context, accountID string) (int, error) {
	userID, err := h.db.GetAccountUserID(ctx, accountID)
	if err != nil || userID == "" {
		return 0, err
	}
	accounts, err := h.contactSyncAccounts(ctx, userID, accountID)
	if err != nil || len(accounts) == 0 {
		return 0, err
	}
	return h.syncContactAccount(ctx, userID, accounts[0])
}

func (h *Handler) syncContactAccount(ctx context.Context, userID string, account contactSyncAccount) (int, error) {
	if !h.beginContactSync(account.ID) {
		return 0, errContactSyncAlreadyRunning
	}
	defer h.endContactSync(account.ID)
	if err := h.db.MarkContactSyncStarted(ctx, userID, account.ID, account.Provider); err != nil {
		log.Printf("contacts sync %s: mark started: %v", account.ID, err)
	}

	imported, err := h.pullContactAccount(ctx, userID, account)
	if err != nil {
		if markErr := h.db.MarkContactSyncError(ctx, userID, account.ID, account.Provider, err.Error()); markErr != nil {
			log.Printf("contacts sync %s: mark error: %v", account.ID, markErr)
		}
		log.Printf("contacts sync %s: %v", account.ID, err)
		return imported, err
	}
	if err := h.db.MarkContactSyncSuccess(ctx, userID, account.ID, account.Provider, "", imported); err != nil {
		log.Printf("contacts sync %s: mark success: %v", account.ID, err)
	}
	log.Printf("contacts sync %s: %d imported or updated", account.ID, imported)
	return imported, nil
}

func (h *Handler) contactSyncRunningAccounts() map[string]bool {
	h.contactSyncMu.Lock()
	defer h.contactSyncMu.Unlock()
	running := make(map[string]bool, len(h.contactSyncRunning))
	for accountID := range h.contactSyncRunning {
		running[accountID] = true
	}
	return running
}

func (h *Handler) beginContactSync(accountID string) bool {
	h.contactSyncMu.Lock()
	defer h.contactSyncMu.Unlock()
	if _, ok := h.contactSyncRunning[accountID]; ok {
		return false
	}
	h.contactSyncRunning[accountID] = struct{}{}
	return true
}

func (h *Handler) endContactSync(accountID string) {
	h.contactSyncMu.Lock()
	delete(h.contactSyncRunning, accountID)
	h.contactSyncMu.Unlock()
}
