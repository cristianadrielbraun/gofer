package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
)

const (
	// DefaultAdminAuditRetentionDays keeps administration events two years
	// unless an administrator chooses between 90 and 3,650 days.
	DefaultAdminAuditRetentionDays = 730
	// AuditPruneBatchSize bounds one pruning transaction.
	AuditPruneBatchSize = 500
)

// What the administration audit log names the targets of auth's changes.
const (
	auditTargetUser     = "user"
	auditTargetInstance = "instance"
)

// auditAdministratorChange records, inside the change's transaction, a
// change the administrator actorUserID made from actorSessionID. If the
// record can't be written, the change fails with it. People are named as
// they are now, so the record still reads after renames and deletions.
func (m *Manager) auditAdministratorChange(ctx context.Context, tx *sql.Tx, actorUserID, actorSessionID string, event audit.Event) error {
	label, err := usernameForAudit(ctx, tx, actorUserID)
	if err != nil {
		return err
	}
	event.Actor = audit.Actor{Kind: audit.ActorAdmin, ID: actorUserID, Label: label, SessionID: actorSessionID}
	return m.auditChange(ctx, tx, event)
}

// auditChange records event, naming a user target that has no name yet.
func (m *Manager) auditChange(ctx context.Context, tx *sql.Tx, event audit.Event) error {
	if event.Target.Kind == auditTargetUser && event.Target.Label == "" {
		label, err := usernameForAudit(ctx, tx, event.Target.ID)
		if err != nil {
			return err
		}
		event.Target.Label = label
	}
	if _, err := m.audit.WriteTx(ctx, tx, event); err != nil {
		return fmt.Errorf("record administrative change %s: %w", event.Action, err)
	}
	return nil
}

func usernameForAudit(ctx context.Context, tx *sql.Tx, userID string) (string, error) {
	if userID == "" {
		return "", nil
	}
	var username string
	err := tx.QueryRowContext(ctx, `SELECT username FROM users WHERE id = ?`, userID).Scan(&username)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("name person for the audit log: %w", err)
	}
	return username, nil
}

// RecordAdministratorChangeTx records, inside tx, a change the
// administrator made outside auth's own transitions, such as a mail
// security setting (storage.WithChangeRecorder).
func (m *Manager) RecordAdministratorChangeTx(ctx context.Context, tx *sql.Tx, actorUserID, actorSessionID string, event audit.Event) error {
	return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, event)
}

// enrollmentTokenAuditAction names issuing or revoking a token of purpose:
// invitations for people who haven't joined, reset tokens for those who
// have.
func enrollmentTokenAuditAction(purpose EnrollmentTokenPurpose, verb string) string {
	if purpose == EnrollmentTokenPurposeEnrollment {
		return "invitation." + verb
	}
	return "user.credential_reset_" + verb
}

// PruneAuditEvents removes one batch of administration events older than
// their retention, and says how many went and the cutoff it used. The
// chain still verifies from the events left (audit.Prune).
func (m *Manager) PruneAuditEvents(ctx context.Context, now time.Time, batch int) (int64, time.Time, error) {
	tx, err := m.db.Write().BeginTx(ctx, nil)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("begin administration event pruning: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	days := DefaultAdminAuditRetentionDays
	err = tx.QueryRowContext(ctx, `SELECT admin_audit_retention_days FROM auth_system_state WHERE id = 1`).Scan(&days)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, fmt.Errorf("read administration event retention: %w", err)
	}
	cutoff := now.UTC().Add(-time.Duration(days) * 24 * time.Hour)
	deleted, err := audit.Prune(ctx, tx, cutoff, batch, now)
	if err != nil {
		return 0, time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return 0, time.Time{}, fmt.Errorf("commit administration event pruning: %w", err)
	}
	return deleted, cutoff, nil
}

// RecordAuditPruning notes in the log itself that retention removed
// deleted events from before cutoff.
func (m *Manager) RecordAuditPruning(ctx context.Context, deleted int64, cutoff time.Time) error {
	tx, err := m.db.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin recording administration event pruning: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := m.auditChange(ctx, tx, audit.Event{
		Action:   "audit.pruned",
		Actor:    audit.Actor{Kind: audit.ActorSystem},
		Target:   audit.Target{Kind: auditTargetInstance},
		Metadata: map[string]any{"deleted": deleted, "older_than": cutoff.UTC()},
	}); err != nil {
		return err
	}
	return tx.Commit()
}

// AuditKey is the key that chains the administration audit log, for
// verifying it.
func (m *Manager) AuditKey() []byte {
	return m.audit.Key()
}
