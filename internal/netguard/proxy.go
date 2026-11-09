package netguard

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// Proxy types an outbound proxy can have.
const (
	ProxyHTTP   = "http"
	ProxySOCKS5 = "socks5"
)

// Proxy is an outbound proxy that connections to servers go through.
//
// Gofer still resolves and checks each server's address itself and asks the
// proxy for that exact address, so the private network check keeps working:
// the proxy never sees, or resolves, a host name. The proxy's own address is
// not checked, because only an administrator defines proxies.
type Proxy struct {
	Type     string
	Host     string
	Port     int
	Username string
	Password string
	// PublicOnly refuses to reach the proxy itself on a private address. It
	// is set for proxies people saved for their own accounts, which must not
	// lead into the Gofer server's network. It is checked on every
	// connection, so a changed DNS answer can't get around it.
	PublicOnly bool
}

// Validate reports why p can't be used, if it can't.
func (p *Proxy) Validate() error {
	switch p.Type {
	case ProxyHTTP, ProxySOCKS5:
	default:
		return fmt.Errorf("proxy type must be %q or %q", ProxyHTTP, ProxySOCKS5)
	}
	host := strings.TrimSpace(p.Host)
	if host == "" || host != p.Host {
		return errors.New("proxy host is required and must not have spaces")
	}
	if strings.ContainsAny(host, "/?#@ \t\r\n") {
		return fmt.Errorf("proxy host %q must be a host name or address, not a URL", host)
	}
	if addr, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil && addr.Zone() != "" {
		return fmt.Errorf("proxy address %q must not name a network interface", host)
	}
	if p.Port < 1 || p.Port > 65535 {
		return errors.New("proxy port must be between 1 and 65535")
	}
	if p.Password != "" && p.Username == "" {
		return errors.New("a proxy password needs a username")
	}
	if strings.ContainsAny(p.Username, "\r\n") || strings.ContainsAny(p.Password, "\r\n") {
		return errors.New("proxy username and password must be on one line")
	}
	switch p.Type {
	case ProxyHTTP:
		if strings.Contains(p.Username, ":") {
			return errors.New("an HTTP proxy username must not contain a colon")
		}
	case ProxySOCKS5:
		// RFC 1929 sends each as one length byte and the bytes.
		if len(p.Username) > 255 || len(p.Password) > 255 {
			return errors.New("a SOCKS5 proxy username and password can be at most 255 bytes each")
		}
	}
	return nil
}

// Address is host:port of the proxy itself.
func (p *Proxy) Address() string {
	return net.JoinHostPort(strings.Trim(p.Host, "[]"), strconv.Itoa(p.Port))
}

// String names the proxy in messages and logs, without its password.
func (p *Proxy) String() string {
	if p == nil {
		return "direct"
	}
	return p.Type + "://" + p.Address()
}

// key tells proxies apart for connection reuse, so a connection made through
// one proxy, or with old credentials, is never handed to another.
func (p *Proxy) key() string {
	if p == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(p.Username + "\x00" + p.Password))
	key := p.Type + "://" + p.Address() + "#" + hex.EncodeToString(sum[:8])
	if p.PublicOnly {
		key += "+public"
	}
	return key
}

// Key is a stable identity for p, suitable for pooling connections by proxy.
// It changes when any of p's fields change, and is empty for no proxy.
func (p *Proxy) Key() string { return p.key() }

type proxyKey struct{}

// WithProxy marks ctx so connections made with it go through p. A nil p means
// direct connections, also when ctx already carried a proxy.
func WithProxy(ctx context.Context, p *Proxy) context.Context {
	return context.WithValue(ctx, proxyKey{}, p)
}

// ProxyFrom returns the proxy connections made with ctx go through, or nil
// for direct connections.
func ProxyFrom(ctx context.Context) *Proxy {
	p, _ := ctx.Value(proxyKey{}).(*Proxy)
	return p
}

// ProxyChosen reports whether ctx was marked with WithProxy at all, a direct
// connection included.
func ProxyChosen(ctx context.Context) bool {
	_, ok := ctx.Value(proxyKey{}).(*Proxy)
	return ok
}

// ProxyError reports a failure connecting through a proxy, as opposed to a
// failure of the server behind it.
type ProxyError struct {
	Proxy string
	Err   error
}

func (e *ProxyError) Error() string { return "proxy " + e.Proxy + ": " + e.Err.Error() }
func (e *ProxyError) Unwrap() error { return e.Err }

// dialThroughProxy connects to the checked address target through p.
func dialThroughProxy(ctx context.Context, dialer *net.Dialer, p *Proxy, target netip.AddrPort) (net.Conn, error) {
	if err := p.Validate(); err != nil {
		return nil, &ProxyError{Proxy: p.String(), Err: err}
	}
	var (
		conn net.Conn
		err  error
	)
	switch p.Type {
	case ProxyHTTP:
		conn, err = dialHTTPConnect(ctx, dialer, p, target)
	case ProxySOCKS5:
		conn, err = dialSOCKS5(ctx, dialer, p, target)
	}
	if err != nil {
		var proxyErr *ProxyError
		if errors.As(err, &proxyErr) {
			return nil, err
		}
		return nil, &ProxyError{Proxy: p.String(), Err: err}
	}
	return conn, nil
}

// handshakeDeadline bounds a proxy handshake by ctx and the dialer timeout.
func handshakeDeadline(ctx context.Context, dialer *net.Dialer) time.Time {
	deadline, ok := ctx.Deadline()
	if dialer.Timeout > 0 {
		if limit := time.Now().Add(dialer.Timeout); !ok || limit.Before(deadline) {
			deadline, ok = limit, true
		}
	}
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	return deadline
}

// proxyServerDialer connects to a proxy itself, refusing a private address
// when the proxy is PublicOnly.
type proxyServerDialer struct {
	dialer *net.Dialer
	proxy  *Proxy
}

func (d proxyServerDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d proxyServerDialer) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	if !d.proxy.PublicOnly {
		return d.dialer.DialContext(ctx, "tcp", d.proxy.Address())
	}
	return (&Dialer{Dialer: d.dialer}).Dial(WithProxy(ctx, nil), "proxy", strings.Trim(d.proxy.Host, "[]"), d.proxy.Port, false)
}

func dialHTTPConnect(ctx context.Context, dialer *net.Dialer, p *Proxy, target netip.AddrPort) (net.Conn, error) {
	conn, err := proxyServerDialer{dialer: dialer, proxy: p}.DialContext(ctx, "tcp", p.Address())
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	_ = conn.SetDeadline(handshakeDeadline(ctx, dialer))

	authority := target.String()
	request := "CONNECT " + authority + " HTTP/1.1\r\nHost: " + authority + "\r\n"
	if p.Username != "" {
		credentials := base64.StdEncoding.EncodeToString([]byte(p.Username + ":" + p.Password))
		request += "Proxy-Authorization: Basic " + credentials + "\r\n"
	}
	request += "\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		_ = conn.Close()
		return nil, contextError(ctx, err)
	}
	reader := bufio.NewReaderSize(conn, 4096)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, contextError(ctx, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		if resp.StatusCode == http.StatusProxyAuthRequired {
			return nil, errors.New("the proxy refused the username or password")
		}
		return nil, fmt.Errorf("the proxy refused to connect to %s: %s", authority, resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	if reader.Buffered() > 0 {
		// The server spoke before the client could, and the bytes are
		// already read: hand them out first.
		return &bufferedConn{Conn: conn, reader: reader}, nil
	}
	return conn, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.reader.Read(b) }

func dialSOCKS5(ctx context.Context, dialer *net.Dialer, p *Proxy, target netip.AddrPort) (net.Conn, error) {
	var auth *xproxy.Auth
	if p.Username != "" {
		auth = &xproxy.Auth{User: p.Username, Password: p.Password}
	}
	socks, err := xproxy.SOCKS5("tcp", p.Address(), auth, proxyServerDialer{dialer: dialer, proxy: p})
	if err != nil {
		return nil, err
	}
	contextDialer, ok := socks.(xproxy.ContextDialer)
	if !ok {
		return nil, errors.New("SOCKS5 dialer does not take a context")
	}
	ctx, cancel := context.WithDeadline(ctx, handshakeDeadline(ctx, dialer))
	defer cancel()
	conn, err := contextDialer.DialContext(ctx, "tcp", target.String())
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func contextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

// DialAddress connects to an address its caller already resolved and
// checked, through the proxy ctx carries, or directly when it carries none.
// It is for callers that check targets themselves, as autodiscovery does.
func DialAddress(ctx context.Context, dialer *net.Dialer, target netip.AddrPort) (net.Conn, error) {
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	if proxy := ProxyFrom(ctx); proxy != nil {
		return dialThroughProxy(ctx, dialer, proxy, target)
	}
	return dialer.DialContext(ctx, "tcp", target.String())
}
