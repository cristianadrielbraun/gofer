package handler

import (
	"context"
	"errors"
	"log"
	"time"
)

// contactPushRetry is how often a pull asked for by a push checks whether
// the pull already running for the account has ended. Tests shorten it.
var contactPushRetry = time.Second

// contactPushTimeout bounds one pull asked for by a push, the wait for a
// pull already running included.
const contactPushTimeout = 2 * time.Minute

// JMAPContactsChanged refreshes the contacts of a JMAP account whose server
// pushed a contact or address book change, now rather than at the next
// scheduled check. Accounts that don't sync contacts are left as they are.
func (h *Handler) JMAPContactsChanged(ctx context.Context, accountID string) {
	if err := h.contactsChangedOnProvider(ctx, accountID); err != nil && ctx.Err() == nil {
		log.Printf("jmap push account=%s contacts: refresh: %v", accountID, err)
	}
}

// contactsChangedOnProvider pulls an account's contacts after its provider
// announced a change, through the owned contact worker in managed mode.
func (h *Handler) contactsChangedOnProvider(ctx context.Context, accountID string) error {
	if h.userStorage != nil {
		return h.wakeUserContactAccountID(ctx, accountID)
	}
	h.syncContactsAfterPush(accountID)
	return nil
}

// syncContactsAfterPush pulls the shared store account's contacts after its
// provider announced a change. A pull already running may have read the
// provider before the change, so this one waits for it to end and pulls
// anyway. Announcements that arrive meanwhile ask for one more pull after
// it, however many there are.
func (h *Handler) syncContactsAfterPush(accountID string) {
	h.contactSyncMu.Lock()
	if h.contactPushSyncs == nil {
		h.contactPushSyncs = make(map[string]bool)
	}
	if _, pending := h.contactPushSyncs[accountID]; pending {
		h.contactPushSyncs[accountID] = true
		h.contactSyncMu.Unlock()
		return
	}
	h.contactPushSyncs[accountID] = false
	h.contactSyncMu.Unlock()
	go func() {
		for {
			h.pullContactsAfterPush(accountID)
			h.contactSyncMu.Lock()
			if !h.contactPushSyncs[accountID] {
				delete(h.contactPushSyncs, accountID)
				h.contactSyncMu.Unlock()
				return
			}
			h.contactPushSyncs[accountID] = false
			h.contactSyncMu.Unlock()
		}
	}()
}

func (h *Handler) pullContactsAfterPush(accountID string) {
	ctx, cancel := context.WithTimeout(context.Background(), contactPushTimeout)
	defer cancel()
	for {
		_, err := h.SyncContactAccount(ctx, accountID)
		if !errors.Is(err, errContactSyncAlreadyRunning) {
			if err != nil {
				log.Printf("contacts push account=%s: refresh: %v", accountID, err)
			}
			return
		}
		select {
		case <-ctx.Done():
			log.Printf("contacts push account=%s: refresh: the pull already running did not end in time", accountID)
			return
		case <-time.After(contactPushRetry):
		}
	}
}
