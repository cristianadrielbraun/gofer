package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const setupCutoverVersion int64 = 1

var ErrSetupCompletionBlocked = errors.New("setup completion is blocked by invalid ownership records")

type CompleteSetupOptions struct {
	Token     string
	Origin    string
	UserAgent string
}

type SetupCompletionResult struct {
	OwnerUserID     string
	Session         *Session
	RevokedSessions int64
}

// CompleteSetup commits the reviewed owner, credentials, setup state, audit
// event, and first authenticated session as one security transition. The raw
// session token is returned only after the transaction commits.
func (m *Manager) CompleteSetup(ctx context.Context, options CompleteSetupOptions) (*SetupCompletionResult, error) {
	canonicalOrigin, err := canonicalAuthOrigin(options.Origin)
	if err != nil || strings.TrimSpace(options.Token) == "" {
		return nil, ErrSetupAccessInvalid
	}

	generatedOwnerID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate setup owner ID: %w", err)
	}
	totpID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate setup TOTP credential ID: %w", err)
	}
	recoveryIDs := make([]string, setupRecoveryCodeCount)
	for index := range recoveryIDs {
		recoveryIDs[index], err = m.tokens.ID()
		if err != nil {
			return nil, fmt.Errorf("generate setup recovery credential ID: %w", err)
		}
	}
	sessionID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate setup session ID: %w", err)
	}
	sessionToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate setup session token: %w", err)
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate setup completion event ID: %w", err)
	}

	now := m.clock.Now().UTC()
	userAgent := boundedUserAgent(options.UserAgent)
	result := &SetupCompletionResult{}
	err = m.runSecurityTransition(ctx, SecurityTransitionSetup, func(tx *sql.Tx) error {
		challenge, draft, topology, err := m.currentSetupSecurityDraft(ctx, tx, options.Token, canonicalOrigin, now)
		if err != nil {
			return err
		}
		if draft.TOTPSecret == "" || draft.TOTPConfirmedStep == nil {
			return ErrSetupTOTPConfirmationRequired
		}
		if !validSetupRecoveryDraft(draft) || draft.RecoveryBatchID == "" || !draft.RecoveryAcknowledged {
			return ErrSetupRecoveryAcknowledgementRequired
		}
		var unassignedMailboxes int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM accounts account
			WHERE account.user_id IS NULL
			   OR NOT EXISTS (SELECT 1 FROM users WHERE users.id = account.user_id)`,
		).Scan(&unassignedMailboxes); err != nil {
			return fmt.Errorf("check setup mailbox ownership: %w", err)
		}
		if unassignedMailboxes != 0 {
			return ErrSetupCompletionBlocked
		}

		ownerID := draft.TargetUserID
		if draft.Mode == SetupOwnerModeCreate {
			ownerID = generatedOwnerID
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO users (
					id, email, email_normalized, username, username_normalized, name,
					status, auth_version, mfa_required, is_admin, last_login_at,
					created_at, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, 'active', 1, 1, 1, ?, ?, ?)`,
				ownerID, draft.Email, draft.EmailNormalized, draft.Username,
				draft.UsernameNormalized, draft.Name, now, now, now,
			); err != nil {
				return fmt.Errorf("create setup owner: %w", err)
			}
		} else {
			update, err := tx.ExecContext(ctx, `
				UPDATE users
				SET email = ?, email_normalized = ?, username = ?, username_normalized = ?,
				    name = ?, status = 'active', auth_version = auth_version + 1,
				    mfa_required = 1, is_admin = 1, last_login_at = ?,
				    disabled_at = NULL, disabled_by = NULL, updated_at = ?
				WHERE id = ?`,
				draft.Email, draft.EmailNormalized, draft.Username, draft.UsernameNormalized,
				draft.Name, now, now, ownerID,
			)
			if err != nil {
				return fmt.Errorf("update setup owner: %w", err)
			}
			changed, err := update.RowsAffected()
			if err != nil {
				return fmt.Errorf("count updated setup owners: %w", err)
			}
			if changed != 1 {
				return ErrSetupOwnerDraftRequired
			}
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO password_credentials (user_id, password_hash, must_change, created_at, changed_at)
			VALUES (?, ?, 0, ?, ?)
			ON CONFLICT(user_id) DO UPDATE SET
				password_hash = excluded.password_hash,
				must_change = 0,
				changed_at = excluded.changed_at,
				reset_at = NULL`,
			ownerID, draft.PasswordHash, now, now,
		); err != nil {
			return fmt.Errorf("persist setup password credential: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE totp_credentials
			SET enabled = 0, revoked_at = ?
			WHERE user_id = ? AND revoked_at IS NULL`, now, ownerID); err != nil {
			return fmt.Errorf("revoke replaced setup TOTP credentials: %w", err)
		}
		encryptedSeed, err := m.encryptTOTPSeed(ownerID, totpID, draft.TOTPSecret)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO totp_credentials (
				id, user_id, encrypted_seed, key_version, algorithm, digits, period,
				issuer, last_accepted_step, enabled, created_at
			) VALUES (?, ?, ?, ?, 'SHA1', 6, 30, ?, ?, 1, ?)`,
			totpID, ownerID, encryptedSeed, totpCredentialKeyVersion,
			setupTOTPIssuer, *draft.TOTPConfirmedStep, now,
		); err != nil {
			return fmt.Errorf("persist setup TOTP credential: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE recovery_codes SET revoked_at = ?
			WHERE user_id = ? AND used_at IS NULL AND revoked_at IS NULL`, now, ownerID); err != nil {
			return fmt.Errorf("revoke replaced setup recovery codes: %w", err)
		}
		for index, codeHash := range draft.RecoveryCodeHashes {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO recovery_codes (id, user_id, batch_id, code_hash, created_at)
				VALUES (?, ?, ?, ?, ?)`,
				recoveryIDs[index], ownerID, draft.RecoveryBatchID, codeHash, now,
			); err != nil {
				return fmt.Errorf("persist setup recovery code: %w", err)
			}
		}

		revoked, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
			WHERE revoked_at IS NULL`, now, ownerID, SessionRevocationAdminAction)
		if err != nil {
			return fmt.Errorf("revoke pre-cutover sessions: %w", err)
		}
		result.RevokedSessions, err = revoked.RowsAffected()
		if err != nil {
			return fmt.Errorf("count revoked pre-cutover sessions: %w", err)
		}

		var authVersion int64
		if err := tx.QueryRowContext(ctx, `SELECT auth_version FROM users WHERE id = ? AND status = 'active'`, ownerID).Scan(&authVersion); err != nil {
			return fmt.Errorf("read setup owner authentication version: %w", err)
		}
		if _, err := m.requireAuthenticationAssurance(
			ctx, tx, ownerID, authVersion, AssuranceLevelMultiFactor,
		); err != nil {
			return err
		}
		idleExpiresAt := now.Add(sessionIdleLifetime)
		absoluteExpiresAt := now.Add(sessionAbsoluteLifetime)
		if idleExpiresAt.After(absoluteExpiresAt) {
			idleExpiresAt = absoluteExpiresAt
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (
				id, user_id, token_hash, auth_version, authentication_method,
				assurance_level, user_agent, authenticated_at, last_used_at,
				idle_expires_at, absolute_expires_at, step_up_at, step_up_method,
				created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, ownerID, hashToken(sessionToken), authVersion,
			AuthenticationMethodPassword, AssuranceLevelMultiFactor, userAgent,
			now, now, idleExpiresAt, absoluteExpiresAt, now, AuthenticationMethodTOTP, now,
		); err != nil {
			return fmt.Errorf("persist setup owner session: %w", err)
		}

		challengeUpdate, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges
			SET consumed_at = ?, payload_ciphertext = NULL
			WHERE id = ? AND challenge_hash = ? AND purpose = ? AND origin = ?
			  AND consumed_at IS NULL`,
			now, challenge.ID, hashToken(options.Token), ChallengePurposeEnrollment, canonicalOrigin,
		)
		if err != nil {
			return fmt.Errorf("consume setup challenge: %w", err)
		}
		changed, err := challengeUpdate.RowsAffected()
		if err != nil {
			return fmt.Errorf("count consumed setup challenges: %w", err)
		}
		if changed != 1 {
			return ErrSetupAccessInvalid
		}

		stateUpdate, err := tx.ExecContext(ctx, `
			UPDATE auth_system_state
			SET initialized = 1, owner_user_id = ?, initialized_at = ?,
			    setup_token_hash = NULL, setup_expires_at = NULL, setup_attempts = 0,
			    cutover_version = ?
			WHERE id = 1 AND initialized = 0 AND setup_token_hash IS NOT NULL`,
			ownerID, now, setupCutoverVersion,
		)
		if err != nil {
			return fmt.Errorf("complete authentication setup state: %w", err)
		}
		changed, err = stateUpdate.RowsAffected()
		if err != nil {
			return fmt.Errorf("count completed authentication setup states: %w", err)
		}
		if changed != 1 {
			return ErrSetupAlreadyInitialized
		}

		metadata, err := setupCompletionEventJSON(topology, draft, result.RevokedSessions)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
			eventID, now, ownerID, ownerID, sessionID, AuthEventSetupCompleted,
			AuthEventReasonSystemInitialization, userAgent, metadata,
		); err != nil {
			return fmt.Errorf("record setup completion event: %w", err)
		}

		stepUpAt := now
		result.OwnerUserID = ownerID
		result.Session = &Session{
			ID: sessionID, UserID: ownerID, Token: sessionToken, AuthVersion: authVersion,
			AuthenticationMethod: AuthenticationMethodPassword,
			AssuranceLevel:       AssuranceLevelMultiFactor, UserAgent: userAgent,
			AuthenticatedAt: now, LastUsedAt: now, IdleExpiresAt: idleExpiresAt,
			AbsoluteExpiresAt: absoluteExpiresAt, StepUpAt: &stepUpAt,
			StepUpMethod: AuthenticationMethodTOTP, CreatedAt: now,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func setupCompletionEventJSON(topology SetupOwnerTopology, draft *SetupOwnerDraft, revokedSessions int64) (string, error) {
	metadata, err := json.Marshal(map[string]any{
		"cutover_version":  setupCutoverVersion,
		"owner_mode":       draft.Mode,
		"topology":         topology.Kind,
		"revoked_sessions": revokedSessions,
	})
	if err != nil {
		return "", fmt.Errorf("encode setup completion event: %w", err)
	}
	return string(metadata), nil
}
