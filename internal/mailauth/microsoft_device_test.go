package mailauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type microsoftDeviceServer struct {
	*httptest.Server
	// replies are the token endpoint's answers in order; the last one repeats.
	replies []string
	polls   atomic.Int32
	scope   string
}

func newMicrosoftDeviceServer(t *testing.T, replies ...string) *microsoftDeviceServer {
	t.Helper()
	s := &microsoftDeviceServer{replies: replies}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
		}
		if r.FormValue("client_secret") != "" {
			t.Errorf("device sign-in sent a client secret")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/devicecode":
			s.scope = r.FormValue("scope")
			_, _ = w.Write([]byte(`{"device_code":"device-secret","user_code":"ABCD-1234","verification_uri":"https://microsoft.com/devicelogin","expires_in":900,"interval":5}`))
		case "/token":
			if r.FormValue("grant_type") != microsoftDeviceCodeGrant || r.FormValue("device_code") != "device-secret" || r.FormValue("client_id") != "client-id" {
				t.Errorf("token request = %v", r.Form)
			}
			n := int(s.polls.Add(1)) - 1
			reply := s.replies[min(n, len(s.replies)-1)]
			if reply == "success" {
				_, _ = w.Write([]byte(`{"access_token":"graph-token","refresh_token":"refresh-token","token_type":"Bearer","expires_in":3600,"scope":"https://graph.microsoft.com/Mail.ReadWrite","id_token":"header.payload.signature"}`))
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": reply})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func newMicrosoftDeviceService(server *microsoftDeviceServer) *Service {
	return New(&Config{
		MicrosoftDeviceCode: true,
		MicrosoftClient: &oauth2.Config{
			ClientID: "client-id",
			Scopes:   microsoftAccountTokenScopes(),
			Endpoint: oauth2.Endpoint{TokenURL: server.URL + "/token", DeviceAuthURL: server.URL + "/devicecode"},
		},
	}, nil, testMailboxCredentialKey)
}

// allowNextPoll skips the wait Microsoft asks for between polls.
func allowNextPoll(m *Service, state string) {
	m.deviceMu.Lock()
	defer m.deviceMu.Unlock()
	m.deviceFlows[accountOAuthFlowSecretHash(state)].nextPoll = time.Time{}
}

func TestLoadConfigUsesDeviceCodeWithoutMicrosoftSecret(t *testing.T) {
	t.Setenv("MICROSOFT_OAUTH_CLIENT_ID", "microsoft-mailbox-client")
	t.Setenv("MICROSOFT_OAUTH_CLIENT_SECRET", "")
	t.Setenv("MICROSOFT_OAUTH_TENANT", "")

	cfg := LoadConfig("http://192.168.1.20:8090", true)
	if cfg.MicrosoftClient == nil || !cfg.MicrosoftDeviceCode {
		t.Fatalf("secret-free Microsoft config = %#v", cfg)
	}
	if got := cfg.MicrosoftClient.Endpoint.DeviceAuthURL; got != "https://login.microsoftonline.com/common/oauth2/v2.0/devicecode" {
		t.Fatalf("device authorization URL = %q", got)
	}

	t.Setenv("MICROSOFT_OAUTH_CLIENT_SECRET", "microsoft-mailbox-secret")
	if cfg := LoadConfig("https://gofer.example", true); cfg.MicrosoftDeviceCode {
		t.Fatal("configured secret still uses device code")
	}
}

func TestMicrosoftDeviceSignInWaitsThenReturnsGrant(t *testing.T) {
	server := newMicrosoftDeviceServer(t, "authorization_pending", "slow_down", "success")
	m := newMicrosoftDeviceService(server)
	ctx := context.Background()

	signIn, err := m.StartMicrosoftDeviceSignIn(ctx, "state", "alice", "session")
	if err != nil {
		t.Fatalf("StartMicrosoftDeviceSignIn() error = %v", err)
	}
	if signIn.UserCode != "ABCD-1234" || signIn.VerificationURI != "https://microsoft.com/devicelogin" || signIn.Interval != 5*time.Second {
		t.Fatalf("sign-in = %#v", signIn)
	}
	if !strings.Contains(server.scope, microsoftGraphMailScope) || !strings.Contains(server.scope, "offline_access") {
		t.Fatalf("device scope = %q", server.scope)
	}
	if limit := time.Now().Add(accountOAuthFlowLifetime); signIn.ExpiresAt.After(limit) {
		t.Fatalf("sign-in outlives its account flow: %v > %v", signIn.ExpiresAt, limit)
	}

	if _, err := m.PollMicrosoftDeviceSignIn(ctx, "state", "alice", "session"); !errors.Is(err, ErrMicrosoftDevicePending) {
		t.Fatalf("first poll error = %v, want pending", err)
	}
	if _, err := m.PollMicrosoftDeviceSignIn(ctx, "state", "alice", "session"); !errors.Is(err, ErrMicrosoftDevicePending) || server.polls.Load() != 1 {
		t.Fatalf("early poll error = %v after %d token requests, want pending without a request", err, server.polls.Load())
	}
	allowNextPoll(m, "state")
	if _, err := m.PollMicrosoftDeviceSignIn(ctx, "state", "alice", "session"); !errors.Is(err, ErrMicrosoftDevicePending) {
		t.Fatalf("slow_down poll error = %v, want pending", err)
	}
	if current, _ := m.MicrosoftDeviceSignIn(ctx, "state", "alice", "session"); current.Interval != 10*time.Second {
		t.Fatalf("interval after slow_down = %v, want 10s", current.Interval)
	}
	allowNextPoll(m, "state")
	token, err := m.PollMicrosoftDeviceSignIn(ctx, "state", "alice", "session")
	if err != nil {
		t.Fatalf("final poll error = %v", err)
	}
	if token.AccessToken != "graph-token" || token.RefreshToken != "refresh-token" || token.Expiry.IsZero() {
		t.Fatalf("token = %#v", token)
	}
	if scope, _ := token.Extra("scope").(string); scope != "https://graph.microsoft.com/Mail.ReadWrite" {
		t.Fatalf("token scope = %q", scope)
	}
	if idToken, _ := token.Extra("id_token").(string); idToken != "header.payload.signature" {
		t.Fatalf("token id_token = %q", idToken)
	}
	if _, err := m.PollMicrosoftDeviceSignIn(ctx, "state", "alice", "session"); !errors.Is(err, ErrAccountOAuthFlowNotFound) {
		t.Fatalf("poll after grant error = %v, want not found", err)
	}
}

func TestMicrosoftDeviceSignInBelongsToItsSession(t *testing.T) {
	server := newMicrosoftDeviceServer(t, "success")
	m := newMicrosoftDeviceService(server)
	ctx := context.Background()
	if _, err := m.StartMicrosoftDeviceSignIn(ctx, "state", "alice", "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.MicrosoftDeviceSignIn(ctx, "state", "bob", "session"); !errors.Is(err, ErrAccountOAuthFlowUserMismatch) {
		t.Fatalf("other user error = %v", err)
	}
	if _, err := m.PollMicrosoftDeviceSignIn(ctx, "state", "alice", "other-session"); !errors.Is(err, ErrAccountOAuthFlowSessionMismatch) {
		t.Fatalf("other session error = %v", err)
	}
	if _, err := m.PollMicrosoftDeviceSignIn(ctx, "other-state", "alice", "session"); !errors.Is(err, ErrAccountOAuthFlowNotFound) {
		t.Fatalf("unknown state error = %v", err)
	}
	if server.polls.Load() != 0 {
		t.Fatal("rejected polls reached Microsoft")
	}
}

func TestMicrosoftDeviceSignInEndsWhenDeclined(t *testing.T) {
	server := newMicrosoftDeviceServer(t, "authorization_declined")
	m := newMicrosoftDeviceService(server)
	ctx := context.Background()
	if _, err := m.StartMicrosoftDeviceSignIn(ctx, "state", "alice", "session"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.PollMicrosoftDeviceSignIn(ctx, "state", "alice", "session"); !errors.Is(err, ErrMicrosoftDeviceDeclined) {
		t.Fatalf("declined poll error = %v", err)
	}
	if _, err := m.MicrosoftDeviceSignIn(ctx, "state", "alice", "session"); !errors.Is(err, ErrAccountOAuthFlowNotFound) {
		t.Fatalf("declined sign-in still pending: %v", err)
	}
}

func TestMicrosoftDeviceSignInRequiresDeviceMode(t *testing.T) {
	server := newMicrosoftDeviceServer(t, "success")
	m := newMicrosoftDeviceService(server)
	m.config.MicrosoftDeviceCode = false
	if _, err := m.StartMicrosoftDeviceSignIn(context.Background(), "state", "alice", "session"); err == nil {
		t.Fatal("redirect configuration started a device sign-in")
	}
}
