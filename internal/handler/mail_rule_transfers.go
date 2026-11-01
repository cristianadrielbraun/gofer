package handler

import (
	"context"
	"log"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Rules that move mail to another account's folder queue it; this worker
// starts a mail transfer for each source account and folder they name,
// which copies the messages and then deletes them where they were.
const mailRuleTransferBatch = 500

// startMailRuleTransfers listens for rules that queued moves, and on start
// picks up the ones left from before.
func (h *Handler) startMailRuleTransfers(ctx context.Context) {
	wake := make(chan string, 64)
	storage.MailRuleTransfersQueued = func(owner string) {
		select {
		case wake <- owner:
		default:
		}
	}
	go func() {
		var owners []string
		var err error
		if h.userStorage != nil {
			owners, err = h.userStorage.MailRuleTransferOwners(ctx)
		} else if h.db != nil {
			owners, err = h.db.MailRuleTransferOwners(ctx)
		}
		if err != nil {
			log.Printf("mail rules: find queued moves: %v", err)
		}
		for _, owner := range owners {
			h.startQueuedMailRuleTransfers(ctx, owner)
		}
		for {
			select {
			case <-ctx.Done():
				return
			case owner := <-wake:
				h.startQueuedMailRuleTransfers(ctx, owner)
			}
		}
	}()
}

// startQueuedMailRuleTransfers starts transfers for the owner's queued
// moves and takes them off the queue. A move that can't start is logged and
// dropped: the message stays where it is.
func (h *Handler) startQueuedMailRuleTransfers(ctx context.Context, owner string) {
	for {
		var queued []storage.MailRuleTransfer
		err := h.withProfileStore(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
			var err error
			queued, err = db.ListMailRuleTransfers(ctx, owner, mailRuleTransferBatch)
			return err
		})
		if err != nil {
			log.Printf("mail rules: queued moves of %s: %v", owner, err)
			return
		}
		if len(queued) == 0 {
			return
		}
		type key struct {
			source, target string
			forGood        bool
		}
		groups := map[key]map[string][]int64{}
		var order []key
		ids := make([]int64, 0, len(queued))
		for _, t := range queued {
			k := key{t.SourceAccountID, t.TargetFolderID, t.ForGood}
			if groups[k] == nil {
				groups[k] = map[string][]int64{}
				order = append(order, k)
			}
			groups[k][t.SourceFolderID] = append(groups[k][t.SourceFolderID], t.MessageID)
			ids = append(ids, t.ID)
		}
		for _, k := range order {
			if err := h.startMailRuleTransfer(ctx, owner, k.source, k.target, k.forGood, groups[k]); err != nil {
				log.Printf("mail rules: move mail of %s from %s to folder %s: %v", owner, k.source, k.target, err)
			}
		}
		err = h.withProfileStore(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
			return db.DeleteMailRuleTransfers(ctx, owner, ids)
		})
		if err != nil {
			log.Printf("mail rules: clear queued moves of %s: %v", owner, err)
			return
		}
		if len(queued) < mailRuleTransferBatch || ctx.Err() != nil {
			return
		}
	}
}

func (h *Handler) startMailRuleTransfer(ctx context.Context, owner, sourceID, targetFolderID string, forGood bool, ids map[string][]int64) error {
	if h.mailTransfers == nil {
		return errMailRuleTransfersUnavailable
	}
	source, ok, err := h.mailExportAccount(ctx, owner, sourceID)
	if err != nil {
		return err
	}
	if !ok || mailTransferSourceProblem(source) != "" {
		return errMailRuleTransferSource
	}
	target, destination, found, err := h.mailTransferDestination(ctx, owner, targetFolderID)
	if err != nil {
		return err
	}
	if !found || mailImportAccountProblem(target) != "" {
		return errMailRuleTransferTarget
	}
	mode := mailTransferMove
	if forGood {
		mode = mailTransferMoveForGood
	}
	_, err = h.mailTransferMessages(ctx, owner, source, target, destination, ids, mode)
	return err
}

type mailRuleTransferError string

func (e mailRuleTransferError) Error() string { return string(e) }

const (
	errMailRuleTransfersUnavailable = mailRuleTransferError("mail transfers are not available")
	errMailRuleTransferSource       = mailRuleTransferError("the account the mail is in can't be read")
	errMailRuleTransferTarget       = mailRuleTransferError("the folder can't take mail")
)
