package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type outlookGraphDraftResponse struct {
	ID                string `json:"id"`
	InternetMessageID string `json:"internetMessageId"`
}

func (h *Handler) saveOutlookGraphDraft(ctx context.Context, accountID string, localMessageID int64, msg *message.OutgoingMessage) error {
	if h.auth == nil {
		return fmt.Errorf("microsoft oauth not configured")
	}
	token, err := h.auth.GetMicrosoftGraphMailTokenForAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if existing, err := h.db.GetDraftProviderInfo(ctx, accountID, msg.MessageID); err == nil && existing != nil {
		if providerID := strings.TrimSpace(existing.ProviderMessageID); providerID != "" {
			if err := h.deleteOutlookGraphMessage(ctx, token, providerID); err != nil && !outlookAPIStatus(err, http.StatusNotFound) {
				return err
			}
		}
	}

	raw, err := message.BuildMIMEMessageForGraph(msg)
	if err != nil {
		return err
	}
	var created outlookGraphDraftResponse
	if err := h.doOutlookRaw(ctx, http.MethodPost, outlookGraphBaseURL+"/me/messages", token, "text/plain", []byte(base64.StdEncoding.EncodeToString(raw)), &created); err != nil {
		return err
	}
	if strings.TrimSpace(created.ID) == "" {
		return fmt.Errorf("graph draft response missing id")
	}
	return h.db.SetMessageProviderMessageID(ctx, localMessageID, strings.TrimSpace(created.ID))
}

func (h *Handler) sendOutlookGraphMessage(ctx context.Context, cfg *models.AccountConfig, msg *message.OutgoingMessage, draftID string) (bool, string, string) {
	if cfg == nil || strings.TrimSpace(cfg.Provider) != providers.ProviderOutlook {
		return false, "", ""
	}
	if h.auth == nil {
		return true, "failed", "microsoft oauth not configured"
	}
	token, err := h.auth.GetMicrosoftGraphMailTokenForAccount(ctx, cfg.AccountID)
	if err != nil {
		return true, "failed", err.Error()
	}
	raw, err := message.BuildMIMEMessageForGraph(msg)
	if err != nil {
		return true, "failed", err.Error()
	}
	if err := h.doOutlookRaw(ctx, http.MethodPost, outlookGraphBaseURL+"/me/sendMail", token, "text/plain", []byte(base64.StdEncoding.EncodeToString(raw)), nil); err != nil {
		return true, "failed", err.Error()
	}

	h.saveSentMessage(ctx, cfg.AccountID, msg)
	h.cacheOutlookSentMessageID(ctx, cfg.AccountID, msg, token)
	if strings.TrimSpace(draftID) != "" {
		draftProvider, _ := h.db.GetDraftProviderInfo(ctx, cfg.AccountID, draftID)
		if folderID, err := h.db.DeleteDraftMessage(ctx, cfg.AccountID, draftID); err == nil && folderID != "" {
			h.publishMutation(cfg.AccountID, folderID)
		}
		if draftProvider != nil {
			if err := h.deleteOutlookGraphDraft(ctx, cfg.AccountID, draftProvider.ProviderMessageID); err != nil {
				log.Printf("outlook draft delete after send account=%s draft=%s: %v", cfg.AccountID, draftID, err)
			}
		}
	}
	return true, "sent", ""
}

func (h *Handler) cacheOutlookSentMessageID(ctx context.Context, accountID string, msg *message.OutgoingMessage, token string) {
	localID, err := h.db.GetMessageLocalIDByInternetID(ctx, accountID, msg.MessageID)
	if err != nil || localID == 0 {
		return
	}
	info := storageMessageMutationInfo(accountID, msg.MessageID)
	providerID, err := h.resolveOutlookMessageID(ctx, token, localID, info)
	if err != nil {
		log.Printf("outlook sent reconcile account=%s message=%d: %v", accountID, localID, err)
		return
	}
	if providerID != "" {
		_ = h.db.SetMessageProviderMessageID(ctx, localID, providerID)
	}
}

func storageMessageMutationInfo(accountID, internetMessageID string) storage.MessageMutationInfo {
	return storage.MessageMutationInfo{
		AccountID:         accountID,
		AccountProvider:   providers.ProviderOutlook,
		InternetMessageID: internetMessageID,
	}
}

func (h *Handler) deleteOutlookGraphDraft(ctx context.Context, accountID, providerMessageID string) error {
	providerMessageID = strings.TrimSpace(providerMessageID)
	if providerMessageID == "" || h.auth == nil {
		return nil
	}
	token, err := h.auth.GetMicrosoftGraphMailTokenForAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if err := h.deleteOutlookGraphMessage(ctx, token, providerMessageID); err != nil && !outlookAPIStatus(err, http.StatusNotFound) {
		return err
	}
	return nil
}

func (h *Handler) deleteOutlookGraphMessage(ctx context.Context, token, providerMessageID string) error {
	endpoint := outlookGraphBaseURL + "/me/messages/" + url.PathEscape(strings.TrimSpace(providerMessageID))
	return h.doOutlookJSON(ctx, http.MethodDelete, endpoint, token, nil, nil)
}

func outlookAPIStatus(err error, status int) bool {
	if apiErr, ok := err.(outlookAPIError); ok {
		return apiErr.Status == status
	}
	return false
}

func (h *Handler) doOutlookRaw(ctx context.Context, method, endpoint, accessToken, contentType string, payload []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Prefer", `IdType="ImmutableId"`)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if out != nil {
		req.Header.Set("Accept", "application/json")
	}

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if readErr != nil {
			return readErr
		}
		return outlookAPIError{Status: resp.StatusCode, Body: string(raw)}
	}
	if readErr != nil {
		return readErr
	}
	if out == nil || len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}
