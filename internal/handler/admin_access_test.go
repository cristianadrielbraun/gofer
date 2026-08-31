package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestAdminRoutesRejectNonAdminUsers(t *testing.T) {
	h := &Handler{}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	for _, target := range []string{
		"/admin/users",
		"/admin/activity",
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

func TestAdminRootRedirectsToFirstSidebarSection(t *testing.T) {
	h := &Handler{}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	for _, test := range []struct {
		path     string
		location string
	}{
		{path: "/admin", location: "/admin/users"},
		{path: "/admin/", location: "/admin/users"},
		{path: "/admin/avatars", location: "/admin/avatars/"},
	} {
		t.Run(test.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, test.path, nil)
			req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{
				ID: "admin", UserType: auth.UserTypeManagement, IsAdmin: true,
			}))
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusFound || rec.Header().Get("Location") != test.location {
				t.Fatalf("redirect = %d %q, want 302 %q", rec.Code, rec.Header().Get("Location"), test.location)
			}
		})
	}
}

func TestLocalModeAdminUsesOperationalHomeAndNavigation(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := auth.NewManager(&auth.Config{Enabled: false}, db)
	h := &Handler{db: db, auth: manager}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	stack := manager.Middleware(mux)

	request := httptest.NewRequest(http.MethodGet, "/admin", nil)
	response := httptest.NewRecorder()
	stack.ServeHTTP(response, request)
	if response.Code != http.StatusFound || response.Header().Get("Location") != "/admin/avatars/" {
		t.Fatalf("local admin root = %d %q", response.Code, response.Header().Get("Location"))
	}

	var avatarHTML string
	for _, path := range []string{
		"/admin/avatars/", "/admin/contacts", "/admin/labels", "/admin/operations", "/admin/security",
	} {
		request = httptest.NewRequest(http.MethodGet, path, nil)
		response = httptest.NewRecorder()
		stack.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("local admin section %q = %d %q", path, response.Code, response.Body.String())
		}
		if path == "/admin/avatars/" {
			avatarHTML = response.Body.String()
		}
	}
	for _, want := range []string{
		`href="/admin/avatars/"`, `href="/admin/contacts"`, `href="/admin/labels"`,
		`href="/admin/operations"`, `href="/admin/security"`, `href="/"`, "Back to webmail",
	} {
		if !strings.Contains(avatarHTML, want) {
			t.Fatalf("local admin navigation omitted %q", want)
		}
	}
	for _, forbidden := range []string{
		`href="/admin/users"`, `href="/admin/activity"`, `href="/admin/account/security"`,
		`action="/auth/logout"`, "Sign out of Admin",
	} {
		if strings.Contains(avatarHTML, forbidden) {
			t.Fatalf("local admin navigation exposed managed-only control %q", forbidden)
		}
	}

	for _, path := range []string{"/admin/users", "/admin/account/security"} {
		request = httptest.NewRequest(http.MethodGet, path, nil)
		response = httptest.NewRecorder()
		stack.ServeHTTP(response, request)
		if response.Code != http.StatusFound || response.Header().Get("Location") != "/admin/avatars/" {
			t.Fatalf("local managed-only section %q = %d %q", path, response.Code, response.Header().Get("Location"))
		}
	}
}

func TestAdminContactAndLabelStatusAggregateWebmailUsers(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users (id, username, username_normalized, name, user_type, is_admin)
		VALUES ('admin-user', 'admin', 'admin', 'Admin', 'management', 1),
		       ('mail-user', 'mail-user', 'mail-user', 'Mail User', 'webmail', 0);
		INSERT INTO contact_profiles (id, user_id, display_name, origin)
		VALUES ('mail-contact', 'mail-user', 'Mail Contact', 'manual');
		INSERT INTO accounts (id, user_id, provider, email_address, display_name)
		VALUES ('mail-account', 'mail-user', 'gmail', 'mail@example.com', 'Mail Account')`); err != nil {
		t.Fatal(err)
	}

	h := &Handler{db: db}
	adminCtx := auth.ContextWithUser(ctx, &auth.User{
		ID: "admin-user", UserType: auth.UserTypeManagement, IsAdmin: true,
	})
	contacts, err := h.contactAdminStatus(adminCtx)
	if err != nil {
		t.Fatal(err)
	}
	if contacts.Total != 1 || len(contacts.AccountSync) != 1 || contacts.AccountSync[0].OwnerUsername != "mail-user" {
		t.Fatalf("admin contact status = %#v", contacts)
	}
	labels, err := h.labelAdminStatus(adminCtx)
	if err != nil {
		t.Fatal(err)
	}
	if len(labels.Accounts) != 1 || labels.Accounts[0].OwnerUsername != "mail-user" {
		t.Fatalf("admin label status = %#v", labels)
	}
}

func TestPrivateTargetExceptionRouteRejectsNonAdminUsers(t *testing.T) {
	h := &Handler{}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	for _, target := range []string{
		"/admin/security/private-target",
		"/admin/users/invitations",
		"/admin/users/webmail-user/credential-reset",
	} {
		req := httptest.NewRequest(http.MethodPost, target, nil)
		req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "user", IsAdmin: false}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d, want 403", target, rec.Code)
		}
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
