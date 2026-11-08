package netguard

import (
	"context"
	"crypto/sha256"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// APITransport carries requests to the provider APIs Gofer itself names:
// Gmail, Google and Microsoft Graph, and their sign-in endpoints. Their hosts
// are fixed in the code rather than entered by people, so they get no private
// network check, unlike servers people enter. Proxies from the environment
// are ignored; a request goes through the proxy of the account it is for:
//
//   - the proxy bound to its bearer token (BindBearerProxy), when it carries
//     an account's access token, and otherwise
//   - the proxy its context carries (WithProxy), or none.
var APITransport http.RoundTripper = &apiTransport{byProxy: map[string]*http.Transport{}}

// APIClient returns a client over APITransport with the timeout. A timeout of
// 0 means none.
func APIClient(timeout time.Duration) *http.Client {
	return &http.Client{Transport: APITransport, Timeout: timeout}
}

type apiTransport struct {
	mu      sync.Mutex
	byProxy map[string]*http.Transport
}

func (t *apiTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if override := apiTestTransport.Load(); override != nil {
		return (*override).RoundTrip(req)
	}
	proxy, bound := bearerProxy(req.Header.Get("Authorization"))
	if !bound {
		proxy = ProxyFrom(req.Context())
	}
	return t.transportFor(proxy).RoundTrip(req)
}

func (t *apiTransport) transportFor(proxy *Proxy) *http.Transport {
	key := proxy.key()
	t.mu.Lock()
	defer t.mu.Unlock()
	inner := t.byProxy[key]
	if inner == nil {
		if proxy != nil {
			pinned := *proxy
			proxy = &pinned
		}
		inner = newAPIHTTPTransport(proxy)
		t.byProxy[key] = inner
	}
	return inner
}

// CloseIdleConnections closes idle connections of every proxy's pool.
func (t *apiTransport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, inner := range t.byProxy {
		inner.CloseIdleConnections()
	}
}

// newAPIHTTPTransport is http.DefaultTransport without the environment's
// proxy, connecting through proxy when it isn't nil.
func newAPIHTTPTransport(proxy *Proxy) *http.Transport {
	netDialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	dial := netDialer.DialContext
	if proxy != nil {
		guarded := &Dialer{Dialer: netDialer}
		dial = func(ctx context.Context, _, address string) (net.Conn, error) {
			host, portText, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			port, err := strconv.Atoi(portText)
			if err != nil {
				return nil, err
			}
			return guarded.Dial(WithProxy(ctx, proxy), "https", host, port, true)
		}
	}
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           dial,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// Access tokens are bound for a while longer than providers keep them valid,
// and bound again each time they are handed out, so a changed proxy applies
// from the next request on.
const bearerBindingTTL = 3 * time.Hour

type bearerBinding struct {
	proxy   *Proxy
	expires time.Time
}

var bearerBindings = struct {
	sync.Mutex
	tokens map[[32]byte]bearerBinding
}{tokens: map[[32]byte]bearerBinding{}}

// BindBearerProxy records that requests carrying the access token go through
// proxy, or directly when it is nil. Gofer binds every account token it hands
// out, so each of the account's API requests takes the account's route
// whatever context it is made with.
func BindBearerProxy(token string, proxy *Proxy) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	if proxy != nil {
		pinned := *proxy
		proxy = &pinned
	}
	now := time.Now()
	key := sha256.Sum256([]byte(token))
	bearerBindings.Lock()
	defer bearerBindings.Unlock()
	if len(bearerBindings.tokens) > 1024 {
		for k, binding := range bearerBindings.tokens {
			if now.After(binding.expires) {
				delete(bearerBindings.tokens, k)
			}
		}
	}
	bearerBindings.tokens[key] = bearerBinding{proxy: proxy, expires: now.Add(bearerBindingTTL)}
}

// bearerProxy returns the proxy bound to the bearer token in an Authorization
// header, and whether one is bound.
func bearerProxy(authorization string) (*Proxy, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(authorization), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return nil, false
	}
	key := sha256.Sum256([]byte(strings.TrimSpace(token)))
	bearerBindings.Lock()
	defer bearerBindings.Unlock()
	binding, ok := bearerBindings.tokens[key]
	if !ok || time.Now().After(binding.expires) {
		return nil, false
	}
	return binding.proxy, true
}

var apiTestTransport atomic.Pointer[http.RoundTripper]

// UseAPITransportForTests sends APITransport's requests through rt until the
// returned function restores the previous one. It is for tests in other
// packages that serve provider APIs from local servers; production code never
// calls it.
func UseAPITransportForTests(rt http.RoundTripper) (restore func()) {
	previous := apiTestTransport.Swap(&rt)
	return func() { apiTestTransport.Store(previous) }
}
