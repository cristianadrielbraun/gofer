package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/retry"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// UserProviderMail is handed only to the queue running under the account gate,
// activity guard and global session bound. It shares that session's identity,
// credentials, cancellation and durable cooldown; it holds no database lease.
type UserProviderMail struct {
	scope    *userIMAPScope
	lifetime context.Context
}

type UserProviderSendResult struct {
	Outcome   models.SendResult
	MessageID string
	Retryable bool
	Error     error
}

func (p *UserProviderMail) RetryAt() time.Time { return p.scope.retryAt }

func (p *UserProviderMail) Send(ctx context.Context, mime []byte) UserProviderSendResult {
	result := UserProviderSendResult{Outcome: models.SendFailed}
	if p == nil || p.scope == nil || p.scope.tokens == nil || p.lifetime == nil || len(mime) == 0 {
		result.Error = errors.New("provider delivery is unavailable")
		return result
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(p.lifetime, cancel)
	defer stop()
	if p.lifetime.Err() != nil {
		result.Error = p.lifetime.Err()
		return result
	}
	scope := p.scope
	o := NewSyncOrchestrator(nil, nil, nil, scope.tokens)
	o.imapScope = scope
	var token string
	var err error
	if scope.config.Provider == "outlook" {
		graph, ok := scope.tokens.(graphMailTokenProvider)
		if !ok {
			result.Error = errors.New("Graph credentials are unavailable")
			return result
		}
		token, err = graph.GetMicrosoftGraphMailTokenForAccount(ctx, scope.id)
	} else if scope.config.Provider == "gmail" {
		token, err = scope.tokens.GetOAuthTokenForAccount(ctx, scope.id)
	} else {
		result.Error = errors.New("unsupported provider delivery")
		return result
	}
	if err != nil {
		result.Error, result.Retryable = o.recordOutlookRetry(ctx, err), true
		return result // No mail request was made.
	}
	endpoint, contentType := gmailAPIBaseURL+"/users/me/messages/send", "application/json"
	body, err := json.Marshal(map[string]string{"raw": base64.RawURLEncoding.EncodeToString(mime)})
	if err != nil {
		result.Error = err
		return result
	}
	if scope.config.Provider == "outlook" {
		endpoint, contentType = outlookGraphBaseURL+"/me/sendMail", "text/plain"
		body = []byte(base64.StdEncoding.EncodeToString(mime))
	}
	var response struct {
		ID string `json:"id"`
	}
	var wireErr error
	attempted, accepted := false, false
	err = o.outlookRequest(ctx, token, func(access string) error {
		if err := scope.call(ctx, func(*storage.DB) error { return nil }); err != nil {
			return err
		}
		headers := map[string]string(nil)
		if scope.config.Provider == "outlook" {
			headers = outlookImmutableIDHeaders()
		}
		attempted = true
		accepted, wireErr = providerMailRaw(ctx, http.MethodPost, endpoint, access, contentType, headers, body, &response)
		return wireErr
	})
	if accepted {
		// A success status confirms acceptance even if the optional identity body
		// is missing or corrupt. Recover its ID during Sent reconciliation.
		result.Outcome, result.MessageID = models.SendSuccess, strings.TrimSpace(response.ID)
		return result
	}
	result.Error = err
	if !attempted {
		result.Retryable = true
		return result
	}
	if status, definitive := providerAPIStatus(wireErr); definitive {
		result.Retryable = status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= 500
		// A rejected first 401 followed by a failed token refresh never accepted
		// mail. Temporary refresh errors may retry; revoked grants need reconnect.
		if status == 401 && !providerAPIUnauthorized(err) {
			result.Retryable = true
		}
	} else {
		result.Outcome = models.SendAmbiguous
	}
	return result
}

func providerMailRaw(ctx context.Context, method, endpoint, token, contentType string, headers map[string]string, body []byte, out any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		retryAt, _ := retry.ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now().UTC())
		return false, &providerAPIError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(raw)), RetryAt: retryAt}
	}
	if out != nil && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusAccepted {
		return true, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	return true, nil
}
