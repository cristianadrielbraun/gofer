package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestSessionLookupFailsClosedForInvalidLifecycleState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *Manager, *Session, time.Time)
	}{
		{
			name: "revoked",
			mutate: func(t *testing.T, manager *Manager, session *Session, _ time.Time) {
				t.Helper()
				if changed, err := manager.RevokeSessionByToken(t.Context(), session.Token, session.UserID, SessionRevocationLogout); err != nil || !changed {
					t.Fatalf("RevokeSessionByToken() = %t, %v", changed, err)
				}
			},
		},
		{
			name: "idle expired",
			mutate: func(t *testing.T, manager *Manager, session *Session, now time.Time) {
				t.Helper()
				if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE sessions SET idle_expires_at = ? WHERE id = ?`, now, session.ID); err != nil {
					t.Fatalf("expire idle session: %v", err)
				}
			},
		},
		{
			name: "absolute expired",
			mutate: func(t *testing.T, manager *Manager, session *Session, now time.Time) {
				t.Helper()
				if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE sessions SET absolute_expires_at = ? WHERE id = ?`, now, session.ID); err != nil {
					t.Fatalf("expire absolute session: %v", err)
				}
			},
		},
		{
			name: "auth version mismatch",
			mutate: func(t *testing.T, manager *Manager, session *Session, _ time.Time) {
				t.Helper()
				if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE users SET auth_version = auth_version + 1 WHERE id = ?`, session.UserID); err != nil {
					t.Fatalf("increment auth version: %v", err)
				}
			},
		},
		{
			name: "user disabled",
			mutate: func(t *testing.T, manager *Manager, session *Session, _ time.Time) {
				t.Helper()
				if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE users SET status = 'disabled' WHERE id = ?`, session.UserID); err != nil {
					t.Fatalf("disable user: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
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
			test.mutate(t, manager, session, now)
			if found, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || found != nil {
				t.Fatalf("GetSessionByToken() = %#v, %v, want nil", found, err)
			}
		})
	}
}

func TestSessionActivityWritesAreThrottled(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
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

	clock.now = now.Add(4 * time.Minute)
	if found, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || found == nil {
		t.Fatalf("GetSessionByToken(before write interval) = %#v, %v", found, err)
	}
	var storedLastUsed time.Time
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT last_used_at FROM sessions WHERE id = ?`, session.ID).Scan(&storedLastUsed); err != nil {
		t.Fatalf("query throttled last use: %v", err)
	}
	if !storedLastUsed.Equal(now) {
		t.Fatalf("last_used_at before interval = %v, want %v", storedLastUsed, now)
	}

	clock.now = now.Add(6 * time.Minute)
	if found, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || found == nil {
		t.Fatalf("GetSessionByToken(after write interval) = %#v, %v", found, err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT last_used_at FROM sessions WHERE id = ?`, session.ID).Scan(&storedLastUsed); err != nil {
		t.Fatalf("query updated last use: %v", err)
	}
	if !storedLastUsed.Equal(clock.now) {
		t.Fatalf("last_used_at after interval = %v, want %v", storedLastUsed, clock.now)
	}
}

func TestRotateSessionPreservesAuthenticationAndRevokesOldToken(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{
		ids:    []string{"original-id", "rotated-id"},
		tokens: []string{"original-token", "rotated-token"},
	})
	insertActiveUser(t, manager, "user-id", false, now)
	original, err := manager.CreateAuthenticatedSession(t.Context(), "user-id", "old-agent", AuthenticationMethodPasskey, AssuranceLevelPhishingResistant)
	if err != nil {
		t.Fatalf("CreateAuthenticatedSession() error = %v", err)
	}
	clock.now = now.Add(30 * time.Minute)
	if changed, err := manager.RecordSessionStepUp(t.Context(), "user-id", original.ID, AuthenticationMethodTOTP); err != nil || !changed {
		t.Fatalf("RecordSessionStepUp() = %t, %v", changed, err)
	}
	if changed, err := manager.RecordSessionStepUp(t.Context(), "other-user", original.ID, AuthenticationMethodTOTP); err != nil || changed {
		t.Fatalf("foreign RecordSessionStepUp() = %t, %v, want false", changed, err)
	}
	clock.now = now.Add(time.Hour)
	rotated, err := manager.RotateSession(t.Context(), original.Token, "new-agent")
	if err != nil {
		t.Fatalf("RotateSession() error = %v", err)
	}
	if rotated.Token != "rotated-token" || rotated.ID != "rotated-id" {
		t.Fatalf("rotated session identity = %q/%q", rotated.ID, rotated.Token)
	}
	if rotated.AuthenticationMethod != original.AuthenticationMethod || rotated.AssuranceLevel != original.AssuranceLevel || !rotated.AuthenticatedAt.Equal(original.AuthenticatedAt) || !rotated.AbsoluteExpiresAt.Equal(original.AbsoluteExpiresAt) {
		t.Fatalf("rotated authentication metadata = %#v, original %#v", rotated, original)
	}
	if rotated.StepUpAt == nil || !rotated.StepUpAt.Equal(now.Add(30*time.Minute)) || rotated.StepUpMethod != AuthenticationMethodTOTP {
		t.Fatalf("rotated step-up metadata = %v/%q", rotated.StepUpAt, rotated.StepUpMethod)
	}
	if found, err := manager.GetSessionByToken(t.Context(), original.Token); err != nil || found != nil {
		t.Fatalf("old token after rotation = %#v, %v, want nil", found, err)
	}
	if found, err := manager.GetSessionByToken(t.Context(), rotated.Token); err != nil || found == nil || found.ID != rotated.ID {
		t.Fatalf("new token after rotation = %#v, %v", found, err)
	}
	sessions, err := manager.ListSessions(t.Context(), "user-id")
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("ListSessions() count = %d, want 2", len(sessions))
	}
	for _, session := range sessions {
		if session.Token != "" {
			t.Fatalf("listed session exposed raw token %q", session.Token)
		}
		if session.ID == original.ID && (session.RevokedAt == nil || session.RevocationReason != SessionRevocationRotation || session.RevokedBy != "user-id") {
			t.Fatalf("original session revocation = %#v", session)
		}
	}
}

func TestConcurrentSessionRotationSucceedsOnce(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"original-id", "rotation-a", "rotation-b"},
		tokens: []string{"original-token", "token-a", "token-b"},
	})
	insertActiveUser(t, manager, "user-id", false, now)
	original, err := manager.CreateSession(t.Context(), "user-id", "agent")
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := manager.RotateSession(t.Context(), original.Token, "rotated-agent")
			errorsCh <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsCh)
	successes, inactive := 0, 0
	for err := range errorsCh {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrSessionNotActive):
			inactive++
		default:
			t.Fatalf("RotateSession() unexpected error = %v", err)
		}
	}
	if successes != 1 || inactive != 1 {
		t.Fatalf("concurrent rotations = successes:%d inactive:%d, want 1/1", successes, inactive)
	}
	var total, active int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*), SUM(CASE WHEN revoked_at IS NULL THEN 1 ELSE 0 END)
		FROM sessions WHERE user_id = 'user-id'`).Scan(&total, &active); err != nil {
		t.Fatalf("count rotated sessions: %v", err)
	}
	if total != 2 || active != 1 {
		t.Fatalf("rotated session rows = total:%d active:%d, want 2/1", total, active)
	}
}

func TestSessionRevocationIsUserScoped(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"one-current", "one-other", "two-current"},
		tokens: []string{"one-token", "one-other-token", "two-token"},
	})
	insertActiveUser(t, manager, "user-one", false, now)
	insertActiveUser(t, manager, "user-two", false, now)
	oneCurrent, err := manager.CreateSession(t.Context(), "user-one", "agent")
	if err != nil {
		t.Fatalf("CreateSession(user-one current) error = %v", err)
	}
	oneOther, err := manager.CreateSession(t.Context(), "user-one", "agent")
	if err != nil {
		t.Fatalf("CreateSession(user-one other) error = %v", err)
	}
	twoCurrent, err := manager.CreateSession(t.Context(), "user-two", "agent")
	if err != nil {
		t.Fatalf("CreateSession(user-two) error = %v", err)
	}

	if changed, err := manager.RevokeSession(t.Context(), "user-one", twoCurrent.ID, "user-one", SessionRevocationAdminAction); err != nil || changed {
		t.Fatalf("foreign RevokeSession() = %t, %v, want false", changed, err)
	}
	if found, err := manager.GetSessionByToken(t.Context(), twoCurrent.Token); err != nil || found == nil {
		t.Fatalf("foreign session after denial = %#v, %v", found, err)
	}
	if changed, err := manager.RevokeOtherSessions(t.Context(), "user-one", oneCurrent.ID, "user-one", SessionRevocationAdminAction); err != nil || changed != 1 {
		t.Fatalf("RevokeOtherSessions() = %d, %v, want 1", changed, err)
	}
	if found, err := manager.GetSessionByToken(t.Context(), oneOther.Token); err != nil || found != nil {
		t.Fatalf("other session after revocation = %#v, %v, want nil", found, err)
	}
	if found, err := manager.GetSessionByToken(t.Context(), oneCurrent.Token); err != nil || found == nil {
		t.Fatalf("current session after other revocation = %#v, %v", found, err)
	}
	if changed, err := manager.RevokeAllSessions(t.Context(), "user-two", "user-two", SessionRevocationAdminAction); err != nil || changed != 1 {
		t.Fatalf("RevokeAllSessions() = %d, %v, want 1", changed, err)
	}
	if found, err := manager.GetSessionByToken(t.Context(), twoCurrent.Token); err != nil || found != nil {
		t.Fatalf("user-two session after revocation = %#v, %v, want nil", found, err)
	}
}

func TestRevokeOtherSessionsRequiresActiveCurrentSession(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"current", "other"},
		tokens: []string{"current-token", "other-token"},
	})
	insertActiveUser(t, manager, "user-id", false, now)
	current, err := manager.CreateSession(t.Context(), "user-id", "agent")
	if err != nil {
		t.Fatalf("CreateSession(current) error = %v", err)
	}
	other, err := manager.CreateSession(t.Context(), "user-id", "agent")
	if err != nil {
		t.Fatalf("CreateSession(other) error = %v", err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE sessions SET idle_expires_at = ? WHERE id = ?`, now, current.ID); err != nil {
		t.Fatalf("expire current session: %v", err)
	}

	changed, err := manager.RevokeOtherSessions(t.Context(), "user-id", current.ID, "user-id", SessionRevocationAdminAction)
	if !errors.Is(err, ErrSessionNotActive) || changed != 0 {
		t.Fatalf("RevokeOtherSessions() = %d, %v, want 0/ErrSessionNotActive", changed, err)
	}
	if found, err := manager.GetSessionByToken(t.Context(), other.Token); err != nil || found == nil {
		t.Fatalf("other session after denied revocation = %#v, %v", found, err)
	}
}

func TestSessionCleanupUsesBoundedBatches(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{})
	insertActiveUser(t, manager, "user-id", false, now)
	tx, err := manager.db.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin seed transaction: %v", err)
	}
	for i := range sessionCleanupBatchSize + 1 {
		value := fmt.Sprintf("expired-session-%d", i)
		if _, err := tx.ExecContext(t.Context(), `
			INSERT INTO sessions (
				id, user_id, token_hash, auth_version, authentication_method, assurance_level,
				authenticated_at, last_used_at, idle_expires_at, absolute_expires_at, created_at
			) VALUES (?, 'user-id', ?, 1, 'legacy', 'legacy', ?, ?, ?, ?, ?)`,
			value, hashToken(value), now.Add(-time.Hour), now.Add(-time.Hour),
			now.Add(-time.Second), now.Add(-time.Second), now.Add(-time.Hour),
		); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert expired session %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit expired sessions: %v", err)
	}
	if err := manager.CleanupExpiredSessions(t.Context()); err != nil {
		t.Fatalf("CleanupExpiredSessions(first) error = %v", err)
	}
	var count int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
		t.Fatalf("count first cleanup: %v", err)
	}
	if count != 1 {
		t.Fatalf("session count after first cleanup = %d, want 1", count)
	}
	if err := manager.CleanupExpiredSessions(t.Context()); err != nil {
		t.Fatalf("CleanupExpiredSessions(second) error = %v", err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&count); err != nil {
		t.Fatalf("count second cleanup: %v", err)
	}
	if count != 0 {
		t.Fatalf("session count after second cleanup = %d, want 0", count)
	}
}

func TestMiddlewareAddsSessionContextAndRejectsAuthVersionMismatch(t *testing.T) {
	now := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"session-id"},
		tokens: []string{"session-token"},
	})
	insertActiveUser(t, manager, "user-id", false, now)
	session, err := manager.CreateSession(t.Context(), "user-id", "agent")
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	called := false
	handler := manager.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if user := GetCurrentUser(r.Context()); user == nil || user.ID != "user-id" {
			t.Fatalf("current user = %#v", user)
		}
		if current := GetCurrentSession(r.Context()); current == nil || current.ID != session.ID {
			t.Fatalf("current session = %#v", current)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session.Token})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if !called || rec.Code != http.StatusNoContent {
		t.Fatalf("valid middleware request called=%t status=%d", called, rec.Code)
	}

	if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE users SET auth_version = auth_version + 1 WHERE id = 'user-id'`); err != nil {
		t.Fatalf("increment auth version: %v", err)
	}
	called = false
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session.Token})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if called || rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("mismatched request called=%t status=%d location=%q", called, rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != sessionCookieName || cookies[0].MaxAge != -1 {
		t.Fatalf("mismatched response cookies = %#v, want cleared session", cookies)
	}
}
