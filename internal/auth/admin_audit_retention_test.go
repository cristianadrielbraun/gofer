package auth

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
)

func TestAdminAuditRetentionIsBoundedVerifiedAndRecorded(t *testing.T) {
	now := time.Date(2026, time.November, 10, 6, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "head-admin", true, now)
	insertActiveUser(t, manager, "milo-rivers", false, now)
	session := insertEnrollmentStepUpSession(t, manager, "head-admin", now, now)
	ordinary := insertEnrollmentStepUpSession(t, manager, "milo-rivers", now, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `INSERT INTO auth_system_state (id, initialized, owner_user_id) VALUES (1, 1, 'head-admin')`); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if days, err := manager.AdminAuditRetention(ctx); err != nil || days != DefaultAdminAuditRetentionDays {
		t.Fatalf("default retention = %d, %v", days, err)
	}
	for _, days := range []int{MinimumAdminAuditRetentionDays - 1, MaximumAdminAuditRetentionDays + 1} {
		if _, err := manager.SetAdminAuditRetention(ctx, "head-admin", session, days); !errors.Is(err, ErrAdminAuditRetentionInvalid) {
			t.Fatalf("retention %d = %v", days, err)
		}
	}
	if _, err := manager.SetAdminAuditRetention(ctx, "milo-rivers", ordinary, 365); !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("ordinary user's retention = %v", err)
	}
	changed, err := manager.SetAdminAuditRetention(ctx, "head-admin", session, 365)
	if err != nil || !changed {
		t.Fatalf("SetAdminAuditRetention = %v, %v", changed, err)
	}
	if changed, err := manager.SetAdminAuditRetention(ctx, "head-admin", session, 365); err != nil || changed {
		t.Fatalf("setting the same retention = %v, %v", changed, err)
	}
	if days, err := manager.AdminAuditRetention(ctx); err != nil || days != 365 {
		t.Fatalf("retention = %d, %v", days, err)
	}

	var out bytes.Buffer
	written, err := manager.ExportAdministrationEvents(ctx, "head-admin", session, audit.Filter{}, audit.FormatJSONL, &out)
	if err != nil || written != 2 || !strings.Contains(out.String(), `"action":"audit.exported"`) || !strings.Contains(out.String(), `"action":"instance.audit_retention_changed"`) {
		t.Fatalf("export = %d, %v, %q", written, err, out.String())
	}
	records := auditRecords(t, manager)
	if len(records) != 2 || records[0].Changes["admin_event_retention_days"].To != float64(365) || records[1].Metadata["format"] != "jsonl" {
		t.Fatalf("records = %+v", records)
	}

	if _, err := manager.db.Write().ExecContext(ctx, `UPDATE sessions SET step_up_at = ? WHERE id = ?`, now.Add(-time.Hour), session); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if _, err := manager.ExportAdministrationEvents(ctx, "head-admin", session, audit.Filter{}, audit.FormatCSV, &out); !errors.Is(err, ErrRecentStepUpRequired) || out.Len() != 0 {
		t.Fatalf("stale export = %v, %q", err, out.String())
	}
}
