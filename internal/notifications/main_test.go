package notifications

import (
	"os"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/handler"
	"github.com/cristianadrielbraun/gofer/internal/netguard/netguardtest"
)

// Tests here serve the Google and Microsoft APIs from local servers by
// replacing http.DefaultTransport, so provider API requests follow it. Proxy
// routing is tested in netguard and mailauth.
func TestMain(m *testing.M) {
	netguardtest.FollowDefaultTransport()
	handler.RequireDAVProxyChoiceForTests()
	os.Exit(m.Run())
}
