package netguard

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// PrivatePolicy approves one private target, such as a calendar server on the
// local network, for protocol ("http" or "https"), host and port.
type PrivatePolicy func(ctx context.Context, protocol, host string, port int) bool

// NewHTTPTransport returns an HTTP transport whose every connection goes
// through Dial: private addresses only when allowPrivate approves them, no
// proxy from the environment, and each redirect checked again as it connects.
// A nil policy approves no private target.
//
// A request whose context carries a proxy (WithProxy) goes through it. Each
// proxy gets its own pool of connections, so a connection opened through one
// proxy, or directly, is never reused for another.
func NewHTTPTransport(allowPrivate PrivatePolicy) http.RoundTripper {
	return &guardedTransport{allowPrivate: allowPrivate, byProxy: map[string]*http.Transport{}}
}

type guardedTransport struct {
	allowPrivate PrivatePolicy
	mu           sync.Mutex
	byProxy      map[string]*http.Transport
}

func (t *guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if override := testTransport.Load(); override != nil && *override != nil {
		return (*override).RoundTrip(req)
	}
	return t.transportFor(ProxyFrom(req.Context())).RoundTrip(req)
}

// transportFor returns the pool for proxy, creating it on first use. Pools of
// proxies nobody uses any more only hold idle connections, which close by
// themselves after IdleConnTimeout.
func (t *guardedTransport) transportFor(proxy *Proxy) *http.Transport {
	key := proxy.key()
	t.mu.Lock()
	defer t.mu.Unlock()
	inner := t.byProxy[key]
	if inner == nil {
		if proxy != nil {
			pinned := *proxy // the caller may change its copy later
			proxy = &pinned
		}
		inner = newGuardedHTTPTransport(t.allowPrivate, proxy)
		t.byProxy[key] = inner
	}
	return inner
}

// CloseIdleConnections closes idle connections of every proxy's pool.
func (t *guardedTransport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, inner := range t.byProxy {
		inner.CloseIdleConnections()
	}
}

var testTransport atomic.Pointer[http.RoundTripper]

// UseTransportForTests sends every guarded transport's requests through rt,
// or through the guarded transports themselves when rt is nil, until the
// returned function restores the previous one. It is for tests in
// other packages that serve outbound requests from local servers; production
// code never calls it.
func UseTransportForTests(rt http.RoundTripper) (restore func()) {
	previous := testTransport.Swap(&rt)
	return func() { testTransport.Store(previous) }
}

// newGuardedHTTPTransport connects through proxy, or directly when it is nil,
// whatever proxy the dial context carries: the pool it belongs to decides.
func newGuardedHTTPTransport(allowPrivate PrivatePolicy, proxy *Proxy) *http.Transport {
	dialer := &Dialer{Dialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}}
	dial := func(ctx context.Context, protocol, address string) (net.Conn, error) {
		ctx = WithProxy(ctx, proxy)
		host, portText, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			return nil, err
		}
		allowed := allowPrivate != nil && allowPrivate(ctx, protocol, host, port)
		return dialer.Dial(ctx, protocol, host, port, allowed)
	}
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			return dial(ctx, "http", address)
		},
		DialTLSContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			conn, err := dial(ctx, "https", address)
			if err != nil {
				return nil, err
			}
			host, _, _ := net.SplitHostPort(address)
			tlsConn := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return tlsConn, nil
		},
		MaxIdleConns:           32,
		IdleConnTimeout:        90 * time.Second,
		ResponseHeaderTimeout:  30 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
	}
}

// ProxyTransport sends every request through proxy, whatever proxy its
// context carries; a nil proxy means direct connections. next must connect
// with Dial, as transports from NewHTTPTransport do.
func ProxyTransport(next http.RoundTripper, proxy *Proxy) http.RoundTripper {
	return proxyTransport{next: next, proxy: proxy}
}

type proxyTransport struct {
	next  http.RoundTripper
	proxy *Proxy
}

func (t proxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.next.RoundTrip(req.WithContext(WithProxy(req.Context(), t.proxy)))
}

// CloseIdleConnections closes the idle connections of next.
func (t proxyTransport) CloseIdleConnections() {
	if closer, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// ProxySourceTransport sends every request through the proxy pick returns for
// it, or directly when pick returns nil. It is for requests made for nobody's
// account in particular, which take the default proxy. A pick that fails
// fails the request rather than connecting directly. next must connect with
// Dial, as transports from NewHTTPTransport do.
func ProxySourceTransport(next http.RoundTripper, pick func(context.Context) (*Proxy, error)) http.RoundTripper {
	return proxySourceTransport{next: next, pick: pick}
}

type proxySourceTransport struct {
	next http.RoundTripper
	pick func(context.Context) (*Proxy, error)
}

func (t proxySourceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	proxy, err := t.pick(req.Context())
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, &ProxyError{Proxy: "default", Err: err}
	}
	return t.next.RoundTrip(req.WithContext(WithProxy(req.Context(), proxy)))
}

// CloseIdleConnections closes the idle connections of next.
func (t proxySourceTransport) CloseIdleConnections() {
	if closer, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
