package handler

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
		INSERT INTO users (id, email, email_normalized, name, status, auth_version, created_at, updated_at)
		VALUES ('user-id', 'user@example.com', 'user@example.com', 'User', 'active', 1, ?, ?)`, now, now); err != nil {
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
