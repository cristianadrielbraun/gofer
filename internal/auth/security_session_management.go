package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const securitySessionActionReferenceContext = "gofer/auth/security-session-action-reference/v1"

var ErrSecuritySessionTargetInvalid = errors.New("security session target is invalid")

type SecuritySessionRevocationResult struct {
	RevokedSessions int64
}

func (m *Manager) securitySessionActionReference(currentSessionID, targetSessionID string) (string, error) {
	if len(m.bucketHashKey) < minimumBucketHashKeyBytes {
		return "", fmt.Errorf("security session action key must be at least %d bytes", minimumBucketHashKeyBytes)
	}
	currentDigest := sha256.Sum256([]byte(currentSessionID))
	targetDigest := sha256.Sum256([]byte(targetSessionID))
	mac := hmac.New(sha256.New, m.bucketHashKey)
	_, _ = mac.Write([]byte(securitySessionActionReferenceContext))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(currentDigest[:])
	_, _ = mac.Write(targetDigest[:])
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func canonicalSecuritySessionActionReference(reference string) bool {
	if len(reference) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(reference)
	return err == nil && len(decoded) == sha256.Size &&
		base64.RawURLEncoding.EncodeToString(decoded) == reference
}

// RevokeSecuritySession signs out one active non-current session selected by
// an opaque reference bound to the exact current session. It independently
// enforces recent verification and exact ownership at mutation time.
func (m *Manager) RevokeSecuritySession(
	ctx context.Context, sessionToken, actionReference, userAgent string,
) (*SecuritySessionRevocationResult, error) {
	if !canonicalSecuritySessionActionReference(actionReference) {
		return nil, ErrSecuritySessionTargetInvalid
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate session revocation event ID: %w", err)
	}
	userAgent = boundedUserAgent(userAgent)
	now := m.clock.Now().UTC()
	result := &SecuritySessionRevocationResult{}
	err = m.runSecurityTransition(ctx, SecurityTransitionSessionRevocation, func(tx *sql.Tx) error {
		current, err := m.currentSecuritySession(ctx, tx, sessionToken, now, true)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT id FROM sessions
			WHERE user_id = ? AND id != ? AND auth_version = ?
			  AND revoked_at IS NULL AND idle_expires_at > ? AND absolute_expires_at > ?
			ORDER BY id`,
			current.UserID, current.ID, current.AuthVersion, now, now,
		)
		if err != nil {
			return fmt.Errorf("list revocable security sessions: %w", err)
		}
		targetSessionID := ""
		for rows.Next() {
			var candidateID string
			if err := rows.Scan(&candidateID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan revocable security session: %w", err)
			}
			candidateReference, err := m.securitySessionActionReference(current.ID, candidateID)
			if err != nil {
				_ = rows.Close()
				return err
			}
			if hmac.Equal([]byte(candidateReference), []byte(actionReference)) {
				targetSessionID = candidateID
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate revocable security sessions: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close revocable security sessions: %w", err)
		}
		if targetSessionID == "" {
			return ErrSecuritySessionTargetInvalid
		}

		revoked, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
			WHERE id = ? AND user_id = ? AND id != ? AND auth_version = ?
			  AND revoked_at IS NULL AND idle_expires_at > ? AND absolute_expires_at > ?`,
			now, current.UserID, SessionRevocationLogout, targetSessionID,
			current.UserID, current.ID, current.AuthVersion, now, now,
		)
		if err != nil {
			return fmt.Errorf("revoke security session: %w", err)
		}
		result.RevokedSessions, err = revoked.RowsAffected()
		if err != nil {
			return fmt.Errorf("count revoked security session: %w", err)
		}
		if result.RevokedSessions != 1 {
			return ErrSecuritySessionTargetInvalid
		}
		return recordUserSessionRevocationEvent(
			ctx, tx, eventID, current, "single", result.RevokedSessions, userAgent, now,
		)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// RevokeOtherSecuritySessions signs out every other active session on the
// current auth version while preserving the exact verified current session.
func (m *Manager) RevokeOtherSecuritySessions(
	ctx context.Context, sessionToken, userAgent string,
) (*SecuritySessionRevocationResult, error) {
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate other-session revocation event ID: %w", err)
	}
	userAgent = boundedUserAgent(userAgent)
	now := m.clock.Now().UTC()
	result := &SecuritySessionRevocationResult{}
	err = m.runSecurityTransition(ctx, SecurityTransitionSessionRevocation, func(tx *sql.Tx) error {
		current, err := m.currentSecuritySession(ctx, tx, sessionToken, now, true)
		if err != nil {
			return err
		}
		revoked, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
			WHERE user_id = ? AND id != ? AND auth_version = ?
			  AND revoked_at IS NULL AND idle_expires_at > ? AND absolute_expires_at > ?`,
			now, current.UserID, SessionRevocationLogout, current.UserID,
			current.ID, current.AuthVersion, now, now,
		)
		if err != nil {
			return fmt.Errorf("revoke other security sessions: %w", err)
		}
		result.RevokedSessions, err = revoked.RowsAffected()
		if err != nil {
			return fmt.Errorf("count revoked other security sessions: %w", err)
		}
		if result.RevokedSessions == 0 {
			return nil
		}
		return recordUserSessionRevocationEvent(
			ctx, tx, eventID, current, "others", result.RevokedSessions, userAgent, now,
		)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func recordUserSessionRevocationEvent(
	ctx context.Context, tx *sql.Tx, eventID string, current *Session,
	scope string, revokedSessions int64, userAgent string, now time.Time,
) error {
	metadata, err := json.Marshal(struct {
		Scope           string `json:"scope"`
		RevokedSessions int64  `json:"revoked_sessions"`
	}{Scope: scope, RevokedSessions: revokedSessions})
	if err != nil {
		return fmt.Errorf("encode session revocation event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO auth_events (
			id, occurred_at, actor_user_id, subject_user_id, session_id,
			event_type, success, reason, user_agent, metadata_json
		) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
		eventID, now, current.UserID, current.UserID, current.ID,
		AuthEventSessionRevoked, AuthEventReasonUserAction, userAgent, string(metadata),
	); err != nil {
		return fmt.Errorf("record session revocation event: %w", err)
	}
	return nil
}
