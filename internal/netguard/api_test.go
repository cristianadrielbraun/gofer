package netguard

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"testing"
)

// An API request goes through the proxy bound to its access token, else the
// one its context carries, else straight out, and reaches a local server: API
// hosts are Gofer's own, so they have no private network check.
func TestAPITransportRoutesByTokenThenContext(t *testing.T) {
	server, _, port := localServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	tokenProxy := startTestProxy(t, ProxyHTTP, "", "")
	contextProxy := startTestProxy(t, ProxySOCKS5, "", "")
	target := "127.0.0.1:" + strconv.Itoa(port)

	get := func(ctx context.Context, token string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := APIClient(0).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		APITransport.(*apiTransport).CloseIdleConnections()
	}
	seen := func(p *testProxy) int {
		_, targets := p.seen()
		for _, got := range targets {
			if got != target {
				t.Fatalf("proxy was asked for %s, want %s", got, target)
			}
		}
		return len(targets)
	}

	BindBearerProxy("token-with-proxy", tokenProxy.config("", ""))
	BindBearerProxy("token-direct", nil)

	get(WithProxy(t.Context(), contextProxy.config("", "")), "token-with-proxy")
	if seen(tokenProxy) != 1 || seen(contextProxy) != 0 {
		t.Fatal("a bound token did not take its own proxy over the context's")
	}
	get(WithProxy(t.Context(), contextProxy.config("", "")), "token-direct")
	if seen(tokenProxy) != 1 || seen(contextProxy) != 0 {
		t.Fatal("a token bound to a direct connection went through a proxy")
	}
	get(WithProxy(t.Context(), contextProxy.config("", "")), "unknown-token")
	if seen(contextProxy) != 1 {
		t.Fatal("an unbound token did not use the context's proxy")
	}
	get(t.Context(), "")
	if seen(tokenProxy) != 1 || seen(contextProxy) != 1 {
		t.Fatal("a request with nothing to route it went through a proxy")
	}

	// Binding again moves the token to the new route.
	BindBearerProxy("token-with-proxy", nil)
	get(t.Context(), "token-with-proxy")
	if seen(tokenProxy) != 1 {
		t.Fatal("a token bound again kept its old proxy")
	}
}

func TestBearerProxyNeedsABearerHeader(t *testing.T) {
	BindBearerProxy("abc", &Proxy{Type: ProxyHTTP, Host: "proxy.example.test", Port: 3128})
	for _, header := range []string{"", "Basic abc", "abc", "Bearer"} {
		if _, ok := bearerProxy(header); ok {
			t.Fatalf("bearerProxy(%q) found a binding", header)
		}
	}
	if p, ok := bearerProxy("bearer  abc "); !ok || p.Host != "proxy.example.test" {
		t.Fatalf("bearerProxy = %v, %v", p, ok)
	}
}
