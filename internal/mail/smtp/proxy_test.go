package smtp

import (
	"net"
	"slices"
	"strconv"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/netguard/netguardtest"
)

// An account with a proxy reaches its SMTP server through it, and one
// without connects directly.
func TestNewClientGoesThroughTheAccountProxy(t *testing.T) {
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
			_, _ = conn.Write([]byte("554 not today\r\n"))
			_ = conn.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	proxy := netguardtest.StartHTTPProxy(t)
	cfg := &models.AccountConfig{SMTPHost: "127.0.0.1", SMTPPort: port, SMTPTLSMode: "plaintext", SMTPAllowPlaintext: true,
		SMTPAllowPrivate: true, AuthMethod: "plain", Username: "user", Proxy: proxy.Config}

	if client, err := NewClient(t.Context(), cfg, "secret"); err == nil {
		_ = client.Close()
	}
	want := []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	if got := proxy.Targets(); len(got) == 0 || !slices.Equal(got[:1], want) {
		t.Fatalf("proxy was asked for %v, want %v", got, want)
	}
	before := len(proxy.Targets())

	cfg.Proxy = nil
	if client, err := NewClient(t.Context(), cfg, "secret"); err == nil {
		_ = client.Close()
	}
	if got := proxy.Targets(); len(got) != before {
		t.Fatalf("a direct connection went through the proxy: %v", got)
	}
}
