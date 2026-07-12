package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

func TestPrivateTargetExceptionRouteRejectsNonAdminUsers(t *testing.T) {
	h := &Handler{}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/admin/security/private-target", nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "user", IsAdmin: false}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestAdminCanAddPrivateTargetException(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	form := url.Values{"protocol": {"http"}, "host": {"127.0.0.1"}, "port": {"8080"}, "acknowledge": {"yes"}}
	req := httptest.NewRequest(http.MethodPost, "/admin/security/private-target", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "admin", IsAdmin: true}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d body = %q, want redirect", rec.Code, rec.Body.String())
	}
	allowed, err := db.IsPrivateTargetAllowed(t.Context(), "http", "127.0.0.1", 8080)
	if err != nil || !allowed {
		t.Fatalf("private target after admin action = %v, %v; want true", allowed, err)
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
