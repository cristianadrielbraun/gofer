package handler

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	goimap "github.com/emersion/go-imap/v2"
)

type outlookMessageMoveResponse struct {
	ID string `json:"id"`
}

func (h *Handler) setRemoteMessageRead(ctx context.Context, messageID int64, info storage.MessageMutationInfo, read bool) {
	if h.trySetGmailMessageRead(ctx, messageID, info, read) {
		return
	}
	if h.trySetOutlookMessageRead(ctx, messageID, info, read) {
		return
	}
	op := goimap.StoreFlagsAdd
	if !read {
		op = goimap.StoreFlagsDel
	}
	if err := h.storeIMAPMessageFlags(ctx, info, op, []goimap.Flag{goimap.FlagSeen}); err != nil {
		log.Printf("imap mark read account=%s message=%d: %v", info.AccountID, messageID, err)
	}
}

func (h *Handler) setRemoteMessageStarred(ctx context.Context, messageID int64, info storage.MessageMutationInfo, starred bool) {
	if h.trySetGmailMessageStarred(ctx, messageID, info, starred) {
		return
	}
	if h.trySetOutlookMessageStarred(ctx, messageID, info, starred) {
		return
	}
	op := goimap.StoreFlagsAdd
	if !starred {
		op = goimap.StoreFlagsDel
	}
	if err := h.storeIMAPMessageFlags(ctx, info, op, []goimap.Flag{goimap.FlagFlagged}); err != nil {
		log.Printf("imap mark starred account=%s message=%d: %v", info.AccountID, messageID, err)
	}
}

func (h *Handler) moveRemoteMessage(ctx context.Context, messageID int64, info storage.MessageMutationInfo, destinationFolderID, destinationIMAPRemoteID string) {
	if h.tryMoveGmailMessage(ctx, messageID, info, destinationFolderID) {
		return
	}
	if h.tryMoveOutlookMessage(ctx, messageID, info, destinationFolderID) {
		return
	}
	if strings.TrimSpace(destinationIMAPRemoteID) == "" {
		return
	}
	if err := h.moveIMAPMessage(ctx, info, destinationIMAPRemoteID); err != nil {
		log.Printf("imap move account=%s message=%d: %v", info.AccountID, messageID, err)
	}
}

func (h *Handler) deleteRemoteMessage(ctx context.Context, messageID int64, info storage.MessageMutationInfo) {
	if h.tryDeleteGmailMessage(ctx, messageID, info) {
		return
	}
	if h.tryDeleteOutlookMessage(ctx, messageID, info) {
		return
	}
	if err := h.deleteIMAPMessage(ctx, info); err != nil {
		log.Printf("imap delete account=%s message=%d: %v", info.AccountID, messageID, err)
	}
}

func (h *Handler) trySetOutlookMessageRead(ctx context.Context, messageID int64, info storage.MessageMutationInfo, read bool) bool {
	token, providerMessageID, ok := h.outlookMutationIdentity(ctx, messageID, info)
	if !ok {
		return false
	}
	endpoint := outlookGraphBaseURL + "/me/messages/" + url.PathEscape(providerMessageID)
	if err := h.doOutlookJSON(ctx, http.MethodPatch, endpoint, token, map[string]bool{"isRead": read}, nil); err != nil {
		log.Printf("outlook mark read account=%s message=%d: %v", info.AccountID, messageID, err)
		return false
	}
	return true
}

func (h *Handler) trySetOutlookMessageStarred(ctx context.Context, messageID int64, info storage.MessageMutationInfo, starred bool) bool {
	token, providerMessageID, ok := h.outlookMutationIdentity(ctx, messageID, info)
	if !ok {
		return false
	}
	flagStatus := "notFlagged"
	if starred {
		flagStatus = "flagged"
	}
	endpoint := outlookGraphBaseURL + "/me/messages/" + url.PathEscape(providerMessageID)
	if err := h.doOutlookJSON(ctx, http.MethodPatch, endpoint, token, map[string]any{"flag": map[string]string{"flagStatus": flagStatus}}, nil); err != nil {
		log.Printf("outlook mark starred account=%s message=%d: %v", info.AccountID, messageID, err)
		return false
	}
	return true
}

func (h *Handler) tryMoveOutlookMessage(ctx context.Context, messageID int64, info storage.MessageMutationInfo, destinationFolderID string) bool {
	token, providerMessageID, ok := h.outlookMutationIdentity(ctx, messageID, info)
	if !ok {
		return false
	}
	destinationProviderID, err := h.db.GetFolderProviderRemoteID(ctx, destinationFolderID)
	if err != nil || strings.TrimSpace(destinationProviderID) == "" {
		if err != nil {
			log.Printf("outlook move destination folder id account=%s folder=%s: %v", info.AccountID, destinationFolderID, err)
		}
		return false
	}
	endpoint := outlookGraphBaseURL + "/me/messages/" + url.PathEscape(providerMessageID) + "/move"
	var moved outlookMessageMoveResponse
	if err := h.doOutlookJSON(ctx, http.MethodPost, endpoint, token, map[string]string{"destinationId": destinationProviderID}, &moved); err != nil {
		log.Printf("outlook move account=%s message=%d: %v", info.AccountID, messageID, err)
		return false
	}
	if moved.ID != "" && moved.ID != providerMessageID {
		if err := h.db.SetMessageProviderMessageID(ctx, messageID, moved.ID); err != nil {
			log.Printf("outlook move cache message id account=%s message=%d: %v", info.AccountID, messageID, err)
		}
	}
	return true
}

func (h *Handler) tryDeleteOutlookMessage(ctx context.Context, messageID int64, info storage.MessageMutationInfo) bool {
	token, providerMessageID, ok := h.outlookMutationIdentity(ctx, messageID, info)
	if !ok {
		return false
	}
	endpoint := outlookGraphBaseURL + "/me/messages/" + url.PathEscape(providerMessageID)
	if err := h.doOutlookJSON(ctx, http.MethodDelete, endpoint, token, nil, nil); err != nil {
		log.Printf("outlook delete account=%s message=%d: %v", info.AccountID, messageID, err)
		return false
	}
	return true
}

func (h *Handler) outlookMutationIdentity(ctx context.Context, messageID int64, info storage.MessageMutationInfo) (string, string, bool) {
	return h.outlookGraphMessageIdentity(ctx, messageID, info, "mutation")
}

func (h *Handler) outlookGraphMessageIdentity(ctx context.Context, messageID int64, info storage.MessageMutationInfo, operation string) (string, string, bool) {
	if strings.TrimSpace(info.AccountProvider) != providers.ProviderOutlook || h.auth == nil {
		return "", "", false
	}
	token, err := h.auth.GetMicrosoftGraphMailTokenForAccount(ctx, info.AccountID)
	if err != nil {
		log.Printf("outlook %s token account=%s message=%d: %v", operation, info.AccountID, messageID, err)
		return "", "", false
	}
	providerMessageID := strings.TrimSpace(info.RemoteMessageID)
	if providerMessageID == "" {
		providerMessageID, err = h.resolveOutlookMessageID(ctx, token, messageID, info)
		if err != nil {
			log.Printf("outlook %s resolve account=%s message=%d: %v", operation, info.AccountID, messageID, err)
			return "", "", false
		}
	}
	if providerMessageID == "" {
		return "", "", false
	}
	return token, providerMessageID, true
}

func (h *Handler) storeIMAPMessageFlags(ctx context.Context, info storage.MessageMutationInfo, op goimap.StoreFlagsOp, flags []goimap.Flag) error {
	sourceRemoteID := strings.TrimSpace(info.FolderRemoteID)
	if sourceRemoteID == "" {
		return fmt.Errorf("message has no remote IMAP folder identity")
	}
	client, err := h.connectIMAP(ctx, info.AccountID)
	if err != nil {
		return err
	}
	defer client.Close()

	uid, err := h.imapUIDForMutation(ctx, client, info)
	if err != nil {
		return err
	}
	return client.StoreFlags(ctx, sourceRemoteID, uid, op, flags)
}

func (h *Handler) moveIMAPMessage(ctx context.Context, info storage.MessageMutationInfo, destinationRemoteID string) error {
	sourceRemoteID := strings.TrimSpace(info.FolderRemoteID)
	if sourceRemoteID == "" {
		return fmt.Errorf("message has no remote IMAP folder identity")
	}
	client, err := h.connectIMAP(ctx, info.AccountID)
	if err != nil {
		return err
	}
	defer client.Close()

	uid, err := h.imapUIDForMutation(ctx, client, info)
	if err != nil {
		return err
	}
	return client.MoveMessage(ctx, sourceRemoteID, uid, destinationRemoteID)
}

func (h *Handler) deleteIMAPMessage(ctx context.Context, info storage.MessageMutationInfo) error {
	sourceRemoteID := strings.TrimSpace(info.FolderRemoteID)
	if sourceRemoteID == "" {
		return fmt.Errorf("message has no remote IMAP folder identity")
	}
	client, err := h.connectIMAP(ctx, info.AccountID)
	if err != nil {
		return err
	}
	defer client.Close()

	uid, err := h.imapUIDForMutation(ctx, client, info)
	if err != nil {
		return err
	}
	return client.DeleteMessages(ctx, sourceRemoteID, []uint32{uid})
}

func (h *Handler) imapUIDForMutation(ctx context.Context, client *imap.Client, info storage.MessageMutationInfo) (uint32, error) {
	if info.RemoteUID > 0 {
		return info.RemoteUID, nil
	}
	uid, err := client.FindUIDByMessageID(ctx, info.FolderRemoteID, info.InternetMessageID)
	if err != nil {
		return 0, err
	}
	if uid == 0 {
		return 0, fmt.Errorf("message has no remote IMAP identity")
	}
	return uid, nil
}
