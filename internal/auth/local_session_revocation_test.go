package auth

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRevokeUserSessionsLocallySignsOutExactTargetWithoutChangingCredentials(t *testing.T) {
	now := time.Date(2026, time.August, 8, 10, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"local-session-event"},
	})
	insertRedemptionUser(t, manager, "target", UserStatusActive, now)
	insertRedemptionUser(t, manager, "other", UserStatusActive, now)
	passwordHash, err := HashPassword("the existing local session passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO password_credentials (user_id, password_hash, created_at, changed_at)
		VALUES ('target', ?, ?, ?)`, passwordHash, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	insertRedemptionSession(t, manager, "target-session-one", "target", now)
	insertRedemptionSession(t, manager, "target-session-two", "target", now)
	insertRedemptionSession(t, manager, "other-session", "other", now)
	insertRedemptionToken(t, manager, "target-reset", "target", "unchanged-reset-secret", EnrollmentTokenPurposeCredentialReset, now.Add(time.Hour))

	result, err := manager.RevokeUserSessionsLocally(t.Context(), "target")
	if err != nil {
		t.Fatalf("RevokeUserSessionsLocally() error = %v", err)
	}
	if result.UserID != "target" || result.RevokedSessions != 2 {
		t.Fatalf("local session revocation result = %#v", result)
	}

	var status UserStatus
	var authVersion int64
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT status, auth_version FROM users WHERE id = 'target'`,
	).Scan(&status, &authVersion); err != nil {
		t.Fatal(err)
	}
	if status != UserStatusActive || authVersion != 1 {
		t.Fatalf("target state changed = status:%q authVersion:%d", status, authVersion)
	}
	var storedPasswordHash string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT password_hash FROM password_credentials WHERE user_id = 'target'`,
	).Scan(&storedPasswordHash); err != nil || storedPasswordHash != passwordHash {
		t.Fatalf("password changed = equal:%t error:%v", storedPasswordHash == passwordHash, err)
	}
	var resetRevoked sql.NullTime
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT revoked_at FROM user_enrollment_tokens WHERE id = 'target-reset'`,
	).Scan(&resetRevoked); err != nil || resetRevoked.Valid {
		t.Fatalf("reset token changed = %#v, %v", resetRevoked, err)
	}

	var targetActive, otherActive int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sessions WHERE user_id = 'target' AND revoked_at IS NULL`,
	).Scan(&targetActive); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sessions WHERE user_id = 'other' AND revoked_at IS NULL`,
	).Scan(&otherActive); err != nil {
		t.Fatal(err)
	}
	var revokedAt time.Time
	var reason string
	var actor sql.NullString
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT revoked_at, revocation_reason, revoked_by
		FROM sessions WHERE id = 'target-session-one'`,
	).Scan(&revokedAt, &reason, &actor); err != nil {
		t.Fatal(err)
	}
	if targetActive != 0 || otherActive != 1 || !revokedAt.Equal(now) ||
		reason != string(SessionRevocationAdminAction) || actor.Valid {
		t.Fatalf("session state = targetActive:%d otherActive:%d revokedAt:%v reason:%q actor:%#v", targetActive, otherActive, revokedAt, reason, actor)
	}

	var eventType, eventReason, subjectID, metadata string
	var actorID, sessionID sql.NullString
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT event_type, reason, actor_user_id, subject_user_id, session_id, metadata_json
		FROM auth_events WHERE id = 'local-session-event'`,
	).Scan(&eventType, &eventReason, &actorID, &subjectID, &sessionID, &metadata); err != nil {
		t.Fatal(err)
	}
	if eventType != string(AuthEventSessionRevoked) || eventReason != string(AuthEventReasonLocalOperator) ||
		actorID.Valid || sessionID.Valid || subjectID != "target" || metadata != `{"revoked_sessions":2}` ||
		strings.Contains(metadata, "target-session") || strings.Contains(metadata, "unchanged-reset-secret") {
		t.Fatalf("local session event = type:%q reason:%q actor:%#v subject:%q session:%#v metadata:%q", eventType, eventReason, actorID, subjectID, sessionID, metadata)
	}
}

func TestRevokeUserSessionsLocallyPreservesDisabledUserAndAuditsZeroSessions(t *testing.T) {
	now := time.Date(2026, time.August, 8, 11, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"zero-session-event"},
	})
	insertRedemptionUser(t, manager, "disabled", UserStatusDisabled, now)
	result, err := manager.RevokeUserSessionsLocally(t.Context(), "disabled")
	if err != nil || result == nil || result.RevokedSessions != 0 {
		t.Fatalf("RevokeUserSessionsLocally(disabled) = %#v, %v", result, err)
	}
	var status UserStatus
	var version int64
	var metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT status, auth_version FROM users WHERE id = 'disabled'`,
	).Scan(&status, &version); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT metadata_json FROM auth_events WHERE id = 'zero-session-event'`,
	).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	if status != UserStatusDisabled || version != 1 || metadata != `{"revoked_sessions":0}` {
		t.Fatalf("disabled zero-session result = status:%q version:%d metadata:%q", status, version, metadata)
	}
}

func TestRevokeUserSessionsLocallyRejectsIneligibleExactTargetsWithoutMutation(t *testing.T) {
	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	for _, target := range []string{"pending", "missing", " pending "} {
		t.Run(target, func(t *testing.T) {
			manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
				ids: []string{"unused-event"},
			})
			insertRedemptionUser(t, manager, "pending", UserStatusPending, now)
			if result, err := manager.RevokeUserSessionsLocally(t.Context(), target); result != nil || !errors.Is(err, ErrLocalSessionRevocationTargetInvalid) {
				t.Fatalf("RevokeUserSessionsLocally(%q) = %#v, %v", target, result, err)
			}
			var events int
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&events); err != nil || events != 0 {
				t.Fatalf("rejected revocation event count = %d, %v", events, err)
			}
		})
	}
}

func TestRevokeUserSessionsLocallyRollsBackWhenAuditEventFails(t *testing.T) {
	now := time.Date(2026, time.August, 8, 13, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"rollback-session-event"},
	})
	insertRedemptionUser(t, manager, "target", UserStatusActive, now)
	insertRedemptionSession(t, manager, "rollback-session", "target", now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_local_session_event
		BEFORE INSERT ON auth_events
		WHEN NEW.event_type = 'session_revoked' AND NEW.reason = 'local_operator'
		BEGIN SELECT RAISE(ABORT, 'reject local session event'); END`); err != nil {
		t.Fatal(err)
	}
	if result, err := manager.RevokeUserSessionsLocally(t.Context(), "target"); result != nil || err == nil {
		t.Fatalf("RevokeUserSessionsLocally(audit failure) = %#v, %v", result, err)
	}
	var revokedAt sql.NullTime
	var events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT revoked_at FROM sessions WHERE id = 'rollback-session'`,
	).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if revokedAt.Valid || events != 0 {
		t.Fatalf("audit rollback state = revokedAt:%#v events:%d", revokedAt, events)
	}
}

func TestRevokeUserSessionsLocallyDoesNotMutateWhenEventIDGenerationFails(t *testing.T) {
	now := time.Date(2026, time.August, 8, 14, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		idErr: errors.New("random source failed"),
	})
	insertRedemptionUser(t, manager, "target", UserStatusActive, now)
	insertRedemptionSession(t, manager, "active-session", "target", now)
	if result, err := manager.RevokeUserSessionsLocally(t.Context(), "target"); result != nil || err == nil {
		t.Fatalf("RevokeUserSessionsLocally(generator failure) = %#v, %v", result, err)
	}
	var active int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sessions WHERE user_id = 'target' AND revoked_at IS NULL`,
	).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active sessions after generator failure = %d, %v", active, err)
	}
}
