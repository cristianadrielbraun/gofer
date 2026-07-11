package handler

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/google/uuid"

	imapclient "github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const imapDraftRevisionHeader = "X-Gofer-Draft-Revision"

func (h *Handler) queueIMAPDraftUpsert(ctx context.Context, accountID string, localMessageID int64, draftKey, folderID, folderRemoteName string, remoteUID, uidValidity uint32, msg *message.OutgoingMessage) error {
	revisionToken := uuid.NewString()
	raw, err := message.BuildMIMEMessageForIMAPDraft(msg, revisionToken)
	if err != nil {
		return err
	}
	_, err = h.db.QueueIMAPDraftUpsert(ctx, storage.QueueIMAPDraftUpsertInput{
		State: storage.IMAPDraftState{
			AccountID: accountID, DraftKey: draftKey, LocalMessageID: localMessageID,
			FolderID: folderID, FolderRemoteName: folderRemoteName,
			RemoteUID: remoteUID, UIDValidity: uidValidity,
		},
		RevisionToken: revisionToken,
		MIMEData:      raw,
		MessageDate:   msg.Date,
	})
	if err == nil {
		h.signalOutgoingWorker()
	}
	return err
}

func (h *Handler) queueIMAPDraftDelete(ctx context.Context, accountID, draftKey string, info *storage.DraftProviderInfo) error {
	if info == nil || strings.TrimSpace(info.AccountProvider) == providers.ProviderGmail || strings.TrimSpace(info.AccountProvider) == providers.ProviderOutlook {
		return nil
	}
	if strings.TrimSpace(info.FolderRemoteName) == "" {
		return fmt.Errorf("remote Drafts folder is not available")
	}
	_, err := h.db.QueueIMAPDraftDelete(ctx, storage.IMAPDraftState{
		AccountID: accountID, DraftKey: draftKey, LocalMessageID: info.MessageID,
		FolderID: info.FolderID, FolderRemoteName: info.FolderRemoteName,
		RemoteUID: info.RemoteUID, UIDValidity: info.UIDValidity,
	})
	if err == nil {
		h.signalOutgoingWorker()
	}
	return err
}

func (h *Handler) runDueIMAPDraftOperations(ctx context.Context) {
	for {
		operations, err := h.db.ClaimDueIMAPDraftOperations(ctx, time.Now(), 5)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("imap-draft: claim operations: %v", err)
			}
			return
		}
		if len(operations) == 0 {
			return
		}
		for _, operation := range operations {
			h.reconcileIMAPDraftOperation(ctx, operation)
		}
	}
}

func (h *Handler) reconcileIMAPDraftOperation(ctx context.Context, operation storage.IMAPDraftOperation) {
	cfg, err := h.accountStore.GetConfig(ctx, operation.State.AccountID)
	if err != nil {
		h.finishIMAPDraftOperation(operation, storage.IMAPDraftStatusFailed, fmt.Errorf("account not found"))
		return
	}
	password, err := h.resolvePassword(ctx, cfg, operation.State.AccountID)
	if err != nil {
		h.finishIMAPDraftOperation(operation, storage.IMAPDraftStatusFailed, fmt.Errorf("get IMAP credentials: %w", err))
		return
	}
	factory := h.sentCopyIMAPFactory
	if factory == nil {
		factory = func(ctx context.Context, cfg *models.AccountConfig, password string) (sentCopyIMAPClient, error) {
			return imapclient.NewClient(ctx, cfg, password)
		}
	}
	client, err := factory(ctx, cfg, password)
	if err != nil {
		h.finishIMAPDraftOperation(operation, storage.IMAPDraftStatusFailed, fmt.Errorf("connect to IMAP for draft sync: %w", err))
		return
	}
	defer client.Close()

	if operation.Kind == storage.IMAPDraftOperationDelete {
		h.deleteRemoteIMAPDraft(ctx, client, operation)
		return
	}
	h.upsertRemoteIMAPDraft(ctx, client, operation)
}

func (h *Handler) upsertRemoteIMAPDraft(ctx context.Context, client sentCopyIMAPClient, operation storage.IMAPDraftOperation) {
	var remoteUID, uidValidity uint32
	if operation.Status == storage.IMAPDraftStatusAmbiguous {
		var err error
		remoteUID, uidValidity, err = client.FindUIDByHeaderWithValidity(ctx, operation.State.FolderRemoteName, imapDraftRevisionHeader, operation.RevisionToken)
		if err != nil {
			h.finishIMAPDraftOperation(operation, storage.IMAPDraftStatusAmbiguous, fmt.Errorf("check ambiguous draft revision: %w", err))
			return
		}
	}
	if remoteUID == 0 {
		result, err := client.AppendMessage(ctx, operation.State.FolderRemoteName, operation.MIMEData, []goimap.Flag{goimap.FlagSeen, goimap.FlagDraft}, operation.MessageDate)
		if err != nil {
			status := storage.IMAPDraftStatusFailed
			if imapclient.IsAppendAmbiguous(err) {
				status = storage.IMAPDraftStatusAmbiguous
			}
			h.finishIMAPDraftOperation(operation, status, err)
			return
		}
		remoteUID, uidValidity = result.UID, result.UIDValidity
		if remoteUID == 0 {
			remoteUID, uidValidity, err = client.FindUIDByHeaderWithValidity(ctx, operation.State.FolderRemoteName, imapDraftRevisionHeader, operation.RevisionToken)
			if err != nil || remoteUID == 0 {
				if err == nil {
					err = fmt.Errorf("appended draft revision has no remote UID")
				}
				h.finishIMAPDraftOperation(operation, storage.IMAPDraftStatusAmbiguous, err)
				return
			}
		}
	}

	oldUID := operation.State.RemoteUID
	if oldUID > 0 && oldUID != remoteUID && (operation.State.UIDValidity == 0 || uidValidity == 0 || operation.State.UIDValidity == uidValidity) {
		if _, err := client.DeleteMessagesIfUIDValidity(ctx, operation.State.FolderRemoteName, []uint32{oldUID}, operation.State.UIDValidity); err != nil {
			h.finishIMAPDraftOperation(operation, storage.IMAPDraftStatusAmbiguous, fmt.Errorf("delete replaced draft UID %d: %w", oldUID, err))
			return
		}
	}
	state, err := h.db.CompleteIMAPDraftUpsert(ctx, operation.ID, remoteUID, uidValidity)
	if err != nil {
		h.finishIMAPDraftOperation(operation, storage.IMAPDraftStatusAmbiguous, fmt.Errorf("save remote draft revision: %w", err))
		return
	}
	h.linkLocalIMAPDraftUID(ctx, state)
}

func (h *Handler) deleteRemoteIMAPDraft(ctx context.Context, client sentCopyIMAPClient, operation storage.IMAPDraftOperation) {
	if operation.State.RemoteUID > 0 {
		if _, err := client.DeleteMessagesIfUIDValidity(ctx, operation.State.FolderRemoteName, []uint32{operation.State.RemoteUID}, operation.State.UIDValidity); err != nil {
			h.finishIMAPDraftOperation(operation, storage.IMAPDraftStatusFailed, fmt.Errorf("delete remote draft UID %d: %w", operation.State.RemoteUID, err))
			return
		}
	}
	if err := h.db.CompleteIMAPDraftDelete(ctx, operation.ID); err != nil {
		h.finishIMAPDraftOperation(operation, storage.IMAPDraftStatusFailed, fmt.Errorf("complete remote draft deletion: %w", err))
	}
}

func (h *Handler) linkLocalIMAPDraftUID(ctx context.Context, state storage.IMAPDraftState) {
	if state.LocalMessageID <= 0 || state.FolderID == "" || state.RemoteUID == 0 {
		return
	}
	storedUIDValidity, err := h.db.GetStoredUIDValidity(ctx, state.FolderID)
	if err != nil {
		log.Printf("imap-draft: read UID validity account=%s draft=%s: %v", state.AccountID, state.DraftKey, err)
		return
	}
	if state.UIDValidity > 0 && storedUIDValidity > 0 && state.UIDValidity != storedUIDValidity {
		log.Printf("imap-draft: skip UID link account=%s draft=%s uidvalidity=%d stored=%d", state.AccountID, state.DraftKey, state.UIDValidity, storedUIDValidity)
		return
	}
	if err := h.db.SetMessageFolderRemoteUID(ctx, state.LocalMessageID, state.FolderID, state.RemoteUID); err != nil {
		log.Printf("imap-draft: link remote UID account=%s draft=%s uid=%d: %v", state.AccountID, state.DraftKey, state.RemoteUID, err)
	}
}

func (h *Handler) finishIMAPDraftOperation(operation storage.IMAPDraftOperation, status string, err error) {
	errText := "Failed to sync draft with IMAP."
	if err != nil {
		errText = err.Error()
	}
	nextAttempt := time.Now().Add(sentCopyRetryDelay(operation.AttemptCount))
	if dbErr := h.db.FinishIMAPDraftOperationWithError(context.Background(), operation.ID, status, errText, nextAttempt); dbErr != nil {
		log.Printf("imap-draft: finish operation %s: %v", operation.ID, dbErr)
		return
	}
	log.Printf("imap-draft: operation %s %s; retry at %s: %s", operation.ID, status, nextAttempt.Format(time.RFC3339), errText)
}
