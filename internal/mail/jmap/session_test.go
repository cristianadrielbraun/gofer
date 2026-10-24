package jmap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

const testSessionJSON = `{
  "capabilities": {
    "urn:ietf:params:jmap:core": {
      "maxSizeUpload": 50000000, "maxConcurrentUpload": 4,
      "maxSizeRequest": 10000000, "maxConcurrentRequests": 4,
      "maxCallsInRequest": 16, "maxObjectsInGet": 500, "maxObjectsInSet": 500
    },
    "urn:ietf:params:jmap:mail": {}
  },
  "accounts": {
    "a1": {"name": "robin@example.test", "isPersonal": true, "isReadOnly": false,
           "accountCapabilities": {"urn:ietf:params:jmap:mail": {}}}
  },
  "primaryAccounts": {"urn:ietf:params:jmap:mail": "a1"},
  "username": "robin@example.test",
  "apiUrl": "/jmap/",
  "downloadUrl": "/jmap/download/{accountId}/{blobId}/{name}?accept={type}",
  "uploadUrl": "/jmap/upload/{accountId}/",
  "eventSourceUrl": "/jmap/eventsource/?types={types}&closeafter={closeafter}&ping={ping}",
  "state": "s1"
}`

func TestSessionFollowsDiscoveryRedirectAndResolvesURLs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jmap", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/jmap/session", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/jmap/session", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "robin@example.test" || pass != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testSessionJSON))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := &Client{HTTP: server.Client(), Auth: BasicAuth("robin@example.test", "secret")}
	session, err := client.Session(context.Background(), server.URL+"/.well-known/jmap")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}

	if got := session.PrimaryAccount(CapabilityMail); got != "a1" {
		t.Fatalf("primary mail account = %q, want a1", got)
	}
	if session.Accounts["a1"].Name != "robin@example.test" {
		t.Fatalf("account name = %q", session.Accounts["a1"].Name)
	}
	if session.Core.MaxCallsInRequest != 16 || session.Core.MaxObjectsInGet != 500 {
		t.Fatalf("core limits = %+v", session.Core)
	}
	if want := server.URL + "/jmap/"; session.APIURL != want {
		t.Fatalf("apiUrl = %q, want %q", session.APIURL, want)
	}
	// Template placeholders must survive resolution unescaped.
	if want := server.URL + "/jmap/download/{accountId}/{blobId}/{name}?accept={type}"; session.DownloadURL != want {
		t.Fatalf("downloadUrl = %q, want %q", session.DownloadURL, want)
	}
}

func TestSessionReturnsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
	}))
	defer server.Close()

	client := &Client{HTTP: server.Client(), Auth: BasicAuth("robin@example.test", "wrong")}
	_, err := client.Session(context.Background(), server.URL+"/.well-known/jmap")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v, want HTTPError 401", err)
	}
}

// A server that takes only bearer tokens refuses the username and password
// with a Bearer challenge; the client then sends the token and keeps using it.
func TestSessionSwitchesToTokenWhenServerAsksForBearer(t *testing.T) {
	var basicTries int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			if _, _, ok := r.BasicAuth(); ok {
				basicTries++
			}
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="https://example.test/meta"`)
			http.Error(w, "not bearer", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(testSessionJSON))
	}))
	defer server.Close()

	client := &Client{HTTP: server.Client(), Auth: BasicAuth("robin@example.test", "tok-1"), TokenAuth: BearerAuth("tok-1")}
	if _, err := client.Session(context.Background(), server.URL+"/jmap/session"); err != nil {
		t.Fatalf("Session: %v", err)
	}
	if _, err := client.Session(context.Background(), server.URL+"/jmap/session"); err != nil {
		t.Fatalf("second Session: %v", err)
	}
	if basicTries != 1 {
		t.Fatalf("username and password sent %d times, want 1", basicTries)
	}
}

// Without a Bearer-only challenge the token is never sent.
func TestSessionKeepsPasswordWhenServerOffersBasic(t *testing.T) {
	var bearerTries int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			bearerTries++
		}
		w.Header().Add("WWW-Authenticate", `Bearer realm="mail"`)
		w.Header().Add("WWW-Authenticate", `Basic realm="mail", charset="UTF-8"`)
		http.Error(w, "bad credentials", http.StatusUnauthorized)
	}))
	defer server.Close()

	client := &Client{HTTP: server.Client(), Auth: BasicAuth("robin@example.test", "wrong"), TokenAuth: BearerAuth("wrong")}
	_, err := client.Session(context.Background(), server.URL+"/jmap/session")
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.WantsBearerOnly() {
		t.Fatalf("err = %v, want a 401 that also offers Basic", err)
	}
	if bearerTries != 0 {
		t.Fatalf("token sent %d times", bearerTries)
	}
}

func TestWantsBearerOnly(t *testing.T) {
	for _, tc := range []struct {
		status    int
		challenge []string
		want      bool
	}{
		{http.StatusUnauthorized, []string{`Bearer resource_metadata="https://example.test/meta"`}, true},
		{http.StatusUnauthorized, []string{`Bearer realm="a", error="invalid_token"`}, true},
		{http.StatusUnauthorized, []string{`Bearer realm="a", Basic realm="a"`}, false},
		{http.StatusUnauthorized, []string{`Basic realm="a"`}, false},
		{http.StatusUnauthorized, nil, false},
		{http.StatusForbidden, []string{`Bearer`}, false},
	} {
		if got := (&HTTPError{StatusCode: tc.status, Challenge: tc.challenge}).WantsBearerOnly(); got != tc.want {
			t.Errorf("%d %q: got %v, want %v", tc.status, tc.challenge, got, tc.want)
		}
	}
}

func TestResolveTemplateURL(t *testing.T) {
	base, _ := url.Parse("https://mail.example.test/jmap/session")
	cases := map[string]string{
		"":                               "",
		"https://other.example.test/api": "https://other.example.test/api",
		"/jmap/":                         "https://mail.example.test/jmap/",
		"api/":                           "https://mail.example.test/jmap/api/",
	}
	for ref, want := range cases {
		if got := resolveTemplateURL(base, ref); got != want {
			t.Errorf("resolveTemplateURL(%q) = %q, want %q", ref, got, want)
		}
	}
}

// liveSession connects to a real server, and skips the test unless
// GOFER_JMAP_TEST_URL, _USER and _PASSWORD are set.
func liveSession(t *testing.T) (*Client, *Session) {
	t.Helper()
	sessionURL := os.Getenv("GOFER_JMAP_TEST_URL")
	user := os.Getenv("GOFER_JMAP_TEST_USER")
	password := os.Getenv("GOFER_JMAP_TEST_PASSWORD")
	if sessionURL == "" || user == "" || password == "" {
		t.Skip("GOFER_JMAP_TEST_URL, GOFER_JMAP_TEST_USER and GOFER_JMAP_TEST_PASSWORD not set")
	}
	client := &Client{Auth: BasicAuth(user, password)}
	session, err := client.Session(context.Background(), sessionURL)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	return client, session
}

func TestSessionLiveServer(t *testing.T) {
	_, session := liveSession(t)
	if session.PrimaryAccount(CapabilityMail) == "" {
		t.Fatalf("no primary mail account in session: %+v", session)
	}
	t.Logf("user=%s mail account=%s apiUrl=%s", session.Username, session.PrimaryAccount(CapabilityMail), session.APIURL)
}
