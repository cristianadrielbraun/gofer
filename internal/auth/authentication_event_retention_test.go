package auth

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAuthenticationEventRetentionRequiresVerifiedAdministratorAndAuditsChange(t *testing.T) {
	now := time.Date(2026, time.September, 2, 7, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "ordinary", false, now)
	adminSessionID := insertEnrollmentStepUpSession(t, manager, "administrator", now, now)
	ordinarySessionID := insertEnrollmentStepUpSession(t, manager, "ordinary", now, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (id, initialized, owner_user_id)
		VALUES (1, 1, 'administrator')`); err != nil {
		t.Fatal(err)
	}

	policy, err := manager.AuthenticationEventRetention(t.Context())
	if err != nil || policy.Days != DefaultAuthenticationEventRetentionDays {
		t.Fatalf("default authentication event retention = %#v, %v", policy, err)
	}
	for _, days := range []int{0, MaximumAuthenticationEventRetentionDays + 1} {
		if result, err := manager.SetAuthenticationEventRetention(t.Context(), SetAuthenticationEventRetentionOptions{
			ActorUserID: "administrator", ActorSessionID: adminSessionID, Days: days,
		}); result != nil || !errors.Is(err, ErrAuthenticationEventRetentionInvalid) {
			t.Fatalf("invalid retention %d = %#v, %v", days, result, err)
		}
	}
	if result, err := manager.SetAuthenticationEventRetention(t.Context(), SetAuthenticationEventRetentionOptions{
		ActorUserID: "ordinary", ActorSessionID: ordinarySessionID, Days: 365,
	}); result != nil || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("ordinary retention change = %#v, %v", result, err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE sessions SET step_up_at = ? WHERE id = ?`, now.Add(-securityStepUpMaximumAge-time.Second), adminSessionID,
	); err != nil {
		t.Fatal(err)
	}
	if result, err := manager.SetAuthenticationEventRetention(t.Context(), SetAuthenticationEventRetentionOptions{
		ActorUserID: "administrator", ActorSessionID: adminSessionID, Days: 365,
	}); result != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale retention change = %#v, %v", result, err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE sessions SET step_up_at = ? WHERE id = ?`, now, adminSessionID,
	); err != nil {
		t.Fatal(err)
	}

	result, err := manager.SetAuthenticationEventRetention(t.Context(), SetAuthenticationEventRetentionOptions{
		ActorUserID: "administrator", ActorSessionID: adminSessionID, Days: 365,
	})
	if err != nil || result == nil || !result.Changed || result.Policy.Days != 365 ||
		result.Policy.UpdatedAt == nil || !result.Policy.UpdatedAt.Equal(now) || result.Policy.UpdatedBy != "administrator" {
		t.Fatalf("authentication event retention change = %#v, %v", result, err)
	}
	var storedDays, events int
	var updatedAt time.Time
	var updatedBy, eventType, reason, metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT auth_event_retention_days, auth_event_retention_updated_at,
		       auth_event_retention_updated_by
		FROM auth_system_state WHERE id = 1`,
	).Scan(&storedDays, &updatedAt, &updatedBy); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT event_type, reason, metadata_json FROM auth_events
		WHERE event_type = ? ORDER BY occurred_at DESC, id DESC LIMIT 1`,
		AuthEventSecurityPolicyChanged,
	).Scan(&eventType, &reason, &metadata); err != nil {
		t.Fatal(err)
	}
	if storedDays != 365 || !updatedAt.Equal(now) || updatedBy != "administrator" ||
		eventType != string(AuthEventSecurityPolicyChanged) || reason != string(AuthEventReasonAdministratorAction) ||
		metadata != `{"setting":"authentication_event_retention_days","from_days":180,"to_days":365}` {
		t.Fatalf("stored authentication event retention = days:%d at:%v by:%q event:%q reason:%q metadata:%q",
			storedDays, updatedAt, updatedBy, eventType, reason, metadata)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_events WHERE event_type = ?`, AuthEventSecurityPolicyChanged,
	).Scan(&events); err != nil {
		t.Fatal(err)
	}
	unchanged, err := manager.SetAuthenticationEventRetention(t.Context(), SetAuthenticationEventRetentionOptions{
		ActorUserID: "administrator", ActorSessionID: adminSessionID, Days: 365,
	})
	if err != nil || unchanged == nil || unchanged.Changed || unchanged.Policy.Days != 365 {
		t.Fatalf("unchanged authentication event retention = %#v, %v", unchanged, err)
	}
	var eventsAfter int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_events WHERE event_type = ?`, AuthEventSecurityPolicyChanged,
	).Scan(&eventsAfter); err != nil || eventsAfter != events {
		t.Fatalf("unchanged retention events = %d, %v; want %d", eventsAfter, err, events)
	}
}

func TestPruneAuthenticationEventsUsesConfiguredWindowAndBoundedBatches(t *testing.T) {
	now := time.Date(2026, time.September, 2, 8, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (id, initialized, owner_user_id, auth_event_retention_days)
		VALUES (1, 1, 'administrator', 30)`); err != nil {
		t.Fatal(err)
	}
	for index, occurredAt := range []time.Time{
		now.Add(-40 * 24 * time.Hour),
		now.Add(-39 * 24 * time.Hour),
		now.Add(-38 * 24 * time.Hour),
		now.Add(-30 * 24 * time.Hour),
		now.Add(-29 * 24 * time.Hour),
	} {
		if _, err := manager.db.Write().ExecContext(t.Context(), `
			INSERT INTO auth_events (id, occurred_at, event_type, success, reason, metadata_json)
			VALUES (?, ?, ?, 1, ?, '{}')`,
			fmt.Sprintf("retention-event-%d", index), occurredAt,
			AuthEventLoginSucceeded, AuthEventReasonChallengeVerified,
		); err != nil {
			t.Fatal(err)
		}
	}

	first, err := manager.PruneAuthenticationEvents(t.Context(), now, 2)
	if err != nil || first.Deleted != 2 || first.RetentionDays != 30 ||
		!first.Cutoff.Equal(now.Add(-30*24*time.Hour)) {
		t.Fatalf("first authentication event prune = %#v, %v", first, err)
	}
	second, err := manager.PruneAuthenticationEvents(t.Context(), now, 2)
	if err != nil || second.Deleted != 1 {
		t.Fatalf("second authentication event prune = %#v, %v", second, err)
	}
	third, err := manager.PruneAuthenticationEvents(t.Context(), now, 2)
	if err != nil || third.Deleted != 0 {
		t.Fatalf("third authentication event prune = %#v, %v", third, err)
	}
	rows, err := manager.db.Read().QueryContext(t.Context(), `
		SELECT id FROM auth_events ORDER BY occurred_at ASC, id ASC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	remaining := make([]string, 0, 2)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		remaining = append(remaining, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(remaining, ","); got != "retention-event-3,retention-event-4" {
		t.Fatalf("remaining authentication events = %q", got)
	}
}

func TestAuthenticationEventRetentionChangeRollsBackWhenAuditFails(t *testing.T) {
	now := time.Date(2026, time.September, 2, 10, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	adminSessionID := insertEnrollmentStepUpSession(t, manager, "administrator", now, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (id, initialized, owner_user_id)
		VALUES (1, 1, 'administrator');
		CREATE TRIGGER reject_retention_audit
		BEFORE INSERT ON auth_events
		WHEN NEW.metadata_json LIKE '%authentication_event_retention_days%'
		BEGIN
			SELECT RAISE(ABORT, 'reject retention audit');
		END`); err != nil {
		t.Fatal(err)
	}

	result, err := manager.SetAuthenticationEventRetention(t.Context(), SetAuthenticationEventRetentionOptions{
		ActorUserID: "administrator", ActorSessionID: adminSessionID, Days: 90,
	})
	if result != nil || err == nil {
		t.Fatalf("retention change with rejected audit = %#v, %v", result, err)
	}
	policy, loadErr := manager.AuthenticationEventRetention(t.Context())
	if loadErr != nil || policy.Days != DefaultAuthenticationEventRetentionDays || policy.UpdatedAt != nil || policy.UpdatedBy != "" {
		t.Fatalf("retention after audit rollback = %#v, %v", policy, loadErr)
	}
}
