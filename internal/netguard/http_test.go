package netguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
)

func localServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int32, int) {
	t.Helper()
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	return server, &hits, port
}

func TestHTTPTransportRefusesUnapprovedPrivateServers(t *testing.T) {
	internal, internalHits, internalPort := localServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	redirector, _, redirectorPort := localServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/admin", http.StatusFound)
	})

	if _, err := (&http.Client{Transport: NewHTTPTransport(nil)}).Get(internal.URL); !IsBlocked(err) {
		t.Fatalf("unapproved private server error = %v", err)
	}
	if internalHits.Load() != 0 {
		t.Fatal("an unapproved private server was contacted")
	}

	approveOnly := func(port int) PrivatePolicy {
		return func(_ context.Context, protocol, host string, p int) bool {
			return protocol == "http" && host == "127.0.0.1" && p == port
		}
	}
	resp, err := (&http.Client{Transport: NewHTTPTransport(approveOnly(internalPort))}).Get(internal.URL)
	if err != nil {
		t.Fatalf("approved private server: %v", err)
	}
	resp.Body.Close()

	before := internalHits.Load()
	if _, err := (&http.Client{Transport: NewHTTPTransport(approveOnly(redirectorPort))}).Get(redirector.URL); !IsBlocked(err) {
		t.Fatalf("redirect to an unapproved private server error = %v", err)
	}
	if internalHits.Load() != before {
		t.Fatal("a redirect reached an unapproved private server")
	}
}

func TestUseTransportForTestsRestoresTheGuard(t *testing.T) {
	server, _, _ := localServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	client := &http.Client{Transport: NewHTTPTransport(nil)}
	restore := UseTransportForTests(http.DefaultTransport)
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("test transport: %v", err)
	}
	resp.Body.Close()
	restore()
	if _, err := client.Get(server.URL); !IsBlocked(err) {
		t.Fatalf("after restore error = %v, want the guard back", err)
	}
}

// Requests for no account in particular take the proxy their source picks,
// and a source that fails stops them before they connect.
func TestProxySourceTransport(t *testing.T) {
	server, hits, _ := localServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	allowAll := func(context.Context, string, string, int) bool { return true }
	proxy := startTestProxy(t, ProxyHTTP, "", "")
	var pick func(context.Context) (*Proxy, error)
	client := &http.Client{Transport: ProxySourceTransport(NewHTTPTransport(allowAll), func(ctx context.Context) (*Proxy, error) { return pick(ctx) })}

	pick = func(context.Context) (*Proxy, error) { return proxy.config("", ""), nil }
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if accepted, _ := proxy.seen(); accepted != 1 {
		t.Fatalf("proxy accepted %d connections, want 1", accepted)
	}

	pick = func(context.Context) (*Proxy, error) { return nil, errors.New("proxy password can't be read") }
	before := hits.Load()
	if _, err := client.Get(server.URL + "/again"); err == nil {
		t.Fatal("a request whose proxy can't be read was sent")
	}
	if hits.Load() != before {
		t.Fatal("the server was contacted without the proxy")
	}
}
