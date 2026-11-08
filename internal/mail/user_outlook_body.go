package mail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/netguard"
	"github.com/cristianadrielbraun/gofer/internal/retry"
)

const userOutlookRawMessageMaxBytes = message.MaxDownloadBytes

func (s *UserIMAP) fetchOutlookRaw(ctx context.Context, scope *userIMAPScope, providerID string) ([]byte, error) {
	graph, ok := scope.tokens.(graphMailTokenProvider)
	if !ok {
		return nil, errors.New("Graph mail credentials are unavailable")
	}
	token, err := graph.GetMicrosoftGraphMailTokenForAccount(ctx, scope.id)
	if err != nil {
		return nil, err
	}
	raw, err := getUserOutlookRaw(ctx, token, providerID)
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
	return getUserOutlookRaw(ctx, token, providerID)
}

func getUserOutlookRaw(ctx context.Context, token, providerID string) ([]byte, error) {
	if strings.TrimSpace(providerID) == "" {
		return nil, errors.New("Graph message identity is unavailable")
	}
	endpoint := outlookGraphBaseURL + "/me/messages/" + url.PathEscape(providerID) + "/$value"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Prefer", `IdType="ImmutableId"`)
	response, err := netguard.APIClient(0).Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		at, _ := retry.ParseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
		return nil, &providerAPIError{StatusCode: response.StatusCode, Body: strings.TrimSpace(string(body)), RetryAt: at}
	}
	limit := message.DownloadLimit(ctx, userOutlookRawMessageMaxBytes)
	if response.ContentLength > limit {
		return nil, &message.TooLargeError{Size: response.ContentLength, Limit: limit}
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, &message.TooLargeError{Limit: limit}
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("Graph message MIME has an invalid size")
	}
	return raw, nil
}
