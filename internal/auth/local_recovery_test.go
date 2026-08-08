package auth

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRecoverUserLocallyInvalidatesAuthenticationAndReturnsOneRawResetToken(t *testing.T) {
	now := time.Date(2026, time.August, 6, 14, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"local-token-id", "local-event-id"}, tokens: []string{"one-time-local-secret"},
	})
	insertRedemptionUser(t, manager, "target", UserStatusActive, now)
	oldPasswordHash, err := HashPassword("the existing local passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO password_credentials (user_id, password_hash, created_at, changed_at)
		VALUES ('target', ?, ?, ?)`, oldPasswordHash, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	insertRedemptionSession(t, manager, "session-one", "target", now)
	insertRedemptionSession(t, manager, "session-two", "target", now)
	insertRedemptionToken(t, manager, "old-reset", "target", "old-reset-secret", EnrollmentTokenPurposeCredentialReset, now.Add(time.Hour))

	result, err := manager.RecoverUserLocally(t.Context(), "target")
	if err != nil {
		t.Fatalf("RecoverUserLocally() error = %v", err)
	}
	if result.Token.Token != "one-time-local-secret" || result.Token.ID != "local-token-id" ||
		result.Token.UserID != "target" || result.Token.Purpose != EnrollmentTokenPurposeCredentialReset ||
		!result.Token.CreatedAt.Equal(now) || !result.Token.ExpiresAt.Equal(now.Add(30*time.Minute)) ||
		result.RevokedSessions != 2 || result.ReplacedTokens != 1 {
		t.Fatalf("local recovery result = %#v", result)
	}

	var status UserStatus
	var authVersion int64
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT status, auth_version FROM users WHERE id = 'target'`,
	).Scan(&status, &authVersion); err != nil {
		t.Fatal(err)
	}
	if status != UserStatusActive || authVersion != 2 {
		t.Fatalf("target after recovery = status:%q authVersion:%d", status, authVersion)
	}
	var storedPasswordHash string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT password_hash FROM password_credentials WHERE user_id = 'target'`,
	).Scan(&storedPasswordHash); err != nil || storedPasswordHash != oldPasswordHash {
		t.Fatalf("password changed before token redemption: hashEqual:%t error:%v", storedPasswordHash == oldPasswordHash, err)
	}
	var revokedSessions int
	var sessionsWithoutReason int
	var sessionsWithActor int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*),
		       SUM(CASE WHEN revocation_reason <> 'credential_reset' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN revoked_by IS NOT NULL THEN 1 ELSE 0 END)
		FROM sessions WHERE user_id = 'target' AND revoked_at = ?`, now,
	).Scan(&revokedSessions, &sessionsWithoutReason, &sessionsWithActor); err != nil {
		t.Fatal(err)
	}
	if revokedSessions != 2 || sessionsWithoutReason != 0 || sessionsWithActor != 0 {
		t.Fatalf("revoked sessions = count:%d wrongReason:%d withActor:%d", revokedSessions, sessionsWithoutReason, sessionsWithActor)
	}

	var oldRevokedAt time.Time
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT revoked_at FROM user_enrollment_tokens WHERE id = 'old-reset'`,
	).Scan(&oldRevokedAt); err != nil || !oldRevokedAt.Equal(now) {
		t.Fatalf("old reset token revokedAt = %v, %v", oldRevokedAt, err)
	}
	var tokenHash string
	var createdBy sql.NullString
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT token_hash, created_by FROM user_enrollment_tokens WHERE id = 'local-token-id'`,
	).Scan(&tokenHash, &createdBy); err != nil {
		t.Fatal(err)
	}
	if tokenHash != hashToken(result.Token.Token) || tokenHash == result.Token.Token || createdBy.Valid {
		t.Fatalf("stored local token = hash:%q createdBy:%#v", tokenHash, createdBy)
	}

	var eventType, reason, subjectID, metadata string
	var actorID, sessionID sql.NullString
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT event_type, reason, actor_user_id, subject_user_id, session_id, metadata_json
		FROM auth_events WHERE id = 'local-event-id'`,
	).Scan(&eventType, &reason, &actorID, &subjectID, &sessionID, &metadata); err != nil {
		t.Fatal(err)
	}
	if eventType != string(AuthEventLocalRecoveryStarted) || reason != string(AuthEventReasonLocalOperator) ||
		actorID.Valid || sessionID.Valid || subjectID != "target" ||
		!strings.Contains(metadata, `"token_id":"local-token-id"`) ||
		strings.Contains(metadata, result.Token.Token) || strings.Contains(metadata, tokenHash) {
		t.Fatalf("local recovery event = type:%q reason:%q actor:%#v subject:%q session:%#v metadata:%q", eventType, reason, actorID, subjectID, sessionID, metadata)
	}
}

func TestRecoverUserLocallyPreservesDisabledStatus(t *testing.T) {
	now := time.Date(2026, time.August, 6, 15, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"disabled-token", "disabled-event"}, tokens: []string{"disabled-secret"},
	})
	insertRedemptionUser(t, manager, "disabled", UserStatusDisabled, now)
	if result, err := manager.RecoverUserLocally(t.Context(), "disabled"); err != nil || result == nil {
		t.Fatalf("RecoverUserLocally(disabled) = %#v, %v", result, err)
	}
	var status UserStatus
	var version int64
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT status, auth_version FROM users WHERE id = 'disabled'`).Scan(&status, &version); err != nil {
		t.Fatal(err)
	}
	if status != UserStatusDisabled || version != 2 {
		t.Fatalf("disabled recovery state = %q, %d", status, version)
	}
}

func TestRecoverUserLocallyRejectsIneligibleExactTargetsWithoutMutation(t *testing.T) {
	now := time.Date(2026, time.August, 6, 16, 0, 0, 0, time.UTC)
	for _, target := range []string{"pending", "missing", " pending "} {
		t.Run(target, func(t *testing.T) {
			manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
				ids: []string{"unused-token", "unused-event"}, tokens: []string{"unused-secret"},
			})
			insertRedemptionUser(t, manager, "pending", UserStatusPending, now)
			if result, err := manager.RecoverUserLocally(t.Context(), target); result != nil || !errors.Is(err, ErrLocalRecoveryTargetInvalid) {
				t.Fatalf("RecoverUserLocally(%q) = %#v, %v", target, result, err)
			}
			var version int64
			var tokens, events int
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT auth_version FROM users WHERE id = 'pending'`).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens`).Scan(&tokens); err != nil {
				t.Fatal(err)
			}
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if version != 1 || tokens != 0 || events != 0 {
				t.Fatalf("rejected recovery mutated state = version:%d tokens:%d events:%d", version, tokens, events)
			}
		})
	}
}

func TestRecoverUserLocallyRollsBackWhenAuditEventFails(t *testing.T) {
	now := time.Date(2026, time.August, 6, 17, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"rollback-token", "rollback-event"}, tokens: []string{"rollback-secret"},
	})
	insertRedemptionUser(t, manager, "target", UserStatusActive, now)
	insertRedemptionSession(t, manager, "rollback-session", "target", now)
	insertRedemptionToken(t, manager, "existing-reset", "target", "existing-secret", EnrollmentTokenPurposeCredentialReset, now.Add(time.Hour))
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_local_recovery_event
		BEFORE INSERT ON auth_events
		WHEN NEW.event_type = 'local_recovery_started'
		BEGIN SELECT RAISE(ABORT, 'reject local recovery event'); END`); err != nil {
		t.Fatal(err)
	}
	if result, err := manager.RecoverUserLocally(t.Context(), "target"); result != nil || err == nil {
		t.Fatalf("RecoverUserLocally(audit failure) = %#v, %v", result, err)
	}
	var version int64
	var sessionRevoked, tokenRevoked sql.NullTime
	var inserted int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT auth_version FROM users WHERE id = 'target'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT revoked_at FROM sessions WHERE id = 'rollback-session'`).Scan(&sessionRevoked); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT revoked_at FROM user_enrollment_tokens WHERE id = 'existing-reset'`).Scan(&tokenRevoked); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens WHERE id = 'rollback-token'`).Scan(&inserted); err != nil {
		t.Fatal(err)
	}
	if version != 1 || sessionRevoked.Valid || tokenRevoked.Valid || inserted != 0 {
		t.Fatalf("audit rollback state = version:%d session:%#v token:%#v inserted:%d", version, sessionRevoked, tokenRevoked, inserted)
	}
}

func TestRecoverUserLocallyDoesNotMutateWhenSecretGenerationFails(t *testing.T) {
	now := time.Date(2026, time.August, 6, 18, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"unused-token-id"}, tokenErr: errors.New("random source failed"),
	})
	insertRedemptionUser(t, manager, "target", UserStatusActive, now)
	if result, err := manager.RecoverUserLocally(t.Context(), "target"); result != nil || err == nil {
		t.Fatalf("RecoverUserLocally(generator failure) = %#v, %v", result, err)
	}
	var version int64
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT auth_version FROM users WHERE id = 'target'`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("auth version after generator failure = %d, %v", version, err)
	}
}
