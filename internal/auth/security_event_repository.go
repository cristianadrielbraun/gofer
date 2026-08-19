package auth

import (
	"context"
	"fmt"
)

const securityEventListLimit = 50

// ListSecurityEvents returns a bounded newest-first view of events whose exact
// subject is the recently verified current user. Deliberately omitted fields
// include event, actor, session, request, and source identifiers plus raw event
// metadata.
func (m *Manager) ListSecurityEvents(ctx context.Context, sessionToken string) (*SecurityEventList, error) {
	now := m.clock.Now().UTC()
	tx, err := m.db.Read().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin security event list: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := currentSecuritySession(ctx, tx, sessionToken, now, true)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT occurred_at, event_type, success, reason, user_agent
		FROM auth_events
		WHERE subject_user_id = ?
		ORDER BY occurred_at DESC, id DESC
		LIMIT ?`, current.UserID, securityEventListLimit+1,
	)
	if err != nil {
		return nil, fmt.Errorf("list security events: %w", err)
	}
	defer rows.Close()
	result := &SecurityEventList{Events: make([]SecurityEventSummary, 0, securityEventListLimit)}
	for rows.Next() {
		if len(result.Events) == securityEventListLimit {
			result.Truncated = true
			break
		}
		var event SecurityEventSummary
		var success int
		if err := rows.Scan(
			&event.OccurredAt, &event.EventType, &success, &event.Reason, &event.UserAgent,
		); err != nil {
			return nil, fmt.Errorf("scan security event: %w", err)
		}
		event.Success = success == 1
		result.Events = append(result.Events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate security events: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close security event list: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit security event list: %w", err)
	}
	return result, nil
}
