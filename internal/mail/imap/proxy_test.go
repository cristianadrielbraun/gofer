package imap

import (
	"net"
	"slices"
	"strconv"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/netguard/netguardtest"
)

// An account with a proxy reaches its IMAP server through it, and one
// without connects directly.
func TestConnectionGoesThroughTheAccountProxy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = conn.Write([]byte("* BYE not today\r\n"))
			_ = conn.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	proxy := netguardtest.StartHTTPProxy(t)
	cfg := &models.AccountConfig{IMAPHost: "127.0.0.1", IMAPPort: port, IMAPTLSMode: "plaintext", IMAPAllowPlaintext: true,
		IMAPAllowPrivate: true, AuthMethod: "plain", Username: "user", Proxy: proxy.Config}

	_ = TestConnection(t.Context(), cfg, "secret")
	want := []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	if got := proxy.Targets(); !slices.Equal(got, want) {
		t.Fatalf("proxy was asked for %v, want %v", got, want)
	}

	cfg.Proxy = nil
	_ = TestConnection(t.Context(), cfg, "secret")
	if got := proxy.Targets(); len(got) != 1 {
		t.Fatalf("a direct connection went through the proxy: %v", got)
	}
}

// A session opened through one proxy, or directly, is not handed out for an
// account that now has to use another.
func TestSessionKeyChangesWithTheProxy(t *testing.T) {
	cfg := &models.AccountConfig{AccountID: "acc", IMAPHost: "imap.example.test", IMAPPort: 993, Username: "user"}
	direct := sessionKey(cfg, "secret")
	proxy := netguardtest.StartHTTPProxy(t)
	cfg.Proxy = proxy.Config
	through := sessionKey(cfg, "secret")
	if direct == through {
		t.Fatal("a direct session and one through a proxy share a key")
	}
	other := *proxy.Config
	other.Port++
	cfg.Proxy = &other
	if sessionKey(cfg, "secret") == through {
		t.Fatal("sessions through two proxies share a key")
	}
}
