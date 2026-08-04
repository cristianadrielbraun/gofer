package auth

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type fixedClock struct {
	now time.Time
}

func (clock *fixedClock) Now() time.Time {
	return clock.now
}

func (*fixedClock) NewTicker(time.Duration) Ticker {
	return &testTicker{ticks: make(chan time.Time)}
}

type testTicker struct {
	ticks chan time.Time
}

func (ticker *testTicker) C() <-chan time.Time { return ticker.ticks }
func (*testTicker) Stop()                      {}

type deterministicTokenGenerator struct {
	ids      []string
	tokens   []string
	idErr    error
	tokenErr error
}

func (generator *deterministicTokenGenerator) ID() (string, error) {
	if generator.idErr != nil {
		return "", generator.idErr
	}
	if len(generator.ids) == 0 {
		return "", errors.New("no deterministic IDs remaining")
	}
	id := generator.ids[0]
	generator.ids = generator.ids[1:]
	return id, nil
}

func (generator *deterministicTokenGenerator) Token(byteCount int) (string, error) {
	if generator.tokenErr != nil {
		return "", generator.tokenErr
	}
	if byteCount != 32 {
		return "", errors.New("unexpected token size")
	}
	if len(generator.tokens) == 0 {
		return "", errors.New("no deterministic tokens remaining")
	}
	token := generator.tokens[0]
	generator.tokens = generator.tokens[1:]
	return token, nil
}

func newDeterministicManager(t *testing.T, clock Clock, tokens TokenGenerator) *Manager {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewManager(&Config{Enabled: true}, db, Dependencies{Clock: clock, Tokens: tokens})
}

func insertActiveUser(t *testing.T, manager *Manager, id string, isAdmin bool, now time.Time) {
	t.Helper()
	admin := 0
	if isAdmin {
		admin = 1
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO users (id, email, email_normalized, name, status, is_admin, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, id+"@example.com", id+"@example.com", id, UserStatusActive, admin, now, now,
	); err != nil {
		t.Fatalf("insert user %q: %v", id, err)
	}
}

func TestCreateSessionUsesInjectedClockAndTokens(t *testing.T) {
	now := time.Date(2026, time.August, 4, 10, 30, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	tokens := &deterministicTokenGenerator{
		ids:    []string{"session-id"},
		tokens: []string{"session-token"},
	}
	manager := newDeterministicManager(t, clock, tokens)
	insertActiveUser(t, manager, "user-id", false, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE users SET auth_version = 5 WHERE id = 'user-id'`); err != nil {
		t.Fatalf("set user auth version: %v", err)
	}

	session, err := manager.CreateSession(t.Context(), "user-id", "test-agent")
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	if session.ID != "session-id" || session.Token != "session-token" {
		t.Fatalf("CreateSession() identity = (%q, %q), want deterministic values", session.ID, session.Token)
	}
	if !session.CreatedAt.Equal(now) || !session.ExpiresAt.Equal(now.Add(30*24*time.Hour)) {
		t.Fatalf("CreateSession() times = (%v, %v), want (%v, %v)", session.CreatedAt, session.ExpiresAt, now, now.Add(30*24*time.Hour))
	}

	stored, err := manager.GetSessionByToken(t.Context(), "session-token")
	if err != nil || stored == nil || stored.ID != "session-id" {
		t.Fatalf("GetSessionByToken() = %#v, %v", stored, err)
	}
	var tokenHash, method, assurance string
	var authVersion int64
	var absoluteExpiresAt time.Time
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT token_hash, auth_version, authentication_method, assurance_level, absolute_expires_at
		FROM sessions WHERE id = ?`, session.ID,
	).Scan(&tokenHash, &authVersion, &method, &assurance, &absoluteExpiresAt); err != nil {
		t.Fatalf("query persisted session metadata: %v", err)
	}
	if tokenHash != hashToken(session.Token) || authVersion != 5 || method != string(AuthenticationMethodLegacy) || assurance != string(AssuranceLevelLegacy) || !absoluteExpiresAt.Equal(session.ExpiresAt) {
		t.Fatalf("persisted session metadata = hash:%q auth:%d method:%q assurance:%q absolute:%v", tokenHash, authVersion, method, assurance, absoluteExpiresAt)
	}
}

func TestSessionExpiryUsesInjectedClock(t *testing.T) {
	now := time.Date(2026, time.August, 4, 10, 30, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{
		ids:    []string{"session-id"},
		tokens: []string{"session-token"},
	})
	insertActiveUser(t, manager, "user-id", false, now)

	session, err := manager.CreateSession(t.Context(), "user-id", "test-agent")
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	clock.now = session.ExpiresAt.Add(-time.Second)
	if found, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || found == nil {
		t.Fatalf("GetSessionByToken(before expiry) = %#v, %v", found, err)
	}

	clock.now = session.ExpiresAt
	if found, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || found != nil {
		t.Fatalf("GetSessionByToken(at expiry) = %#v, %v, want nil", found, err)
	}
	if err := manager.CleanupExpiredSessions(t.Context()); err != nil {
		t.Fatalf("CleanupExpiredSessions(at expiry) error = %v", err)
	}
	var count int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions WHERE id = ?`, session.ID).Scan(&count); err != nil {
		t.Fatalf("count session at expiry: %v", err)
	}
	if count != 1 {
		t.Fatalf("session count at exact expiry = %d, want 1", count)
	}

	clock.now = session.ExpiresAt.Add(time.Second)
	if err := manager.CleanupExpiredSessions(t.Context()); err != nil {
		t.Fatalf("CleanupExpiredSessions(after expiry) error = %v", err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions WHERE id = ?`, session.ID).Scan(&count); err != nil {
		t.Fatalf("count cleaned session: %v", err)
	}
	if count != 0 {
		t.Fatalf("session count after cleanup = %d, want 0", count)
	}
}

func TestCreateSessionDoesNotPersistWhenTokenGenerationFails(t *testing.T) {
	now := time.Date(2026, time.August, 4, 10, 30, 0, 0, time.UTC)
	tokenErr := errors.New("random source unavailable")
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:      []string{"session-id"},
		tokenErr: tokenErr,
	})
	insertActiveUser(t, manager, "user-id", false, now)

	session, err := manager.CreateSession(t.Context(), "user-id", "test-agent")
	if session != nil || !errors.Is(err, tokenErr) {
		t.Fatalf("CreateSession() = %#v, %v, want token error", session, err)
	}
	var count int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Fatalf("session count = %d, want 0", count)
	}
}

func TestGenerateStatePropagatesTokenGenerationFailure(t *testing.T) {
	tokenErr := errors.New("random source unavailable")
	manager := newDeterministicManager(t, &fixedClock{}, &deterministicTokenGenerator{tokenErr: tokenErr})

	state, err := manager.GenerateState()
	if state != "" || !errors.Is(err, tokenErr) {
		t.Fatalf("GenerateState() = %q, %v, want token error", state, err)
	}
}

func TestSetUserStatusRollsBackWhenSessionRevocationFails(t *testing.T) {
	now := time.Date(2026, time.August, 4, 10, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{})
	insertActiveUser(t, manager, "admin-id", true, now)
	insertActiveUser(t, manager, "user-id", false, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO sessions (id, user_id, token, user_agent, expires_at, created_at)
		VALUES ('session-id', 'user-id', 'session-token', 'test-agent', ?, ?)`, now.Add(time.Hour), now); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_session_revocation
		BEFORE DELETE ON sessions
		BEGIN
			SELECT RAISE(ABORT, 'forced session revocation failure');
		END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	if err := manager.SetUserStatus(t.Context(), "user-id", UserStatusDisabled, "admin-id"); err == nil {
		t.Fatal("SetUserStatus() error = nil, want session revocation failure")
	}
	user, err := manager.GetUserByID(t.Context(), "user-id")
	if err != nil {
		t.Fatalf("GetUserByID() error = %v", err)
	}
	if user.Status != UserStatusActive || user.AuthVersion != 1 || user.DisabledAt != nil || user.DisabledBy != "" {
		t.Fatalf("user after rollback = %#v, want unchanged active user", user)
	}
	var count int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions WHERE id = 'session-id'`).Scan(&count); err != nil {
		t.Fatalf("count session after rollback: %v", err)
	}
	if count != 1 {
		t.Fatalf("session count after rollback = %d, want 1", count)
	}
}
