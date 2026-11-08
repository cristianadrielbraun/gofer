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

// Credentials are bound for a while longer than providers keep access tokens
// valid, and bound again each time they are handed out, so a changed proxy
// applies from the next request on.
const credentialBindingTTL = 3 * time.Hour

type credentialBinding struct {
	proxy   *Proxy
	expires time.Time
}

var credentialBindings = struct {
	sync.Mutex
	tokens map[[32]byte]credentialBinding
}{tokens: map[[32]byte]credentialBinding{}}

// BindBearerProxy records that requests carrying the access token go through
// proxy, or directly when it is nil. Gofer binds every account token it hands
// out, so each of the account's API requests takes the account's route
// whatever context it is made with.
func BindBearerProxy(token string, proxy *Proxy) {
	token = strings.TrimSpace(token)
	if token == "" {
		return
	}
	bindCredential(bearerKey(token), proxy)
}

// BindBasicProxy records that requests signing in with username and password
// go through proxy, or directly when it is nil, as BindBearerProxy does for
// tokens. CalDAV and CardDAV requests sign in this way.
func BindBasicProxy(username, password string, proxy *Proxy) {
	if password == "" {
		return
	}
	bindCredential(basicKey(username, password), proxy)
}

// CredentialProxy returns the proxy bound to the credentials req carries, a
// bearer token or a username and password, and whether any is bound.
func CredentialProxy(req *http.Request) (*Proxy, bool) {
	if username, password, ok := req.BasicAuth(); ok {
		return boundProxy(basicKey(username, password))
	}
	return bearerProxy(req.Header.Get("Authorization"))
}

func bearerKey(token string) [32]byte {
	return sha256.Sum256([]byte("bearer\x00" + strings.TrimSpace(token)))
}

func basicKey(username, password string) [32]byte {
	return sha256.Sum256([]byte("basic\x00" + username + "\x00" + password))
}

func bindCredential(key [32]byte, proxy *Proxy) {
	if proxy != nil {
		pinned := *proxy
		proxy = &pinned
	}
	now := time.Now()
	credentialBindings.Lock()
	defer credentialBindings.Unlock()
	if len(credentialBindings.tokens) > 1024 {
		for k, binding := range credentialBindings.tokens {
			if now.After(binding.expires) {
				delete(credentialBindings.tokens, k)
			}
		}
	}
	credentialBindings.tokens[key] = credentialBinding{proxy: proxy, expires: now.Add(credentialBindingTTL)}
}

// bearerProxy returns the proxy bound to the bearer token in an Authorization
// header, and whether one is bound.
func bearerProxy(authorization string) (*Proxy, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(authorization), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return nil, false
	}
	return boundProxy(bearerKey(token))
}

func boundProxy(key [32]byte) (*Proxy, bool) {
	credentialBindings.Lock()
	defer credentialBindings.Unlock()
	binding, ok := credentialBindings.tokens[key]
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
