package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/authz"
)

func TestRefusedAdministrativeAttemptsAreRecordedOncePerWindow(t *testing.T) {
	now := time.Date(2026, time.November, 10, 6, 30, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, secureTokenGenerator{})
	insertActiveUser(t, manager, "head-admin", true, now)
	insertActiveUser(t, manager, "milo-rivers", false, now)
	stale := insertEnrollmentStepUpSession(t, manager, "head-admin", now.Add(-time.Hour), now)
	disable := func() error {
		ctx, record := manager.WithDeniedAttempts(t.Context())
		_, err := manager.SetAdministratorUserStatus(ctx, SetAdministratorUserStatusOptions{
			ActorUserID: "head-admin", ActorSessionID: stale, TargetUserID: "milo-rivers", Status: UserStatusDisabled,
		})
		record()
		return err
	}

	for range 3 {
		if err := disable(); !errors.Is(err, ErrRecentStepUpRequired) {
			t.Fatalf("stale disable = %v", err)
		}
	}
	records := auditRecords(t, manager)
	if len(records) != 1 {
		t.Fatalf("one refusal in its window should be recorded once: %+v", records)
	}
	refused := records[0]
	if refused.Action != AuditActionAccessDenied || refused.Outcome != audit.OutcomeDenied ||
		refused.Target.Kind != "permission" || refused.Target.ID != string(authz.UsersAccess) ||
		refused.Metadata["reason"] != DeniedReasonVerificationRequired ||
		refused.Actor.Kind != audit.ActorAdmin || refused.Actor.ID != "head-admin" || refused.Actor.SessionID != stale {
		t.Fatalf("refusal record = %+v", refused)
	}
	var status string
	if err := manager.db.Read().QueryRow(`SELECT status FROM users WHERE id = 'milo-rivers'`).Scan(&status); err != nil || status != "active" {
		t.Fatalf("the refused change happened anyway: %q, %v", status, err)
	}

	clock.now = now.Add(deniedAttemptWindow)
	if err := disable(); !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale disable after the window = %v", err)
	}
	if records := auditRecords(t, manager); len(records) != 2 {
		t.Fatalf("a refusal after its window should be recorded again: %+v", records)
	}

	// Someone who isn't an administrator is refused for want of the
	// permission.
	ordinary := insertEnrollmentStepUpSession(t, manager, "milo-rivers", clock.now, clock.now)
	ctx, record := manager.WithDeniedAttempts(t.Context())
	if _, err := manager.SetAdminAuditRetention(ctx, "milo-rivers", ordinary, 365); !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("ordinary retention = %v", err)
	}
	record()
	records = auditRecords(t, manager)
	if last := records[len(records)-1]; len(records) != 3 || last.Metadata["reason"] != DeniedReasonForbidden ||
		last.Target.ID != string(authz.ActivityRetention) || last.Actor.ID != "milo-rivers" {
		t.Fatalf("forbidden refusal = %+v", records)
	}

	// Refusals made without a request to note them on, such as from the
	// command line, aren't recorded; neither are failures that aren't
	// refusals.
	if _, err := manager.SetAdministratorUserStatus(t.Context(), SetAdministratorUserStatusOptions{
		ActorUserID: "head-admin", ActorSessionID: stale, TargetUserID: "milo-rivers", Status: UserStatusDisabled,
	}); !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("unnoted stale disable = %v", err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE sessions SET step_up_at = ? WHERE id = ?`, clock.now, stale); err != nil {
		t.Fatal(err)
	}
	ctx, record = manager.WithDeniedAttempts(t.Context())
	if _, err := manager.SetAdministratorUserStatus(ctx, SetAdministratorUserStatusOptions{
		ActorUserID: "head-admin", ActorSessionID: stale, TargetUserID: "nobody", Status: UserStatusDisabled,
	}); err == nil {
		t.Fatal("disabled someone who doesn't exist")
	}
	record()
	if records := auditRecords(t, manager); len(records) != 3 {
		t.Fatalf("records = %+v", records)
	}

	verified, err := audit.Verify(t.Context(), manager.db.Read(), manager.AuditKey())
	if err != nil || !verified.OK() {
		t.Fatalf("audit chain = %+v, %v", verified, err)
	}
}
