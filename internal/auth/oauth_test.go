package auth

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type oauthRoundTripFunc func(*http.Request) (*http.Response, error)

func (f oauthRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestGoogleApplicationLoginDoesNotCreateOrStoreMailboxAccess(t *testing.T) {
	now := time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"session-id"},
		tokens: []string{"session-token"},
	})
	insertActiveUser(t, manager, "person", false, now)
	manager.config.GoogleLoginClient = &oauth2.Config{
		ClientID:     "login-client",
		ClientSecret: "login-secret",
		RedirectURL:  "https://gofer.example/auth/google/callback",
		Scopes:       []string{"openid", "email", "profile"},
		Endpoint: oauth2.Endpoint{
			TokenURL: "https://accounts.example/token",
		},
	}

	client := &http.Client{Transport: oauthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := ""
		switch request.URL.String() {
		case "https://accounts.example/token":
			body = `{"access_token":"application-access-token","refresh_token":"application-refresh-token","token_type":"Bearer","expires_in":3600}`
		case "https://openidconnect.googleapis.com/v1/userinfo":
			if got := request.Header.Get("Authorization"); got != "Bearer application-access-token" {
				t.Fatalf("userinfo authorization = %q", got)
			}
			body = `{"sub":"google-subject","email":"person@example.com","email_verified":true,"name":"Person","picture":"https://images.example/person.png"}`
		default:
			t.Fatalf("unexpected OAuth request %s %s", request.Method, request.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	ctx := context.WithValue(t.Context(), oauth2.HTTPClient, client)

	user, result, err := manager.HandleGoogleCallback(ctx, "authorization-code", "Test Browser")
	if err != nil || user == nil || user.ID != "person" || result == nil || result.Session == nil {
		t.Fatalf("HandleGoogleCallback() = user:%#v result:%#v error:%v", user, result, err)
	}
	for table, want := range map[string]int{"oauth_accounts": 0, "accounts": 0, "sessions": 1} {
		var count int
		if err := manager.db.Read().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != want {
			t.Fatalf("%s rows = %d, want %d", table, count, want)
		}
	}
}

func TestGoogleApplicationLoginAuthorizationRequestsIdentityOnly(t *testing.T) {
	manager := NewManager(&Config{GoogleLoginClient: &oauth2.Config{
		ClientID: "login-client",
		Scopes:   []string{"openid", "email", "profile"},
		Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example/authorize"},
	}}, nil)

	rawURL := manager.GoogleLoginOAuthURL("state-value")
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	query := parsed.Query()
	if query.Get("state") != "state-value" || query.Get("scope") != "openid email profile" {
		t.Fatalf("authorization query = %q", query.Encode())
	}
	if query.Get("access_type") != "" || query.Get("prompt") != "" {
		t.Fatalf("application login requested offline or forced consent access: %q", query.Encode())
	}
}
