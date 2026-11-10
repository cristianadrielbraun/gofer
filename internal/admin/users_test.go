package admin

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type recordedHooks struct {
	calls []string
	err   error
}

func (hooks *recordedHooks) UserStatusChanged(_ context.Context, userID string, status auth.UserStatus) error {
	hooks.calls = append(hooks.calls, userID+"="+string(status))
	return hooks.err
}

const sessionInsert = `INSERT INTO sessions (
	id, user_id, token_hash, auth_version, authentication_method, assurance_level, user_agent,
	authenticated_at, last_used_at, idle_expires_at, absolute_expires_at, step_up_at, step_up_method, created_at
) VALUES (?, ?, ?, 1, 'password', 'multi_factor', 'Sample Browser', ?, ?, ?, ?, ?, 'totp', ?)`

// newTestService opens a managed instance with an administrator who
// verified recently, an ordinary webmail user, and nobody else.
func newTestService(t *testing.T, hooks Hooks) (*Service, *storage.DB, Actor) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := auth.NewManager(&auth.Config{Mode: auth.ModeManaged}, db, auth.Dependencies{BucketHashKey: []byte("sample-instance-secret-for-tests")})
	now := time.Now().UTC()
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO users (id, username, username_normalized, name, status, auth_version, user_type, is_admin, created_at, updated_at)
		  VALUES ('head-admin', 'head-admin', 'head-admin', 'Head Admin', 'active', 1, 'management', 1, ?, ?)`, []any{now, now}},
		{`INSERT INTO users (id, username, username_normalized, name, status, auth_version, user_type, is_admin, created_at, updated_at)
		  VALUES ('milo-rivers', 'milo-rivers', 'milo-rivers', 'Milo Rivers', 'active', 1, 'webmail', 0, ?, ?)`, []any{now, now}},
		{`INSERT INTO auth_system_state (id, initialized, owner_user_id) VALUES (1, 1, 'head-admin')`, nil},
		{sessionInsert, []any{"admin-session", "head-admin", strings.Repeat("a", 64), now, now, now.Add(time.Hour), now.Add(24 * time.Hour), now, now}},
		{sessionInsert, []any{"milo-session", "milo-rivers", strings.Repeat("b", 64), now, now, now.Add(time.Hour), now.Add(24 * time.Hour), now, now}},
	} {
		if _, err := db.Write().ExecContext(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return NewService(manager, hooks), db, Actor{UserID: "head-admin", SessionID: "admin-session"}
}

func TestSetStatusRunsHooksOnlyAfterACommittedChange(t *testing.T) {
	hooks := &recordedHooks{}
	service, db, actor := newTestService(t, hooks)
	ctx := t.Context()

	result, err := service.SetStatus(ctx, actor, "milo-rivers", auth.UserStatusDisabled)
	if err != nil || result.Status != auth.UserStatusDisabled || !result.Changed {
		t.Fatalf("SetStatus = %+v, %v", result, err)
	}
	if len(hooks.calls) != 1 || hooks.calls[0] != "milo-rivers=disabled" {
		t.Fatalf("hooks = %v", hooks.calls)
	}

	// Someone who isn't an administrator is refused, and nothing follows.
	if _, err := service.SetStatus(ctx, Actor{UserID: "milo-rivers", SessionID: "milo-session"}, "head-admin", auth.UserStatusDisabled); CodeOf(err) != CodeForbidden {
		t.Fatalf("webmail actor = %v (%s)", err, CodeOf(err))
	}
	// A session that hasn't verified recently has to first.
	if _, err := db.Write().ExecContext(ctx, `UPDATE sessions SET step_up_at = ? WHERE id = 'admin-session'`, time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.SetStatus(ctx, actor, "milo-rivers", auth.UserStatusActive); CodeOf(err) != CodeVerificationRequired {
		t.Fatalf("stale session = %v (%s)", err, CodeOf(err))
	}
	if len(hooks.calls) != 1 {
		t.Fatalf("a refused change ran hooks: %v", hooks.calls)
	}

	// When what follows fails, the change still stands and says so.
	if _, err := db.Write().ExecContext(ctx, `UPDATE sessions SET step_up_at = ? WHERE id = 'admin-session'`, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	hooks.err = errors.New("sample mail runtime failure")
	result, err = service.SetStatus(ctx, actor, "milo-rivers", auth.UserStatusActive)
	var hookErr *HookError
	if !errors.As(err, &hookErr) || result == nil || result.Status != auth.UserStatusActive {
		t.Fatalf("failing hook = %+v, %v", result, err)
	}
	var status string
	if err := db.Read().QueryRow(`SELECT status FROM users WHERE id = 'milo-rivers'`).Scan(&status); err != nil || status != "active" {
		t.Fatalf("the change didn't stand: %q, %v", status, err)
	}
}

func TestIssueCredentialResetOnlyForWebmailUsers(t *testing.T) {
	service, _, actor := newTestService(t, nil)
	ctx := t.Context()
	if _, _, err := service.IssueCredentialReset(ctx, actor, "head-admin"); CodeOf(err) != CodeUnavailable {
		t.Fatalf("administrator target = %v (%s)", err, CodeOf(err))
	}
	if _, _, err := service.IssueCredentialReset(ctx, actor, "nobody"); CodeOf(err) != CodeUnavailable {
		t.Fatalf("missing target = %v (%s)", err, CodeOf(err))
	}
	token, target, err := service.IssueCredentialReset(ctx, actor, "milo-rivers")
	if err != nil || token == nil || token.Token == "" || target.Username != "milo-rivers" {
		t.Fatalf("IssueCredentialReset = %+v, %+v, %v", token, target, err)
	}
}

func TestCodeOfUnknownErrorsIsInternal(t *testing.T) {
	if CodeOf(nil) != "" || CodeOf(errors.New("sample")) != CodeInternal || CodeOf(&HookError{Err: errors.New("sample")}) != CodeInternal {
		t.Fatal("unexpected codes")
	}
}
