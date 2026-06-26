package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const outlookGraphMIMEFetchMaxBytes int64 = 64 << 20

func (h *Handler) fetchOutlookGraphMessageMIME(ctx context.Context, messageID int64) ([]byte, bool, error) {
	info, err := h.db.GetMessageMutationInfo(ctx, messageID)
	if err != nil || info == nil {
		return nil, false, err
	}
	token, providerMessageID, ok := h.outlookGraphMessageIdentity(ctx, messageID, *info, "body fetch")
	if !ok {
		return nil, false, nil
	}
	bodyData, err := h.fetchOutlookGraphMessageMIMEByID(ctx, token, providerMessageID)
	return bodyData, true, err
}

func (h *Handler) fetchOutlookGraphMessageMIMEByID(ctx context.Context, token, providerMessageID string) ([]byte, error) {
	endpoint := outlookGraphBaseURL + "/me/messages/" + url.PathEscape(providerMessageID) + "/$value"
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

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, outlookGraphMIMEFetchMaxBytes+1))
	if int64(len(raw)) > outlookGraphMIMEFetchMaxBytes {
		return nil, fmt.Errorf("outlook message MIME exceeds %d bytes", outlookGraphMIMEFetchMaxBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if readErr != nil {
			return nil, readErr
		}
		return nil, outlookAPIError{Status: resp.StatusCode, Body: string(raw)}
	}
	if readErr != nil {
		return nil, readErr
	}
	return raw, nil
}
