package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// transitionEventMetadata is deliberately closed: callers cannot persist raw
// identifiers, credentials, provider claims, request bodies, or error strings.
type transitionEventMetadata struct {
	Method     AuthenticationMethod    `json:"method,omitempty"`
	Stage      string                  `json:"stage,omitempty"`
	Revocation SessionRevocationReason `json:"revocation_reason,omitempty"`
}

// appendTransitionEvent must use the transaction that owns the state change.
// Event IDs are independent database-generated identifiers, never bearer hashes.
func (m *Manager) appendTransitionEvent(ctx context.Context, tx *sql.Tx, actor, subject, session string, kind AuthEventType, success bool, reason AuthEventReason, metadata transitionEventMetadata) error {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO auth_events (
			id, occurred_at, actor_user_id, subject_user_id, session_id,
			event_type, success, reason, metadata_json
		) VALUES (lower(hex(randomblob(16))), ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.clock.Now().UTC(), nullableIdentifier(actor), nullableIdentifier(subject), nullableIdentifier(session), kind, success, reason, string(encoded))
	if err != nil {
		return fmt.Errorf("append authentication transition event: %w", err)
	}
	return nil
}

func (m *Manager) appendPrimaryAuthenticationEvent(ctx context.Context, tx *sql.Tx, userID string, session *Session, method AuthenticationMethod) error {
	if session == nil {
		return m.appendTransitionEvent(ctx, tx, "", userID, "", AuthEventPrimaryVerified, true, AuthEventReasonPolicyRequired, transitionEventMetadata{Method: method, Stage: "mfa_pending"})
	}
	return m.appendTransitionEvent(ctx, tx, userID, userID, session.ID, AuthEventLoginSucceeded, true, AuthEventReasonChallengeVerified, transitionEventMetadata{Method: method})
}

func (m *Manager) rejectThrottledAuthentication(ctx context.Context, decision LoginThrottleDecision, actor, subject, session string, kind AuthEventType, method AuthenticationMethod) error {
	err := m.runSecurityTransition(ctx, SecurityTransitionLoginThrottle, func(tx *sql.Tx) error {
		// These IDs come from a validated session/challenge, never a submitted name.
		return m.appendTransitionEvent(ctx, tx, actor, subject, session, kind, false, AuthEventReasonThrottled, transitionEventMetadata{Method: method})
	})
	if err != nil {
		return err
	}
	return m.loginThrottleError(decision)
}
