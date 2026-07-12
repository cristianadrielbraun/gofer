package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"golang.org/x/oauth2"
)

func TestProviderErrorsCarryRetryAfterAndSanitizeBodies(t *testing.T) {
	now := time.Now().UTC()
	googleResponse := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header: http.Header{
			"Retry-After":  []string{"120"},
			"X-Request-ID": []string{"google-request-1"},
		},
	}
	googleErr := newGoogleAPIError(googleResponse, []byte(`{"access_token":"should-not-be-visible"}`))
	when, ok := googleErr.RetryAfter()
	if !ok || when.Before(now.Add(119*time.Second)) || when.After(now.Add(121*time.Second)) {
		t.Fatalf("Google RetryAfter() = %s, %v; want roughly two minutes", when, ok)
	}
	if googleErr.RequestID != "google-request-1" || strings.Contains(googleErr.Body, "should-not-be-visible") || strings.Contains(googleErr.Error(), "should-not-be-visible") {
		t.Fatalf("Google error = %#v / %q, want request ID and sanitized body", googleErr, googleErr.Error())
	}

	outlookDate := now.Add(2 * time.Minute).Format(http.TimeFormat)
	outlookResponse := &http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header: http.Header{
			"Retry-After": []string{outlookDate},
			"request-id":  []string{"graph-request-1"},
		},
	}
	outlookErr := newOutlookAPIError(outlookResponse, []byte(`authorization: Bearer secret-value`))
	when, ok = outlookErr.RetryAfter()
	if !ok || when.Before(now.Add(119*time.Second)) || when.After(now.Add(121*time.Second)) {
		t.Fatalf("Graph RetryAfter() = %s, %v; want roughly two minutes", when, ok)
	}
	if outlookErr.RequestID != "graph-request-1" || strings.Contains(outlookErr.Body, "secret-value") || strings.Contains(outlookErr.Error(), "secret-value") {
		t.Fatalf("Graph error = %#v / %q, want request ID and sanitized body", outlookErr, outlookErr.Error())
	}
}

func TestGmailSendCarriesRetryAfterSeconds(t *testing.T) {
	ctx := context.Background()
	h, _ := newGmailAPITestHandler(t, ctx)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/me/messages/send" {
			t.Fatalf("unexpected Gmail path %q", r.URL.Path)
		}
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rateLimitExceeded"}`))
	}))
	defer server.Close()
	previousBase := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBase })

	_, _, err := h.sendGmailAPIRaw(ctx, &models.AccountConfig{AccountID: "acc", Provider: providers.ProviderGmail}, []byte("raw MIME"))
	if err == nil || !errors.Is(err, errOutgoingSendRetryable) {
		t.Fatalf("sendGmailAPIRaw() error = %v, want retryable", err)
	}
	when, ok := retryAfterAt(err, time.Now().UTC())
	if !ok || when.Before(time.Now().UTC().Add(119*time.Second)) {
		t.Fatalf("retryAfterAt() = %s, %v; want at least 119 seconds", when, ok)
	}
}

func TestGraphSendCarriesRetryAfterHTTPDate(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", time.Now().UTC().Add(2*time.Minute).Format(http.TimeFormat))
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"temporarilyUnavailable"}`))
	}))
	defer server.Close()

	err := (&Handler{}).doOutlookRaw(ctx, http.MethodPost, server.URL, "access-token", "text/plain", []byte("raw MIME"), nil)
	if err == nil {
		t.Fatal("doOutlookRaw() error = nil, want Graph 503")
	}
	when, ok := retryAfterAt(err, time.Now().UTC())
	if !ok || when.Before(time.Now().UTC().Add(119*time.Second)) {
		t.Fatalf("retryAfterAt() = %s, %v; want at least 119 seconds", when, ok)
	}
}

func TestRetryableWrapperPreservesRetryAfter(t *testing.T) {
	fixed := time.Date(2026, time.July, 12, 12, 0, 0, 0, time.UTC)
	wrapped := markOutgoingSendRetryablePreserving(googleAPIError{RetryAt: fixed})
	if !errors.Is(wrapped, errOutgoingSendRetryable) {
		t.Fatalf("errors.Is(%v, retryable) = false", wrapped)
	}
	if got, ok := retryAfterAt(wrapped, fixed.Add(-time.Minute)); !ok || !got.Equal(fixed) {
		t.Fatalf("retryAfterAt() = %s, %v; want %s, true", got, ok, fixed)
	}
}

func TestRetryDelayJitterIsBoundedAndInjectable(t *testing.T) {
	base := outgoingSendRetryDelay(3)
	if got := outgoingSendRetryDelayWithJitter(3, 0); got != base/2 {
		t.Fatalf("minimum jitter = %s, want %s", got, base/2)
	}
	if got := outgoingSendRetryDelayWithJitter(3, 1); got != base {
		t.Fatalf("maximum jitter = %s, want %s", got, base)
	}
	if got := outgoingSendRetryDelayWithJitter(3, -1); got != base/2 {
		t.Fatalf("negative random jitter = %s, want %s", got, base/2)
	}
	if got := outgoingSendRetryDelayWithJitter(3, 2); got != base {
		t.Fatalf("large random jitter = %s, want %s", got, base)
	}
}

func TestOAuthFailuresSeparatePermanentAuthorizationFromTemporaryOutages(t *testing.T) {
	permanent := &oauth2.RetrieveError{ErrorCode: "invalid_grant", ErrorDescription: "refresh token was revoked"}
	if !isPermanentOAuthError(permanent) {
		t.Fatal("invalid_grant was not classified as permanent")
	}
	temporary := &oauth2.RetrieveError{Response: &http.Response{StatusCode: http.StatusServiceUnavailable}}
	if isPermanentOAuthError(temporary) {
		t.Fatal("token endpoint 503 was classified as permanent")
	}
	graphPermanent := &auth.OAuthTokenError{Status: http.StatusBadRequest, Code: "invalid_grant", Description: "token revoked"}
	if !isPermanentOAuthError(graphPermanent) {
		t.Fatal("Graph invalid_grant was not classified as permanent")
	}
	reconnect := markOutgoingSendReconnect(permanent)
	if !errors.Is(reconnect, errOutgoingSendReconnect) || !strings.Contains(reconnect.Error(), "Reconnect account") {
		t.Fatalf("reconnect error = %v, want action-required marker", reconnect)
	}
	if strings.Contains(reconnect.Error(), "refresh token was revoked") {
		t.Fatalf("reconnect error leaked provider details: %v", reconnect)
	}
}

func TestGmailRevokedOAuthStopsBeforeDeliveryRetryLoop(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	var tokenCalls int
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token revoked"}`))
	}))
	defer tokenServer.Close()
	h.auth = auth.NewManager(&auth.Config{GoogleClient: &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: tokenServer.URL}}}, db)
	if _, err := db.Write().ExecContext(ctx, `UPDATE oauth_accounts SET expires_at = ? WHERE provider = ?`, time.Now().Add(-time.Hour), providers.OAuthGoogle); err != nil {
		t.Fatalf("expire cached OAuth token: %v", err)
	}

	_, _, err := h.sendGmailAPIRaw(ctx, &models.AccountConfig{AccountID: "acc", Provider: providers.ProviderGmail}, []byte("raw MIME"))
	if err == nil || !errors.Is(err, errOutgoingSendReconnect) {
		t.Fatalf("sendGmailAPIRaw() error = %v, want reconnect classification", err)
	}
	if strings.Contains(err.Error(), "refresh token revoked") || strings.Contains(err.Error(), "refresh-token") {
		t.Fatalf("reconnect error leaked OAuth details: %v", err)
	}
	if tokenCalls > 2 {
		t.Fatalf("token endpoint calls = %d, want the OAuth client probe only, not a delivery retry loop", tokenCalls)
	}
}
