package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const outlookGraphAttachmentFetchMaxBytes int64 = 64 << 20

func (h *Handler) ensureAttachmentStorage(ctx context.Context, info *storage.AttachmentFetchInfo) (string, error) {
	if info == nil {
		return "", fmt.Errorf("attachment fetch info not found")
	}
	if strings.TrimSpace(info.StoragePath) != "" {
		if _, err := os.Stat(info.StoragePath); err == nil {
			return info.StoragePath, nil
		}
	}
	switch strings.TrimSpace(info.AccountProvider) {
	case providers.ProviderGmail:
		if !gmailAPIMailRuntimeEnabled() {
			return strings.TrimSpace(info.StoragePath), fmt.Errorf("gmail api attachment fetch is disabled")
		}
		if h.auth == nil || h.blobStore == nil {
			return strings.TrimSpace(info.StoragePath), fmt.Errorf("gmail api attachment fetch is unavailable")
		}
		providerMessageID := strings.TrimSpace(info.ProviderMessageID)
		if providerMessageID == "" {
			mutationInfo, err := h.db.GetMessageMutationInfo(ctx, info.MessageID)
			if err != nil {
				return strings.TrimSpace(info.StoragePath), err
			}
			if mutationInfo == nil {
				return strings.TrimSpace(info.StoragePath), fmt.Errorf("message mutation info not found")
			}
			token, resolvedMessageID, ok := h.gmailMessageIdentity(ctx, info.MessageID, *mutationInfo, "attachment fetch")
			if !ok {
				return strings.TrimSpace(info.StoragePath), fmt.Errorf("gmail message identity unavailable")
			}
			content, err := h.fetchGmailAPIAttachmentContent(ctx, token, resolvedMessageID, info.ProviderAttachmentID)
			if err != nil {
				return strings.TrimSpace(info.StoragePath), err
			}
			return h.storeFetchedAttachment(ctx, info, content)
		}
		token, err := h.auth.GetOAuthTokenForAccount(ctx, info.AccountID)
		if err != nil {
			return strings.TrimSpace(info.StoragePath), err
		}
		content, err := h.fetchGmailAPIAttachmentContent(ctx, token, providerMessageID, info.ProviderAttachmentID)
		if err != nil {
			return strings.TrimSpace(info.StoragePath), err
		}
		return h.storeFetchedAttachment(ctx, info, content)
	case providers.ProviderOutlook:
	default:
		return strings.TrimSpace(info.StoragePath), fmt.Errorf("attachment is not provider backed")
	}
	if h.auth == nil || h.blobStore == nil {
		return strings.TrimSpace(info.StoragePath), fmt.Errorf("outlook graph attachment fetch is unavailable")
	}
	providerMessageID := strings.TrimSpace(info.ProviderMessageID)
	if providerMessageID == "" {
		mutationInfo, err := h.db.GetMessageMutationInfo(ctx, info.MessageID)
		if err != nil {
			return strings.TrimSpace(info.StoragePath), err
		}
		if mutationInfo == nil {
			return strings.TrimSpace(info.StoragePath), fmt.Errorf("message mutation info not found")
		}
		token, resolvedMessageID, ok := h.outlookGraphMessageIdentity(ctx, info.MessageID, *mutationInfo, "attachment fetch")
		if !ok {
			return strings.TrimSpace(info.StoragePath), fmt.Errorf("outlook message identity unavailable")
		}
		content, err := h.fetchOutlookGraphAttachmentContent(ctx, token, resolvedMessageID, info.ProviderAttachmentID)
		if err != nil {
			return strings.TrimSpace(info.StoragePath), err
		}
		return h.storeFetchedAttachment(ctx, info, content)
	}
	token, err := h.auth.GetMicrosoftGraphMailTokenForAccount(ctx, info.AccountID)
	if err != nil {
		return strings.TrimSpace(info.StoragePath), err
	}
	content, err := h.fetchOutlookGraphAttachmentContent(ctx, token, providerMessageID, info.ProviderAttachmentID)
	if err != nil {
		return strings.TrimSpace(info.StoragePath), err
	}
	return h.storeFetchedAttachment(ctx, info, content)
}

func (h *Handler) storeFetchedAttachment(ctx context.Context, info *storage.AttachmentFetchInfo, content []byte) (string, error) {
	path, err := h.blobStore.StoreAttachment(ctx, info.AccountID, info.MessageID, info.ID, info.Filename, bytes.NewReader(content))
	if err != nil {
		return "", err
	}
	if err := h.db.UpdateAttachmentStoragePath(ctx, info.ID, path); err != nil {
		return "", err
	}
	info.StoragePath = path
	return path, nil
}

func (h *Handler) fetchOutlookGraphAttachmentContent(ctx context.Context, token, providerMessageID, providerAttachmentID string) ([]byte, error) {
	providerMessageID = strings.TrimSpace(providerMessageID)
	providerAttachmentID = strings.TrimSpace(providerAttachmentID)
	if providerMessageID == "" || providerAttachmentID == "" {
		return nil, fmt.Errorf("outlook attachment identity unavailable")
	}
	endpoint := outlookGraphBaseURL + "/me/messages/" + url.PathEscape(providerMessageID) + "/attachments/" + url.PathEscape(providerAttachmentID) + "/$value"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Prefer", `IdType="ImmutableId"`)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, outlookGraphAttachmentFetchMaxBytes+1))
	if int64(len(raw)) > outlookGraphAttachmentFetchMaxBytes {
		return nil, fmt.Errorf("outlook attachment exceeds %d bytes", outlookGraphAttachmentFetchMaxBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if readErr != nil {
			return nil, readErr
		}
		return nil, newOutlookAPIError(resp, raw)
	}
	if readErr != nil {
		return nil, readErr
	}
	return raw, nil
}
