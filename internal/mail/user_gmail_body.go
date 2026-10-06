package mail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const userGmailRawMessageMaxBytes = 64 << 20

func (s *UserIMAP) fetchGmailRaw(ctx context.Context, scope *userIMAPScope, providerID string) ([]byte, error) {
	token, err := scope.tokens.GetOAuthTokenForAccount(ctx, scope.id)
	if err != nil {
		return nil, err
	}
	raw, err := getUserGmailRaw(ctx, token, providerID)
	if !providerAPIUnauthorized(err) {
		return raw, err
	}
	refresh, ok := scope.tokens.(refreshingTokenProvider)
	if !ok {
		return nil, err
	}
	token, err = refresh.RefreshOAuthTokenForAccount(ctx, scope.id)
	if err != nil {
		return nil, err
	}
	return getUserGmailRaw(ctx, token, providerID)
}

// RAW returns one complete RFC message, so attachments/body parsing can reuse
// the immutable candidate publication path without per-part network requests.
func getUserGmailRaw(ctx context.Context, token, providerID string) ([]byte, error) {
	if strings.TrimSpace(providerID) == "" {
		return nil, errors.New("Gmail message identity is unavailable")
	}
	endpoint := gmailAPIBaseURL + "/users/me/messages/" + url.PathEscape(providerID) + "?format=raw"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, &providerAPIError{StatusCode: response.StatusCode, Body: strings.TrimSpace(string(body))}
	}
	var message struct {
		ID  string `json:"id"`
		Raw string `json:"raw"`
	}
	encodedLimit := int64((userGmailRawMessageMaxBytes+2)/3*4 + 4096)
	if err := json.NewDecoder(io.LimitReader(response.Body, encodedLimit)).Decode(&message); err != nil {
		return nil, fmt.Errorf("decode Gmail raw message: %w", err)
	}
	if message.ID != providerID {
		return nil, errors.New("Gmail returned a different message identity")
	}
	encoded := strings.TrimSpace(message.Raw)
	if encoded == "" {
		return nil, errors.New("Gmail returned an empty raw message")
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(encoded)
	}
	if err != nil {
		return nil, fmt.Errorf("decode Gmail message MIME: %w", err)
	}
	if len(raw) == 0 || len(raw) > userGmailRawMessageMaxBytes {
		return nil, errors.New("Gmail raw message has an invalid size")
	}
	return raw, nil
}
