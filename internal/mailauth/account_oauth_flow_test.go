package mailauth

import (
	"context"
	"errors"
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
	manager, db := newAccountOAuthFlowTestManager(t, false)
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO users (id, email, name) VALUES ('default', 'local@gofer.local', 'Local User')`); err != nil {
		t.Fatalf("insert default user: %v", err)
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
