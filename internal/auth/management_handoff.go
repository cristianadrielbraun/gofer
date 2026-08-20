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

var (
	ErrManagementHandoffRequired      = errors.New("management account separation is required")
	ErrManagementHandoffUnavailable   = errors.New("management account separation is unavailable")
	ErrManagementEnrollmentIncomplete = errors.New("management account security enrollment is incomplete")
)

type CreateManagementHandoffOptions struct {
	ActorUserID    string
	ActorSessionID string
	Name           string
	Username       string
}

type ManagementHandoff struct {
	ID        string
	SourceID  string
	Target    AdministratorUserSummary
	Name      string
	Token     EnrollmentToken
	Status    string
	CreatedAt time.Time
}

type ManagementHandoffStatus struct {
	ID        string
	SourceID  string
	Target    AdministratorUserSummary
	Name      string
	Status    string
	CreatedAt time.Time
}

func (m *Manager) CreateManagementHandoff(ctx context.Context, options CreateManagementHandoffOptions) (*ManagementHandoff, error) {
	actorUserID := strings.TrimSpace(options.ActorUserID)
	actorSessionID := strings.TrimSpace(options.ActorSessionID)
	if actorUserID == "" {
		return nil, ErrAdministratorRequired
	}
	if actorSessionID == "" {
		return nil, ErrRecentStepUpRequired
	}
	fieldErrors := make(map[string]string)
	name, err := PrepareDisplayName(options.Name)
	if err != nil {
		fieldErrors["name"] = err.Error()
	}
	username, usernameNormalized, err := PrepareUsername(options.Username)
	if err != nil {
		fieldErrors["username"] = err.Error()
	}
	if len(fieldErrors) != 0 {
		return nil, &AdministratorUserInvitationValidationError{Fields: fieldErrors}
	}

	handoffID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate management handoff ID: %w", err)
	}
	targetID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate management user ID: %w", err)
	}
	tokenID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate management invitation ID: %w", err)
	}
	rawToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate management invitation token: %w", err)
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate management handoff event ID: %w", err)
	}
	now := m.clock.Now().UTC()
	handoff := &ManagementHandoff{
		ID: handoffID, SourceID: actorUserID, Name: name, Status: "pending", CreatedAt: now,
		Target: AdministratorUserSummary{
			ID: targetID, Username: username, Status: UserStatusPending,
			UserType: UserTypeManagement,
		},
		Token: EnrollmentToken{
			ID: tokenID, Token: rawToken, UserID: targetID, CreatedBy: actorUserID,
			Purpose: EnrollmentTokenPurposeEnrollment, CreatedAt: now,
			ExpiresAt: now.Add(defaultEnrollmentTokenLifetime),
		},
	}

	err = m.runSecurityTransition(ctx, SecurityTransitionRoleChange, func(tx *sql.Tx) error {
		if err := requireActiveAdministrator(ctx, tx, actorUserID); err != nil {
			return err
		}
		if err := requireRecentAdministratorStepUp(ctx, tx, actorUserID, actorSessionID, now); err != nil {
			return err
		}
		var userType UserType
		var separatedResources int
		if err := tx.QueryRowContext(ctx, `
			SELECT user_type,
			       (SELECT COUNT(*) FROM accounts WHERE user_id = users.id) +
			       (SELECT COUNT(*) FROM auth_identities WHERE user_id = users.id)
			FROM users WHERE id = ? AND status = 'active' AND is_admin = 1`, actorUserID,
		).Scan(&userType, &separatedResources); err != nil {
			return ErrManagementHandoffUnavailable
		}
		if userType != UserTypeWebmail || separatedResources == 0 {
			return ErrManagementHandoffUnavailable
		}
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM management_handoffs WHERE source_user_id = ? AND status = 'pending'`, actorUserID).Scan(&pending); err != nil {
			return fmt.Errorf("check pending management handoff: %w", err)
		}
		if pending != 0 {
			return ErrManagementHandoffUnavailable
		}
		collisions, err := administratorInvitationIdentifierCollisions(ctx, tx, usernameNormalized)
		if err != nil {
			return err
		}
		if len(collisions) != 0 {
			return &AdministratorUserInvitationValidationError{Fields: collisions}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO users (
				id, username, username_normalized, name,
				status, auth_version, mfa_required, user_type, is_admin, created_at, updated_at
			) VALUES (?, ?, ?, ?, 'pending', 1, 0, 'management', 0, ?, ?)`,
			targetID, username, usernameNormalized, name, now, now,
		); err != nil {
			return fmt.Errorf("create pending management user: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_enrollment_tokens (
				id, user_id, created_by, token_hash, purpose, created_at, expires_at
			) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			tokenID, targetID, actorUserID, hashToken(rawToken), EnrollmentTokenPurposeEnrollment,
			now, handoff.Token.ExpiresAt,
		); err != nil {
			return fmt.Errorf("create management enrollment token: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO management_handoffs (id, source_user_id, target_user_id, created_at)
			VALUES (?, ?, ?, ?)`, handoffID, actorUserID, targetID, now); err != nil {
			return fmt.Errorf("create management handoff: %w", err)
		}
		metadata, err := json.Marshal(map[string]any{"handoff_id": handoffID, "target_user_id": targetID})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, actorUserID, targetID, actorSessionID,
			AuthEventManagementHandoffStarted, AuthEventReasonAdministratorAction, string(metadata),
		); err != nil {
			return fmt.Errorf("record management handoff start: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return handoff, nil
}

func (m *Manager) GetPendingManagementHandoff(ctx context.Context, sourceUserID string) (*ManagementHandoffStatus, error) {
	sourceUserID = strings.TrimSpace(sourceUserID)
	if err := m.requireActiveAdministrator(ctx, sourceUserID); err != nil {
		return nil, err
	}
	status := &ManagementHandoffStatus{SourceID: sourceUserID}
	var isAdmin int
	err := m.db.Read().QueryRowContext(ctx, `
		SELECT handoff.id, target.id, target.username,
		       target.name, target.status, target.user_type, target.is_admin,
		       handoff.status, handoff.created_at
		FROM management_handoffs handoff
		JOIN users target ON target.id = handoff.target_user_id
		WHERE handoff.source_user_id = ? AND handoff.status = 'pending'`, sourceUserID,
	).Scan(
		&status.ID, &status.Target.ID, &status.Target.Username,
		&status.Name, &status.Target.Status, &status.Target.UserType, &isAdmin,
		&status.Status, &status.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load pending management handoff: %w", err)
	}
	status.Target.IsAdmin = isAdmin == 1
	return status, nil
}

func (m *Manager) GetPendingManagementEnrollment(ctx context.Context, targetUserID string) (*ManagementHandoffStatus, error) {
	targetUserID = strings.TrimSpace(targetUserID)
	status := &ManagementHandoffStatus{}
	var isAdmin int
	err := m.db.Read().QueryRowContext(ctx, `
		SELECT handoff.id, handoff.source_user_id, target.id,
		       target.username, target.name,
		       target.status, target.user_type, target.is_admin,
		       handoff.status, handoff.created_at
		FROM management_handoffs handoff
		JOIN users target ON target.id = handoff.target_user_id
		WHERE handoff.target_user_id = ? AND handoff.status = 'pending'
		  AND target.user_type = 'management' AND target.is_admin = 0`, targetUserID,
	).Scan(
		&status.ID, &status.SourceID, &status.Target.ID,
		&status.Target.Username, &status.Name,
		&status.Target.Status, &status.Target.UserType, &isAdmin,
		&status.Status, &status.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load pending management enrollment: %w", err)
	}
	status.Target.IsAdmin = isAdmin == 1
	return status, nil
}

func (m *Manager) CompleteManagementHandoff(ctx context.Context, sessionToken, userAgent string) (*Session, error) {
	current, err := m.GetSessionByToken(ctx, sessionToken)
	if err != nil || current == nil {
		return nil, ErrSecuritySessionInvalid
	}
	now := m.clock.Now().UTC()
	newSessionID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate management session ID: %w", err)
	}
	newSessionToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate management session token: %w", err)
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate management handoff completion event ID: %w", err)
	}
	userAgent = boundedUserAgent(userAgent)
	var result *Session
	err = m.runSecurityTransition(ctx, SecurityTransitionRoleChange, func(tx *sql.Tx) error {
		session, err := currentSecuritySession(ctx, tx, sessionToken, now, true)
		if err != nil || session.ID != current.ID {
			return ErrSecuritySessionInvalid
		}
		var handoffID, sourceID string
		var targetType, sourceType UserType
		var targetAdmin, sourceAdmin, targetAccounts, targetIdentities, sourceAccounts, sourceIdentities, hasTOTP, recoveryCodes int
		err = tx.QueryRowContext(ctx, `
			SELECT handoff.id, handoff.source_user_id,
			       target.user_type, target.is_admin,
			       source.user_type, source.is_admin,
			       (SELECT COUNT(*) FROM accounts WHERE user_id = target.id),
			       (SELECT COUNT(*) FROM auth_identities WHERE user_id = target.id),
			       (SELECT COUNT(*) FROM accounts WHERE user_id = source.id),
			       (SELECT COUNT(*) FROM auth_identities WHERE user_id = source.id),
			       EXISTS(SELECT 1 FROM totp_credentials t WHERE t.user_id = target.id AND t.enabled = 1 AND t.revoked_at IS NULL),
			       (SELECT COUNT(*) FROM recovery_codes r WHERE r.user_id = target.id AND r.used_at IS NULL AND r.revoked_at IS NULL)
			FROM management_handoffs handoff
			JOIN users source ON source.id = handoff.source_user_id
			JOIN users target ON target.id = handoff.target_user_id
			WHERE handoff.target_user_id = ? AND handoff.status = 'pending'
			  AND source.status = 'active' AND target.status = 'active'`, session.UserID,
		).Scan(
			&handoffID, &sourceID, &targetType, &targetAdmin, &sourceType, &sourceAdmin,
			&targetAccounts, &targetIdentities, &sourceAccounts, &sourceIdentities, &hasTOTP, &recoveryCodes,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrManagementHandoffUnavailable
		}
		if err != nil {
			return fmt.Errorf("load management handoff completion: %w", err)
		}
		if targetType != UserTypeManagement || targetAdmin != 0 || targetAccounts != 0 || targetIdentities != 0 ||
			sourceType != UserTypeWebmail || sourceAdmin != 1 || sourceAccounts+sourceIdentities == 0 {
			return ErrManagementHandoffUnavailable
		}
		if hasTOTP != 1 || recoveryCodes == 0 || session.StepUpAt == nil ||
			session.StepUpMethod != AuthenticationMethodTOTP ||
			session.StepUpAt.UTC().Before(now.Add(-securityStepUpMaximumAge)) {
			return ErrManagementEnrollmentIncomplete
		}

		var targetVersion int64
		if err := tx.QueryRowContext(ctx, `SELECT auth_version FROM users WHERE id = ?`, session.UserID).Scan(&targetVersion); err != nil {
			return err
		}
		promoted, err := tx.ExecContext(ctx, `
			UPDATE users SET is_admin = 1, mfa_required = 1,
				auth_version = auth_version + 1, updated_at = ?
			WHERE id = ? AND user_type = 'management' AND is_admin = 0`, now, session.UserID)
		if err != nil {
			return fmt.Errorf("activate management administrator: %w", err)
		}
		if rows, err := promoted.RowsAffected(); err != nil || rows != 1 {
			if err != nil {
				return fmt.Errorf("count activated management administrators: %w", err)
			}
			return ErrManagementHandoffUnavailable
		}
		demoted, err := tx.ExecContext(ctx, `
			UPDATE users SET is_admin = 0, mfa_required = 0,
				auth_version = auth_version + 1, updated_at = ?
			WHERE id = ? AND user_type = 'webmail' AND is_admin = 1`, now, sourceID)
		if err != nil {
			return fmt.Errorf("demote separated webmail user: %w", err)
		}
		if rows, err := demoted.RowsAffected(); err != nil || rows != 1 {
			if err != nil {
				return fmt.Errorf("count demoted webmail users: %w", err)
			}
			return ErrManagementHandoffUnavailable
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth_system_state SET owner_user_id = ?
			WHERE id = 1 AND owner_user_id = ?`, session.UserID, sourceID); err != nil {
			return fmt.Errorf("transfer instance owner: %w", err)
		}
		for _, userID := range []string{sourceID, session.UserID} {
			if _, err := tx.ExecContext(ctx, `
				UPDATE sessions SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
				WHERE user_id = ? AND revoked_at IS NULL`,
				now, session.UserID, SessionRevocationRoleChanged, userID,
			); err != nil {
				return fmt.Errorf("revoke handoff sessions: %w", err)
			}
		}
		completed, err := tx.ExecContext(ctx, `
			UPDATE management_handoffs SET status = 'completed', completed_at = ?
			WHERE id = ? AND status = 'pending'`, now, handoffID)
		if err != nil {
			return fmt.Errorf("complete management handoff: %w", err)
		}
		if rows, err := completed.RowsAffected(); err != nil || rows != 1 {
			if err != nil {
				return fmt.Errorf("count completed management handoffs: %w", err)
			}
			return ErrManagementHandoffUnavailable
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
				idle_expires_at, absolute_expires_at, step_up_at, step_up_method, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			newSessionID, session.UserID, hashToken(newSessionToken), targetVersion+1,
			AuthenticationMethodPassword, AssuranceLevelMultiFactor, userAgent,
			now, now, idleExpiresAt, absoluteExpiresAt, now, AuthenticationMethodTOTP, now,
		); err != nil {
			return fmt.Errorf("create management administrator session: %w", err)
		}
		metadata, err := json.Marshal(map[string]any{"handoff_id": handoffID, "demoted_user_id": sourceID})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
			eventID, now, session.UserID, sourceID, newSessionID,
			AuthEventManagementHandoffCompleted, AuthEventReasonAdministratorAction,
			userAgent, string(metadata),
		); err != nil {
			return fmt.Errorf("record management handoff completion: %w", err)
		}
		stepUpAt := now
		result = &Session{
			ID: newSessionID, UserID: session.UserID, Token: newSessionToken,
			AuthVersion: targetVersion + 1, AuthenticationMethod: AuthenticationMethodPassword,
			AssuranceLevel: AssuranceLevelMultiFactor, UserAgent: userAgent,
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
