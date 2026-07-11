package handler

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	goimap "github.com/emersion/go-imap/v2"

	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type messageMutationIMAPClient interface {
	StoreFlags(ctx context.Context, folderRemoteName string, uid uint32, op goimap.StoreFlagsOp, flags []goimap.Flag) error
	MoveMessageWithDestUID(ctx context.Context, folderRemoteName string, uid uint32, destFolderRemoteName string) (uint32, error)
	FindUIDByMessageID(ctx context.Context, remoteName, messageID string) (uint32, error)
	Close() error
}

type messageMutationIMAPClientFactory func(context.Context, *models.AccountConfig, string) (messageMutationIMAPClient, error)

func (h *Handler) signalMessageMutationWorker() {
	select {
	case h.messageMutationWake <- struct{}{}:
	default:
	}
}

func (h *Handler) queueMessageMoves(ctx context.Context, infos []storage.ThreadMessageMutationInfo, destinationFolderID string) error {
	bySource := make(map[string][]int64)
	for _, info := range infos {
		bySource[info.FolderID] = append(bySource[info.FolderID], info.MessageID)
	}
	for sourceFolderID, messageIDs := range bySource {
		if err := h.db.MoveMessagesAndQueue(ctx, messageIDs, sourceFolderID, destinationFolderID); err != nil {
			return err
		}
	}
	h.signalMessageMutationWorker()
	return nil
}

func (h *Handler) StartMessageMutationWorker(ctx context.Context) {
	go func() {
		if count, err := h.db.MarkInterruptedMessageMutationsPending(ctx); err != nil {
			log.Printf("message-mutation: recover interrupted operations: %v", err)
		} else if count > 0 {
			log.Printf("message-mutation: recovered %d interrupted operation(s)", count)
		}
		h.runDueMessageMutations(ctx)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.messageMutationWake:
				h.runDueMessageMutations(ctx)
			case <-ticker.C:
				h.runDueMessageMutations(ctx)
			}
		}
	}()
}

func (h *Handler) runDueMessageMutations(ctx context.Context) {
	for {
		mutations, err := h.db.ClaimDueMessageMutations(ctx, time.Now(), 25)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("message-mutation: claim operations: %v", err)
			}
			return
		}
		if len(mutations) == 0 {
			return
		}
		for _, mutation := range mutations {
			h.applyQueuedMessageMutation(ctx, mutation)
		}
	}
}

func (h *Handler) applyQueuedMessageMutation(parent context.Context, mutation storage.MessageMutation) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	if err := h.applyRemoteMessageMutation(ctx, mutation); err != nil {
		nextAttempt := time.Now().Add(sentCopyRetryDelay(mutation.AttemptCount))
		if dbErr := h.db.FinishMessageMutationWithError(context.Background(), mutation.ID, err.Error(), nextAttempt); dbErr != nil {
			log.Printf("message-mutation: save failure id=%s: %v", mutation.ID, dbErr)
			return
		}
		log.Printf("message-mutation: %s message=%d failed; retry at %s: %v", mutation.Kind, mutation.MessageID, nextAttempt.Format(time.RFC3339), err)
		return
	}
	if err := h.db.CompleteMessageMutation(context.Background(), mutation.ID); err != nil {
		log.Printf("message-mutation: mark applied id=%s: %v", mutation.ID, err)
		nextAttempt := time.Now().Add(sentCopyRetryDelay(mutation.AttemptCount))
		if dbErr := h.db.FinishMessageMutationWithError(context.Background(), mutation.ID, "Provider update succeeded, but Gofer could not save the result: "+err.Error(), nextAttempt); dbErr != nil {
			log.Printf("message-mutation: schedule applied-state retry id=%s: %v", mutation.ID, dbErr)
		}
	}
}

func (h *Handler) applyRemoteMessageMutation(ctx context.Context, mutation storage.MessageMutation) error {
	var info *storage.MessageMutationInfo
	var err error
	if mutation.Kind == storage.MessageMutationMove {
		info, err = h.db.GetMessageMutationInfoIncludingDeletedInFolder(ctx, mutation.MessageID, mutation.FolderID)
	} else if mutation.FolderID != "" {
		info, err = h.db.GetMessageMutationInfoInFolder(ctx, mutation.MessageID, mutation.FolderID)
	} else {
		info, err = h.db.GetMessageMutationInfo(ctx, mutation.MessageID)
	}
	if err != nil {
		return err
	}
	if info == nil {
		return h.db.DiscardMessageMutation(ctx, mutation.ID)
	}
	if info.AccountID != mutation.AccountID {
		return fmt.Errorf("message account changed")
	}
	if messageMutationProvider(info.AccountProvider) != mutation.ProviderType {
		return fmt.Errorf("message provider changed from %s", mutation.ProviderType)
	}
	switch mutation.ProviderType {
	case storage.MessageMutationProviderGmail:
		return h.applyGmailMessageMutation(ctx, mutation, *info)
	case storage.MessageMutationProviderOutlook:
		return h.applyOutlookMessageMutation(ctx, mutation, *info)
	case storage.MessageMutationProviderIMAP:
		return h.applyIMAPMessageMutation(ctx, mutation, *info)
	default:
		return fmt.Errorf("unsupported message mutation provider %q", mutation.ProviderType)
	}
}

func messageMutationProvider(provider string) string {
	switch strings.TrimSpace(provider) {
	case providers.ProviderGmail:
		return storage.MessageMutationProviderGmail
	case providers.ProviderOutlook:
		return storage.MessageMutationProviderOutlook
	default:
		return storage.MessageMutationProviderIMAP
	}
}

func (h *Handler) applyGmailMessageMutation(ctx context.Context, mutation storage.MessageMutation, info storage.MessageMutationInfo) error {
	if h.auth == nil {
		return fmt.Errorf("Gmail authentication is not available")
	}
	token, err := h.auth.GetOAuthTokenForAccount(ctx, info.AccountID)
	if err != nil {
		return fmt.Errorf("get Gmail token: %w", err)
	}
	providerMessageID := strings.TrimSpace(info.RemoteMessageID)
	if providerMessageID == "" {
		providerMessageID, err = h.resolveGmailMessageID(ctx, token, mutation.MessageID, info)
		if err != nil {
			return fmt.Errorf("resolve Gmail message: %w", err)
		}
	}
	if providerMessageID == "" {
		return fmt.Errorf("Gmail message identity is unavailable")
	}
	var addLabels, removeLabels []string
	switch mutation.Kind {
	case storage.MessageMutationMove:
		return h.applyGmailMessageMove(ctx, mutation, info, token, providerMessageID)
	case storage.MessageMutationRead:
		if mutation.TargetValue {
			removeLabels = []string{"UNREAD"}
		} else {
			addLabels = []string{"UNREAD"}
		}
	case storage.MessageMutationStarred:
		if mutation.TargetValue {
			addLabels = []string{"STARRED"}
		} else {
			removeLabels = []string{"STARRED"}
		}
	default:
		return fmt.Errorf("unsupported Gmail mutation %q", mutation.Kind)
	}
	return h.modifyGmailMessageLabels(ctx, token, providerMessageID, addLabels, removeLabels)
}

func (h *Handler) applyOutlookMessageMutation(ctx context.Context, mutation storage.MessageMutation, info storage.MessageMutationInfo) error {
	if h.auth == nil {
		return fmt.Errorf("Outlook authentication is not available")
	}
	token, err := h.auth.GetMicrosoftGraphMailTokenForAccount(ctx, info.AccountID)
	if err != nil {
		return fmt.Errorf("get Outlook token: %w", err)
	}
	providerMessageID := strings.TrimSpace(info.RemoteMessageID)
	if providerMessageID == "" {
		providerMessageID, err = h.resolveOutlookMessageID(ctx, token, mutation.MessageID, info)
		if err != nil {
			return fmt.Errorf("resolve Outlook message: %w", err)
		}
	}
	if providerMessageID == "" {
		return fmt.Errorf("Outlook message identity is unavailable")
	}
	endpoint := outlookGraphBaseURL + "/me/messages/" + url.PathEscape(providerMessageID)
	switch mutation.Kind {
	case storage.MessageMutationMove:
		destinationProviderID, err := h.db.GetFolderProviderRemoteID(ctx, mutation.DestinationFolderID)
		if err != nil {
			return fmt.Errorf("load Outlook destination: %w", err)
		}
		destinationProviderID = strings.TrimSpace(destinationProviderID)
		if destinationProviderID == "" {
			return fmt.Errorf("Outlook destination has no provider identity")
		}
		var moved outlookMessageMoveResponse
		err = h.doOutlookJSON(ctx, http.MethodPost, endpoint+"/move", token, map[string]string{"destinationId": destinationProviderID}, &moved)
		if outlookAPIStatus(err, http.StatusNotFound) {
			providerMessageID, err = h.resolveOutlookMessageID(ctx, token, mutation.MessageID, info)
			if err == nil {
				endpoint = outlookGraphBaseURL + "/me/messages/" + url.PathEscape(providerMessageID)
				err = h.doOutlookJSON(ctx, http.MethodPost, endpoint+"/move", token, map[string]string{"destinationId": destinationProviderID}, &moved)
			}
		}
		if err != nil {
			return err
		}
		movedID := strings.TrimSpace(moved.ID)
		if movedID == "" {
			movedID = providerMessageID
		}
		return h.db.AdvanceMessageMoveMutation(ctx, mutation.ID, mutation.DestinationFolderID, movedID, 0)
	case storage.MessageMutationRead:
		return h.doOutlookJSON(ctx, http.MethodPatch, endpoint, token, map[string]bool{"isRead": mutation.TargetValue}, nil)
	case storage.MessageMutationStarred:
		status := "notFlagged"
		if mutation.TargetValue {
			status = "flagged"
		}
		return h.doOutlookJSON(ctx, http.MethodPatch, endpoint, token, map[string]any{"flag": map[string]string{"flagStatus": status}}, nil)
	default:
		return fmt.Errorf("unsupported Outlook mutation %q", mutation.Kind)
	}
}

func (h *Handler) applyIMAPMessageMutation(ctx context.Context, mutation storage.MessageMutation, info storage.MessageMutationInfo) error {
	if strings.TrimSpace(info.FolderRemoteID) == "" {
		return fmt.Errorf("message has no remote IMAP folder identity")
	}
	if h.accountStore == nil {
		return fmt.Errorf("IMAP account storage is not available")
	}
	cfg, err := h.accountStore.GetConfig(ctx, info.AccountID)
	if err != nil {
		return err
	}
	password, err := h.resolvePassword(ctx, cfg, info.AccountID)
	if err != nil {
		return err
	}
	factory := h.messageMutationIMAPFactory
	if factory == nil {
		factory = func(ctx context.Context, cfg *models.AccountConfig, password string) (messageMutationIMAPClient, error) {
			return imap.NewClient(ctx, cfg, password)
		}
	}
	client, err := factory(ctx, cfg, password)
	if err != nil {
		return err
	}
	defer client.Close()
	if mutation.Kind == storage.MessageMutationMove {
		return h.applyIMAPMessageMove(ctx, client, mutation, info)
	}
	uid := info.RemoteUID
	if uid == 0 {
		uid, err = client.FindUIDByMessageID(ctx, info.FolderRemoteID, info.InternetMessageID)
		if err != nil {
			return err
		}
	}
	if uid == 0 {
		return fmt.Errorf("message has no remote IMAP identity")
	}
	op := goimap.StoreFlagsDel
	if mutation.TargetValue {
		op = goimap.StoreFlagsAdd
	}
	var flag goimap.Flag
	switch mutation.Kind {
	case storage.MessageMutationRead:
		flag = goimap.FlagSeen
	case storage.MessageMutationStarred:
		flag = goimap.FlagFlagged
	default:
		return fmt.Errorf("unsupported IMAP mutation %q", mutation.Kind)
	}
	return client.StoreFlags(ctx, info.FolderRemoteID, uid, op, []goimap.Flag{flag})
}

func (h *Handler) applyGmailMessageMove(ctx context.Context, mutation storage.MessageMutation, info storage.MessageMutationInfo, token, providerMessageID string) error {
	destinationProviderID, destinationRole, err := h.db.GetFolderProviderRemoteInfo(ctx, mutation.DestinationFolderID)
	if err != nil {
		return fmt.Errorf("load Gmail destination: %w", err)
	}
	destinationProviderID = strings.TrimSpace(destinationProviderID)
	destinationRole = strings.TrimSpace(destinationRole)
	if destinationRole == "" {
		return fmt.Errorf("Gmail destination has no role")
	}
	if strings.TrimSpace(info.FolderRole) == "trash" && destinationRole != "trash" {
		if err := h.untrashGmailMessage(ctx, token, providerMessageID); err != nil {
			return err
		}
	}
	removeLabels := []string{}
	if sourceLabelID, err := h.db.GetFolderProviderRemoteID(ctx, mutation.FolderID); err == nil {
		removeLabels = append(removeLabels, gmailSourceMoveRemoveLabels(info.FolderRole, sourceLabelID, destinationProviderID)...)
	}
	switch destinationRole {
	case "trash":
		err = h.trashGmailMessage(ctx, token, providerMessageID)
	case "inbox":
		removeLabels = append(removeLabels, "SPAM", "TRASH")
		err = h.modifyGmailMessageLabels(ctx, token, providerMessageID, []string{"INBOX"}, removeLabels)
	case "junk", "spam":
		removeLabels = append(removeLabels, "INBOX", "TRASH")
		err = h.modifyGmailMessageLabels(ctx, token, providerMessageID, []string{"SPAM"}, removeLabels)
	case "archive":
		removeLabels = append(removeLabels, "INBOX")
		err = h.modifyGmailMessageLabels(ctx, token, providerMessageID, nil, removeLabels)
	case "starred":
		removeLabels = append(removeLabels, "INBOX")
		err = h.modifyGmailMessageLabels(ctx, token, providerMessageID, []string{"STARRED"}, removeLabels)
	default:
		if destinationProviderID == "" || destinationProviderID == "ARCHIVE" {
			return fmt.Errorf("Gmail destination has no provider identity")
		}
		removeLabels = append(removeLabels, "INBOX")
		err = h.modifyGmailMessageLabels(ctx, token, providerMessageID, []string{destinationProviderID}, removeLabels)
	}
	if err != nil {
		return err
	}
	return h.db.AdvanceMessageMoveMutation(ctx, mutation.ID, mutation.DestinationFolderID, providerMessageID, 0)
}

func (h *Handler) applyIMAPMessageMove(ctx context.Context, client messageMutationIMAPClient, mutation storage.MessageMutation, info storage.MessageMutationInfo) error {
	destinationRemoteID, err := h.db.GetFolderRemoteID(ctx, mutation.DestinationFolderID)
	if err != nil {
		return fmt.Errorf("load IMAP destination: %w", err)
	}
	destinationRemoteID = strings.TrimSpace(destinationRemoteID)
	if destinationRemoteID == "" {
		return fmt.Errorf("IMAP destination has no remote identity")
	}
	uid := info.RemoteUID
	if uid == 0 {
		uid, err = client.FindUIDByMessageID(ctx, info.FolderRemoteID, info.InternetMessageID)
		if err != nil {
			return err
		}
	}
	if uid == 0 {
		destinationUID, findErr := client.FindUIDByMessageID(ctx, destinationRemoteID, info.InternetMessageID)
		if findErr == nil && destinationUID > 0 {
			return h.db.AdvanceMessageMoveMutation(ctx, mutation.ID, mutation.DestinationFolderID, "", destinationUID)
		}
		return fmt.Errorf("message has no remote IMAP identity")
	}
	destinationUID, err := client.MoveMessageWithDestUID(ctx, info.FolderRemoteID, uid, destinationRemoteID)
	if err != nil {
		recoveredUID, findErr := client.FindUIDByMessageID(ctx, destinationRemoteID, info.InternetMessageID)
		if findErr != nil || recoveredUID == 0 {
			return err
		}
		destinationUID = recoveredUID
	}
	return h.db.AdvanceMessageMoveMutation(ctx, mutation.ID, mutation.DestinationFolderID, "", destinationUID)
}
