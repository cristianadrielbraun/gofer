package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newUserLifecycleManager(t *testing.T) *Manager {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewManager(&Config{Enabled: true}, db)
}

func TestUserLifecycleUsesNormalizedIdentifiersAndRevokesSessions(t *testing.T) {
	manager := newUserLifecycleManager(t)
	admin, err := manager.CreateOrUpdateUser(t.Context(), "Admin@example.com", "Admin", "")
	if err != nil {
		t.Fatalf("CreateOrUpdateUser(admin) error = %v", err)
	}
	user, err := manager.CreateOrUpdateUser(t.Context(), " Person@Example.COM ", "Person", "")
	if err != nil {
		t.Fatalf("CreateOrUpdateUser(person) error = %v", err)
	}
	found, err := manager.GetUserByLoginIdentifier(t.Context(), "person@example.com")
	if err != nil || found == nil || found.ID != user.ID || found.EmailNormalized != "person@example.com" {
		t.Fatalf("normalized lookup = %#v, %v", found, err)
	}
	session, err := manager.CreateSession(t.Context(), user.ID, "test-agent")
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if err := manager.SetUserStatus(t.Context(), user.ID, UserStatusDisabled, admin.ID); err != nil {
		t.Fatalf("SetUserStatus(disabled) error = %v", err)
	}
	disabled, err := manager.GetUserByID(t.Context(), user.ID)
	if err != nil || disabled.Status != UserStatusDisabled || disabled.AuthVersion != 2 || disabled.DisabledAt == nil || disabled.DisabledBy != admin.ID {
		t.Fatalf("disabled user = %#v, %v", disabled, err)
	}
	if got, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || got != nil {
		t.Fatalf("revoked session = %#v, %v", got, err)
	}
	sessions, err := manager.ListSessions(t.Context(), user.ID)
	if err != nil || len(sessions) != 1 || sessions[0].RevokedAt == nil || sessions[0].RevocationReason != SessionRevocationUserDisabled || sessions[0].RevokedBy != admin.ID {
		t.Fatalf("disabled user sessions = %#v, %v", sessions, err)
	}
	if session, err := manager.CreateSession(t.Context(), user.ID, "disabled-agent"); !errors.Is(err, ErrUserNotActive) || session != nil {
		t.Fatalf("CreateSession(disabled) = %#v, %v, want ErrUserNotActive", session, err)
	}
}

func TestUserLifecycleProtectsLastActiveAdministrator(t *testing.T) {
	manager := newUserLifecycleManager(t)
	admin, err := manager.CreateOrUpdateUser(t.Context(), "admin@example.com", "Admin", "")
	if err != nil {
		t.Fatalf("CreateOrUpdateUser() error = %v", err)
	}
	if err := manager.SetUserStatus(t.Context(), admin.ID, UserStatusDisabled, admin.ID); !errors.Is(err, ErrLastActiveAdmin) {
		t.Fatalf("SetUserStatus(last admin) error = %v, want ErrLastActiveAdmin", err)
	}
}

func TestMiddlewareRejectsDisabledUserWithExistingSession(t *testing.T) {
	manager := newUserLifecycleManager(t)
	user, err := manager.CreateOrUpdateUser(t.Context(), "user@example.com", "User", "")
	if err != nil {
		t.Fatalf("CreateOrUpdateUser() error = %v", err)
	}
	session, err := manager.CreateSession(t.Context(), user.ID, "test-agent")
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE users SET status = 'disabled', disabled_at = CURRENT_TIMESTAMP WHERE id = ?`, user.ID); err != nil {
		t.Fatalf("disable user directly: %v", err)
	}

	called := false
	handler := manager.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session.Token})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if called || rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("disabled request called=%t status=%d location=%q", called, rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != sessionCookieName || cookies[0].MaxAge != -1 {
		t.Fatalf("disabled response cookies = %#v, want cleared session", cookies)
	}
}
