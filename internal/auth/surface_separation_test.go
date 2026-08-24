package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func insertSurfaceSession(t *testing.T, manager *Manager, id, userID, token string, now time.Time) {
	t.Helper()
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method, assurance_level,
			user_agent, authenticated_at, last_used_at, idle_expires_at, absolute_expires_at,
			step_up_method, step_up_at, created_at
		) VALUES (?, ?, ?, 1, 'password', 'multi_factor', 'Surface Browser', ?, ?, ?, ?, 'totp', ?, ?)`,
		id, userID, hashToken(token), now, now, now.Add(time.Hour), now.Add(time.Hour), now, now,
	); err != nil {
		t.Fatalf("insert surface session %q: %v", id, err)
	}
}

func newSurfaceTestManager(t *testing.T) (*Manager, time.Time) {
	t.Helper()
	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{})
	return manager, now
}

func assertSurfaceResponse(t *testing.T, manager *Manager, token, method, target string, wantStatus int, wantLocation, wantBody string) {
	t.Helper()
	called := false
	handler := manager.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(method, target, nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != wantStatus || recorder.Header().Get("Location") != wantLocation || recorder.Body.String() != wantBody {
		t.Fatalf("response = status:%d location:%q body:%q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	if called != (wantStatus == http.StatusNoContent) {
		t.Fatalf("protected handler called = %t", called)
	}
}

func TestMiddlewareSeparatesWebmailAndManagementSurfaces(t *testing.T) {
	t.Run("webmail user cannot access management routes", func(t *testing.T) {
		manager, now := newSurfaceTestManager(t)
		insertActiveUser(t, manager, "webmail-user", false, now)
		insertSurfaceSession(t, manager, "webmail-session", "webmail-user", "webmail-token", now)

		assertSurfaceResponse(t, manager, "webmail-token", http.MethodGet, "/", http.StatusNoContent, "", "")
		assertSurfaceResponse(t, manager, "webmail-token", http.MethodGet, "/admin/users", http.StatusSeeOther, "/", "<a href=\"/\">See Other</a>.\n\n")
		assertSurfaceResponse(t, manager, "webmail-token", http.MethodGet, "/api/admin/mail-operations/status", http.StatusForbidden, "", "{\"error\":\"account_surface_forbidden\"}\n")
	})

	t.Run("management administrator cannot access webmail routes", func(t *testing.T) {
		manager, now := newSurfaceTestManager(t)
		insertActiveUser(t, manager, "management-admin", true, now)
		insertSurfaceSession(t, manager, "management-session", "management-admin", "management-token", now)

		assertSurfaceResponse(t, manager, "management-token", http.MethodGet, "/admin/users", http.StatusNoContent, "", "")
		assertSurfaceResponse(t, manager, "management-token", http.MethodGet, "/", http.StatusSeeOther, "/admin", "<a href=\"/admin\">See Other</a>.\n\n")
		assertSurfaceResponse(t, manager, "management-token", http.MethodGet, "/api/folders/unread", http.StatusForbidden, "", "{\"error\":\"account_surface_forbidden\"}\n")
	})

	t.Run("non-administrator management identity cannot authenticate", func(t *testing.T) {
		manager, now := newSurfaceTestManager(t)
		if _, err := manager.db.Write().ExecContext(t.Context(), `
			DROP TRIGGER users_management_admin_insert;
			DROP TRIGGER users_management_admin_update;
		`); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.db.Write().ExecContext(t.Context(), `
			INSERT INTO users (id, username, username_normalized, name, status, user_type, is_admin, created_at, updated_at)
			VALUES ('pending-management', 'pending-management', 'pending-management', 'Pending', 'active', 'management', 0, ?, ?)`, now, now); err != nil {
			t.Fatal(err)
		}
		insertSurfaceSession(t, manager, "pending-session", "pending-management", "pending-token", now)

		assertSurfaceResponse(t, manager, "pending-token", http.MethodGet, "/admin/account/security", http.StatusSeeOther, "/admin/login", "<a href=\"/admin/login\">See Other</a>.\n\n")
		assertSurfaceResponse(t, manager, "pending-token", http.MethodGet, "/admin/users", http.StatusSeeOther, "/admin/login", "<a href=\"/admin/login\">See Other</a>.\n\n")
		assertSurfaceResponse(t, manager, "pending-token", http.MethodGet, "/", http.StatusSeeOther, "/login", "<a href=\"/login\">See Other</a>.\n\n")
	})
}
