package handler

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/retry"
	"golang.org/x/oauth2"
)

type retryAfterError interface {
	RetryAfter() (time.Time, bool)
}

func newGoogleAPIError(resp *http.Response, body []byte) googleAPIError {
	return googleAPIError{
		Status:    resp.StatusCode,
		Body:      sanitizeProviderErrorBody(string(body)),
		RetryAt:   providerRetryAfter(resp),
		RequestID: providerRequestID(resp.Header),
	}
}

func newOutlookAPIError(resp *http.Response, body []byte) outlookAPIError {
	return outlookAPIError{
		Status:    resp.StatusCode,
		Body:      sanitizeProviderErrorBody(string(body)),
		RetryAt:   providerRetryAfter(resp),
		RequestID: providerRequestID(resp.Header),
	}
}

func providerRetryAfter(resp *http.Response) time.Time {
	if resp == nil {
		return time.Time{}
	}
	when, ok := retry.ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now().UTC())
	if !ok {
		return time.Time{}
	}
	return when
}

func providerRequestID(headers http.Header) string {
	for _, name := range []string{"X-Request-ID", "X-Google-Request-ID", "request-id", "client-request-id"} {
		for key, values := range headers {
			if !strings.EqualFold(key, name) || len(values) == 0 {
				continue
			}
			if value := strings.TrimSpace(values[0]); value != "" {
				return value
			}
		}
	}
	return ""
}

var (
	outgoingSecretPattern = regexp.MustCompile(`(?i)(access[_ -]?token|refresh[_ -]?token|authorization|password|client[_ -]?secret|secret)["']?\s*[:=]\s*["']?[^,\s"'}]+`)
	outgoingBearerPattern = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
)

func sanitizeProviderErrorBody(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = outgoingBearerPattern.ReplaceAllString(value, "Bearer [redacted]")
	value = outgoingSecretPattern.ReplaceAllString(value, "$1=[redacted]")
	if len(value) > 500 {
		value = value[:500] + "…"
	}
	return value
}

func sanitizeOutgoingErrorText(value string) string {
	return sanitizeProviderErrorBody(value)
}

func retryAfterAt(err error, now time.Time) (time.Time, bool) {
	if err == nil {
		return time.Time{}, false
	}
	var retryErr retryAfterError
	if errors.As(err, &retryErr) {
		if when, ok := retryErr.RetryAfter(); ok && !when.Before(now) {
			return when.UTC(), true
		}
	}
	var oauthErr *oauth2.RetrieveError
	if errors.As(err, &oauthErr) && oauthErr.Response != nil {
		return retry.ParseRetryAfter(oauthErr.Response.Header.Get("Retry-After"), now)
	}
	return time.Time{}, false
}

type outgoingRetryableError struct {
	err error
}

func (e *outgoingRetryableError) Error() string {
	if e == nil || e.err == nil {
		return errOutgoingSendRetryable.Error()
	}
	return errOutgoingSendRetryable.Error() + ": " + e.err.Error()
}

func (e *outgoingRetryableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *outgoingRetryableError) Is(target error) bool {
	return target == errOutgoingSendRetryable || (e != nil && errors.Is(e.err, target))
}

func markOutgoingSendRetryablePreserving(err error) error {
	if err == nil || errors.Is(err, errOutgoingSendRetryable) {
		return err
	}
	return &outgoingRetryableError{err: err}
}

func oauthErrorMetadata(err error) (code, description string, status int) {
	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) {
		return strings.TrimSpace(retrieveErr.ErrorCode), strings.TrimSpace(retrieveErr.ErrorDescription), oauthResponseStatus(retrieveErr.Response)
	}
	var tokenErr interface {
		OAuthErrorCode() string
		OAuthErrorDescription() string
		OAuthErrorStatus() int
	}
	if errors.As(err, &tokenErr) {
		return strings.TrimSpace(tokenErr.OAuthErrorCode()), strings.TrimSpace(tokenErr.OAuthErrorDescription()), tokenErr.OAuthErrorStatus()
	}
	return "", "", 0
}

func oauthResponseStatus(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func isPermanentOAuthError(err error) bool {
	if err == nil {
		return false
	}
	code, description, status := oauthErrorMetadata(err)
	if permanentOAuthCode(code) {
		return true
	}
	text := strings.ToLower(strings.TrimSpace(code + " " + description + " " + err.Error()))
	for _, marker := range []string{
		"invalid_grant", "invalid_client", "unauthorized_client", "interaction_required", "consent_required",
		"login_required", "account_disabled", "access_denied", "reauthenticate", "re-authenticate",
		"refresh token has expired", "refresh token expired", "refresh token revoked", "aadsts700082", "aadsts65001",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	if status == http.StatusBadRequest && (strings.Contains(text, "consent") || strings.Contains(text, "revoked")) {
		return true
	}
	return strings.Contains(text, "no refresh token available") ||
		strings.Contains(text, "no oauth token found") ||
		strings.Contains(text, "oauth not configured") ||
		strings.Contains(text, "oauth config is not configured")
}

func permanentOAuthCode(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "invalid_grant", "invalid_client", "unauthorized_client", "interaction_required", "consent_required", "login_required", "account_disabled", "access_denied":
		return true
	default:
		return false
	}
}

type outgoingReconnectError struct {
	reason string
}

func (e *outgoingReconnectError) Error() string {
	if e == nil || strings.TrimSpace(e.reason) == "" {
		return "Reconnect account"
	}
	return "Reconnect account: " + e.reason
}

func (e *outgoingReconnectError) Is(target error) bool {
	return target == errOutgoingSendReconnect
}

func markOutgoingSendReconnect(err error) error {
	return &outgoingReconnectError{reason: oauthReconnectReason(err)}
}

func oauthReconnectReason(err error) string {
	code, description, _ := oauthErrorMetadata(err)
	text := strings.ToLower(strings.TrimSpace(code + " " + description + " " + err.Error()))
	switch {
	case strings.Contains(text, "invalid_client"):
		return "the OAuth client configuration was rejected by the provider"
	case strings.Contains(text, "consent_required"), strings.Contains(text, "interaction_required"), strings.Contains(text, "consent"):
		return "the provider requires consent again"
	default:
		return "the provider authorization is no longer valid"
	}
}

func boundedRandom(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func equalJitter(base time.Duration, randomValue float64) time.Duration {
	if base <= 0 {
		return 0
	}
	return base/2 + time.Duration(float64(base-base/2)*boundedRandom(randomValue))
}

func outgoingSendRetryDelayWithJitter(attempt int, randomValue float64) time.Duration {
	return equalJitter(outgoingSendRetryDelay(attempt), randomValue)
}

func sentCopyRetryDelayWithJitter(attempt int, randomValue float64) time.Duration {
	return equalJitter(sentCopyRetryDelay(attempt), randomValue)
}

func (h *Handler) outgoingNowUTC() time.Time {
	if h != nil && h.outgoingNow != nil {
		return h.outgoingNow().UTC()
	}
	return time.Now().UTC()
}

func (h *Handler) outgoingRandomValue() float64 {
	if h != nil && h.outgoingRandom != nil {
		return boundedRandom(h.outgoingRandom())
	}
	return 0.5
}

func retryInSeconds(delay time.Duration) int {
	if delay <= 0 {
		return 0
	}
	return int((delay + time.Second - 1) / time.Second)
}
