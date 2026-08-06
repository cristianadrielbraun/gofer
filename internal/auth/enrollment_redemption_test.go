package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const redemptionTestPassword = "an excellent redeemed passphrase"

func insertRedemptionUser(t *testing.T, manager *Manager, id string, status UserStatus, now time.Time) {
	t.Helper()
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, email, email_normalized, username, username_normalized, name,
			status, auth_version, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		id, id+"@example.com", id+"@example.com", id, id, id, status, now, now,
	); err != nil {
		t.Fatalf("insert redemption user %q: %v", id, err)
	}
}

func insertRedemptionToken(t *testing.T, manager *Manager, id, userID, rawToken string, purpose EnrollmentTokenPurpose, expiresAt time.Time) {
	t.Helper()
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO user_enrollment_tokens (
			id, user_id, token_hash, purpose, created_at, expires_at
		) VALUES (?, ?, ?, ?, ?, ?)`,
		id, userID, hashToken(rawToken), purpose, expiresAt.Add(-time.Hour), expiresAt,
	); err != nil {
		t.Fatalf("insert redemption token %q: %v", id, err)
	}
}

func insertRedemptionSession(t *testing.T, manager *Manager, id, userID string, now time.Time) {
	t.Helper()
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method,
			assurance_level, user_agent, authenticated_at, last_used_at,
			idle_expires_at, absolute_expires_at, created_at
		) VALUES (?, ?, ?, 1, 'password', 'single_factor', 'old browser', ?, ?, ?, ?, ?)`,
		id, userID, hashToken(id+"-raw"), now, now, now.Add(time.Hour), now.Add(24*time.Hour), now,
	); err != nil {
		t.Fatalf("insert redemption session %q: %v", id, err)
	}
}

func TestRedeemEnrollmentTokenActivatesUserAndConsumesTokenAtomically(t *testing.T) {
	now := time.Date(2026, time.August, 6, 9, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{ids: []string{"enrollment-completed-event"}})
	insertRedemptionUser(t, manager, "invitee", UserStatusPending, now)
	insertRedemptionToken(t, manager, "enrollment-token", "invitee", "raw-enrollment-secret", EnrollmentTokenPurposeEnrollment, now.Add(time.Hour))

	result, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{
		Token: " raw-enrollment-secret ", NewPassword: redemptionTestPassword, UserAgent: " Enrollment Browser/1.0 ",
	})
	if err != nil {
		t.Fatalf("RedeemEnrollmentToken() error = %v", err)
	}
	if result.UserID != "invitee" || result.Purpose != EnrollmentTokenPurposeEnrollment || result.RevokedSessions != 0 {
		t.Fatalf("enrollment result = %#v", result)
	}

	var status UserStatus
	var passwordHash string
	var mustChange int
	var usedAt time.Time
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT status FROM users WHERE id = 'invitee'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT password_hash, must_change FROM password_credentials WHERE user_id = 'invitee'`,
	).Scan(&passwordHash, &mustChange); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT used_at FROM user_enrollment_tokens WHERE id = 'enrollment-token'`,
	).Scan(&usedAt); err != nil {
		t.Fatal(err)
	}
	matches, _, err := VerifyPassword(passwordHash, redemptionTestPassword)
	if err != nil || !matches || status != UserStatusActive || mustChange != 0 || !usedAt.Equal(now) {
		t.Fatalf("enrollment state = status:%q matches:%t mustChange:%d usedAt:%v error:%v", status, matches, mustChange, usedAt, err)
	}

	var eventType, subjectID, userAgent, metadata string
	var actorID sql.NullString
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT event_type, actor_user_id, subject_user_id, user_agent, metadata_json
		FROM auth_events WHERE id = 'enrollment-completed-event'`,
	).Scan(&eventType, &actorID, &subjectID, &userAgent, &metadata); err != nil {
		t.Fatal(err)
	}
	if eventType != string(AuthEventEnrollmentCompleted) || actorID.Valid || subjectID != "invitee" || userAgent != "Enrollment Browser/1.0" ||
		strings.Contains(metadata, "raw-enrollment-secret") || strings.Contains(metadata, hashToken("raw-enrollment-secret")) {
		t.Fatalf("enrollment event = type:%q actor:%#v subject:%q agent:%q metadata:%q", eventType, actorID, subjectID, userAgent, metadata)
	}

	if replay, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{
		Token: "raw-enrollment-secret", NewPassword: "another excellent redeemed passphrase",
	}); replay != nil || !errors.Is(err, ErrEnrollmentTokenInvalid) {
		t.Fatalf("replayed enrollment token = %#v, %v", replay, err)
	}
	var eventCount int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&eventCount); err != nil || eventCount != 1 {
		t.Fatalf("event count after replay = %d, %v", eventCount, err)
	}
}

func TestRedeemCredentialResetReplacesPasswordInvalidatesAuthAndPreservesDisabledState(t *testing.T) {
	now := time.Date(2026, time.August, 6, 10, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{ids: []string{"active-reset-event", "disabled-reset-event"}})
	oldHash, err := HashPassword("the previous account passphrase")
	if err != nil {
		t.Fatal(err)
	}
	for _, user := range []struct {
		id     string
		status UserStatus
		token  string
	}{
		{id: "active", status: UserStatusActive, token: "active-reset-secret"},
		{id: "disabled", status: UserStatusDisabled, token: "disabled-reset-secret"},
	} {
		insertRedemptionUser(t, manager, user.id, user.status, now)
		if _, err := manager.db.Write().ExecContext(t.Context(), `
			INSERT INTO password_credentials (user_id, password_hash, must_change, created_at, changed_at)
			VALUES (?, ?, 1, ?, ?)`, user.id, oldHash, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		insertRedemptionSession(t, manager, user.id+"-session-one", user.id, now)
		insertRedemptionSession(t, manager, user.id+"-session-two", user.id, now)
		insertRedemptionToken(t, manager, user.id+"-reset-token", user.id, user.token, EnrollmentTokenPurposeCredentialReset, now.Add(time.Hour))

		result, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{Token: user.token, NewPassword: redemptionTestPassword})
		if err != nil || result == nil || result.RevokedSessions != 2 {
			t.Fatalf("reset %q = %#v, %v", user.id, result, err)
		}
		var status UserStatus
		var authVersion int64
		var passwordHash string
		var mustChange int
		var resetAt time.Time
		if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT status, auth_version FROM users WHERE id = ?`, user.id).Scan(&status, &authVersion); err != nil {
			t.Fatal(err)
		}
		if err := manager.db.Read().QueryRowContext(t.Context(), `
			SELECT password_hash, must_change, reset_at FROM password_credentials WHERE user_id = ?`, user.id,
		).Scan(&passwordHash, &mustChange, &resetAt); err != nil {
			t.Fatal(err)
		}
		matches, _, verifyErr := VerifyPassword(passwordHash, redemptionTestPassword)
		var activeSessions int
		if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions WHERE user_id = ? AND revoked_at IS NULL`, user.id).Scan(&activeSessions); err != nil {
			t.Fatal(err)
		}
		if status != user.status || authVersion != 2 || !matches || verifyErr != nil || mustChange != 0 || !resetAt.Equal(now) || activeSessions != 0 {
			t.Fatalf("reset state %q = status:%q version:%d matches:%t mustChange:%d reset:%v sessions:%d error:%v", user.id, status, authVersion, matches, mustChange, resetAt, activeSessions, verifyErr)
		}
	}
}

func TestRedeemEnrollmentTokenRejectsInactiveTokenAndPolicyFailuresWithoutMutation(t *testing.T) {
	now := time.Date(2026, time.August, 6, 11, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		status    UserStatus
		purpose   EnrollmentTokenPurpose
		expiresAt time.Time
		used      bool
		revoked   bool
		rawToken  string
		supplied  string
	}{
		{name: "unknown", status: UserStatusPending, purpose: EnrollmentTokenPurposeEnrollment, expiresAt: now.Add(time.Hour), rawToken: "stored", supplied: "unknown"},
		{name: "expired", status: UserStatusPending, purpose: EnrollmentTokenPurposeEnrollment, expiresAt: now, rawToken: "expired", supplied: "expired"},
		{name: "used", status: UserStatusPending, purpose: EnrollmentTokenPurposeEnrollment, expiresAt: now.Add(time.Hour), used: true, rawToken: "used", supplied: "used"},
		{name: "revoked", status: UserStatusPending, purpose: EnrollmentTokenPurposeEnrollment, expiresAt: now.Add(time.Hour), revoked: true, rawToken: "revoked", supplied: "revoked"},
		{name: "wrong state", status: UserStatusActive, purpose: EnrollmentTokenPurposeEnrollment, expiresAt: now.Add(time.Hour), rawToken: "wrong-state", supplied: "wrong-state"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{})
			insertRedemptionUser(t, manager, "person", test.status, now)
			insertRedemptionToken(t, manager, "token", "person", test.rawToken, test.purpose, test.expiresAt)
			if test.used {
				_, _ = manager.db.Write().ExecContext(t.Context(), `UPDATE user_enrollment_tokens SET used_at = ? WHERE id = 'token'`, now.Add(-time.Minute))
			}
			if test.revoked {
				_, _ = manager.db.Write().ExecContext(t.Context(), `UPDATE user_enrollment_tokens SET revoked_at = ? WHERE id = 'token'`, now.Add(-time.Minute))
			}
			result, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{Token: test.supplied, NewPassword: redemptionTestPassword})
			if result != nil || !errors.Is(err, ErrEnrollmentTokenInvalid) {
				t.Fatalf("inactive redemption = %#v, %v", result, err)
			}
			var credentials int
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM password_credentials`).Scan(&credentials); err != nil || credentials != 0 {
				t.Fatalf("credential count = %d, %v", credentials, err)
			}
		})
	}

	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{})
	insertRedemptionUser(t, manager, "person", UserStatusPending, now)
	insertRedemptionToken(t, manager, "token", "person", "valid", EnrollmentTokenPurposeEnrollment, now.Add(time.Hour))
	if result, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{Token: "valid", NewPassword: "short"}); result != nil || !errors.Is(err, ErrPasswordTooShort) {
		t.Fatalf("policy rejection = %#v, %v", result, err)
	}
	var usedAt sql.NullTime
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT used_at FROM user_enrollment_tokens WHERE id = 'token'`).Scan(&usedAt); err != nil || usedAt.Valid {
		t.Fatalf("token after policy rejection = %#v, %v", usedAt, err)
	}
}

func TestRedeemEnrollmentTokenRollsBackWhenSecurityEventFails(t *testing.T) {
	now := time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{ids: []string{"rejected-event"}})
	insertRedemptionUser(t, manager, "invitee", UserStatusPending, now)
	insertRedemptionToken(t, manager, "token", "invitee", "rollback-secret", EnrollmentTokenPurposeEnrollment, now.Add(time.Hour))
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_redemption_event
		BEFORE INSERT ON auth_events
		BEGIN
			SELECT RAISE(ABORT, 'forced redemption event failure');
		END`); err != nil {
		t.Fatal(err)
	}
	result, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{Token: "rollback-secret", NewPassword: redemptionTestPassword})
	if result != nil || err == nil {
		t.Fatalf("event failure = %#v, %v", result, err)
	}
	var status UserStatus
	var usedAt sql.NullTime
	var credentials int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT status FROM users WHERE id = 'invitee'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT used_at FROM user_enrollment_tokens WHERE id = 'token'`).Scan(&usedAt); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM password_credentials`).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if status != UserStatusPending || usedAt.Valid || credentials != 0 {
		t.Fatalf("rolled-back state = status:%q used:%#v credentials:%d", status, usedAt, credentials)
	}
}

func TestRedeemEnrollmentTokenConcurrentUseHasOneWinner(t *testing.T) {
	now := time.Date(2026, time.August, 6, 13, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{ids: []string{"event-one", "event-two"}})
	insertRedemptionUser(t, manager, "invitee", UserStatusPending, now)
	insertRedemptionToken(t, manager, "token", "invitee", "concurrent-secret", EnrollmentTokenPurposeEnrollment, now.Add(time.Hour))

	start := make(chan struct{})
	errorsFound := make(chan error, 2)
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{
				Token: "concurrent-secret", NewPassword: fmt.Sprintf("a concurrent redeemed passphrase %d", index),
			})
			errorsFound <- err
		}(index)
	}
	close(start)
	wait.Wait()
	close(errorsFound)
	var successes, invalid int
	for err := range errorsFound {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrEnrollmentTokenInvalid):
			invalid++
		default:
			t.Fatalf("concurrent redemption error = %v", err)
		}
	}
	if successes != 1 || invalid != 1 {
		t.Fatalf("concurrent redemption = successes:%d invalid:%d", successes, invalid)
	}
	var used, credentials, events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens WHERE used_at IS NOT NULL`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM password_credentials`).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if used != 1 || credentials != 1 || events != 1 {
		t.Fatalf("concurrent persisted state = used:%d credentials:%d events:%d", used, credentials, events)
	}
}
