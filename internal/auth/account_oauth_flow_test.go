package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newAccountOAuthFlowTestManager(t *testing.T, enabled bool) (*Manager, *storage.DB) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := NewManager(&Config{Enabled: enabled}, db)
	return manager, db
}

func TestAccountOAuthFlowIsBoundToUserSessionAndSingleUse(t *testing.T) {
	ctx := context.Background()
	manager, db := newAccountOAuthFlowTestManager(t, true)
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO users (id, email, name) VALUES
		('user-one', 'one@example.com', 'One'),
		('user-two', 'two@example.com', 'Two')`); err != nil {
		t.Fatalf("insert users: %v", err)
	}

	state, err := manager.CreateAccountOAuthFlow(ctx, "user-one", "session-one", "gmail", map[string]string{
		"email_address": "one@gmail.com",
		"display_name":  "One Gmail",
	})
	if err != nil {
		t.Fatalf("CreateAccountOAuthFlow() error = %v", err)
	}
	var storedStateHash, storedSessionHash string
	if err := db.Read().QueryRowContext(ctx, `SELECT state_hash, session_token_hash FROM oauth_account_flows`).Scan(&storedStateHash, &storedSessionHash); err != nil {
		t.Fatalf("query stored flow: %v", err)
	}
	if storedStateHash == state || storedSessionHash == "session-one" {
		t.Fatal("OAuth flow stored raw state or session token")
	}

	if _, err := manager.ConsumeAccountOAuthFlow(ctx, state, "user-two", "session-one", "gmail"); !errors.Is(err, ErrAccountOAuthFlowUserMismatch) {
		t.Fatalf("wrong-user consume error = %v", err)
	}
	if _, err := manager.ConsumeAccountOAuthFlow(ctx, state, "user-one", "session-two", "gmail"); !errors.Is(err, ErrAccountOAuthFlowSessionMismatch) {
		t.Fatalf("wrong-session consume error = %v", err)
	}
	if _, err := manager.ConsumeAccountOAuthFlow(ctx, state, "user-one", "session-one", "outlook"); !errors.Is(err, ErrAccountOAuthFlowProviderMismatch) {
		t.Fatalf("wrong-provider consume error = %v", err)
	}

	flow, err := manager.ConsumeAccountOAuthFlow(ctx, state, "user-one", "session-one", "gmail")
	if err != nil {
		t.Fatalf("ConsumeAccountOAuthFlow() error = %v", err)
	}
	if flow.UserID != "user-one" || flow.Provider != "gmail" || flow.FormData["email_address"] != "one@gmail.com" {
		t.Fatalf("consumed flow = %#v", flow)
	}
	if _, err := manager.ConsumeAccountOAuthFlow(ctx, state, "user-one", "session-one", "gmail"); !errors.Is(err, ErrAccountOAuthFlowNotFound) {
		t.Fatalf("replayed consume error = %v", err)
	}
}

func TestAccountOAuthFlowRejectsAndDeletesExpiredState(t *testing.T) {
	ctx := context.Background()
	manager, db := newAccountOAuthFlowTestManager(t, true)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO users (id, email, name) VALUES ('user', 'user@example.com', 'User')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	state := "expired-state"
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO oauth_account_flows (
			state_hash, user_id, session_token_hash, provider, form_data, expires_at
		) VALUES (?, 'user', ?, 'gmail', '{}', ?)`,
		accountOAuthFlowSecretHash(state), accountOAuthFlowSecretHash("session"), time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("insert expired flow: %v", err)
	}
	if _, err := manager.ConsumeAccountOAuthFlow(ctx, state, "user", "session", "gmail"); !errors.Is(err, ErrAccountOAuthFlowExpired) {
		t.Fatalf("expired consume error = %v", err)
	}
	var count int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_account_flows`).Scan(&count); err != nil {
		t.Fatalf("count flows: %v", err)
	}
	if count != 0 {
		t.Fatalf("expired flows = %d, want 0", count)
	}
}

func TestAccountOAuthFlowWorksForSingleUserDefaultAccount(t *testing.T) {
	ctx := context.Background()
	manager, _ := newAccountOAuthFlowTestManager(t, false)
	if err := manager.EnsureDefaultUser(); err != nil {
		t.Fatalf("EnsureDefaultUser() error = %v", err)
	}
	state, err := manager.CreateAccountOAuthFlow(ctx, "default", "", "gmail", map[string]string{"email_address": "local@gmail.com"})
	if err != nil {
		t.Fatalf("CreateAccountOAuthFlow() error = %v", err)
	}
	flow, err := manager.ConsumeAccountOAuthFlow(ctx, state, "default", "", "gmail")
	if err != nil {
		t.Fatalf("ConsumeAccountOAuthFlow() error = %v", err)
	}
	if flow.UserID != "default" {
		t.Fatalf("flow user = %q, want default", flow.UserID)
	}
}

func TestAccountOAuthCallbacksRequireAuthentication(t *testing.T) {
	for _, path := range []string{"/auth/google/account/callback", "/auth/microsoft/account/callback"} {
		if isPublicPath(path) {
			t.Fatalf("account callback %q is still public", path)
		}
	}
	if !isPublicPath("/auth/google/callback") {
		t.Fatal("login callback must remain public")
	}
}

func TestSingleUserMiddlewareStillProvidesDefaultUserToAccountCallback(t *testing.T) {
	manager, _ := newAccountOAuthFlowTestManager(t, false)
	if err := manager.EnsureDefaultUser(); err != nil {
		t.Fatalf("EnsureDefaultUser() error = %v", err)
	}
	called := false
	handler := manager.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		user := GetCurrentUser(r.Context())
		if user == nil || user.ID != "default" {
			t.Fatalf("callback user = %#v, want default", user)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/auth/google/account/callback", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if !called || rec.Code != http.StatusNoContent {
		t.Fatalf("called = %v status = %d, want callback reached with 204", called, rec.Code)
	}
}
