package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/mail"
)

// Personal and managed mode serve signed-in users from the per-user routes,
// so the About page's routes have to be mounted there too.
func TestUserStorageRoutesServeAboutPage(t *testing.T) {
	assertUserStorageRoutes(t, []struct{ method, path string }{
		{http.MethodPost, "/api/settings/about/update-check"},
		{http.MethodGet, "/settings/about/licenses"},
	})
}

// assertUserStorageRoutes checks that each route is mounted on the per-user
// routes under its own pattern.
func assertUserStorageRoutes(t *testing.T, routes []struct{ method, path string }) {
	t.Helper()
	f := verifiedOwnedStatusFixture(t)
	root, cancel := context.WithCancel(t.Context())
	syncer := mail.NewSyncOrchestrator(f.system, nil, nil, nil)
	imap, err := mail.NewUserIMAP(root, f.h.userAccounts, f.h.blobStore, syncer.Events())
	if err != nil {
		t.Fatal(err)
	}
	h := New(f.system, nil, syncer, f.h.blobStore, f.manager, "")
	t.Cleanup(func() { cancel(); imap.Wait(); h.WaitAvatarWorkers() })
	mux := http.NewServeMux()
	if err := h.RegisterUserStorageRoutes(root, mux, f.h.userStorage, UserStorageOptions{Accounts: f.h.userAccounts, IMAP: imap}); err != nil {
		t.Fatal(err)
	}
	for _, route := range routes {
		req := httptest.NewRequest(route.method, route.path, nil)
		if _, pattern := mux.Handler(req); pattern != route.method+" "+route.path {
			t.Errorf("%s %s is not routed for users; matched %q", route.method, route.path, pattern)
		}
	}
}
