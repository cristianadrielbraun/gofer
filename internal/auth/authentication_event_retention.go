package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	DefaultAuthenticationEventRetentionDays = 180
	MinimumAuthenticationEventRetentionDays = 1
	MaximumAuthenticationEventRetentionDays = 365
	AuthenticationEventPruneBatchSize       = 100
)

var (
	ErrAuthenticationEventRetentionInvalid     = errors.New("authentication event retention period is invalid")
	ErrAuthenticationEventRetentionUnavailable = errors.New("authentication event retention is unavailable before setup completes")
)

type AuthenticationEventRetentionPolicy struct {
	Days      int
	UpdatedAt *time.Time
	UpdatedBy string
}

type SetAuthenticationEventRetentionOptions struct {
	ActorUserID    string
	ActorSessionID string
	Days           int
}

type SetAuthenticationEventRetentionResult struct {
	Policy  AuthenticationEventRetentionPolicy
	Changed bool
}

type AuthenticationEventPruneResult struct {
	RetentionDays int
	Cutoff        time.Time
	Deleted       int64
}

type authenticationEventRetentionQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func validAuthenticationEventRetentionDays(days int) bool {
	return days >= MinimumAuthenticationEventRetentionDays && days <= MaximumAuthenticationEventRetentionDays
}

func readAuthenticationEventRetention(
	ctx context.Context,
	queryer authenticationEventRetentionQueryer,
) (AuthenticationEventRetentionPolicy, error) {
	var days int
	var updatedAt sql.NullTime
	var updatedBy sql.NullString
	err := queryer.QueryRowContext(ctx, `
		SELECT auth_event_retention_days, auth_event_retention_updated_at,
		       auth_event_retention_updated_by
		FROM auth_system_state WHERE id = 1`,
	).Scan(&days, &updatedAt, &updatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthenticationEventRetentionPolicy{Days: DefaultAuthenticationEventRetentionDays}, nil
	}
	if err != nil {
		return AuthenticationEventRetentionPolicy{}, fmt.Errorf("read authentication event retention: %w", err)
	}
	if !validAuthenticationEventRetentionDays(days) {
		return AuthenticationEventRetentionPolicy{}, fmt.Errorf(
			"%w: %d", ErrAuthenticationEventRetentionInvalid, days,
		)
	}
	result := AuthenticationEventRetentionPolicy{Days: days, UpdatedBy: updatedBy.String}
	if updatedAt.Valid {
		value := updatedAt.Time
		result.UpdatedAt = &value
	}
	return result, nil
}

func (m *Manager) AuthenticationEventRetention(ctx context.Context) (AuthenticationEventRetentionPolicy, error) {
	return readAuthenticationEventRetention(ctx, m.db.Read())
}

// SetAuthenticationEventRetention changes the instance audit-history window
// only for an exact, recently verified management administrator session. The
// setting and its audit event commit atomically.
func (m *Manager) SetAuthenticationEventRetention(
	ctx context.Context,
	options SetAuthenticationEventRetentionOptions,
) (*SetAuthenticationEventRetentionResult, error) {
	actorUserID := strings.TrimSpace(options.ActorUserID)
	actorSessionID := strings.TrimSpace(options.ActorSessionID)
	if actorUserID == "" {
		return nil, ErrAdministratorRequired
	}
	if actorSessionID == "" {
		return nil, ErrRecentStepUpRequired
	}
	if !validAuthenticationEventRetentionDays(options.Days) {
		return nil, fmt.Errorf("%w: %d", ErrAuthenticationEventRetentionInvalid, options.Days)
	}

	now := m.clock.Now().UTC()
	result := &SetAuthenticationEventRetentionResult{}
	err := m.runSecurityTransition(ctx, SecurityTransitionPolicyChange, func(tx *sql.Tx) error {
		if err := requireActiveManagementAdministrator(ctx, tx, actorUserID); err != nil {
			return err
		}
		if err := requireRecentAdministratorStepUp(ctx, tx, actorUserID, actorSessionID, now); err != nil {
			return err
		}

		var initialized, currentDays int
		err := tx.QueryRowContext(ctx, `
			SELECT initialized, auth_event_retention_days
			FROM auth_system_state WHERE id = 1`,
		).Scan(&initialized, &currentDays)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrAuthenticationEventRetentionUnavailable
		}
		if err != nil {
			return fmt.Errorf("load authentication event retention: %w", err)
		}
		if initialized != 1 {
			return ErrAuthenticationEventRetentionUnavailable
		}
		if !validAuthenticationEventRetentionDays(currentDays) {
			return fmt.Errorf("%w: %d", ErrAuthenticationEventRetentionInvalid, currentDays)
		}
		if currentDays == options.Days {
			policy, err := readAuthenticationEventRetention(ctx, tx)
			if err != nil {
				return err
			}
			result.Policy = policy
			return nil
		}

		eventID, err := m.tokens.ID()
		if err != nil {
			return fmt.Errorf("generate authentication event retention event ID: %w", err)
		}
		updated, err := tx.ExecContext(ctx, `
			UPDATE auth_system_state
			SET auth_event_retention_days = ?, auth_event_retention_updated_at = ?,
			    auth_event_retention_updated_by = ?
			WHERE id = 1 AND initialized = 1 AND auth_event_retention_days = ?`,
			options.Days, now, actorUserID, currentDays,
		)
		if err != nil {
			return fmt.Errorf("update authentication event retention: %w", err)
		}
		rowsAffected, err := updated.RowsAffected()
		if err != nil {
			return fmt.Errorf("count updated authentication event retention policies: %w", err)
		}
		if rowsAffected != 1 {
			return errors.New("authentication event retention changed concurrently")
		}

		metadata, err := json.Marshal(struct {
			Setting  string `json:"setting"`
			FromDays int    `json:"from_days"`
			ToDays   int    `json:"to_days"`
		}{
			Setting:  "authentication_event_retention_days",
			FromDays: currentDays,
			ToDays:   options.Days,
		})
		if err != nil {
			return fmt.Errorf("encode authentication event retention event: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, session_id, event_type,
				success, reason, metadata_json
			) VALUES (?, ?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, actorUserID, actorSessionID, AuthEventSecurityPolicyChanged,
			AuthEventReasonAdministratorAction, string(metadata),
		); err != nil {
			return fmt.Errorf("record authentication event retention change: %w", err)
		}

		result.Policy = AuthenticationEventRetentionPolicy{
			Days: options.Days, UpdatedAt: timePointer(now), UpdatedBy: actorUserID,
		}
		result.Changed = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// PruneAuthenticationEvents deletes one bounded oldest-first batch outside the
// configured retention window. Callers repeat it until Deleted is zero while
// keeping each SQLite writer transaction small.
func (m *Manager) PruneAuthenticationEvents(
	ctx context.Context,
	now time.Time,
	batch int,
) (AuthenticationEventPruneResult, error) {
	if batch <= 0 {
		batch = AuthenticationEventPruneBatchSize
	}
	if batch > 1000 {
		batch = 1000
	}
	now = now.UTC()

	tx, err := m.db.Write().BeginTx(ctx, nil)
	if err != nil {
		return AuthenticationEventPruneResult{}, fmt.Errorf("begin authentication event pruning: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	policy, err := readAuthenticationEventRetention(ctx, tx)
	if err != nil {
		return AuthenticationEventPruneResult{}, err
	}
	cutoff := now.Add(-time.Duration(policy.Days) * 24 * time.Hour)
	deleted, err := tx.ExecContext(ctx, `
		DELETE FROM auth_events
		WHERE id IN (
			SELECT id FROM auth_events
			WHERE occurred_at < ?
			ORDER BY occurred_at ASC, id ASC
			LIMIT ?
		)`, cutoff, batch,
	)
	if err != nil {
		return AuthenticationEventPruneResult{}, fmt.Errorf("prune authentication events: %w", err)
	}
	count, err := deleted.RowsAffected()
	if err != nil {
		return AuthenticationEventPruneResult{}, fmt.Errorf("count pruned authentication events: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return AuthenticationEventPruneResult{}, fmt.Errorf("commit authentication event pruning: %w", err)
	}
	return AuthenticationEventPruneResult{
		RetentionDays: policy.Days,
		Cutoff:        cutoff,
		Deleted:       count,
	}, nil
}
