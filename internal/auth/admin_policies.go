package auth

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/authz"
	"github.com/cristianadrielbraun/gofer/internal/policy"
)

// DefaultPolicy works out the Default policy's settings, for the Policies
// page: what applies to everyone no other policy changes.
func (m *Manager) DefaultPolicy(ctx context.Context, actorUserID string) (*policy.Effective, error) {
	tx, err := m.db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin Default policy read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := authz.Require(ctx, tx, strings.TrimSpace(actorUserID), authz.PoliciesRead); err != nil {
		return nil, err
	}
	return policy.ForInstance(ctx, tx)
}

// CountAuthenticationEventsOlderThan counts the sign-in and security
// events that keeping them only days would remove now, for the change's
// confirmation.
func (m *Manager) CountAuthenticationEventsOlderThan(ctx context.Context, actorUserID string, days int) (int64, error) {
	tx, err := m.db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, fmt.Errorf("begin security event count: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := authz.Require(ctx, tx, strings.TrimSpace(actorUserID), authz.ActivityRetention); err != nil {
		return 0, err
	}
	// The same cutoff pruning uses.
	cutoff := m.clock.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	var count int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_events WHERE occurred_at < ?`, cutoff).Scan(&count); err != nil {
		return 0, fmt.Errorf("count security events to remove: %w", err)
	}
	return count, nil
}
