package auth

import (
	"context"
	"database/sql"
	"fmt"
)

const securityEventPageSize int64 = 20

// GetSecurityEventOverview returns only the number of events whose exact
// subject is the recently verified current user.
func (m *Manager) GetSecurityEventOverview(ctx context.Context, sessionToken string) (*SecurityEventOverview, error) {
	now := m.clock.Now().UTC()
	tx, err := m.db.Read().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin security event overview: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := m.currentSecuritySession(ctx, tx, sessionToken, now, true)
	if err != nil {
		return nil, err
	}
	totalEvents, err := countSecurityEvents(ctx, tx, current.UserID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit security event overview: %w", err)
	}
	return &SecurityEventOverview{TotalEvents: totalEvents}, nil
}

// ListSecurityEventPage returns one bounded newest-first page of events whose
// exact subject is the recently verified current user. Deliberately omitted
// fields include event, actor, session, request, and source identifiers plus
// raw event metadata.
func (m *Manager) ListSecurityEventPage(
	ctx context.Context, sessionToken string, requestedPage int64,
) (*SecurityEventPage, error) {
	now := m.clock.Now().UTC()
	tx, err := m.db.Read().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin security event page: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := m.currentSecuritySession(ctx, tx, sessionToken, now, true)
	if err != nil {
		return nil, err
	}
	totalEvents, err := countSecurityEvents(ctx, tx, current.UserID)
	if err != nil {
		return nil, err
	}
	totalPages := int64(1)
	if totalEvents > 0 {
		totalPages = (totalEvents-1)/securityEventPageSize + 1
	}
	page := requestedPage
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * securityEventPageSize
	rows, err := tx.QueryContext(ctx, `
		SELECT occurred_at, event_type, success, reason, user_agent
		FROM auth_events
		WHERE subject_user_id = ?
		ORDER BY occurred_at DESC, id DESC
		LIMIT ? OFFSET ?`, current.UserID, securityEventPageSize, offset,
	)
	if err != nil {
		return nil, fmt.Errorf("list security event page: %w", err)
	}
	defer rows.Close()
	result := &SecurityEventPage{
		Events:      make([]SecurityEventSummary, 0, int(securityEventPageSize)),
		TotalEvents: totalEvents,
		Page:        page,
		TotalPages:  totalPages,
		PageSize:    securityEventPageSize,
	}
	for rows.Next() {
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
		return nil, fmt.Errorf("iterate security event page: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close security event page: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit security event page: %w", err)
	}
	return result, nil
}

func countSecurityEvents(ctx context.Context, tx *sql.Tx, userID string) (int64, error) {
	var count int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM auth_events WHERE subject_user_id = ?`, userID,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("count security events: %w", err)
	}
	return count, nil
}
