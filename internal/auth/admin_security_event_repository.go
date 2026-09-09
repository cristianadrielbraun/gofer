package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const administratorSecurityEventPageSize int64 = 50

type AdministratorSecurityEventFilter string

const (
	AdministratorSecurityEventFilterAll        AdministratorSecurityEventFilter = ""
	AdministratorSecurityEventFilterFailures   AdministratorSecurityEventFilter = "failures"
	AdministratorSecurityEventFilterRecovery   AdministratorSecurityEventFilter = "recovery"
	AdministratorSecurityEventFilterPolicy     AdministratorSecurityEventFilter = "policy"
	AdministratorSecurityEventFilterIdentities AdministratorSecurityEventFilter = "identities"
	AdministratorSecurityEventFilterSessions   AdministratorSecurityEventFilter = "sessions"
)

func (filter AdministratorSecurityEventFilter) Valid() bool {
	switch filter {
	case AdministratorSecurityEventFilterAll,
		AdministratorSecurityEventFilterFailures,
		AdministratorSecurityEventFilterRecovery,
		AdministratorSecurityEventFilterPolicy,
		AdministratorSecurityEventFilterIdentities,
		AdministratorSecurityEventFilterSessions:
		return true
	default:
		return false
	}
}

// AdministratorSecurityEventSummary is the complete event projection allowed
// outside the authentication domain. Event IDs, user IDs, session IDs, source
// hashes, request IDs, and raw metadata are deliberately omitted.
type AdministratorSecurityEventSummary struct {
	OccurredAt      time.Time
	EventType       AuthEventType
	Success         bool
	Reason          AuthEventReason
	UserAgent       string
	ActorUsername   string
	SubjectUsername string
	SubjectDeleted  bool
}

type AdministratorSecurityEventPage struct {
	Events      []AdministratorSecurityEventSummary
	TotalEvents int64
	Page        int64
	TotalPages  int64
	PageSize    int64
}

// ListAdministratorSecurityEventPage returns a bounded newest-first page of
// instance-wide authentication events to one exact, recently verified
// management administrator session. Raw audit metadata never crosses this
// boundary; the retained username for a deleted user is the only whitelisted
// metadata field projected from deletion events.
func (m *Manager) ListAdministratorSecurityEventPage(
	ctx context.Context,
	actorUserID string,
	actorSessionID string,
	filter AdministratorSecurityEventFilter,
	requestedPage int64,
) (*AdministratorSecurityEventPage, error) {
	actorUserID = strings.TrimSpace(actorUserID)
	actorSessionID = strings.TrimSpace(actorSessionID)
	if actorUserID == "" {
		return nil, ErrAdministratorRequired
	}
	if actorSessionID == "" {
		return nil, ErrRecentStepUpRequired
	}
	if !filter.Valid() {
		return nil, fmt.Errorf("invalid administrator security event filter %q", filter)
	}

	tx, err := m.db.Read().BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin administrator security event page: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireActiveManagementAdministrator(ctx, tx, actorUserID); err != nil {
		return nil, err
	}
	if err := requireRecentAdministratorStepUp(
		ctx, tx, actorUserID, actorSessionID, m.clock.Now().UTC(),
	); err != nil {
		return nil, err
	}

	filterClause, filterArguments := administratorSecurityEventFilterQuery(filter)
	var totalEvents int64
	if err := tx.QueryRowContext(
		ctx, `SELECT COUNT(*) FROM auth_events event`+filterClause, filterArguments...,
	).Scan(&totalEvents); err != nil {
		return nil, fmt.Errorf("count administrator security events: %w", err)
	}
	totalPages := int64(1)
	if totalEvents > 0 {
		totalPages = (totalEvents-1)/administratorSecurityEventPageSize + 1
	}
	page := requestedPage
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * administratorSecurityEventPageSize
	queryArguments := append(append([]any{}, filterArguments...), administratorSecurityEventPageSize, offset)
	rows, err := tx.QueryContext(ctx, `
		SELECT event.occurred_at, event.event_type, event.success, event.reason,
		       event.user_agent, actor.username, subject.username, event.metadata_json
		FROM auth_events event
		LEFT JOIN users actor ON actor.id = event.actor_user_id
		LEFT JOIN users subject ON subject.id = event.subject_user_id
		`+filterClause+`
		ORDER BY event.occurred_at DESC, event.id DESC
		LIMIT ? OFFSET ?`, queryArguments...,
	)
	if err != nil {
		return nil, fmt.Errorf("list administrator security events: %w", err)
	}
	defer rows.Close()
	result := &AdministratorSecurityEventPage{
		Events:      make([]AdministratorSecurityEventSummary, 0, int(administratorSecurityEventPageSize)),
		TotalEvents: totalEvents,
		Page:        page,
		TotalPages:  totalPages,
		PageSize:    administratorSecurityEventPageSize,
	}
	for rows.Next() {
		var event AdministratorSecurityEventSummary
		var success int
		var actorUsername, subjectUsername sql.NullString
		var metadata string
		if err := rows.Scan(
			&event.OccurredAt, &event.EventType, &success, &event.Reason,
			&event.UserAgent, &actorUsername, &subjectUsername, &metadata,
		); err != nil {
			return nil, fmt.Errorf("scan administrator security event: %w", err)
		}
		event.Success = success == 1
		if actorUsername.Valid {
			event.ActorUsername = strings.TrimSpace(actorUsername.String)
		}
		if subjectUsername.Valid {
			event.SubjectUsername = strings.TrimSpace(subjectUsername.String)
		} else if username := deletedSecurityEventUsername(event.EventType, metadata); username != "" {
			event.SubjectUsername = username
			event.SubjectDeleted = true
		}
		result.Events = append(result.Events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate administrator security events: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close administrator security events: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit administrator security event page: %w", err)
	}
	return result, nil
}

func administratorSecurityEventFilterQuery(filter AdministratorSecurityEventFilter) (string, []any) {
	switch filter {
	case AdministratorSecurityEventFilterFailures:
		return " WHERE event.success = 0", nil
	case AdministratorSecurityEventFilterRecovery:
		return " WHERE event.event_type IN (?, ?, ?, ?)", []any{
			AuthEventRecoveryUsed,
			AuthEventCredentialResetRequested,
			AuthEventCredentialResetCompleted,
			AuthEventLocalRecoveryStarted,
		}
	case AdministratorSecurityEventFilterPolicy:
		return " WHERE event.event_type IN (?, ?)", []any{AuthEventSecurityPolicyChanged, AuthEventPasswordChangeRequired}
	case AdministratorSecurityEventFilterIdentities:
		return " WHERE event.event_type IN (?, ?)", []any{
			AuthEventIdentityLinked,
			AuthEventIdentityUnlinked,
		}
	case AdministratorSecurityEventFilterSessions:
		return " WHERE event.event_type IN (?, ?, ?, ?, ?, ?)", []any{
			AuthEventPrimaryVerified,
			AuthEventLoginSucceeded,
			AuthEventLoginFailed,
			AuthEventSessionRevoked,
			AuthEventStepUpSucceeded,
			AuthEventStepUpFailed,
		}
	default:
		return "", nil
	}
}

func deletedSecurityEventUsername(eventType AuthEventType, metadata string) string {
	if eventType != AuthEventUserDeletionStarted && eventType != AuthEventUserDeleted {
		return ""
	}
	var value struct {
		TargetUsername string `json:"target_username"`
	}
	if err := json.Unmarshal([]byte(metadata), &value); err != nil {
		return ""
	}
	username := strings.TrimSpace(value.TargetUsername)
	if len([]rune(username)) > usernameMaximumLength {
		return ""
	}
	return username
}
