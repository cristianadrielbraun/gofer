package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// People enter CalDAV and CardDAV servers themselves, so a server on the
// Gofer host's network is refused unless a mail security exception approves it.
func TestDAVRequestsRefuseUnapprovedPrivateServers(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><d:multistatus xmlns:d="DAV:"/>`))
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	previous, previousPlaintext := davPrivateTargetPolicy.Load(), davPlaintextPolicy.Load()
	t.Cleanup(func() { davPrivateTargetPolicy.Store(previous); davPlaintextPolicy.Store(previousPlaintext) })
	cfg := models.ContactSyncConfig{BaseURL: server.URL + "/", AddressBookURL: server.URL + "/books/default/", Username: "lena@example.test"}

	// Plain HTTP needs its own approval, whatever the address.
	setDAVPrivateTargetPolicy(func(context.Context, string, string, int) bool { return true })
	setDAVPlaintextPolicy(nil)
	err := testCardDAVAddressBook(davTestContext(t), cfg, "secret")
	if err == nil || !strings.Contains(err.Error(), "HTTP plaintext exception") || hits.Load() != 0 {
		t.Fatalf("unapproved plain HTTP DAV server: err=%v hits=%d", err, hits.Load())
	}

	setDAVPlaintextPolicy(func(_ context.Context, host string, p int) bool { return host == "127.0.0.1" && p == port })
	setDAVPrivateTargetPolicy(nil)
	err = testCardDAVAddressBook(davTestContext(t), cfg, "secret")
	if err == nil || !strings.Contains(err.Error(), "private network") || hits.Load() != 0 {
		t.Fatalf("unapproved private DAV server: err=%v hits=%d", err, hits.Load())
	}

	setDAVPrivateTargetPolicy(func(_ context.Context, protocol, host string, p int) bool {
		return protocol == "http" && host == "127.0.0.1" && p == port
	})
	_ = testCardDAVAddressBook(davTestContext(t), cfg, "secret")
	if hits.Load() == 0 {
		t.Fatal("an approved private DAV server was not contacted")
	}
}

func TestPushSubscriptionsNeedAnHTTPSPushService(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"https://push.example.test/send/abc":   true,
		"http://push.example.test/send/abc":    false,
		"https://user:pass@push.example.test":  false,
		"ftp://push.example.test/":             false,
		"not a url":                            false,
		"https://" + strings.Repeat("a", 2100): false,
	} {
		if got := validPushEndpoint(endpoint); got != want {
			t.Errorf("validPushEndpoint(%.40q) = %v, want %v", endpoint, got, want)
		}
	}
}
