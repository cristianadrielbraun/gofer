package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
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
		"/api/admin/avatars/status",
		"/api/admin/events",
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

	req := httptest.NewRequest(http.MethodPost, "/admin/policies/default/settings/audit.security_retention_days", nil)
	req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "user", IsAdmin: false}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST a policy setting status = %d, want 403", rec.Code)
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
		{path: "/admin", location: "/admin/overview"},
		{path: "/admin/", location: "/admin/overview"},
		{path: "/admin/avatars", location: "/admin/system/diagnostics/avatars/"},
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

func TestOldAdminAddressesRedirectWithTheirQuery(t *testing.T) {
	h := &Handler{}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	for _, test := range []struct {
		path     string
		location string
	}{
		{path: "/admin/avatars", location: views.AdminAvatarsPath},
		{path: "/admin/avatars/", location: views.AdminAvatarsPath},
		{path: "/admin/avatars/senders?user_id=mail+user", location: views.AdminAvatarsPath + "senders?user_id=mail+user"},
		{path: "/admin/contacts?user_id=mail+user", location: views.AdminContactsPath + "?user_id=mail+user"},
		{path: "/admin/contacts/", location: views.AdminContactsPath},
		{path: "/admin/labels", location: views.AdminLabelsPath},
		{path: "/admin/labels/?user_id=mail+user", location: views.AdminLabelsPath + "?user_id=mail+user"},
		{path: "/admin/operations", location: views.AdminOperationsPath},
		{path: "/admin/operations/", location: views.AdminOperationsPath},
		{path: "/admin/account/security", location: auth.ManagementAccountPath},
		{path: "/admin/account/security?session_revoked=1", location: auth.ManagementAccountPath + "?session_revoked=1"},
		{path: "/admin/system", location: views.AdminAvatarsPath},
		{path: "/admin/system/", location: views.AdminAvatarsPath},
		{path: "/admin/system/diagnostics", location: views.AdminAvatarsPath},
		{path: "/admin/system/diagnostics/avatars", location: views.AdminAvatarsPath},
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

// Open mode has no console: its owner finds the diagnostics in Settings,
// and the console's addresses lead there.
func TestOpenModeConsoleMovedToSettings(t *testing.T) {
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
	do := func(method, path string, header ...string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, nil)
		for i := 0; i+1 < len(header); i += 2 {
			request.Header.Set(header[i], header[i+1])
		}
		response := httptest.NewRecorder()
		stack.ServeHTTP(response, request)
		return response
	}

	for path, want := range map[string]string{
		"/admin":                             "/settings/diagnostics/avatars/",
		"/admin/":                            "/settings/diagnostics/avatars/",
		"/admin/overview":                    "/settings/diagnostics/avatars/",
		"/admin/users":                       "/settings/diagnostics/avatars/",
		"/admin/system/diagnostics/avatars/": "/settings/diagnostics/avatars/",
		"/admin/system/diagnostics/avatars/senders": "/settings/diagnostics/avatars/senders",
		"/admin/avatars/providers":                  "/settings/diagnostics/avatars/providers",
		"/admin/system/diagnostics/contacts":        "/settings/diagnostics/contacts",
		"/admin/labels":                             "/settings/diagnostics/labels",
		"/admin/system/diagnostics/operations":      "/settings/operations",
		"/admin/security":                           "/settings/security",
		"/admin/account":                            "/settings/security",
		"/settings/diagnostics":                     "/settings/diagnostics/avatars/",
	} {
		if response := do(http.MethodGet, path); response.Code != http.StatusFound || response.Header().Get("Location") != want {
			t.Errorf("GET %s = %d %q, want %q", path, response.Code, response.Header().Get("Location"), want)
		}
	}
	for _, path := range []string{"/api/admin/avatars/status", "/api/admin/events"} {
		if response := do(http.MethodGet, path); response.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, response.Code)
		}
	}
	if response := do(http.MethodPost, "/admin/avatar-backfill/recheck"); response.Code != http.StatusNotFound {
		t.Errorf("POST to the console = %d, want 404", response.Code)
	}

	for _, path := range []string{
		"/settings/diagnostics/avatars/", "/settings/diagnostics/avatars/senders", "/settings/diagnostics/contacts", "/settings/diagnostics/labels",
	} {
		response := do(http.MethodGet, path)
		html := response.Body.String()
		if response.Code != http.StatusOK || !strings.Contains(html, "<html") || !strings.Contains(html, `data-settings-sidebar-value="diagnostics"`) ||
			strings.Contains(html, "data-admin-webmail-scope-form") || strings.Contains(html, `"/api/admin`) {
			t.Fatalf("GET %s = %d %q", path, response.Code, html)
		}
	}
	if response := do(http.MethodGet, "/settings/diagnostics/avatars/nowhere"); response.Code != http.StatusNotFound {
		t.Errorf("unknown avatar tab = %d", response.Code)
	}
	partial := do(http.MethodGet, "/settings/diagnostics/labels", "HX-Request", "true")
	if partial.Code != http.StatusOK || strings.Contains(partial.Body.String(), "<html") || !strings.Contains(partial.Body.String(), `id="settings-content"`) {
		t.Fatalf("labels in place = %d %q", partial.Code, partial.Body.String())
	}
	for _, path := range []string{"/api/diagnostics/avatars/status", "/api/diagnostics/contacts/status", "/api/diagnostics/labels/status"} {
		response := do(http.MethodGet, path)
		if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
			t.Errorf("GET %s = %d %q", path, response.Code, response.Header().Get("Content-Type"))
		}
	}
	settings := do(http.MethodGet, "/settings/accounts").Body.String()
	if !strings.Contains(settings, `href="/settings/diagnostics/avatars/"`) {
		t.Fatal("Settings doesn't offer the diagnostics")
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
	contacts, err := h.contactAdminStatus(adminCtx, models.AdminWebmailScope{})
	if err != nil {
		t.Fatal(err)
	}
	if contacts.Total != 1 || len(contacts.AccountSync) != 1 || contacts.AccountSync[0].OwnerUsername != "mail-user" {
		t.Fatalf("admin contact status = %#v", contacts)
	}
	labels, err := h.labelAdminStatus(adminCtx, models.AdminWebmailScope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(labels.Accounts) != 1 || labels.Accounts[0].OwnerUsername != "mail-user" {
		t.Fatalf("admin label status = %#v", labels)
	}

	scope, err := h.adminWebmailScope(adminCtx, "mail-user")
	if err != nil {
		t.Fatal(err)
	}
	if scope.SelectedUsername != "mail-user" || len(scope.Users) != 1 {
		t.Fatalf("selected webmail scope = %#v", scope)
	}
	contacts, err = h.contactAdminStatus(adminCtx, scope)
	if err != nil || contacts.Scope.SelectedUserID != "mail-user" || contacts.Total != 1 {
		t.Fatalf("scoped admin contact status = %#v, %v", contacts, err)
	}
	labels, err = h.labelAdminStatus(adminCtx, scope)
	if err != nil || labels.Scope.SelectedUserID != "mail-user" || len(labels.Accounts) != 1 {
		t.Fatalf("scoped admin label status = %#v, %v", labels, err)
	}
	if _, err := h.adminWebmailScope(adminCtx, "admin-user"); !errors.Is(err, errAdminWebmailScopeNotFound) {
		t.Fatalf("management identity scope error = %v, want not found", err)
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
