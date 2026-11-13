package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrPasskeyTOTPPolicyEnforced = errors.New("administrator policy requires a separate TOTP factor")

// SetPasskeyTOTPRequired changes only the extra authenticator-code requirement
// for WebAuthn login. It does not affect password, federated, or TOTP enrollment.
// All old sessions are revoked atomically with the policy and audit event.
func (m *Manager) SetPasskeyTOTPRequired(ctx context.Context, sessionToken string, required bool, userAgent string) (*Session, error) {
	session, err := m.GetSessionByToken(ctx, sessionToken)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, ErrSecuritySessionInvalid
	}
	now := m.clock.Now().UTC()
	if err := m.requireRecentSecurityStepUp(ctx, session, now); err != nil {
		return nil, err
	}
	newID, newToken, eventID, err := m.securityRotationMaterial()
	if err != nil {
		return nil, err
	}
	userAgent = boundedUserAgent(userAgent)
	var rotated *Session
	err = m.runSecurityTransition(ctx, SecurityTransitionPolicyChange, func(tx *sql.Tx) error {
		current, err := m.currentSecuritySession(ctx, tx, sessionToken, now, true)
		if err != nil {
			return err
		}
		if current.ID != session.ID {
			return ErrSecuritySessionInvalid
		}
		var userType UserType
		var previous int
		if err := tx.QueryRowContext(ctx, `SELECT user_type, passkey_totp_required FROM users WHERE id=? AND auth_version=? AND status='active'`, current.UserID, current.AuthVersion).Scan(&userType, &previous); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrSecuritySessionInvalid
			}
			return fmt.Errorf("read passkey TOTP preference: %w", err)
		}
		if userType != UserTypeWebmail {
			return ErrSecuritySessionInvalid
		}
		policy, err := m.loadAuthenticationPolicy(ctx, tx, current.UserID, current.AuthVersion)
		if err != nil {
			return err
		}
		if !required && policy.MFAEnrollmentRequired && !policy.PasskeyCountsAsSecondFactor {
			return ErrPasskeyTOTPPolicyEnforced
		}
		want := 0
		if required {
			want = 1
		}
		if previous == want {
			// The session's bearer remains unchanged on a no-op update.
			copySession := *current
			copySession.Token = sessionToken
			rotated = &copySession
			return nil
		}
		var nextVersion int64
		if err := tx.QueryRowContext(ctx, `UPDATE users SET passkey_totp_required=?, auth_version=auth_version+1, updated_at=? WHERE id=? AND auth_version=? RETURNING auth_version`, want, now, current.UserID, current.AuthVersion).Scan(&nextVersion); err != nil {
			return fmt.Errorf("update passkey TOTP preference: %w", err)
		}
		if err := revokeSecuritySessions(ctx, tx, current.UserID, now); err != nil {
			return err
		}
		// Any pending security challenges based on the old policy are stale.
		if _, err := tx.ExecContext(ctx, `UPDATE auth_challenges SET consumed_at=COALESCE(consumed_at,?), payload_ciphertext=NULL WHERE user_id=? AND consumed_at IS NULL`, now, current.UserID); err != nil {
			return fmt.Errorf("invalidate security challenges: %w", err)
		}
		policy, err = m.loadAuthenticationPolicy(ctx, tx, current.UserID, nextVersion)
		if err != nil {
			return err
		}
		// Enabling an additional factor can invalidate a passkey-only session.
		// Do not preserve that session; redirect the user to sign in again.
		if policy.allowsAssurance(current.AssuranceLevel) {
			rotated, err = insertRotatedSecuritySession(ctx, tx, current, nextVersion, newID, newToken, userAgent, current.StepUpMethod, now)
			if err != nil {
				return err
			}
		}
		metadata := `{"setting":"passkey_totp_required","from":false,"to":false}`
		if previous == 1 && want == 0 {
			metadata = `{"setting":"passkey_totp_required","from":true,"to":false}`
		}
		if previous == 0 && want == 1 {
			metadata = `{"setting":"passkey_totp_required","from":false,"to":true}`
		}
		var rotatedID any
		if rotated != nil {
			rotatedID = rotated.ID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_events (id, occurred_at, actor_user_id, subject_user_id, session_id, event_type, success, reason, user_agent, metadata_json) VALUES (?,?,?,?,?,?,1,?,?,?)`, eventID, now, current.UserID, current.UserID, rotatedID, AuthEventSecurityPolicyChanged, AuthEventReasonUserAction, userAgent, metadata); err != nil {
			return fmt.Errorf("audit passkey TOTP preference: %w", err)
		}
		return nil
	})
	return rotated, err
}
