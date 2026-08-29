package auth

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestRequestPasswordResetRecordsOnePrivateIdempotentWebmailRequest(t *testing.T) {
	now := time.Date(2026, time.August, 29, 10, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{
		ids: []string{"request-event-one", "request-event-two"},
	})
	insertActiveUser(t, manager, "webmail", false, now.Add(-time.Hour))
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE users SET username = 'Mail.User', username_normalized = 'mail.user'
		WHERE id = 'webmail'`); err != nil {
		t.Fatal(err)
	}

	options := PasswordResetRequestOptions{
		Identifier: "  MAIL.User  ", Source: "198.51.100.70", UserAgent: "  Request Browser/1.0  ",
	}
	if err := manager.RequestPasswordReset(t.Context(), options); err != nil {
		t.Fatalf("RequestPasswordReset() error = %v", err)
	}
	clock.now = now.Add(10 * time.Minute)
	if err := manager.RequestPasswordReset(t.Context(), options); err != nil {
		t.Fatalf("RequestPasswordReset(repeated) error = %v", err)
	}

	var requestedAt time.Time
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT password_reset_requested_at FROM users WHERE id = 'webmail'`,
	).Scan(&requestedAt); err != nil {
		t.Fatal(err)
	}
	if !requestedAt.Equal(now) {
		t.Fatalf("password reset requested at = %v, want stable %v", requestedAt, now)
	}
	var eventCount int
	var eventType, reason, userAgent, sourceHash, metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*), event_type, reason, user_agent, source_hash, metadata_json
		FROM auth_events WHERE subject_user_id = 'webmail'`,
	).Scan(&eventCount, &eventType, &reason, &userAgent, &sourceHash, &metadata); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 || eventType != string(AuthEventCredentialResetRequested) ||
		reason != string(AuthEventReasonUserAction) || userAgent != "Request Browser/1.0" ||
		sourceHash == "" || sourceHash == options.Source || metadata != "{}" {
		t.Fatalf("password reset request event = count:%d type:%q reason:%q agent:%q source:%q metadata:%q", eventCount, eventType, reason, userAgent, sourceHash, metadata)
	}
	if strings.Contains(sourceHash, "mail.user") || strings.Contains(metadata, "mail.user") || strings.Contains(metadata, options.Source) {
		t.Fatal("password reset request audit data exposed submitted identity or source")
	}
	var throttleCount int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_throttle WHERE action = ?`, passwordResetRequestThrottleAction,
	).Scan(&throttleCount); err != nil || throttleCount != 3 {
		t.Fatalf("password reset request throttle rows = %d, %v", throttleCount, err)
	}
}

func TestRequestPasswordResetHidesAndIgnoresIneligibleAccounts(t *testing.T) {
	now := time.Date(2026, time.August, 29, 11, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"pending-event", "management-event", "unknown-event"},
	})
	insertEnrollmentTokenUser(t, manager, "pending", UserStatusPending, false, now)
	insertEnrollmentTokenUser(t, manager, "management", UserStatusActive, true, now)

	for _, identifier := range []string{"pending", "management", "unknown"} {
		if err := manager.RequestPasswordReset(t.Context(), PasswordResetRequestOptions{
			Identifier: identifier, Source: "source-" + identifier, UserAgent: "private request test",
		}); err != nil {
			t.Fatalf("RequestPasswordReset(%q) error = %v", identifier, err)
		}
	}
	var requested, events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM users WHERE password_reset_requested_at IS NOT NULL`,
	).Scan(&requested); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_events WHERE event_type = ?`, AuthEventCredentialResetRequested,
	).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if requested != 0 || events != 0 {
		t.Fatalf("ineligible password reset state = requests:%d events:%d", requested, events)
	}
}

func TestRequestPasswordResetDoesNotCreateStateWhileThrottled(t *testing.T) {
	now := time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{ids: []string{"blocked-event"}})
	insertActiveUser(t, manager, "blocked-user", false, now)
	buckets, err := manager.authenticationThrottleBuckets(passwordResetRequestThrottleAction, "blocked-user", "198.51.100.80")
	if err != nil {
		t.Fatal(err)
	}
	identifierBucket := buckets[0]
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_throttle (
			bucket_hash, action, failure_count, first_attempt_at, last_attempt_at, blocked_until, expires_at
		) VALUES (?, ?, 5, ?, ?, ?, ?)`,
		identifierBucket.hash, identifierBucket.action, now.Add(-time.Minute), now, now.Add(time.Minute), now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if err := manager.RequestPasswordReset(t.Context(), PasswordResetRequestOptions{
		Identifier: "blocked-user", Source: "198.51.100.80", UserAgent: "blocked browser",
	}); err != nil {
		t.Fatalf("RequestPasswordReset(throttled) error = %v", err)
	}
	var requestedAt sql.NullTime
	var events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT password_reset_requested_at FROM users WHERE id = 'blocked-user'`).Scan(&requestedAt); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events WHERE event_type = ?`, AuthEventCredentialResetRequested).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if requestedAt.Valid || events != 0 {
		t.Fatalf("throttled password reset request = requested:%v events:%d", requestedAt, events)
	}
}

func TestRequestPasswordResetRollsBackStateAndThrottleWhenAuditFails(t *testing.T) {
	now := time.Date(2026, time.August, 29, 13, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{ids: []string{"failed-event"}})
	insertActiveUser(t, manager, "rollback-user", false, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_password_reset_request_event
		BEFORE INSERT ON auth_events
		WHEN NEW.event_type = 'credential_reset_requested'
		BEGIN SELECT RAISE(ABORT, 'forced audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := manager.RequestPasswordReset(t.Context(), PasswordResetRequestOptions{
		Identifier: "rollback-user", Source: "198.51.100.90", UserAgent: "rollback browser",
	}); err == nil {
		t.Fatal("RequestPasswordReset() succeeded despite audit failure")
	}
	var requestedAt sql.NullTime
	var throttleCount int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT password_reset_requested_at FROM users WHERE id = 'rollback-user'`).Scan(&requestedAt); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_throttle WHERE action = ?`, passwordResetRequestThrottleAction).Scan(&throttleCount); err != nil {
		t.Fatal(err)
	}
	if requestedAt.Valid || throttleCount != 0 {
		t.Fatalf("rolled-back password reset request = requested:%v throttle:%d", requestedAt, throttleCount)
	}
}
