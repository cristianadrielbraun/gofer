package handler

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// roundTripFunc answers DAV requests in tests without a network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// CalDAV reaches a plain HTTP server only at the host and port an HTTP
// plaintext exception approves, redirects included; HTTPS is unaffected.
func TestCalDAVPlainHTTPNeedsAnApprovedException(t *testing.T) {
	previousTransport, previousPolicy := davHTTPTransport, davPlaintextPolicy.Load()
	t.Cleanup(func() { davHTTPTransport = previousTransport; davPlaintextPolicy.Store(previousPolicy) })
	var reached []string
	davHTTPTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		reached = append(reached, req.URL.String())
		response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: req}
		if req.URL.Path == "/moved" {
			response.StatusCode = http.StatusTemporaryRedirect
			response.Header.Set("Location", "http://calendar.example.test:8080/dav/")
		}
		return response, nil
	})
	setDAVPlaintextPolicy(func(_ context.Context, host string, port int) bool {
		return host == "192.168.0.10" && port == 8687
	})
	client := &http.Client{Transport: calDAVHTTPTransport}
	get := func(url string) error {
		t.Helper()
		req, _ := http.NewRequestWithContext(davTestContext(t), http.MethodGet, url, nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}

	if err := get("http://192.168.0.10:8687/dav/cal/"); err != nil {
		t.Fatalf("approved plain HTTP server refused: %v", err)
	}
	if err := get("https://calendar.example.test/dav/"); err != nil {
		t.Fatalf("HTTPS refused: %v", err)
	}
	for _, refused := range []string{"http://192.168.0.10:8688/dav/", "http://192.168.0.11:8687/dav/", "http://192.168.0.10:8687/moved", "ftp://192.168.0.10/dav/"} {
		reached = nil
		err := get(refused)
		if err == nil {
			t.Fatalf("%s was allowed", refused)
		}
		if strings.HasPrefix(refused, "http:") && !strings.Contains(err.Error(), "HTTP plaintext exception") {
			t.Fatalf("%s: error %q does not say how to approve it", refused, err)
		}
		for _, url := range reached {
			if strings.Contains(url, "calendar.example.test:8080") || strings.Contains(url, "8688") || strings.Contains(url, ".11:") {
				t.Fatalf("an unapproved server was contacted: %s", url)
			}
		}
	}
	if _, err := normalizeCalDAVBaseURL("http://192.168.0.10:8687/dav/cal/"); err != nil {
		t.Fatalf("plain HTTP server address rejected before connecting: %v", err)
	}
	if got, _ := normalizeCalDAVBaseURL("calendar.example.test"); got != "https://calendar.example.test" {
		t.Fatalf("address without a scheme = %q, want HTTPS", got)
	}
}

// A JMAP account's server is tried for CardDAV and CalDAV when no address was
// entered, right after any address that was.
func TestDAVDiscoveryTriesTheJMAPServer(t *testing.T) {
	for sessionURL, want := range map[string]string{
		"http://192.168.0.10:8687/.well-known/jmap": "http://192.168.0.10:8687",
		"https://JMAP.example.test/jmap/session":    "https://jmap.example.test",
		"":                                          "",
		"ftp://jmap.example.test/":                  "",
		"not a url":                                 "",
	} {
		if got := jmapServerAddress(sessionURL); got != want {
			t.Errorf("jmapServerAddress(%q) = %q, want %q", sessionURL, got, want)
		}
	}
	// A cancelled context stops before any domain guesses, leaving the
	// server addresses in the order given.
	ctx, cancel := context.WithCancel(davTestContext(t))
	cancel()
	got := calDAVAutodiscoveryCandidates(ctx, []string{"https://dav.example.test/cal/", "", jmapServerAddress("http://192.168.0.10:8687/.well-known/jmap")}, "robin@example.test")
	if strings.Join(got, " ") != "https://dav.example.test/cal/ http://192.168.0.10:8687" {
		t.Fatalf("CalDAV candidates = %v", got)
	}
	if got := contactSyncDiscoveryCandidates("", jmapServerAddress("http://192.168.0.10:8687/.well-known/jmap"), "", "", "robin@example.test"); strings.Join(got, " ") != "http://192.168.0.10:8687 robin@example.test" {
		t.Fatalf("CardDAV candidates = %v", got)
	}
}
