package handler

import (
	"context"
	"os"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/netguard"
	"github.com/cristianadrielbraun/gofer/internal/netguard/netguardtest"
)

// Tests here serve the Google and Microsoft APIs from local servers by
// replacing http.DefaultTransport, so provider API requests follow it. Proxy
// routing is tested in netguard and mailauth.
func TestMain(m *testing.M) {
	netguardtest.FollowDefaultTransport()
	RequireDAVProxyChoiceForTests()
	os.Exit(m.Run())
}

// davTestContext is a test's context with a direct connection chosen, for
// tests that make CalDAV and CardDAV requests without an account's sign-in.
func davTestContext(t testing.TB) context.Context {
	return netguard.WithProxy(t.Context(), nil)
}

func netguardBindForTest(username, password string, proxy *netguardtest.Proxy) {
	var config *netguard.Proxy
	if proxy != nil {
		config = proxy.Config
	}
	netguard.BindBasicProxy(username, password, config)
}
