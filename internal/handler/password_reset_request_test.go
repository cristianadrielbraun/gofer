package handler

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/httpguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func passwordResetRequestStack(t *testing.T) (*storage.DB, http.Handler) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := auth.NewManager(&auth.Config{
		Enabled: true, BaseURL: "https://gofer.example", SecureCookies: true,
	}, db, auth.Dependencies{BucketHashKey: []byte("password-reset-handler-key-32byt")})
	now := time.Now().UTC().Add(-time.Hour)
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, username, username_normalized, name, status, auth_version,
			user_type, is_admin, created_at, updated_at
		) VALUES
			('active-user', 'active.user', 'active.user', 'Active', 'active', 1, 'webmail', 0, ?, ?),
			('disabled-user', 'disabled.user', 'disabled.user', 'Disabled', 'disabled', 1, 'webmail', 0, ?, ?),
			('pending-user', 'pending.user', 'pending.user', 'Pending', 'pending', 1, 'webmail', 0, ?, ?),
			('management-user', 'management.user', 'management.user', 'Management', 'active', 1, 'management', 1, ?, ?)`,
		now, now, now, now, now, now, now, now,
	); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db, auth: manager}
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	return db, manager.Middleware(mux)
}

func postPasswordResetRequest(stack http.Handler, identifier string) *httptest.ResponseRecorder {
	form := url.Values{"identifier": {identifier}}
	request := httptest.NewRequest(http.MethodPost, passwordResetRequestPath, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	request.Header.Set("User-Agent", "Password Reset Handler Test/1.0")
	request.RemoteAddr = "198.51.100.101:45000"
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	return recorder
}

func TestPasswordResetRequestRouteIsPublicLocalAndNoStore(t *testing.T) {
	_, stack := passwordResetRequestStack(t)
	request := httptest.NewRequest(http.MethodGet, passwordResetRequestPath, nil)
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("password reset request page = %d %q", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{
		`action="/account/recover"`, `name="identifier"`, "Request password reset token",
		"Gofer will not send one by email", `href="/account/redeem"`, "I already have a reset token", `href="/login"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("password reset request page missing %q: %q", want, body)
		}
	}
	if strings.Contains(body, "fonts.googleapis.com") || strings.Contains(body, "fonts.gstatic.com") {
		t.Fatal("password reset request page loads remote fonts")
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Referrer-Policy") != "no-referrer" || recorder.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("password reset request security headers = cache:%q referrer:%q robots:%q", recorder.Header().Get("Cache-Control"), recorder.Header().Get("Referrer-Policy"), recorder.Header().Get("X-Robots-Tag"))
	}
}

func TestPasswordResetRequestResponseDoesNotRevealAccountEligibility(t *testing.T) {
	db, stack := passwordResetRequestStack(t)
	identifiers := []string{"ACTIVE.User", "disabled.user", "pending.user", "management.user", "missing.user"}
	var genericBody string
	for _, identifier := range identifiers {
		response := postPasswordResetRequest(stack, identifier)
		if response.Code != http.StatusAccepted {
			t.Fatalf("password reset request for %q = %d %q", identifier, response.Code, response.Body.String())
		}
		if genericBody == "" {
			genericBody = response.Body.String()
		} else if response.Body.String() != genericBody {
			t.Fatalf("password reset response differs for %q", identifier)
		}
		if strings.Contains(response.Body.String(), identifier) {
			t.Fatalf("password reset response echoed submitted identifier %q", identifier)
		}
	}

	rows, err := db.Read().QueryContext(t.Context(), `
		SELECT id, password_reset_requested_at FROM users ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	requested := map[string]bool{}
	for rows.Next() {
		var id string
		var requestedAt sql.NullTime
		if err := rows.Scan(&id, &requestedAt); err != nil {
			t.Fatal(err)
		}
		requested[id] = requestedAt.Valid
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !requested["active-user"] || !requested["disabled-user"] || requested["pending-user"] || requested["management-user"] {
		t.Fatalf("password reset request states = %#v", requested)
	}
	var events int
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_events WHERE event_type = 'credential_reset_requested'`,
	).Scan(&events); err != nil || events != 2 {
		t.Fatalf("password reset request events = %d, %v", events, err)
	}
}

func TestPasswordResetRequestPostRemainsProtectedByCanonicalOriginGuard(t *testing.T) {
	db, stack := passwordResetRequestStack(t)
	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "https://gofer.example")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	guard, err := httpguard.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"identifier": {"active.user"}}
	request := httptest.NewRequest(http.MethodPost, passwordResetRequestPath, strings.NewReader(form.Encode()))
	request.Host = "gofer.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://attacker.example")
	recorder := httptest.NewRecorder()
	guard.Middleware(stack).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "cross-origin request blocked") {
		t.Fatalf("cross-origin password reset request = %d %q", recorder.Code, recorder.Body.String())
	}
	var requestedAt sql.NullTime
	if err := db.Read().QueryRowContext(t.Context(), `SELECT password_reset_requested_at FROM users WHERE id = 'active-user'`).Scan(&requestedAt); err != nil {
		t.Fatal(err)
	}
	if requestedAt.Valid {
		t.Fatal("cross-origin password reset request mutated user state")
	}
}
