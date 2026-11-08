package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/netguard"
	"github.com/cristianadrielbraun/gofer/internal/netguard/netguardtest"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// CalDAV and CardDAV requests carry only the login they sign in with, and
// take the proxy of the account that login belongs to: adding an account
// signs in through the default proxy, its contacts sync through the proxy it
// chose, from whatever context, and stop when it connects directly.
func TestSharedModeDAVGoesThroughTheAccountProxy(t *testing.T) {
	h, server := newSharedDAVAccountHandler(t)
	useNetguardForLocalDAV(t)
	host := strings.TrimPrefix(server.URL, "http://")
	defaultProxy, chosenProxy := netguardtest.StartHTTPProxy(t), netguardtest.StartHTTPProxy(t)
	if _, err := h.accountStore.SaveOutboundProxy(t.Context(), config.OutboundProxyInput{Name: "Default", Type: "http",
		Host: defaultProxy.Config.Host, Port: defaultProxy.Config.Port, IsDefault: true}, "admin"); err != nil {
		t.Fatal(err)
	}
	chosen, err := h.accountStore.SaveOutboundProxy(t.Context(), config.OutboundProxyInput{Name: "Chosen", Type: "http",
		Host: chosenProxy.Config.Host, Port: chosenProxy.Config.Port}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	asked := func(p *netguardtest.Proxy) int {
		t.Helper()
		count := 0
		for _, target := range p.Targets() {
			if target != host {
				t.Fatalf("proxy was asked for %s, want %s", target, host)
			}
			count++
		}
		return count
	}
	// The HTTP client may finish a connection it began for a request that
	// another connection served; wait for those before the next step.
	settle := func() {
		t.Helper()
		last, stable := -1, 0
		for stable < 3 {
			time.Sleep(20 * time.Millisecond)
			now := len(defaultProxy.Targets()) + len(chosenProxy.Targets())
			if now == last {
				stable++
			} else {
				last, stable = now, 0
			}
		}
	}

	book := server.URL + "/books/family/"
	form := url.Values{"service": {"contacts"}, "server_url": {server.URL}, "username": {"dav-user"}, "password": {"dav-pass"}, "item": {book}, "default_item": {book}}
	rec := httptest.NewRecorder()
	h.handleCreateDAVAccount(rec, davAccountRequest("/api/accounts/dav", form))
	if asked(defaultProxy) == 0 {
		t.Fatalf("adding the account (%d %q) didn't sign in through the default proxy", rec.Code, rec.Body.String())
	}
	ids := sharedDAVAccounts(t, h)
	if len(ids) != 1 {
		t.Fatalf("created %v (%d %q)", ids, rec.Code, rec.Body.String())
	}

	sync := func() {
		t.Helper()
		cfg, password, err := h.cardDAVConfig(t.Context(), "alice", ids[0])
		if err != nil {
			t.Fatal(err)
		}
		cfg.AddressBookURL = book
		// The sync's own context says nothing about proxies.
		_, _ = cardDAVSync(t.Context(), cfg, password)
	}
	if err := h.accountStore.SetAccountProxy(t.Context(), "alice", ids[0], chosen); err != nil {
		t.Fatal(err)
	}
	settle()
	before := asked(defaultProxy)
	sync()
	if asked(chosenProxy) == 0 || asked(defaultProxy) != before {
		t.Fatal("the contacts sync didn't go through the account's own proxy")
	}

	if err := h.accountStore.SetAccountProxy(t.Context(), "alice", ids[0], storage.AccountProxyDirect); err != nil {
		t.Fatal(err)
	}
	settle()
	through := asked(chosenProxy)
	sync()
	if asked(chosenProxy) != through || asked(defaultProxy) != before {
		t.Fatalf("the contacts sync of a direct account went through a proxy: chosen %d -> %d, default %d -> %d", through, asked(chosenProxy), before, asked(defaultProxy))
	}
}

// A request signing in with a login bound to a proxy goes through it; one
// with an unknown login uses its context's proxy.
func TestDAVTransportRoutesByLogin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusMultiStatus) }))
	defer server.Close()
	allowLocalDAVServers(t)
	useNetguardForLocalDAV(t)
	proxy := netguardtest.StartHTTPProxy(t)
	get := func(username string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), "PROPFIND", server.URL+"/", nil)
		req.SetBasicAuth(username, "login-routing-pass")
		resp, err := (&http.Client{Transport: cardDAVHTTPTransport}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	netguardBindForTest("routed-user", "login-routing-pass", proxy)
	get("routed-user")
	if len(proxy.Targets()) != 1 {
		t.Fatalf("bound login went through %v", proxy.Targets())
	}
	netguardBindForTest("unknown-user", "login-routing-pass", nil)
	get("unknown-user")
	if len(proxy.Targets()) != 1 {
		t.Fatalf("a login bound to a direct connection went through the proxy: %v", proxy.Targets())
	}
}

// useNetguardForLocalDAV sends DAV requests through netguard's own
// transport, which is where proxies are used, approving local servers rather
// than going around netguard as allowLocalDAVServers does.
func useNetguardForLocalDAV(t *testing.T) {
	t.Helper()
	t.Cleanup(netguard.UseTransportForTests(nil))
	previous := davPrivateTargetPolicy.Load()
	setDAVPrivateTargetPolicy(func(context.Context, string, string, int) bool { return true })
	t.Cleanup(func() { davPrivateTargetPolicy.Store(previous) })
}
