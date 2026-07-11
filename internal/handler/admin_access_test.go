package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

func TestAdminRoutesRejectNonAdminUsers(t *testing.T) {
	h := &Handler{}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	for _, target := range []string{
		"/admin/security",
		"/api/admin/contacts/status",
		"/api/admin/labels/status",
		"/api/avatars/status",
	} {
		t.Run(target, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, target, nil)
			req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "user", IsAdmin: false}))
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
		})
	}
}

func TestAdminOnlyAllowsAdminUser(t *testing.T) {
	h := &Handler{}
	called := false
	handler := h.adminOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/admin/security", nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "admin", IsAdmin: true}))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !called || rec.Code != http.StatusNoContent {
		t.Fatalf("called = %v status = %d, want admin handler called with 204", called, rec.Code)
	}
}
