package autodiscover

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/netguard"
	"github.com/cristianadrielbraun/gofer/internal/netguard/netguardtest"
)

// Discovery for an account being added connects through the proxy it will
// use, asking for the address it checked.
func TestDiscoveryDialsThroughTheAccountProxy(t *testing.T) {
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
			conn.Close()
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	proxy := netguardtest.StartHTTPProxy(t)
	ctx := netguard.WithProxy(t.Context(), proxy.Config)
	conn, err := dialResolvedTarget(ctx, "mail.example.test", port, time.Second, []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}})
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if got := proxy.Targets(); len(got) != 1 || got[0] != "127.0.0.1:"+strconv.Itoa(port) {
		t.Fatalf("proxy was asked for %v", got)
	}
	if _, err := dialResolvedTarget(ctx, "mail.example.test", port, time.Second, nil); err == nil {
		t.Fatal("a name with no checked address was handed to the proxy")
	}
}
