package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrLocalRecoveryTargetInvalid = errors.New("user is not eligible for local recovery")

type LocalRecoveryResult struct {
	Token           EnrollmentToken
	RevokedSessions int64
	ReplacedTokens  int64
}

// RecoverUserLocally immediately invalidates the target's authentication state
// and returns one short-lived credential-reset secret. The caller must enforce
// exact operator confirmation and hold Gofer's exclusive runtime lock. The raw
// token is returned only here and is never persisted.
func (m *Manager) RecoverUserLocally(ctx context.Context, userID string) (*LocalRecoveryResult, error) {
	if userID == "" {
		return nil, ErrLocalRecoveryTargetInvalid
	}
	tokenID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate local recovery token ID: %w", err)
	}
	rawToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate local recovery token: %w", err)
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate local recovery event ID: %w", err)
	}

	now := m.clock.Now().UTC()
	result := &LocalRecoveryResult{Token: EnrollmentToken{
		ID: tokenID, Token: rawToken, UserID: userID,
		Purpose:   EnrollmentTokenPurposeCredentialReset,
		CreatedAt: now, ExpiresAt: now.Add(defaultCredentialResetTokenLifetime),
	}}
	err = m.runSecurityTransition(ctx, SecurityTransitionRecovery, func(tx *sql.Tx) error {
		var status UserStatus
		if err := tx.QueryRowContext(ctx, `SELECT status FROM users WHERE id = ?`, userID).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrLocalRecoveryTargetInvalid
			}
			return fmt.Errorf("read local recovery target: %w", err)
		}
		if status != UserStatusActive && status != UserStatusDisabled {
			return ErrLocalRecoveryTargetInvalid
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE users
			SET auth_version = auth_version + 1, updated_at = ?
			WHERE id = ?`, now, userID); err != nil {
			return fmt.Errorf("invalidate local recovery target: %w", err)
		}
		revoked, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET revoked_at = ?, revoked_by = NULL, revocation_reason = ?
			WHERE user_id = ? AND revoked_at IS NULL`,
			now, SessionRevocationCredentialReset, userID,
		)
		if err != nil {
			return fmt.Errorf("revoke local recovery sessions: %w", err)
		}
		result.RevokedSessions, err = revoked.RowsAffected()
		if err != nil {
			return fmt.Errorf("count local recovery sessions: %w", err)
		}

		replaced, err := tx.ExecContext(ctx, `
			UPDATE user_enrollment_tokens
			SET revoked_at = ?
			WHERE user_id = ? AND purpose = ?
			  AND used_at IS NULL AND revoked_at IS NULL`,
			now, userID, EnrollmentTokenPurposeCredentialReset,
		)
		if err != nil {
			return fmt.Errorf("revoke replaced local recovery tokens: %w", err)
		}
		result.ReplacedTokens, err = replaced.RowsAffected()
		if err != nil {
			return fmt.Errorf("count replaced local recovery tokens: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_enrollment_tokens (
				id, user_id, token_hash, purpose, created_at, expires_at
			) VALUES (?, ?, ?, ?, ?, ?)`,
			result.Token.ID, result.Token.UserID, hashToken(result.Token.Token),
			result.Token.Purpose, result.Token.CreatedAt, result.Token.ExpiresAt,
		); err != nil {
			return fmt.Errorf("insert local recovery token: %w", err)
		}

		metadata, err := localRecoveryEventJSON(result.Token.ID, result.RevokedSessions, result.ReplacedTokens)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, subject_user_id, event_type, success, reason, metadata_json
			) VALUES (?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, userID, AuthEventLocalRecoveryStarted,
			AuthEventReasonLocalOperator, metadata,
		); err != nil {
			return fmt.Errorf("record local recovery event: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func localRecoveryEventJSON(tokenID string, revokedSessions, replacedTokens int64) (string, error) {
	metadata := struct {
		TokenID         string                 `json:"token_id"`
		Purpose         EnrollmentTokenPurpose `json:"purpose"`
		RevokedSessions int64                  `json:"revoked_sessions"`
		ReplacedTokens  int64                  `json:"replaced_tokens"`
	}{
		TokenID: tokenID, Purpose: EnrollmentTokenPurposeCredentialReset,
		RevokedSessions: revokedSessions, ReplacedTokens: replacedTokens,
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode local recovery event metadata: %w", err)
	}
	return string(encoded), nil
}
