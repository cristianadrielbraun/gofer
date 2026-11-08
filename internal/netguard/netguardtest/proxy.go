// Package netguardtest has an HTTP CONNECT proxy for tests in other packages,
// to check that their connections go through the proxy they were given.
package netguardtest

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/netguard"
)

// Proxy is a running test proxy.
type Proxy struct {
	// Config is how to connect through it.
	Config *netguard.Proxy

	mu      sync.Mutex
	targets []string
}

// Targets returns the host:port of every connection clients asked it for, in
// order.
func (p *Proxy) Targets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

// StartHTTPProxy starts an HTTP CONNECT proxy on the loopback address that
// relays every connection it is asked for. It stops when the test ends.
func StartHTTPProxy(t testing.TB) *Proxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	p := &Proxy{Config: &netguard.Proxy{Type: netguard.ProxyHTTP, Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port}}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go p.serve(conn)
		}
	}()
	return p
}

func (p *Proxy) serve(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	req, err := http.ReadRequest(reader)
	if err != nil || req.Method != http.MethodConnect {
		return
	}
	p.mu.Lock()
	p.targets = append(p.targets, req.Host)
	p.mu.Unlock()
	upstream, err := net.Dial("tcp", req.Host)
	if err != nil {
		_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer upstream.Close()
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, reader); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, upstream); done <- struct{}{} }()
	<-done
}

// FollowDefaultTransport sends provider API requests (netguard.APITransport)
// the way http.DefaultClient would send them when each is made: through its
// transport when a test set one, else through http.DefaultTransport. It is
// for packages whose tests serve provider APIs by replacing either. Call it
// from TestMain.
func FollowDefaultTransport() (restore func()) {
	return netguard.UseAPITransportForTests(currentDefaultTransport{})
}

type currentDefaultTransport struct{}

func (currentDefaultTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if transport := http.DefaultClient.Transport; transport != nil {
		return transport.RoundTrip(req)
	}
	return http.DefaultTransport.RoundTrip(req)
}
