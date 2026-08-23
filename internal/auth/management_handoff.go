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
	"strings"
	"time"
)

var (
	ErrManagementHandoffRequired       = errors.New("management account separation is required")
	ErrManagementHandoffUnavailable    = errors.New("management account separation is unavailable")
	ErrManagementInvitationUnavailable = errors.New("management handoff invitation cannot be reissued")
	ErrManagementEnrollmentIncomplete  = errors.New("management account security enrollment is incomplete")
)

const managementHandoffActionReferenceContext = "gofer/auth/management-handoff-action-reference/v1"

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
	ID                        string
	SourceID                  string
	Target                    AdministratorUserSummary
	Name                      string
	Status                    string
	CreatedAt                 time.Time
	InvitationState           AdministratorUserInvitationState
	InvitationExpiresAt       *time.Time
	InvitationActionReference string
}

type ReissueManagementHandoffInvitationOptions struct {
	ActorUserID     string
	ActorSessionID  string
	ActionReference string
	Lifetime        time.Duration
}

type CancelManagementHandoffOptions struct {
	ActorUserID     string
	ActorSessionID  string
	ActionReference string
}

type pendingManagementHandoffTarget struct {
	handoffID        string
	targetID         string
	name             string
	username         string
	createdAt        time.Time
	targetStatus     UserStatus
	targetType       UserType
	targetAdmin      int
	targetAccounts   int
	targetIdentities int
	latestTokenID    string
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

func (m *Manager) managementHandoffActionReference(actorSessionID, handoffID, targetID, latestTokenID string) (string, error) {
	if len(m.bucketHashKey) < minimumBucketHashKeyBytes {
		return "", fmt.Errorf("management handoff action key must be at least %d bytes", minimumBucketHashKeyBytes)
	}
	mac := hmac.New(sha256.New, m.bucketHashKey)
	_, _ = mac.Write([]byte(managementHandoffActionReferenceContext))
	for _, value := range []string{actorSessionID, handoffID, targetID, latestTokenID} {
		digest := sha256.Sum256([]byte(value))
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write(digest[:])
	}
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func canonicalManagementHandoffActionReference(reference string) bool {
	if len(reference) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(reference)
	return err == nil && len(decoded) == sha256.Size &&
		base64.RawURLEncoding.EncodeToString(decoded) == reference
}

func loadPendingManagementHandoffTarget(ctx context.Context, tx *sql.Tx, sourceUserID string) (*pendingManagementHandoffTarget, error) {
	target := &pendingManagementHandoffTarget{}
	err := tx.QueryRowContext(ctx, `
		SELECT handoff.id, target.id, target.name, target.username, handoff.created_at,
		       target.status, target.user_type, target.is_admin,
		       (SELECT COUNT(*) FROM accounts WHERE user_id = target.id),
		       (SELECT COUNT(*) FROM auth_identities WHERE user_id = target.id),
		       COALESCE((
		           SELECT token.id FROM user_enrollment_tokens token
		           WHERE token.user_id = target.id AND token.purpose = 'enrollment'
		           ORDER BY token.created_at DESC, token.id DESC LIMIT 1
		       ), '')
		FROM management_handoffs handoff
		JOIN users source ON source.id = handoff.source_user_id
		JOIN users target ON target.id = handoff.target_user_id
		WHERE handoff.source_user_id = ? AND handoff.status = 'pending'
		  AND source.status = 'active' AND source.user_type = 'webmail' AND source.is_admin = 1
		  AND ((SELECT COUNT(*) FROM accounts WHERE user_id = source.id) +
		       (SELECT COUNT(*) FROM auth_identities WHERE user_id = source.id)) > 0`, sourceUserID,
	).Scan(
		&target.handoffID, &target.targetID, &target.name, &target.username, &target.createdAt,
		&target.targetStatus, &target.targetType, &target.targetAdmin,
		&target.targetAccounts, &target.targetIdentities, &target.latestTokenID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrManagementHandoffUnavailable
	}
	if err != nil {
		return nil, fmt.Errorf("load pending management handoff target: %w", err)
	}
	if target.targetType != UserTypeManagement || target.targetAdmin != 0 ||
		target.targetAccounts != 0 || target.targetIdentities != 0 {
		return nil, ErrManagementHandoffUnavailable
	}
	return target, nil
}

func (m *Manager) pendingManagementHandoffTargetForAction(
	ctx context.Context, tx *sql.Tx, sourceUserID, actorSessionID, actionReference string,
) (*pendingManagementHandoffTarget, error) {
	if !canonicalManagementHandoffActionReference(actionReference) {
		return nil, ErrManagementHandoffUnavailable
	}
	target, err := loadPendingManagementHandoffTarget(ctx, tx, sourceUserID)
	if err != nil {
		return nil, err
	}
	expected, err := m.managementHandoffActionReference(
		actorSessionID, target.handoffID, target.targetID, target.latestTokenID,
	)
	if err != nil {
		return nil, err
	}
	if !hmac.Equal([]byte(expected), []byte(actionReference)) {
		return nil, ErrManagementHandoffUnavailable
	}
	return target, nil
}

func (m *Manager) GetPendingManagementHandoff(ctx context.Context, sourceUserID, actorSessionID string) (*ManagementHandoffStatus, error) {
	sourceUserID = strings.TrimSpace(sourceUserID)
	actorSessionID = strings.TrimSpace(actorSessionID)
	if err := m.requireActiveAdministrator(ctx, sourceUserID); err != nil {
		return nil, err
	}
	status := &ManagementHandoffStatus{SourceID: sourceUserID}
	var isAdmin int
	var tokenID sql.NullString
	var tokenExpiresAt, tokenUsedAt, tokenRevokedAt sql.NullTime
	err := m.db.Read().QueryRowContext(ctx, `
		SELECT handoff.id, target.id, target.username,
		       target.name, target.status, target.user_type, target.is_admin,
		       handoff.status, handoff.created_at,
		       token.id, token.expires_at, token.used_at, token.revoked_at
		FROM management_handoffs handoff
		JOIN users target ON target.id = handoff.target_user_id
		LEFT JOIN user_enrollment_tokens token ON token.id = (
			SELECT candidate.id FROM user_enrollment_tokens candidate
			WHERE candidate.user_id = target.id AND candidate.purpose = 'enrollment'
			ORDER BY candidate.created_at DESC, candidate.id DESC LIMIT 1
		)
		WHERE handoff.source_user_id = ? AND handoff.status = 'pending'`, sourceUserID,
	).Scan(
		&status.ID, &status.Target.ID, &status.Target.Username,
		&status.Name, &status.Target.Status, &status.Target.UserType, &isAdmin,
		&status.Status, &status.CreatedAt,
		&tokenID, &tokenExpiresAt, &tokenUsedAt, &tokenRevokedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load pending management handoff: %w", err)
	}
	status.Target.IsAdmin = isAdmin == 1
	status.InvitationState = AdministratorUserInvitationNotIssued
	now := m.clock.Now().UTC()
	switch {
	case tokenID.Valid && !tokenUsedAt.Valid && !tokenRevokedAt.Valid && tokenExpiresAt.Valid && tokenExpiresAt.Time.After(now):
		status.InvitationState = AdministratorUserInvitationActive
		expiresAt := tokenExpiresAt.Time
		status.InvitationExpiresAt = &expiresAt
	case tokenID.Valid && tokenRevokedAt.Valid:
		status.InvitationState = AdministratorUserInvitationRevoked
	case tokenID.Valid && !tokenUsedAt.Valid && tokenExpiresAt.Valid && !tokenExpiresAt.Time.After(now):
		status.InvitationState = AdministratorUserInvitationExpired
		expiresAt := tokenExpiresAt.Time
		status.InvitationExpiresAt = &expiresAt
	}
	if actorSessionID != "" {
		latestTokenID := ""
		if tokenID.Valid {
			latestTokenID = tokenID.String
		}
		status.InvitationActionReference, err = m.managementHandoffActionReference(
			actorSessionID, status.ID, status.Target.ID, latestTokenID,
		)
		if err != nil {
			return nil, err
		}
	}
	return status, nil
}

// ReissueManagementHandoffInvitation replaces the enrollment bearer for the
// same still-pending management identity. The action reference is bound to the
// exact administrator session and latest invitation generation, so concurrent
// or replayed submissions cannot both return usable bearer tokens.
func (m *Manager) ReissueManagementHandoffInvitation(
	ctx context.Context, options ReissueManagementHandoffInvitationOptions,
) (*ManagementHandoff, error) {
	actorUserID := strings.TrimSpace(options.ActorUserID)
	actorSessionID := strings.TrimSpace(options.ActorSessionID)
	actionReference := strings.TrimSpace(options.ActionReference)
	if actorUserID == "" {
		return nil, ErrAdministratorRequired
	}
	if actorSessionID == "" {
		return nil, ErrRecentStepUpRequired
	}
	if !canonicalManagementHandoffActionReference(actionReference) {
		return nil, ErrManagementHandoffUnavailable
	}
	lifetime, err := enrollmentTokenLifetime(EnrollmentTokenPurposeEnrollment, options.Lifetime)
	if err != nil {
		return nil, err
	}
	tokenID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate reissued management invitation ID: %w", err)
	}
	rawToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate reissued management invitation token: %w", err)
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate management invitation reissue event ID: %w", err)
	}
	now := m.clock.Now().UTC()
	var result *ManagementHandoff
	err = m.runSecurityTransition(ctx, SecurityTransitionEnrollment, func(tx *sql.Tx) error {
		if err := requireActiveAdministrator(ctx, tx, actorUserID); err != nil {
			return err
		}
		if err := requireRecentAdministratorStepUp(ctx, tx, actorUserID, actorSessionID, now); err != nil {
			return err
		}
		target, err := m.pendingManagementHandoffTargetForAction(
			ctx, tx, actorUserID, actorSessionID, actionReference,
		)
		if err != nil {
			return err
		}
		if target.targetStatus != UserStatusPending {
			return ErrManagementInvitationUnavailable
		}
		revoked, err := tx.ExecContext(ctx, `
			UPDATE user_enrollment_tokens SET revoked_at = ?
			WHERE user_id = ? AND purpose = 'enrollment'
			  AND used_at IS NULL AND revoked_at IS NULL`, now, target.targetID)
		if err != nil {
			return fmt.Errorf("revoke prior management invitations: %w", err)
		}
		revokedCount, err := revoked.RowsAffected()
		if err != nil {
			return fmt.Errorf("count revoked management invitations: %w", err)
		}
		token := EnrollmentToken{
			ID: tokenID, Token: rawToken, UserID: target.targetID, CreatedBy: actorUserID,
			Purpose: EnrollmentTokenPurposeEnrollment, CreatedAt: now, ExpiresAt: now.Add(lifetime),
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_enrollment_tokens (
				id, user_id, created_by, token_hash, purpose, created_at, expires_at
			) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			token.ID, token.UserID, token.CreatedBy, hashToken(token.Token), token.Purpose,
			token.CreatedAt, token.ExpiresAt,
		); err != nil {
			return fmt.Errorf("create reissued management invitation: %w", err)
		}
		metadata, err := json.Marshal(struct {
			HandoffID    string `json:"handoff_id"`
			TokenID      string `json:"token_id"`
			RevokedCount int64  `json:"revoked_count"`
		}{HandoffID: target.handoffID, TokenID: token.ID, RevokedCount: revokedCount})
		if err != nil {
			return fmt.Errorf("encode management invitation reissue metadata: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
				success, reason, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, actorUserID, target.targetID, actorSessionID,
			AuthEventManagementHandoffReissued, AuthEventReasonAdministratorAction, string(metadata),
		); err != nil {
			return fmt.Errorf("record management invitation reissue: %w", err)
		}
		result = &ManagementHandoff{
			ID: target.handoffID, SourceID: actorUserID, Name: target.name,
			Target: AdministratorUserSummary{
				ID: target.targetID, Username: target.username, Status: target.targetStatus,
				UserType: target.targetType,
			},
			Token: token, Status: "pending", CreatedAt: target.createdAt,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// CancelManagementHandoff revokes the unfinished replacement identity's
// enrollment bearers and sessions, disables it, and cancels the handoff. The
// source administrator, both users' roles, instance ownership, and mailbox
// ownership are deliberately left unchanged.
func (m *Manager) CancelManagementHandoff(ctx context.Context, options CancelManagementHandoffOptions) error {
	actorUserID := strings.TrimSpace(options.ActorUserID)
	actorSessionID := strings.TrimSpace(options.ActorSessionID)
	actionReference := strings.TrimSpace(options.ActionReference)
	if actorUserID == "" {
		return ErrAdministratorRequired
	}
	if actorSessionID == "" {
		return ErrRecentStepUpRequired
	}
	if !canonicalManagementHandoffActionReference(actionReference) {
		return ErrManagementHandoffUnavailable
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return fmt.Errorf("generate management handoff cancellation event ID: %w", err)
	}
	now := m.clock.Now().UTC()
	return m.runSecurityTransition(ctx, SecurityTransitionEnrollment, func(tx *sql.Tx) error {
		if err := requireActiveAdministrator(ctx, tx, actorUserID); err != nil {
			return err
		}
		if err := requireRecentAdministratorStepUp(ctx, tx, actorUserID, actorSessionID, now); err != nil {
			return err
		}
		target, err := m.pendingManagementHandoffTargetForAction(
			ctx, tx, actorUserID, actorSessionID, actionReference,
		)
		if err != nil {
			return err
		}
		if target.targetStatus != UserStatusPending && target.targetStatus != UserStatusActive &&
			target.targetStatus != UserStatusDisabled {
			return ErrManagementHandoffUnavailable
		}
		revokedTokens, err := tx.ExecContext(ctx, `
			UPDATE user_enrollment_tokens SET revoked_at = ?
			WHERE user_id = ? AND used_at IS NULL AND revoked_at IS NULL`, now, target.targetID)
		if err != nil {
			return fmt.Errorf("revoke canceled management invitations: %w", err)
		}
		revokedTokenCount, err := revokedTokens.RowsAffected()
		if err != nil {
			return fmt.Errorf("count canceled management invitations: %w", err)
		}
		revokedSessions, err := tx.ExecContext(ctx, `
			UPDATE sessions SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
			WHERE user_id = ? AND revoked_at IS NULL`,
			now, actorUserID, SessionRevocationUserDisabled, target.targetID,
		)
		if err != nil {
			return fmt.Errorf("revoke canceled management sessions: %w", err)
		}
		revokedSessionCount, err := revokedSessions.RowsAffected()
		if err != nil {
			return fmt.Errorf("count canceled management sessions: %w", err)
		}
		disabled, err := tx.ExecContext(ctx, `
			UPDATE users
			SET status = 'disabled',
			    disabled_at = COALESCE(disabled_at, ?),
			    disabled_by = COALESCE(disabled_by, ?),
			    auth_version = auth_version + CASE WHEN status = 'disabled' THEN 0 ELSE 1 END,
			    updated_at = ?
			WHERE id = ? AND user_type = 'management' AND is_admin = 0`,
			now, actorUserID, now, target.targetID,
		)
		if err != nil {
			return fmt.Errorf("disable canceled management target: %w", err)
		}
		if rows, err := disabled.RowsAffected(); err != nil || rows != 1 {
			if err != nil {
				return fmt.Errorf("count disabled management targets: %w", err)
			}
			return ErrManagementHandoffUnavailable
		}
		canceled, err := tx.ExecContext(ctx, `
			UPDATE management_handoffs SET status = 'canceled', canceled_at = ?
			WHERE id = ? AND status = 'pending'`, now, target.handoffID)
		if err != nil {
			return fmt.Errorf("cancel management handoff: %w", err)
		}
		if rows, err := canceled.RowsAffected(); err != nil || rows != 1 {
			if err != nil {
				return fmt.Errorf("count canceled management handoffs: %w", err)
			}
			return ErrManagementHandoffUnavailable
		}
		metadata, err := json.Marshal(struct {
			HandoffID       string     `json:"handoff_id"`
			PreviousStatus  UserStatus `json:"previous_target_status"`
			RevokedTokens   int64      `json:"revoked_tokens"`
			RevokedSessions int64      `json:"revoked_sessions"`
		}{
			HandoffID: target.handoffID, PreviousStatus: target.targetStatus,
			RevokedTokens: revokedTokenCount, RevokedSessions: revokedSessionCount,
		})
		if err != nil {
			return fmt.Errorf("encode management handoff cancellation metadata: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
				success, reason, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, actorUserID, target.targetID, actorSessionID,
			AuthEventManagementHandoffCanceled, AuthEventReasonAdministratorAction, string(metadata),
		); err != nil {
			return fmt.Errorf("record management handoff cancellation: %w", err)
		}
		return nil
	})
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
