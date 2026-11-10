package auth

import (
	"context"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
)

func auditRecords(t *testing.T, manager *Manager) []audit.Record {
	t.Helper()
	page, err := audit.List(context.Background(), manager.db.Read(), audit.Filter{}, 0, audit.MaxPageSize)
	if err != nil {
		t.Fatal(err)
	}
	// Oldest first reads more naturally in the assertions.
	records := page.Records
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}
	return records
}

func TestAdministratorChangesAreAudited(t *testing.T) {
	now := time.Date(2026, time.November, 10, 6, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "head-admin", true, now)
	insertActiveUser(t, manager, "milo-rivers", false, now)
	session := insertEnrollmentStepUpSession(t, manager, "head-admin", now, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `INSERT INTO auth_system_state (id, initialized, owner_user_id) VALUES (1, 1, 'head-admin')`); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	if _, err := manager.SetAdministratorUserStatus(ctx, SetAdministratorUserStatusOptions{
		ActorUserID: "head-admin", ActorSessionID: session, TargetUserID: "milo-rivers", Status: UserStatusDisabled,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetAdministratorUserMFAPolicy(ctx, SetAdministratorUserMFAPolicyOptions{
		ActorUserID: "head-admin", ActorSessionID: session, TargetUserID: "milo-rivers", Required: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetAdministratorUserStatus(ctx, SetAdministratorUserStatusOptions{
		ActorUserID: "head-admin", ActorSessionID: session, TargetUserID: "milo-rivers", Status: UserStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetAuthenticationEventRetention(ctx, SetAuthenticationEventRetentionOptions{
		ActorUserID: "head-admin", ActorSessionID: session, Days: 90,
	}); err != nil {
		t.Fatal(err)
	}
	// Repeating a change that changes nothing records nothing.
	if _, err := manager.SetAdministratorUserStatus(ctx, SetAdministratorUserStatusOptions{
		ActorUserID: "head-admin", ActorSessionID: session, TargetUserID: "milo-rivers", Status: UserStatusActive,
	}); err != nil {
		t.Fatal(err)
	}

	records := auditRecords(t, manager)
	want := []struct {
		action, target, field string
		from, to              any
	}{
		{"user.disabled", "milo-rivers", "status", "active", "disabled"},
		{"user.mfa_required", "milo-rivers", "mfa_required", false, true},
		{"user.enabled", "milo-rivers", "status", "disabled", "active"},
		{"instance.security_retention_changed", "", "security_event_retention_days", float64(180), float64(90)},
	}
	if len(records) != len(want) {
		t.Fatalf("audit records = %+v", records)
	}
	for index, expected := range want {
		record := records[index]
		change := record.Changes[expected.field]
		if record.Action != expected.action || record.Target.ID != expected.target || record.Target.Label != expected.target ||
			record.Actor.Kind != audit.ActorAdmin || record.Actor.ID != "head-admin" || record.Actor.Label != "head-admin" ||
			record.Actor.SessionID != session || change.From != expected.from || change.To != expected.to {
			t.Errorf("record %d = %+v, want %+v", index, record, expected)
		}
		var events int
		if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE id = ?`, record.CorrelationID).Scan(&events); err != nil || events != 1 {
			t.Errorf("record %d isn't tied to its security event %q: %d, %v", index, record.CorrelationID, events, err)
		}
	}
	if records[0].Metadata["revoked_sessions"] != float64(0) {
		t.Errorf("disable metadata = %v", records[0].Metadata)
	}
	verified, err := audit.Verify(ctx, manager.db.Read(), manager.AuditKey())
	if err != nil || !verified.OK() || verified.Checked != 4 {
		t.Fatalf("audit chain = %+v, %v", verified, err)
	}
}

func TestAdministratorChangeRollsBackWhenItsAuditFails(t *testing.T) {
	now := time.Date(2026, time.November, 10, 6, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "head-admin", true, now)
	insertActiveUser(t, manager, "milo-rivers", false, now)
	session := insertEnrollmentStepUpSession(t, manager, "head-admin", now, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER audit_unavailable BEFORE INSERT ON audit_events
		BEGIN SELECT RAISE(ABORT, 'audit log unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetAdministratorUserStatus(t.Context(), SetAdministratorUserStatusOptions{
		ActorUserID: "head-admin", ActorSessionID: session, TargetUserID: "milo-rivers", Status: UserStatusDisabled,
	}); err == nil {
		t.Fatal("a change was made without its audit record")
	}
	var status string
	var events int
	if err := manager.db.Read().QueryRow(`SELECT status FROM users WHERE id = 'milo-rivers'`).Scan(&status); err != nil || status != string(UserStatusActive) {
		t.Fatalf("status after the failed audit = %q, %v", status, err)
	}
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE subject_user_id = 'milo-rivers'`).Scan(&events); err != nil || events != 0 {
		t.Fatalf("security events after the failed audit = %d, %v", events, err)
	}
}

func TestAuditEventsArePrunedAfterTheirRetention(t *testing.T) {
	now := time.Date(2026, time.November, 10, 6, 30, 0, 0, time.UTC)
	clock := &fixedClock{now: now.Add(-100 * 24 * time.Hour)}
	manager := newDeterministicManager(t, clock, secureTokenGenerator{})
	insertActiveUser(t, manager, "head-admin", true, now)
	insertActiveUser(t, manager, "milo-rivers", false, now)
	session := insertEnrollmentStepUpSession(t, manager, "head-admin", clock.now, clock.now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `INSERT INTO auth_system_state (id, initialized, owner_user_id, admin_audit_retention_days) VALUES (1, 1, 'head-admin', 90)`); err != nil {
		t.Fatal(err)
	}
	// A change 100 days ago, outside the 90-day window, and one today.
	if _, err := manager.SetAdministratorUserStatus(t.Context(), SetAdministratorUserStatusOptions{
		ActorUserID: "head-admin", ActorSessionID: session, TargetUserID: "milo-rivers", Status: UserStatusDisabled,
	}); err != nil {
		t.Fatal(err)
	}
	clock.now = now
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE sessions SET step_up_at = ?, idle_expires_at = ?, absolute_expires_at = ? WHERE id = ?`,
		now, now.Add(time.Hour), now.Add(24*time.Hour), session,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SetAdministratorUserStatus(t.Context(), SetAdministratorUserStatusOptions{
		ActorUserID: "head-admin", ActorSessionID: session, TargetUserID: "milo-rivers", Status: UserStatusActive,
	}); err != nil {
		t.Fatal(err)
	}

	deleted, cutoff, err := manager.PruneAuditEvents(t.Context(), now, AuditPruneBatchSize)
	if err != nil || deleted != 1 || !cutoff.Equal(now.Add(-90*24*time.Hour)) {
		t.Fatalf("PruneAuditEvents = %d, %v, %v", deleted, cutoff, err)
	}
	if err := manager.RecordAuditPruning(t.Context(), deleted, cutoff); err != nil {
		t.Fatal(err)
	}
	records := auditRecords(t, manager)
	if len(records) != 2 || records[0].Action != "user.enabled" || records[1].Action != "audit.pruned" ||
		records[1].Actor.Kind != audit.ActorSystem || records[1].Metadata["deleted"] != float64(1) {
		t.Fatalf("records after pruning = %+v", records)
	}
	verified, err := audit.Verify(t.Context(), manager.db.Read(), manager.AuditKey())
	if err != nil || !verified.OK() || verified.Checked != 2 {
		t.Fatalf("chain after pruning = %+v, %v", verified, err)
	}
	if deleted, _, err := manager.PruneAuditEvents(t.Context(), now, AuditPruneBatchSize); err != nil || deleted != 0 {
		t.Fatalf("second pruning = %d, %v", deleted, err)
	}
}
