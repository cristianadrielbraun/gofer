package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/authz"
)

const (
	// MinimumAdminAuditRetentionDays and MaximumAdminAuditRetentionDays
	// bound how long administration events are kept.
	MinimumAdminAuditRetentionDays = 90
	MaximumAdminAuditRetentionDays = 3650
)

// ErrAdminAuditRetentionInvalid is a retention outside the bounds.
var ErrAdminAuditRetentionInvalid = errors.New("administration event retention is out of bounds")

// AdminAuditRetention returns how many days administration events are kept.
func (m *Manager) AdminAuditRetention(ctx context.Context) (int, error) {
	days := DefaultAdminAuditRetentionDays
	err := m.db.Read().QueryRowContext(ctx, `SELECT admin_audit_retention_days FROM auth_system_state WHERE id = 1`).Scan(&days)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read administration event retention: %w", err)
	}
	return days, nil
}

// SetAdminAuditRetention keeps administration events for days, and says
// whether that changed anything. The change itself is recorded.
func (m *Manager) SetAdminAuditRetention(ctx context.Context, actorUserID, actorSessionID string, days int) (bool, error) {
	if days < MinimumAdminAuditRetentionDays || days > MaximumAdminAuditRetentionDays {
		return false, ErrAdminAuditRetentionInvalid
	}
	actorUserID, actorSessionID = strings.TrimSpace(actorUserID), strings.TrimSpace(actorSessionID)
	now := m.clock.Now().UTC()
	changed := false
	err := m.runSecurityTransition(ctx, SecurityTransitionPolicyChange, func(tx *sql.Tx) error {
		if err := requireAdministratorPermission(ctx, tx, actorUserID, actorSessionID, authz.ActivityRetention, now); err != nil {
			return err
		}
		current := DefaultAdminAuditRetentionDays
		err := tx.QueryRowContext(ctx, `SELECT admin_audit_retention_days FROM auth_system_state WHERE id = 1`).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("administration event retention needs an initialized instance")
		}
		if err != nil {
			return fmt.Errorf("read administration event retention: %w", err)
		}
		if current == days {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE auth_system_state SET admin_audit_retention_days = ? WHERE id = 1`, days); err != nil {
			return fmt.Errorf("save administration event retention: %w", err)
		}
		changed = true
		return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
			Action:  "instance.audit_retention_changed",
			Target:  audit.Target{Kind: auditTargetInstance},
			Changes: map[string]audit.Change{"admin_event_retention_days": {From: current, To: days}},
		})
	})
	return changed, err
}

// ExportAdministrationEvents writes the administration events filter keeps
// to out, for an administrator who may export them. The export is recorded
// first, in its own transaction, so a long download never holds the
// database's writer.
func (m *Manager) ExportAdministrationEvents(ctx context.Context, actorUserID, actorSessionID string, filter audit.Filter, format audit.Format, out io.Writer) (int64, error) {
	actorUserID, actorSessionID = strings.TrimSpace(actorUserID), strings.TrimSpace(actorSessionID)
	now := m.clock.Now().UTC()
	err := m.runSecurityTransition(ctx, SecurityTransitionPolicyChange, func(tx *sql.Tx) error {
		if err := requireAdministratorPermission(ctx, tx, actorUserID, actorSessionID, authz.ActivityExport, now); err != nil {
			return err
		}
		return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
			Action:   "audit.exported",
			Target:   audit.Target{Kind: auditTargetInstance},
			Metadata: map[string]any{"format": string(format), "action_filter": filter.Action},
		})
	})
	if err != nil {
		return 0, err
	}
	return audit.Export(ctx, m.db.Read(), filter, format, out)
}
