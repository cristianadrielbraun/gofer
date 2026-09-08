package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestLogoutRevokesSessionWithTypedMetadata(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := auth.NewManager(&auth.Config{Enabled: true}, db)
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (id, username, username_normalized, name, status, auth_version, created_at, updated_at)
		VALUES ('user-id', 'user', 'user', 'User', 'active', 1, ?, ?)`, now, now); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	session, err := manager.CreateSession(t.Context(), "user-id", "test-agent")
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	handler := &Handler{auth: manager}
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: session.Token})
	req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "user-id"}))
	recorder := httptest.NewRecorder()
	handler.handleLogout(recorder, req)

	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/login" {
		t.Fatalf("logout response = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if found, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || found != nil {
		t.Fatalf("session after logout = %#v, %v, want nil", found, err)
	}
	var revokedBy, reason string
	var revokedAt time.Time
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT revoked_at, COALESCE(revoked_by, ''), revocation_reason
		FROM sessions WHERE id = ?`, session.ID).Scan(&revokedAt, &revokedBy, &reason); err != nil {
		t.Fatalf("query revoked session: %v", err)
	}
	if revokedAt.IsZero() || revokedBy != "user-id" || reason != string(auth.SessionRevocationLogout) {
		t.Fatalf("logout revocation = at:%v by:%q reason:%q", revokedAt, revokedBy, reason)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) < 1 || cookies[0].Name != "gofer_session" || cookies[0].MaxAge != -1 {
		t.Fatalf("logout cookies = %#v, want cleared session cookie", cookies)
	}
}

func TestLogoutAuditFailurePreservesSessionAndCookieForRetry(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := auth.NewManager(&auth.Config{Enabled: true}, db)
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `INSERT INTO users (id,username,username_normalized,name,status,auth_version,created_at,updated_at) VALUES ('person','person','person','Person','active',1,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	session, err := manager.CreateSession(t.Context(), "person", "browser")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `CREATE TRIGGER reject_logout_audit BEFORE INSERT ON auth_events BEGIN SELECT RAISE(ABORT,'private-storage-error'); END`); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{auth: manager}
	req := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: session.Token})
	recorder := httptest.NewRecorder()
	handler.handleLogout(recorder, req)
	if recorder.Code != http.StatusInternalServerError || len(recorder.Result().Cookies()) != 0 || recorder.Header().Get("Location") != "" {
		t.Fatalf("response=%d cookies=%v", recorder.Code, recorder.Result().Cookies())
	}
	if recorder.Body.String() != "Unable to sign out. Please try again.\n" {
		t.Fatalf("unexpected error response %q", recorder.Body.String())
	}
	if active, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || active == nil {
		t.Fatalf("session=%v err=%v", active, err)
	}
	var count int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("events=%d err=%v", count, err)
	}
}

func TestAdministratorLogoutReturnsToAdministratorLogin(t *testing.T) {
	manager, _, stack, cookie, _ := completedSecuritySettingsStack(t)
	proof := csrfProofForSession(t, manager, cookie.Value, "/auth/logout")
	request := httptest.NewRequest(http.MethodPost, "/auth/logout", strings.NewReader(url.Values{auth.CSRFFormFieldName: {proof}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/admin/login" {
		t.Fatalf("administrator logout = %d %q, body=%q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	if session, err := manager.GetSessionByToken(t.Context(), cookie.Value); err != nil || session != nil {
		t.Fatalf("administrator session after logout = %v, %v", session, err)
	}
}
