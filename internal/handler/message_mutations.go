package handler

import (
	"context"
	"database/sql"
	"errors"
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

var errProviderDeleteAlreadyGone = errors.New("provider message was already deleted")

type messageMutationIMAPClient interface {
	StoreFlags(ctx context.Context, folderRemoteName string, uid uint32, op goimap.StoreFlagsOp, flags []goimap.Flag) error
	StoreFlagsBatch(ctx context.Context, folderRemoteName string, uids []uint32, op goimap.StoreFlagsOp, flags []goimap.Flag) error
	StoreFlagColorBatchIfUIDValidity(ctx context.Context, folder string, uids []uint32, starred bool, bits int, validity uint32) error
	MoveMessageWithDestUID(ctx context.Context, folderRemoteName string, uid uint32, destFolderRemoteName string) (uint32, error)
	DeleteMessagesIfUIDValidity(ctx context.Context, folderRemoteName string, uids []uint32, expectedUIDValidity uint32) (bool, error)
	FindUIDByMessageID(ctx context.Context, remoteName, messageID string) (uint32, error)
	Close() error
}

type messageMutationIMAPClientFactory func(context.Context, *models.AccountConfig, string) (messageMutationIMAPClient, error)

type outlookMessageMoveResponse struct {
	ID string `json:"id"`
}

func (h *Handler) signalMessageMutationWorker() {
	select {
	case h.messageMutationWake <- struct{}{}:
	default:
	}
}

func (h *Handler) queueMessageMoves(ctx context.Context, infos []storage.ThreadMessageMutationInfo, destinationFolderID string) error {
	userID := h.userID(ctx)
	bySource := make(map[string][]int64)
	for _, info := range infos {
		bySource[info.FolderID] = append(bySource[info.FolderID], info.MessageID)
	}
	for sourceFolderID, messageIDs := range bySource {
		if err := h.db.MoveMessagesAndQueueForUser(ctx, messageIDs, sourceFolderID, destinationFolderID, userID); err != nil {
			return err
		}
	}
	h.signalMessageMutationWorker()
	return nil
}

func (h *Handler) queuePermanentDeletes(ctx context.Context, infos []storage.ThreadMessageMutationInfo) error {
	userID := h.userID(ctx)
	byFolder := make(map[string][]int64)
	for _, info := range infos {
		byFolder[info.FolderID] = append(byFolder[info.FolderID], info.MessageID)
	}
	for folderID, messageIDs := range byFolder {
		if err := h.db.PermanentlyDeleteMessagesAndQueueForUser(ctx, messageIDs, folderID, userID); err != nil {
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
		// Read, starred and delete changes of one folder and value go together.
		batched := make(map[string]bool)
		for i, mutation := range mutations {
			if !storage.MessageMutationBatchable(mutation) {
				h.applyQueuedMessageMutation(ctx, mutation)
				continue
			}
			key := messageMutationBatchKey(mutation)
			if batched[key] {
				continue
			}
			batched[key] = true
			group := []storage.MessageMutation{mutation}
			for _, other := range mutations[i+1:] {
				if storage.MessageMutationBatchable(other) && messageMutationBatchKey(other) == key {
					group = append(group, other)
				}
			}
			h.applyQueuedMessageMutationBatch(ctx, group)
		}
	}
}

func (h *Handler) applyQueuedMessageMutation(parent context.Context, mutation storage.MessageMutation) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	if _, err := h.db.GetMessageMutationInternal(ctx, mutation.ID); err == sql.ErrNoRows {
		return
	} else if err != nil {
		log.Printf("message-mutation: reload id=%s: %v", mutation.ID, err)
		return
	}
	applyErr := h.applyRemoteMessageMutation(ctx, mutation)
	alreadyGone := errors.Is(applyErr, errProviderDeleteAlreadyGone)
	if applyErr != nil && !alreadyGone {
		nextAttempt := time.Now().Add(sentCopyRetryDelay(mutation.AttemptCount))
		if dbErr := h.db.FinishMessageMutationWithError(context.Background(), mutation.ID, applyErr.Error(), nextAttempt); dbErr != nil {
			log.Printf("message-mutation: save failure id=%s: %v", mutation.ID, dbErr)
			return
		}
		log.Printf("message-mutation: %s message=%d failed; retry at %s: %v", mutation.Kind, mutation.MessageID, nextAttempt.Format(time.RFC3339), applyErr)
		return
	}
	complete := h.db.CompleteMessageMutation
	if mutation.Kind == storage.MessageMutationDelete {
		complete = h.db.CompleteMessageDeleteMutation
	}
	if err := complete(context.Background(), mutation.ID); err != nil {
		log.Printf("message-mutation: mark applied id=%s: %v", mutation.ID, err)
		nextAttempt := time.Now().Add(sentCopyRetryDelay(mutation.AttemptCount))
		if dbErr := h.db.FinishMessageMutationWithError(context.Background(), mutation.ID, "Provider update succeeded, but Gofer could not save the result: "+err.Error(), nextAttempt); dbErr != nil {
			log.Printf("message-mutation: schedule applied-state retry id=%s: %v", mutation.ID, dbErr)
		}
		return
	}
	if alreadyGone {
		if err := h.db.ConfirmMessageDeleteMutation(context.Background(), mutation.ID); err != nil {
			log.Printf("message-mutation: confirm already deleted id=%s: %v", mutation.ID, err)
		}
	}
}

func (h *Handler) applyRemoteMessageMutation(ctx context.Context, mutation storage.MessageMutation) error {
	info, err := h.db.GetMessageMutationInfoForReplay(ctx, mutation)
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
	case storage.MessageMutationProviderJMAP:
		return h.applyJMAPMessageMutation(ctx, mutation, *info)
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
	case providers.ProviderJMAP:
		return storage.MessageMutationProviderJMAP
	default:
		return storage.MessageMutationProviderIMAP
	}
}

func (h *Handler) applyGmailMessageMutation(ctx context.Context, mutation storage.MessageMutation, info storage.MessageMutationInfo) error {
	if h.mailCredentials() == nil {
		return fmt.Errorf("Gmail authentication is not available")
	}
	token, err := h.mailCredentials().GetOAuthTokenForAccount(ctx, info.AccountID)
	if err != nil {
		return fmt.Errorf("get Gmail token: %w", err)
	}
	if err := h.applyProviderFolderCopies(ctx, mutation, info, token); err != nil {
		return err
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
	switch mutation.Kind {
	case storage.MessageMutationDelete:
		err := h.deleteGmailAPIMessage(ctx, token, providerMessageID)
		if googleAPIStatus(err, http.StatusNotFound) {
			return errProviderDeleteAlreadyGone
		}
		return err
	case storage.MessageMutationMove:
		return h.applyGmailMessageMove(ctx, mutation, info, token, providerMessageID)
	case storage.MessageMutationRead, storage.MessageMutationStarred:
		addLabels, removeLabels := gmailFlagLabels(mutation)
		return h.modifyGmailMessageLabels(ctx, token, providerMessageID, addLabels, removeLabels)
	default:
		return fmt.Errorf("unsupported Gmail mutation %q", mutation.Kind)
	}
}

// gmailFlagLabels returns the labels a read or starred change adds and removes.
func gmailFlagLabels(mutation storage.MessageMutation) (add, remove []string) {
	label := "UNREAD"
	on := !mutation.TargetValue
	if mutation.Kind == storage.MessageMutationStarred {
		label, on = "STARRED", mutation.TargetValue
	}
	if on {
		return []string{label}, nil
	}
	return nil, []string{label}
}

// outlookFlagPayload is the Graph update for a read or starred change.
func outlookFlagPayload(mutation storage.MessageMutation) any {
	if mutation.Kind == storage.MessageMutationRead {
		return map[string]bool{"isRead": mutation.TargetValue}
	}
	status := "notFlagged"
	if mutation.TargetValue {
		status = "flagged"
	}
	return map[string]any{"flag": map[string]string{"flagStatus": status}}
}

func (h *Handler) applyOutlookMessageMutation(ctx context.Context, mutation storage.MessageMutation, info storage.MessageMutationInfo) error {
	if h.mailCredentials() == nil {
		return fmt.Errorf("Outlook authentication is not available")
	}
	token, err := h.mailCredentials().GetMicrosoftGraphMailTokenForAccount(ctx, info.AccountID)
	if err != nil {
		return fmt.Errorf("get Outlook token: %w", err)
	}
	if err := h.applyProviderFolderCopies(ctx, mutation, info, token); err != nil {
		return err
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
	case storage.MessageMutationDelete:
		err := h.doOutlookJSON(ctx, http.MethodDelete, endpoint, token, nil, nil)
		if outlookAPIStatus(err, http.StatusNotFound) {
			return errProviderDeleteAlreadyGone
		}
		return err
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
	case storage.MessageMutationRead, storage.MessageMutationStarred:
		return h.doOutlookJSON(ctx, http.MethodPatch, endpoint, token, outlookFlagPayload(mutation), nil)
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
	if mutation.Kind == storage.MessageMutationDelete {
		uid := info.RemoteUID
		if uid == 0 {
			uid, err = client.FindUIDByMessageID(ctx, info.FolderRemoteID, info.InternetMessageID)
			if err != nil {
				return err
			}
		}
		if uid == 0 {
			return errProviderDeleteAlreadyGone
		}
		expectedUIDValidity := mutation.SourceUIDValidity
		if expectedUIDValidity == 0 {
			expectedUIDValidity, err = h.db.GetFolderUIDValidity(ctx, mutation.FolderID)
			if err != nil {
				return err
			}
			if expectedUIDValidity == 0 {
				return fmt.Errorf("IMAP UIDVALIDITY is unavailable; waiting for folder sync before deleting")
			}
		}
		validityChanged, err := client.DeleteMessagesIfUIDValidity(ctx, info.FolderRemoteID, []uint32{uid}, expectedUIDValidity)
		if err != nil {
			return err
		}
		if validityChanged {
			return fmt.Errorf("IMAP UIDVALIDITY changed; waiting for folder resync before deleting")
		}
		return nil
	}
	if mutation.Kind == storage.MessageMutationMove {
		return h.applyIMAPMessageMove(ctx, client, mutation, info)
	}
	uid := info.RemoteUID
	if uid == 0 {
		if mutation.IsScopedFlagColor() {
			return fmt.Errorf("message has no remote IMAP identity; waiting for folder sync")
		}
		uid, err = client.FindUIDByMessageID(ctx, info.FolderRemoteID, info.InternetMessageID)
		if err != nil {
			return err
		}
	}
	if uid == 0 {
		return fmt.Errorf("message has no remote IMAP identity")
	}
	if bits, color := mutation.FlagBitsTarget(); color {
		validity := mutation.SourceUIDValidity
		if validity == 0 {
			validity, err = h.db.GetFolderUIDValidity(ctx, info.FolderID)
			if err != nil {
				return err
			}
		}
		return client.StoreFlagColorBatchIfUIDValidity(ctx, info.FolderRemoteID, []uint32{uid}, mutation.TargetValue, bits, validity)
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
	if err := h.moveGmailMessage(ctx, mutation, info, token, providerMessageID); err != nil {
		return err
	}
	return h.db.AdvanceMessageMoveMutation(ctx, mutation.ID, mutation.DestinationFolderID, providerMessageID, 0)
}

// moveGmailMessage changes one Gmail message's labels for a queued move.
func (h *Handler) moveGmailMessage(ctx context.Context, mutation storage.MessageMutation, info storage.MessageMutationInfo, token, providerMessageID string) error {
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
	return err
}

// applyProviderFolderCopies makes a change reach the further copies of the
// message in its folder. It runs before the folder state's own copy, so a
// retry repeats it; a copy the provider no longer has is done.
func (h *Handler) applyProviderFolderCopies(ctx context.Context, mutation storage.MessageMutation, info storage.MessageMutationInfo, token string) error {
	outlookDestination := ""
	if len(info.FolderCopyIDs) > 0 && mutation.Kind == storage.MessageMutationMove && mutation.ProviderType == storage.MessageMutationProviderOutlook {
		var err error
		if outlookDestination, err = h.db.GetFolderProviderRemoteID(ctx, mutation.DestinationFolderID); err != nil {
			return fmt.Errorf("load Outlook destination: %w", err)
		}
		if outlookDestination = strings.TrimSpace(outlookDestination); outlookDestination == "" {
			return fmt.Errorf("Outlook destination has no provider identity")
		}
	}
	for _, id := range info.FolderCopyIDs {
		var err error
		if mutation.ProviderType == storage.MessageMutationProviderGmail {
			switch mutation.Kind {
			case storage.MessageMutationDelete:
				err = h.deleteGmailAPIMessage(ctx, token, id)
			case storage.MessageMutationMove:
				err = h.moveGmailMessage(ctx, mutation, info, token, id)
			default:
				add, remove := gmailFlagLabels(mutation)
				err = h.modifyGmailMessageLabels(ctx, token, id, add, remove)
			}
			if googleAPIStatus(err, http.StatusNotFound) {
				err = nil
			}
		} else {
			endpoint := outlookGraphBaseURL + "/me/messages/" + url.PathEscape(id)
			switch mutation.Kind {
			case storage.MessageMutationDelete:
				err = h.doOutlookJSON(ctx, http.MethodDelete, endpoint, token, nil, nil)
			case storage.MessageMutationMove:
				err = h.doOutlookJSON(ctx, http.MethodPost, endpoint+"/move", token, map[string]string{"destinationId": outlookDestination}, nil)
			default:
				err = h.doOutlookJSON(ctx, http.MethodPatch, endpoint, token, outlookFlagPayload(mutation), nil)
			}
			if outlookAPIStatus(err, http.StatusNotFound) {
				err = nil
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
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

// readMailAction is the action that marks mail read, or unread.
func readMailAction(read bool) string {
	if read {
		return storage.MailActionMarkRead
	}
	return storage.MailActionMarkUnread
}

// starMailAction is the action that stars mail, or unstars it.
func starMailAction(starred bool) string {
	if starred {
		return storage.MailActionStar
	}
	return storage.MailActionUnstar
}

// spamMailAction is the action that marks mail as spam, or as not spam.
func spamMailAction(disposition spamDisposition) string {
	if disposition == spamDispositionSpam {
		return storage.MailActionSpam
	}
	return storage.MailActionNotSpam
}
