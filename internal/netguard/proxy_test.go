package netguard

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// echoServer answers every line it reads with "echo: " and the line.
func echoServer(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					_, _ = io.WriteString(conn, "echo: "+line)
				}
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

// testProxy records what clients asked it for. With username set it demands
// those credentials.
type testProxy struct {
	kind     string
	username string
	password string
	port     int

	mu       sync.Mutex
	targets  []string // what each client asked to connect to
	accepted int
}

func (p *testProxy) record(target string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.targets = append(p.targets, target)
}

func (p *testProxy) seen() (int, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accepted, append([]string(nil), p.targets...)
}

func (p *testProxy) config(username, password string) *Proxy {
	return &Proxy{Type: p.kind, Host: "127.0.0.1", Port: p.port, Username: username, Password: password}
}

func startTestProxy(t *testing.T, kind, username, password string) *testProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	p := &testProxy{kind: kind, username: username, password: password, port: listener.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			p.accepted++
			p.mu.Unlock()
			if kind == ProxyHTTP {
				go p.serveHTTP(conn)
			} else {
				go p.serveSOCKS5(conn)
			}
		}
	}()
	return p
}

func (p *testProxy) serveHTTP(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	req, err := http.ReadRequest(reader)
	if err != nil || req.Method != http.MethodConnect {
		return
	}
	p.record(req.Host)
	if p.username != "" {
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte(p.username+":"+p.password))
		if req.Header.Get("Proxy-Authorization") != want {
			_, _ = io.WriteString(conn, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
			return
		}
	}
	upstream, err := net.Dial("tcp", req.Host)
	if err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer upstream.Close()
	_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
	relay(&bufferedConn{Conn: conn, reader: reader}, upstream)
}

func (p *testProxy) serveSOCKS5(conn net.Conn) {
	defer conn.Close()
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil || header[0] != 5 {
		return
	}
	methods := make([]byte, header[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	want := byte(0x00)
	if p.username != "" {
		want = 0x02
	}
	if !strings.Contains(string(methods), string([]byte{want})) {
		_, _ = conn.Write([]byte{5, 0xff})
		return
	}
	_, _ = conn.Write([]byte{5, want})
	if want == 0x02 {
		readField := func() string {
			length := make([]byte, 1)
			if _, err := io.ReadFull(conn, length); err != nil {
				return ""
			}
			field := make([]byte, length[0])
			_, _ = io.ReadFull(conn, field)
			return string(field)
		}
		version := make([]byte, 1)
		if _, err := io.ReadFull(conn, version); err != nil {
			return
		}
		username, password := readField(), readField()
		if username != p.username || password != p.password {
			_, _ = conn.Write([]byte{1, 1})
			return
		}
		_, _ = conn.Write([]byte{1, 0})
	}
	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil || request[1] != 1 {
		return
	}
	var host string
	switch request[3] {
	case 1:
		ip := make([]byte, 4)
		_, _ = io.ReadFull(conn, ip)
		host = net.IP(ip).String()
	case 4:
		ip := make([]byte, 16)
		_, _ = io.ReadFull(conn, ip)
		host = net.IP(ip).String()
	case 3:
		length := make([]byte, 1)
		_, _ = io.ReadFull(conn, length)
		name := make([]byte, length[0])
		_, _ = io.ReadFull(conn, name)
		host = string(name)
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portBytes))))
	p.record(target)
	upstream, err := net.Dial("tcp", target)
	if err != nil {
		_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
	relay(conn, upstream)
}

func relay(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}

func roundTripLine(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(conn, "hello\n"); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "echo: hello\n" {
		t.Fatalf("read %q through the proxy", line)
	}
}

// Both proxy types are asked for the address Gofer resolved and checked, never
// the host name, and carry the connection once the credentials match.
func TestDialThroughProxyAsksForTheCheckedAddress(t *testing.T) {
	for _, kind := range []string{ProxyHTTP, ProxySOCKS5} {
		t.Run(kind, func(t *testing.T) {
			target := echoServer(t)
			proxy := startTestProxy(t, kind, "ana", "s3cret pass")
			resolver := &fakeResolver{answers: map[string][]netip.Addr{"mail.internal.test": {netip.MustParseAddr("127.0.0.1")}}}
			dialer := &Dialer{Dialer: &net.Dialer{Timeout: 2 * time.Second}, Resolver: resolver}

			ctx := WithProxy(t.Context(), proxy.config("ana", "s3cret pass"))
			conn, err := dialer.Dial(ctx, "imap", "mail.internal.test", target, true)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			roundTripLine(t, conn)

			if _, targets := proxy.seen(); len(targets) != 1 || targets[0] != net.JoinHostPort("127.0.0.1", strconv.Itoa(target)) {
				t.Fatalf("proxy was asked for %v", targets)
			}
		})
	}
}

func TestDialThroughProxyReportsRefusedCredentials(t *testing.T) {
	for _, kind := range []string{ProxyHTTP, ProxySOCKS5} {
		t.Run(kind, func(t *testing.T) {
			target := echoServer(t)
			proxy := startTestProxy(t, kind, "ana", "right")
			ctx := WithProxy(t.Context(), proxy.config("ana", "wrong"))
			_, err := Dial(ctx, &net.Dialer{Timeout: 2 * time.Second}, "smtp", "127.0.0.1", target, true)
			var proxyErr *ProxyError
			if !errors.As(err, &proxyErr) {
				t.Fatalf("error = %v, want a proxy error", err)
			}
			if strings.Contains(err.Error(), "wrong") {
				t.Fatalf("error %q shows the password", err)
			}
		})
	}
}

// A proxy is no way around the private network check: the target is refused
// before the proxy is contacted at all.
func TestDialThroughProxyKeepsThePrivateCheck(t *testing.T) {
	target := echoServer(t)
	proxy := startTestProxy(t, ProxySOCKS5, "", "")
	ctx := WithProxy(t.Context(), proxy.config("", ""))
	if _, err := Dial(ctx, &net.Dialer{Timeout: time.Second}, "imap", "127.0.0.1", target, false); !IsBlocked(err) {
		t.Fatalf("error = %v, want a private target refusal", err)
	}
	if accepted, _ := proxy.seen(); accepted != 0 {
		t.Fatal("the proxy was contacted for a refused target")
	}
}

func TestWithProxyNilMeansDirect(t *testing.T) {
	target := echoServer(t)
	proxy := startTestProxy(t, ProxyHTTP, "", "")
	ctx := WithProxy(WithProxy(t.Context(), proxy.config("", "")), nil)
	conn, err := Dial(ctx, &net.Dialer{Timeout: time.Second}, "imap", "127.0.0.1", target, true)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	roundTripLine(t, conn)
	if accepted, _ := proxy.seen(); accepted != 0 {
		t.Fatal("a direct connection went through the proxy")
	}
}

// Each proxy has its own connection pool: a kept-alive connection made through
// a proxy is not reused for a direct request, nor the other way round.
func TestHTTPTransportKeepsConnectionsPerProxy(t *testing.T) {
	server, _, port := localServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	allowAll := func(_ context.Context, _, _ string, _ int) bool { return true }
	client := &http.Client{Transport: NewHTTPTransport(allowAll)}
	proxy := startTestProxy(t, ProxyHTTP, "", "")
	through := proxy.config("", "")

	get := func(ctx context.Context) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	get(WithProxy(t.Context(), through))
	get(t.Context())
	get(WithProxy(t.Context(), through))
	if accepted, targets := proxy.seen(); accepted != 1 || len(targets) != 1 || targets[0] != "127.0.0.1:"+strconv.Itoa(port) {
		t.Fatalf("proxy accepted %d connections for %v, want one reused connection", accepted, targets)
	}

	// Other credentials for the same proxy are another pool.
	get(WithProxy(t.Context(), proxy.config("someone", "else")))
	if accepted, _ := proxy.seen(); accepted != 2 {
		t.Fatalf("proxy accepted %d connections, want a new one for other credentials", accepted)
	}
}

func TestProxyValidate(t *testing.T) {
	valid := []Proxy{
		{Type: ProxyHTTP, Host: "proxy.example.test", Port: 3128},
		{Type: ProxySOCKS5, Host: "10.0.0.2", Port: 1080, Username: "ana", Password: "pw"},
		{Type: ProxySOCKS5, Host: "::1", Port: 1080},
		{Type: ProxyHTTP, Host: "[2001:db8::1]", Port: 8080, Username: "ana"},
	}
	for _, p := range valid {
		if err := p.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v", p, err)
		}
	}
	invalid := []Proxy{
		{Type: "https", Host: "proxy.example.test", Port: 3128},
		{Type: "socks4", Host: "proxy.example.test", Port: 1080},
		{Type: ProxyHTTP, Host: "", Port: 3128},
		{Type: ProxyHTTP, Host: " proxy.example.test", Port: 3128},
		{Type: ProxyHTTP, Host: "http://proxy.example.test", Port: 3128},
		{Type: ProxyHTTP, Host: "user@proxy.example.test", Port: 3128},
		{Type: ProxyHTTP, Host: "fe80::1%eth0", Port: 3128},
		{Type: ProxyHTTP, Host: "proxy.example.test", Port: 0},
		{Type: ProxyHTTP, Host: "proxy.example.test", Port: 65536},
		{Type: ProxyHTTP, Host: "proxy.example.test", Port: 3128, Password: "pw"},
		{Type: ProxyHTTP, Host: "proxy.example.test", Port: 3128, Username: "a:b"},
		{Type: ProxyHTTP, Host: "proxy.example.test", Port: 3128, Username: "ana\r\nX-Evil: 1"},
		{Type: ProxySOCKS5, Host: "proxy.example.test", Port: 1080, Username: strings.Repeat("a", 256)},
	}
	for _, p := range invalid {
		if err := p.Validate(); err == nil {
			t.Errorf("Validate(%+v) accepted an invalid proxy", p)
		}
	}
}

func TestProxyStringHidesThePassword(t *testing.T) {
	p := &Proxy{Type: ProxySOCKS5, Host: "proxy.example.test", Port: 1080, Username: "ana", Password: "s3cret"}
	if got := p.String(); got != "socks5://proxy.example.test:1080" {
		t.Fatalf("String() = %q", got)
	}
	if (*Proxy)(nil).String() != "direct" {
		t.Fatal("a nil proxy should read as direct")
	}
	other := *p
	other.Password = "changed"
	if p.Key() == other.Key() || strings.Contains(p.Key(), "s3cret") {
		t.Fatalf("Key() = %q, want it to change with the password without showing it", p.Key())
	}
}

// A proxy someone saved for their own accounts is refused on a private
// address, so it can't lead into the Gofer server's network; an
// administrator's proxy may be there.
func TestPublicOnlyProxyRefusesPrivateAddresses(t *testing.T) {
	for _, kind := range []string{ProxyHTTP, ProxySOCKS5} {
		t.Run(kind, func(t *testing.T) {
			target := echoServer(t)
			proxy := startTestProxy(t, kind, "", "")
			own := proxy.config("", "")
			own.PublicOnly = true
			_, err := Dial(WithProxy(t.Context(), own), &net.Dialer{Timeout: time.Second}, "imap", "127.0.0.1", target, true)
			if !IsBlocked(err) || !strings.Contains(err.Error(), "public address") {
				t.Fatalf("error = %v, want the private proxy refused", err)
			}
			if accepted, _ := proxy.seen(); accepted != 0 {
				t.Fatal("a private proxy was contacted")
			}
			conn, err := Dial(WithProxy(t.Context(), proxy.config("", "")), &net.Dialer{Timeout: time.Second}, "imap", "127.0.0.1", target, true)
			if err != nil {
				t.Fatalf("administrator's local proxy: %v", err)
			}
			conn.Close()
			if own.Key() == proxy.config("", "").Key() {
				t.Fatal("a public-only proxy shares connections with the same proxy unchecked")
			}
		})
	}
}
