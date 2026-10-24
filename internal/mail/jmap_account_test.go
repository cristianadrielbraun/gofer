package mail

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/netguard"
)

// jmapTestServer serves a minimal session whose apiUrl is apiURL, or its own
// /jmap/ when apiURL is empty, and answers API calls with an empty batch.
func jmapTestServer(t *testing.T, apiURL string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "robin@example.test" || pass != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/.well-known/jmap":
			api := apiURL
			if api == "" {
				api = server.URL + "/jmap/"
			}
			_, _ = io.WriteString(w, `{"capabilities": {"urn:ietf:params:jmap:core": {}}, "accounts": {},
				"primaryAccounts": {}, "username": "robin@example.test", "apiUrl": "`+api+`", "state": "s1"}`)
		case "/jmap/":
			_, _ = io.WriteString(w, `{"methodResponses": [["Core/echo", {}, "0"]], "sessionState": "s1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func jmapTestConfig(sessionURL string, allowPlaintext, allowPrivate bool) *models.AccountConfig {
	return &models.AccountConfig{
		Provider:           "jmap",
		JMAPSessionURL:     sessionURL,
		Username:           "robin@example.test",
		JMAPAllowPlaintext: allowPlaintext,
		JMAPAllowPrivate:   allowPrivate,
	}
}

func TestJMAPAccountClientReachesApprovedPlainHTTPServer(t *testing.T) {
	server := jmapTestServer(t, "")
	client, err := newJMAPAccountClient(jmapTestConfig(server.URL+"/.well-known/jmap", true, true), "secret")
	if err != nil {
		t.Fatalf("newJMAPAccountClient: %v", err)
	}
	session, err := client.Session(context.Background(), server.URL+"/.well-known/jmap")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if session.Username != "robin@example.test" {
		t.Fatalf("session = %+v", session)
	}
}

func TestJMAPAccountClientNeedsPlaintextException(t *testing.T) {
	server := jmapTestServer(t, "")
	_, err := newJMAPAccountClient(jmapTestConfig(server.URL+"/.well-known/jmap", false, true), "secret")
	if err == nil || !strings.Contains(err.Error(), "HTTP plaintext exception") {
		t.Fatalf("err = %v, want a plaintext exception error", err)
	}
}

func TestJMAPAccountClientNeedsPrivateTargetException(t *testing.T) {
	server := jmapTestServer(t, "")
	client, err := newJMAPAccountClient(jmapTestConfig(server.URL+"/.well-known/jmap", true, false), "secret")
	if err != nil {
		t.Fatalf("newJMAPAccountClient: %v", err)
	}
	_, err = client.Session(context.Background(), server.URL+"/.well-known/jmap")
	if !netguard.IsBlocked(err) {
		t.Fatalf("err = %v, want a blocked private address", err)
	}
}

// The approval covers one endpoint: an apiUrl on another port of the same
// host is neither approved for plain HTTP nor as a private target.
func TestJMAPAccountClientRefusesOtherPlainHTTPEndpoints(t *testing.T) {
	other := jmapTestServer(t, "")
	server := jmapTestServer(t, other.URL+"/jmap/")
	client, err := newJMAPAccountClient(jmapTestConfig(server.URL+"/.well-known/jmap", true, true), "secret")
	if err != nil {
		t.Fatalf("newJMAPAccountClient: %v", err)
	}
	session, err := client.Session(context.Background(), server.URL+"/.well-known/jmap")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	_, err = client.Call(context.Background(), session, nil, jmap.Invocation{Name: "Core/echo", CallID: "0"})
	if err == nil || !strings.Contains(err.Error(), "without encryption") {
		t.Fatalf("err = %v, want the other endpoint refused", err)
	}
}

func TestJMAPAccountClientRefusesRedirectToOtherEndpoint(t *testing.T) {
	other := jmapTestServer(t, "")
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/.well-known/jmap", http.StatusTemporaryRedirect)
	}))
	defer redirecting.Close()
	client, err := newJMAPAccountClient(jmapTestConfig(redirecting.URL+"/.well-known/jmap", true, true), "secret")
	if err != nil {
		t.Fatalf("newJMAPAccountClient: %v", err)
	}
	_, err = client.Session(context.Background(), redirecting.URL+"/.well-known/jmap")
	if err == nil || !strings.Contains(err.Error(), "without encryption") {
		t.Fatalf("err = %v, want the redirect refused", err)
	}
}

func TestJMAPAccountClientRejectsOtherProviders(t *testing.T) {
	if _, err := newJMAPAccountClient(&models.AccountConfig{Provider: "imap"}, "secret"); err == nil {
		t.Fatal("built a JMAP client for an IMAP account")
	}
}

func TestJMAPSendingWarning(t *testing.T) {
	capability := map[string]json.RawMessage{jmap.CapabilityMail: nil, jmap.CapabilitySubmission: nil}
	mailOnly := map[string]json.RawMessage{jmap.CapabilityMail: nil}
	for _, tc := range []struct {
		name        string
		session     map[string]json.RawMessage
		account     map[string]json.RawMessage
		wantWarning bool
	}{
		{"can send", capability, capability, false},
		{"account capabilities not listed", capability, nil, false},
		{"server without sending", mailOnly, mailOnly, true},
		{"account without sending", capability, mailOnly, true},
	} {
		session := &jmap.Session{
			Capabilities:    tc.session,
			Accounts:        map[string]jmap.Account{"a1": {AccountCapabilities: tc.account}},
			PrimaryAccounts: map[string]string{jmap.CapabilityMail: "a1"},
		}
		if got := JMAPSendingWarning(session); (got != "") != tc.wantWarning {
			t.Errorf("%s: warning = %q, want warning %v", tc.name, got, tc.wantWarning)
		}
	}
}
