package handler

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const gmailAPIMessageFetchMaxBytes int64 = 64 << 20

type gmailAPIMessageResponse struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
	Raw      string `json:"raw"`
}

type gmailAPIDraftResponse struct {
	ID      string                  `json:"id"`
	Message gmailAPIMessageResponse `json:"message"`
}

func gmailAPIMailRuntimeEnabled() bool {
	return true
}

func (h *Handler) shouldUseGmailAPIMailRuntime(provider string) bool {
	return strings.TrimSpace(provider) == providers.ProviderGmail && gmailAPIMailRuntimeEnabled()
}

func (h *Handler) trySetGmailMessageRead(ctx context.Context, messageID int64, info storage.MessageMutationInfo, read bool) bool {
	if !h.shouldUseGmailAPIMailRuntime(info.AccountProvider) {
		return false
	}
	token, providerMessageID, ok := h.gmailMessageIdentity(ctx, messageID, info, "read mutation")
	if !ok {
		return true
	}
	var addLabels, removeLabels []string
	if read {
		removeLabels = []string{"UNREAD"}
	} else {
		addLabels = []string{"UNREAD"}
	}
	if err := h.modifyGmailMessageLabels(ctx, token, providerMessageID, addLabels, removeLabels); err != nil {
		log.Printf("gmail mark read account=%s message=%d: %v", info.AccountID, messageID, err)
	}
	return true
}

func (h *Handler) trySetGmailMessageStarred(ctx context.Context, messageID int64, info storage.MessageMutationInfo, starred bool) bool {
	if !h.shouldUseGmailAPIMailRuntime(info.AccountProvider) {
		return false
	}
	token, providerMessageID, ok := h.gmailMessageIdentity(ctx, messageID, info, "star mutation")
	if !ok {
		return true
	}
	var addLabels, removeLabels []string
	if starred {
		addLabels = []string{"STARRED"}
	} else {
		removeLabels = []string{"STARRED"}
	}
	if err := h.modifyGmailMessageLabels(ctx, token, providerMessageID, addLabels, removeLabels); err != nil {
		log.Printf("gmail mark starred account=%s message=%d: %v", info.AccountID, messageID, err)
	}
	return true
}

func (h *Handler) tryMoveGmailMessage(ctx context.Context, messageID int64, info storage.MessageMutationInfo, destinationFolderID string) bool {
	if !h.shouldUseGmailAPIMailRuntime(info.AccountProvider) {
		return false
	}
	token, providerMessageID, ok := h.gmailMessageIdentity(ctx, messageID, info, "move mutation")
	if !ok {
		return true
	}
	destinationProviderID, destinationRole, err := h.db.GetFolderProviderRemoteInfo(ctx, destinationFolderID)
	if err != nil {
		log.Printf("gmail move destination folder account=%s folder=%s: %v", info.AccountID, destinationFolderID, err)
		return true
	}
	if strings.TrimSpace(destinationRole) == "" {
		return true
	}
	if strings.TrimSpace(info.FolderRole) == "trash" && destinationRole != "trash" {
		if err := h.untrashGmailMessage(ctx, token, providerMessageID); err != nil {
			log.Printf("gmail untrash before move account=%s message=%d: %v", info.AccountID, messageID, err)
			return true
		}
	}

	removeLabels := []string{}
	if sourceLabelID, err := h.db.GetFolderProviderRemoteID(ctx, info.FolderID); err == nil {
		removeLabels = append(removeLabels, gmailSourceMoveRemoveLabels(info.FolderRole, sourceLabelID, destinationProviderID)...)
	}

	switch destinationRole {
	case "trash":
		if err := h.trashGmailMessage(ctx, token, providerMessageID); err != nil {
			log.Printf("gmail move trash account=%s message=%d: %v", info.AccountID, messageID, err)
		}
	case "inbox":
		removeLabels = append(removeLabels, "SPAM", "TRASH")
		if err := h.modifyGmailMessageLabels(ctx, token, providerMessageID, []string{"INBOX"}, removeLabels); err != nil {
			log.Printf("gmail move inbox account=%s message=%d: %v", info.AccountID, messageID, err)
		}
	case "junk", "spam":
		removeLabels = append(removeLabels, "INBOX", "TRASH")
		if err := h.modifyGmailMessageLabels(ctx, token, providerMessageID, []string{"SPAM"}, removeLabels); err != nil {
			log.Printf("gmail move spam account=%s message=%d: %v", info.AccountID, messageID, err)
		}
	case "archive":
		removeLabels = append(removeLabels, "INBOX")
		if err := h.modifyGmailMessageLabels(ctx, token, providerMessageID, nil, removeLabels); err != nil {
			log.Printf("gmail archive account=%s message=%d: %v", info.AccountID, messageID, err)
		}
	case "starred":
		removeLabels = append(removeLabels, "INBOX")
		if err := h.modifyGmailMessageLabels(ctx, token, providerMessageID, []string{"STARRED"}, removeLabels); err != nil {
			log.Printf("gmail move starred account=%s message=%d: %v", info.AccountID, messageID, err)
		}
	default:
		destinationProviderID = strings.TrimSpace(destinationProviderID)
		if destinationProviderID == "" || destinationProviderID == "ARCHIVE" {
			return true
		}
		removeLabels = append(removeLabels, "INBOX")
		if err := h.modifyGmailMessageLabels(ctx, token, providerMessageID, []string{destinationProviderID}, removeLabels); err != nil {
			log.Printf("gmail move label account=%s message=%d: %v", info.AccountID, messageID, err)
		}
	}
	return true
}

func gmailSourceMoveRemoveLabels(sourceRole, sourceProviderID, destinationProviderID string) []string {
	sourceProviderID = strings.TrimSpace(sourceProviderID)
	if sourceProviderID == "" || sourceProviderID == "ARCHIVE" || sourceProviderID == strings.TrimSpace(destinationProviderID) {
		return nil
	}
	switch sourceRole {
	case "custom":
		return []string{sourceProviderID}
	default:
		return nil
	}
}

func (h *Handler) tryDeleteGmailMessage(ctx context.Context, messageID int64, info storage.MessageMutationInfo) bool {
	if !h.shouldUseGmailAPIMailRuntime(info.AccountProvider) {
		return false
	}
	token, providerMessageID, ok := h.gmailMessageIdentity(ctx, messageID, info, "delete mutation")
	if !ok {
		return true
	}
	if err := h.deleteGmailAPIMessage(ctx, token, providerMessageID); err != nil {
		log.Printf("gmail delete account=%s message=%d: %v", info.AccountID, messageID, err)
	}
	return true
}

func (h *Handler) gmailMessageIdentity(ctx context.Context, messageID int64, info storage.MessageMutationInfo, operation string) (string, string, bool) {
	if !h.shouldUseGmailAPIMailRuntime(info.AccountProvider) || h.auth == nil {
		return "", "", false
	}
	token, err := h.auth.GetOAuthTokenForAccount(ctx, info.AccountID)
	if err != nil {
		log.Printf("gmail %s token account=%s message=%d: %v", operation, info.AccountID, messageID, err)
		return "", "", false
	}
	providerMessageID := strings.TrimSpace(info.RemoteMessageID)
	if providerMessageID == "" {
		providerMessageID, err = h.resolveGmailMessageID(ctx, token, messageID, info)
		if err != nil {
			log.Printf("gmail %s resolve account=%s message=%d: %v", operation, info.AccountID, messageID, err)
			return "", "", false
		}
	}
	if providerMessageID == "" {
		return "", "", false
	}
	return token, providerMessageID, true
}

func (h *Handler) modifyGmailMessageLabels(ctx context.Context, token, providerMessageID string, addLabels, removeLabels []string) error {
	payload := map[string][]string{}
	if addLabels = cleanGmailLabelIDs(addLabels); len(addLabels) > 0 {
		payload["addLabelIds"] = addLabels
	}
	if removeLabels = cleanGmailLabelIDs(removeLabels); len(removeLabels) > 0 {
		payload["removeLabelIds"] = removeLabels
	}
	if len(payload) == 0 {
		return nil
	}
	endpoint := gmailAPIBaseURL + "/users/me/messages/" + url.PathEscape(strings.TrimSpace(providerMessageID)) + "/modify"
	return doGoogleJSON(ctx, http.MethodPost, endpoint, token, payload, nil)
}

func cleanGmailLabelIDs(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func (h *Handler) trashGmailMessage(ctx context.Context, token, providerMessageID string) error {
	endpoint := gmailAPIBaseURL + "/users/me/messages/" + url.PathEscape(strings.TrimSpace(providerMessageID)) + "/trash"
	return doGoogleJSON(ctx, http.MethodPost, endpoint, token, map[string]any{}, nil)
}

func (h *Handler) untrashGmailMessage(ctx context.Context, token, providerMessageID string) error {
	endpoint := gmailAPIBaseURL + "/users/me/messages/" + url.PathEscape(strings.TrimSpace(providerMessageID)) + "/untrash"
	return doGoogleJSON(ctx, http.MethodPost, endpoint, token, map[string]any{}, nil)
}

func (h *Handler) deleteGmailAPIMessage(ctx context.Context, token, providerMessageID string) error {
	providerMessageID = strings.TrimSpace(providerMessageID)
	if providerMessageID == "" {
		return nil
	}
	endpoint := gmailAPIBaseURL + "/users/me/messages/" + url.PathEscape(providerMessageID)
	return doGoogleJSON(ctx, http.MethodDelete, endpoint, token, nil, nil)
}

func (h *Handler) fetchGmailAPIMessageMIME(ctx context.Context, messageID int64) ([]byte, bool, error) {
	info, err := h.db.GetMessageMutationInfo(ctx, messageID)
	if err != nil || info == nil {
		return nil, false, err
	}
	if !h.shouldUseGmailAPIMailRuntime(info.AccountProvider) {
		return nil, false, nil
	}
	token, providerMessageID, ok := h.gmailMessageIdentity(ctx, messageID, *info, "body fetch")
	if !ok {
		return nil, true, fmt.Errorf("gmail message identity unavailable")
	}
	bodyData, err := h.fetchGmailAPIMessageMIMEByID(ctx, token, providerMessageID)
	return bodyData, true, err
}

func (h *Handler) fetchGmailAPIMessageMIMEByID(ctx context.Context, token, providerMessageID string) ([]byte, error) {
	values := url.Values{}
	values.Set("format", "raw")
	endpoint := gmailAPIBaseURL + "/users/me/messages/" + url.PathEscape(strings.TrimSpace(providerMessageID)) + "?" + values.Encode()
	var response gmailAPIMessageResponse
	if err := doGoogleJSON(ctx, http.MethodGet, endpoint, token, nil, &response); err != nil {
		return nil, err
	}
	raw, err := decodeGmailRaw(response.Raw)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > gmailAPIMessageFetchMaxBytes {
		return nil, fmt.Errorf("gmail message MIME exceeds %d bytes", gmailAPIMessageFetchMaxBytes)
	}
	return raw, nil
}

func (h *Handler) fetchGmailAPIAttachmentContent(ctx context.Context, token, providerMessageID, providerAttachmentID string) ([]byte, error) {
	providerMessageID = strings.TrimSpace(providerMessageID)
	providerAttachmentID = strings.TrimSpace(providerAttachmentID)
	if providerMessageID == "" || providerAttachmentID == "" {
		return nil, fmt.Errorf("gmail attachment identity unavailable")
	}
	endpoint := gmailAPIBaseURL + "/users/me/messages/" + url.PathEscape(providerMessageID) + "/attachments/" + url.PathEscape(providerAttachmentID)
	var response struct {
		Data string `json:"data"`
		Size int64  `json:"size"`
	}
	if err := doGoogleJSON(ctx, http.MethodGet, endpoint, token, nil, &response); err != nil {
		return nil, err
	}
	raw, err := decodeGmailRaw(response.Data)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > gmailAPIMessageFetchMaxBytes {
		return nil, fmt.Errorf("gmail attachment exceeds %d bytes", gmailAPIMessageFetchMaxBytes)
	}
	return raw, nil
}

func decodeGmailRaw(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	if raw, err := base64.RawURLEncoding.DecodeString(value); err == nil {
		return raw, nil
	}
	return base64.URLEncoding.DecodeString(value)
}

func (h *Handler) saveGmailAPIDraft(ctx context.Context, accountID string, localMessageID int64, msg *message.OutgoingMessage) error {
	if !gmailAPIMailRuntimeEnabled() {
		return nil
	}
	if h.auth == nil {
		return fmt.Errorf("google oauth not configured")
	}
	token, err := h.auth.GetOAuthTokenForAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if existing, err := h.db.GetDraftProviderInfo(ctx, accountID, msg.MessageID); err == nil && existing != nil {
		if providerID := strings.TrimSpace(existing.ProviderMessageID); providerID != "" {
			if err := h.deleteGmailAPIMessage(ctx, token, providerID); err != nil && !googleAPIStatus(err, http.StatusNotFound) {
				return err
			}
		}
	}
	raw, err := message.BuildMIMEMessageForGraph(msg)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"message": map[string]string{
			"raw": base64.RawURLEncoding.EncodeToString(raw),
		},
	}
	var created gmailAPIDraftResponse
	if err := doGoogleJSON(ctx, http.MethodPost, gmailAPIBaseURL+"/users/me/drafts", token, payload, &created); err != nil {
		return err
	}
	providerID := strings.TrimSpace(created.Message.ID)
	if providerID == "" {
		providerID = strings.TrimSpace(created.ID)
	}
	if providerID == "" {
		return fmt.Errorf("gmail draft response missing id")
	}
	return h.db.SetMessageProviderMessageID(ctx, localMessageID, providerID)
}

func (h *Handler) deleteGmailAPIDraft(ctx context.Context, accountID, providerMessageID string) error {
	providerMessageID = strings.TrimSpace(providerMessageID)
	if providerMessageID == "" || h.auth == nil || !gmailAPIMailRuntimeEnabled() {
		return nil
	}
	token, err := h.auth.GetOAuthTokenForAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if err := h.deleteGmailAPIMessage(ctx, token, providerMessageID); err != nil && !googleAPIStatus(err, http.StatusNotFound) {
		return err
	}
	return nil
}

func (h *Handler) sendGmailAPIMessage(ctx context.Context, cfg *models.AccountConfig, msg *message.OutgoingMessage, draftID string) (bool, string, string) {
	if cfg == nil || !h.shouldUseGmailAPIMailRuntime(cfg.Provider) {
		return false, "", ""
	}
	raw, err := message.BuildMIMEMessageForGraph(msg)
	if err != nil {
		return true, "failed", err.Error()
	}
	providerMessageID, token, err := h.sendGmailAPIRaw(ctx, cfg, raw)
	if err != nil {
		return true, "failed", err.Error()
	}

	h.saveSentMessageSnapshot(ctx, cfg.AccountID, msg, raw)
	h.cacheGmailSentMessageID(ctx, cfg.AccountID, msg, token, providerMessageID)
	if strings.TrimSpace(draftID) != "" {
		draftProvider, _ := h.db.GetDraftProviderInfo(ctx, cfg.AccountID, draftID)
		if folderID, err := h.db.DeleteDraftMessage(ctx, cfg.AccountID, draftID); err == nil && folderID != "" {
			h.publishMutation(cfg.AccountID, folderID)
		}
		if draftProvider != nil {
			if err := h.deleteGmailAPIDraft(ctx, cfg.AccountID, draftProvider.ProviderMessageID); err != nil {
				log.Printf("gmail draft delete after send account=%s draft=%s: %v", cfg.AccountID, draftID, err)
			}
		}
	}
	return true, "sent", ""
}

func (h *Handler) sendGmailAPIRaw(ctx context.Context, cfg *models.AccountConfig, raw []byte) (string, string, error) {
	if h.auth == nil {
		return "", "", fmt.Errorf("google oauth not configured")
	}
	token, err := h.auth.GetOAuthTokenForAccount(ctx, cfg.AccountID)
	if err != nil {
		return "", "", err
	}
	payload := map[string]string{"raw": base64.RawURLEncoding.EncodeToString(raw)}
	var sent gmailAPIMessageResponse
	if err := doGoogleJSON(ctx, http.MethodPost, gmailAPIBaseURL+"/users/me/messages/send", token, payload, &sent); err != nil {
		if _, definitive := err.(googleAPIError); !definitive {
			err = fmt.Errorf("%w: %v", errOutgoingSendAmbiguous, err)
		}
		return "", token, err
	}
	return strings.TrimSpace(sent.ID), token, nil
}

func (h *Handler) cacheGmailSentMessageID(ctx context.Context, accountID string, msg *message.OutgoingMessage, token, providerMessageID string) {
	localID, err := h.db.GetMessageLocalIDByInternetID(ctx, accountID, msg.MessageID)
	if err != nil || localID == 0 {
		return
	}
	providerMessageID = strings.TrimSpace(providerMessageID)
	if providerMessageID == "" {
		info := storage.MessageMutationInfo{
			AccountID:         accountID,
			AccountProvider:   providers.ProviderGmail,
			InternetMessageID: msg.MessageID,
		}
		providerMessageID, err = h.resolveGmailMessageID(ctx, token, localID, info)
		if err != nil {
			log.Printf("gmail sent reconcile account=%s message=%d: %v", accountID, localID, err)
			return
		}
	}
	if providerMessageID != "" {
		_ = h.db.SetMessageProviderMessageID(ctx, localID, providerMessageID)
	}
}
