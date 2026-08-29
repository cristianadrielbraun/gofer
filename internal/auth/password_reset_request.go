package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type PasswordResetRequestOptions struct {
	Identifier string
	Source     string
	UserAgent  string
}

// RequestPasswordReset records a pending administrator-assisted reset for an
// eligible webmail user. Unknown, pending, management, and throttled requests
// deliberately return the same nil result so public callers cannot use this
// operation to discover application-login accounts.
func (m *Manager) RequestPasswordReset(ctx context.Context, options PasswordResetRequestOptions) error {
	identifier := normalizeLoginIdentifier(options.Identifier)
	source := strings.TrimSpace(options.Source)
	userAgent := boundedUserAgent(options.UserAgent)
	buckets, err := m.authenticationThrottleBuckets(passwordResetRequestThrottleAction, identifier, source)
	if err != nil {
		return err
	}
	sourceHash, err := m.authenticationThrottleBucketHash(
		passwordResetRequestThrottleAction, loginThrottleBucketSource, source,
	)
	if err != nil {
		return err
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return fmt.Errorf("generate password reset request event ID: %w", err)
	}
	now := m.clock.Now().UTC()

	return m.runSecurityTransition(ctx, SecurityTransitionLoginThrottle, func(tx *sql.Tx) error {
		retryAt, err := loginThrottleRetryAtTx(ctx, tx, buckets, now)
		if err != nil {
			return err
		}
		for _, bucket := range buckets {
			if _, err := recordLoginThrottleFailure(ctx, tx, bucket, now); err != nil {
				return err
			}
		}
		if retryAt.After(now) {
			return nil
		}

		var userID string
		err = tx.QueryRowContext(ctx, `
			UPDATE users
			SET password_reset_requested_at = ?, updated_at = ?
			WHERE username_normalized = ?
			  AND status IN ('active', 'disabled')
			  AND user_type = 'webmail'
			  AND is_admin = 0
			  AND deletion_pending = 0
			  AND password_reset_requested_at IS NULL
			RETURNING id`, now, now, identifier,
		).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("record password reset request: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, subject_user_id, event_type, success, reason,
				user_agent, source_hash, metadata_json
			) VALUES (?, ?, ?, ?, 1, ?, ?, ?, '{}')`,
			eventID, now, userID, AuthEventCredentialResetRequested,
			AuthEventReasonUserAction, userAgent, sourceHash,
		); err != nil {
			return fmt.Errorf("record password reset request event: %w", err)
		}
		return nil
	})
}
