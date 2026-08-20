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
	return NewManager(&Config{Enabled: true, BaseURL: "https://gofer.example"}, db)
}

func TestUserLifecycleUsesNormalizedIdentifiersAndRevokesSessions(t *testing.T) {
	manager := newUserLifecycleManager(t)
	admin, err := manager.CreateOrUpdateUser(t.Context(), "Admin", "Admin", "")
	if err != nil {
		t.Fatalf("CreateOrUpdateUser(admin) error = %v", err)
	}
	user, err := manager.CreateOrUpdateUser(t.Context(), " Person.User ", "Person", "")
	if err != nil {
		t.Fatalf("CreateOrUpdateUser(person) error = %v", err)
	}
	found, err := manager.GetUserByUsername(t.Context(), "person.user")
	if err != nil || found == nil || found.ID != user.ID || found.UsernameNormalized != "person.user" {
		t.Fatalf("normalized lookup = %#v, %v", found, err)
	}
	session, err := manager.CreateAuthenticatedSession(
		t.Context(), user.ID, "test-agent", AuthenticationMethodPassword, AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatalf("CreateAuthenticatedSession() error = %v", err)
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
	admin, err := manager.CreateOrUpdateUser(t.Context(), "admin", "Admin", "")
	if err != nil {
		t.Fatalf("CreateOrUpdateUser() error = %v", err)
	}
	if err := manager.SetUserStatus(t.Context(), admin.ID, UserStatusDisabled, admin.ID); !errors.Is(err, ErrLastActiveAdmin) {
		t.Fatalf("SetUserStatus(last admin) error = %v, want ErrLastActiveAdmin", err)
	}
}

func TestUserLifecycleRequiresStrongFactorBeforeActivationUnderGlobalMFA(t *testing.T) {
	manager := newUserLifecycleManager(t)
	admin, err := manager.CreateOrUpdateUser(t.Context(), "admin", "Admin", "")
	if err != nil {
		t.Fatal(err)
	}
	user, err := manager.CreateOrUpdateUser(t.Context(), "user", "User", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetUserStatus(t.Context(), user.ID, UserStatusPending, admin.ID); err != nil {
		t.Fatalf("SetUserStatus(pending) error = %v", err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (id, initialized, owner_user_id, mfa_policy)
		VALUES (1, 1, ?, 'all_users')`, admin.ID); err != nil {
		t.Fatal(err)
	}

	if err := manager.SetUserStatus(t.Context(), user.ID, UserStatusActive, admin.ID); !errors.Is(err, ErrInstanceMFAEnrollmentNeeded) {
		t.Fatalf("SetUserStatus(factorless active) error = %v", err)
	}
	blocked, err := manager.GetUserByID(t.Context(), user.ID)
	if err != nil || blocked.Status != UserStatusPending {
		t.Fatalf("blocked user = %#v, %v", blocked, err)
	}

	insertPolicyTestTOTP(t, manager, user.ID, blocked.UpdatedAt)
	if err := manager.SetUserStatus(t.Context(), user.ID, UserStatusActive, admin.ID); err != nil {
		t.Fatalf("SetUserStatus(factor-ready active) error = %v", err)
	}
	active, err := manager.GetUserByID(t.Context(), user.ID)
	if err != nil || active.Status != UserStatusActive {
		t.Fatalf("activated user = %#v, %v", active, err)
	}
}

func TestMiddlewareRejectsDisabledUserWithExistingSession(t *testing.T) {
	manager := newUserLifecycleManager(t)
	user, err := manager.CreateOrUpdateUser(t.Context(), "user", "User", "")
	if err != nil {
		t.Fatalf("CreateOrUpdateUser() error = %v", err)
	}
	session, err := manager.CreateAuthenticatedSession(t.Context(), user.ID, "test-agent", AuthenticationMethodPassword, AssuranceLevelMultiFactor)
	if err != nil {
		t.Fatalf("CreateAuthenticatedSession() error = %v", err)
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
