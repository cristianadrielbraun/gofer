package auth

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestAccountOAuthCallbacksRequireAuthentication(t *testing.T) {
	for _, path := range []string{"/auth/google/account/callback", "/auth/microsoft/account/callback"} {
		if isPublicPath(path) {
			t.Fatalf("account callback %q is still public", path)
		}
	}
	if !isPublicPath("/auth/google/callback") {
		t.Fatal("login callback must remain public")
	}
}

func newAccountOAuthFlowTestManager(t *testing.T, enabled bool) (*Manager, *storage.DB) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewManager(&Config{Enabled: enabled}, db), db
}

func TestSingleUserMiddlewareStillProvidesDefaultUserToAccountCallback(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := NewManager(&Config{Enabled: false}, db)
	if err := manager.EnsureDefaultUser(); err != nil {
		t.Fatalf("EnsureDefaultUser() error = %v", err)
	}
	called := false
	handler := manager.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		user := GetCurrentUser(r.Context())
		if user == nil || user.ID != "default" {
			t.Fatalf("callback user = %#v, want default", user)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/auth/google/account/callback", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !called || rec.Code != http.StatusNoContent {
		t.Fatalf("called = %v status = %d, want callback reached with 204", called, rec.Code)
	}
}
