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

const administratorUserInvitationActionReferenceContext = "gofer/auth/administrator-user-invitation-action-reference/v1"

var (
	ErrAdministratorUserInvitationTargetInvalid = errors.New("administrator user invitation target is invalid")
	ErrAdministratorUserInvitationNotActive     = errors.New("administrator user invitation is not active")
)

type AdministratorUserInvitationState string

const (
	AdministratorUserInvitationNotIssued AdministratorUserInvitationState = "not_issued"
	AdministratorUserInvitationActive    AdministratorUserInvitationState = "active"
	AdministratorUserInvitationExpired   AdministratorUserInvitationState = "expired"
	AdministratorUserInvitationRevoked   AdministratorUserInvitationState = "revoked"
)

// AdministratorUserInvitationValidationError reports only fields that an
// administrator can correct. It never contains stored account data.
type AdministratorUserInvitationValidationError struct {
	Fields map[string]string
}

func (err *AdministratorUserInvitationValidationError) Error() string {
	return "invalid user invitation details"
}

type CreateAdministratorUserInvitationOptions struct {
	ActorUserID    string
	ActorSessionID string
	Name           string
	Username       string
	Email          string
	Lifetime       time.Duration
}

type AdministratorUserInvitation struct {
	User  AdministratorUserSummary
	Name  string
	Token EnrollmentToken
}

type RotateAdministratorUserInvitationOptions struct {
	ActorUserID     string
	ActorSessionID  string
	ActionReference string
	Lifetime        time.Duration
}

type administratorUserInvitationTarget struct {
	ID       string
	Name     string
	Username string
	Email    string
}

// CreateAdministratorUserInvitation creates an ordinary pending Gofer user
// and its first enrollment token in one security transition. The raw token is
// returned only from this call; SQLite stores only its SHA-256 hash.
func (m *Manager) CreateAdministratorUserInvitation(ctx context.Context, options CreateAdministratorUserInvitationOptions) (*AdministratorUserInvitation, error) {
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
	email, emailNormalized, err := PrepareEmail(options.Email)
	if err != nil {
		fieldErrors["email"] = err.Error()
	}
	if len(fieldErrors) != 0 {
		return nil, &AdministratorUserInvitationValidationError{Fields: fieldErrors}
	}

	lifetime, err := enrollmentTokenLifetime(EnrollmentTokenPurposeEnrollment, options.Lifetime)
	if err != nil {
		return nil, err
	}
	userID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate invited user ID: %w", err)
	}
	tokenID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate invitation token ID: %w", err)
	}
	rawToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate invitation token: %w", err)
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate invitation event ID: %w", err)
	}

	now := m.clock.Now().UTC()
	result := &AdministratorUserInvitation{
		Name: name,
		User: AdministratorUserSummary{
			ID: userID, Username: username, Email: email, Status: UserStatusPending,
		},
		Token: EnrollmentToken{
			ID: tokenID, Token: rawToken, UserID: userID, CreatedBy: actorUserID,
			Purpose: EnrollmentTokenPurposeEnrollment, CreatedAt: now, ExpiresAt: now.Add(lifetime),
		},
	}

	err = m.runSecurityTransition(ctx, SecurityTransitionEnrollment, func(tx *sql.Tx) error {
		if err := requireActiveManagementAdministrator(ctx, tx, actorUserID); err != nil {
			return err
		}
		if err := requireRecentAdministratorStepUp(ctx, tx, actorUserID, actorSessionID, now); err != nil {
			return err
		}
		collisions, err := administratorInvitationIdentifierCollisions(ctx, tx, usernameNormalized, emailNormalized)
		if err != nil {
			return err
		}
		if len(collisions) != 0 {
			return &AdministratorUserInvitationValidationError{Fields: collisions}
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO users (
				id, email, email_normalized, username, username_normalized, name,
				status, auth_version, mfa_required, user_type, is_admin, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, 'pending', 1, 0, 'webmail', 0, ?, ?)`,
			result.User.ID, result.User.Email, emailNormalized, result.User.Username,
			usernameNormalized, result.Name, now, now,
		); err != nil {
			return fmt.Errorf("create pending invited user: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_enrollment_tokens (
				id, user_id, created_by, token_hash, purpose, created_at, expires_at
			) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			result.Token.ID, result.Token.UserID, result.Token.CreatedBy,
			hashToken(result.Token.Token), result.Token.Purpose,
			result.Token.CreatedAt, result.Token.ExpiresAt,
		); err != nil {
			return fmt.Errorf("create invited user enrollment token: %w", err)
		}

		metadata, err := enrollmentTokenEventJSON(result.Token.ID, result.Token.Purpose, 0)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
				success, reason, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, actorUserID, result.User.ID, actorSessionID,
			AuthEventEnrollmentIssued, AuthEventReasonAdministratorAction, metadata,
		); err != nil {
			return fmt.Errorf("record invited user enrollment issuance: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func administratorInvitationIdentifierCollisions(ctx context.Context, tx *sql.Tx, usernameNormalized, emailNormalized string) (map[string]string, error) {
	fields := make(map[string]string)
	for field, normalized := range map[string]string{
		"username": usernameNormalized,
		"email":    emailNormalized,
	} {
		var conflictingID string
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM users
			WHERE
				CASE
					WHEN trim(COALESCE(username_normalized, '')) != '' THEN username_normalized
					ELSE lower(trim(COALESCE(username, '')))
				END = ? OR
				CASE
					WHEN trim(COALESCE(email_normalized, '')) != '' THEN email_normalized
					ELSE lower(trim(email))
				END = ?
			ORDER BY id LIMIT 1`, normalized, normalized).Scan(&conflictingID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("check invited user %s collision: %w", field, err)
		}
		fields[field] = "That " + field + " is already used by another Gofer user."
	}
	return fields, nil
}

func (m *Manager) administratorUserInvitationActionReference(actorSessionID, targetUserID string) (string, error) {
	if len(m.bucketHashKey) < minimumBucketHashKeyBytes {
		return "", fmt.Errorf("administrator invitation action key must be at least %d bytes", minimumBucketHashKeyBytes)
	}
	actorDigest := sha256.Sum256([]byte(actorSessionID))
	targetDigest := sha256.Sum256([]byte(targetUserID))
	mac := hmac.New(sha256.New, m.bucketHashKey)
	_, _ = mac.Write([]byte(administratorUserInvitationActionReferenceContext))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(actorDigest[:])
	_, _ = mac.Write(targetDigest[:])
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func canonicalAdministratorUserInvitationActionReference(reference string) bool {
	if len(reference) != base64.RawURLEncoding.EncodedLen(sha256.Size) {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(reference)
	return err == nil && len(decoded) == sha256.Size &&
		base64.RawURLEncoding.EncodeToString(decoded) == reference
}

func (m *Manager) administratorUserInvitationTarget(
	ctx context.Context, tx *sql.Tx, actorSessionID, actionReference string,
) (*administratorUserInvitationTarget, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, name, COALESCE(username, ''), email
		FROM users
		WHERE status = 'pending' AND user_type = 'webmail' AND is_admin = 0
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list administrator invitation targets: %w", err)
	}
	defer rows.Close()
	var target *administratorUserInvitationTarget
	for rows.Next() {
		candidate := &administratorUserInvitationTarget{}
		if err := rows.Scan(&candidate.ID, &candidate.Name, &candidate.Username, &candidate.Email); err != nil {
			return nil, fmt.Errorf("scan administrator invitation target: %w", err)
		}
		candidateReference, err := m.administratorUserInvitationActionReference(actorSessionID, candidate.ID)
		if err != nil {
			return nil, err
		}
		if hmac.Equal([]byte(candidateReference), []byte(actionReference)) {
			target = candidate
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate administrator invitation targets: %w", err)
	}
	if target == nil {
		return nil, ErrAdministratorUserInvitationTargetInvalid
	}
	return target, nil
}

// RevokeAdministratorUserInvitation invalidates every currently redeemable
// enrollment invitation for one pending standard user selected through an
// opaque reference bound to the exact administrator session.
func (m *Manager) RevokeAdministratorUserInvitation(
	ctx context.Context, actorUserID, actorSessionID, actionReference string,
) error {
	actorUserID = strings.TrimSpace(actorUserID)
	actorSessionID = strings.TrimSpace(actorSessionID)
	actionReference = strings.TrimSpace(actionReference)
	if actorUserID == "" {
		return ErrAdministratorRequired
	}
	if actorSessionID == "" {
		return ErrRecentStepUpRequired
	}
	if !canonicalAdministratorUserInvitationActionReference(actionReference) {
		return ErrAdministratorUserInvitationTargetInvalid
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return fmt.Errorf("generate administrator invitation revocation event ID: %w", err)
	}
	now := m.clock.Now().UTC()
	return m.runSecurityTransition(ctx, SecurityTransitionEnrollment, func(tx *sql.Tx) error {
		if err := requireActiveManagementAdministrator(ctx, tx, actorUserID); err != nil {
			return err
		}
		if err := requireRecentAdministratorStepUp(ctx, tx, actorUserID, actorSessionID, now); err != nil {
			return err
		}
		target, err := m.administratorUserInvitationTarget(ctx, tx, actorSessionID, actionReference)
		if err != nil {
			return err
		}
		var tokenID string
		if err := tx.QueryRowContext(ctx, `
			SELECT id FROM user_enrollment_tokens
			WHERE user_id = ? AND purpose = 'enrollment'
			  AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?
			ORDER BY created_at DESC, id DESC LIMIT 1`, target.ID, now,
		).Scan(&tokenID); errors.Is(err, sql.ErrNoRows) {
			return ErrAdministratorUserInvitationNotActive
		} else if err != nil {
			return fmt.Errorf("load active administrator invitation: %w", err)
		}
		revoked, err := tx.ExecContext(ctx, `
			UPDATE user_enrollment_tokens SET revoked_at = ?
			WHERE user_id = ? AND purpose = 'enrollment'
			  AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
			now, target.ID, now,
		)
		if err != nil {
			return fmt.Errorf("revoke administrator invitation: %w", err)
		}
		revokedCount, err := revoked.RowsAffected()
		if err != nil {
			return fmt.Errorf("count revoked administrator invitations: %w", err)
		}
		if revokedCount == 0 {
			return ErrAdministratorUserInvitationNotActive
		}
		metadata, err := json.Marshal(struct {
			TokenID      string                 `json:"token_id"`
			Purpose      EnrollmentTokenPurpose `json:"purpose"`
			RevokedCount int64                  `json:"revoked_count"`
		}{TokenID: tokenID, Purpose: EnrollmentTokenPurposeEnrollment, RevokedCount: revokedCount})
		if err != nil {
			return fmt.Errorf("encode administrator invitation revocation metadata: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
				success, reason, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, actorUserID, target.ID, actorSessionID,
			AuthEventEnrollmentRevoked, AuthEventReasonAdministratorAction, string(metadata),
		); err != nil {
			return fmt.Errorf("record administrator invitation revocation: %w", err)
		}
		return nil
	})
}

// RotateAdministratorUserInvitation revokes every prior unused invitation and
// creates one replacement. The raw replacement token is returned exactly once;
// only its SHA-256 hash is persisted.
func (m *Manager) RotateAdministratorUserInvitation(
	ctx context.Context, options RotateAdministratorUserInvitationOptions,
) (*AdministratorUserInvitation, error) {
	actorUserID := strings.TrimSpace(options.ActorUserID)
	actorSessionID := strings.TrimSpace(options.ActorSessionID)
	actionReference := strings.TrimSpace(options.ActionReference)
	if actorUserID == "" {
		return nil, ErrAdministratorRequired
	}
	if actorSessionID == "" {
		return nil, ErrRecentStepUpRequired
	}
	if !canonicalAdministratorUserInvitationActionReference(actionReference) {
		return nil, ErrAdministratorUserInvitationTargetInvalid
	}
	lifetime, err := enrollmentTokenLifetime(EnrollmentTokenPurposeEnrollment, options.Lifetime)
	if err != nil {
		return nil, err
	}
	tokenID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate rotated administrator invitation token ID: %w", err)
	}
	rawToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate rotated administrator invitation token: %w", err)
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate rotated administrator invitation event ID: %w", err)
	}
	now := m.clock.Now().UTC()
	var invitation *AdministratorUserInvitation
	err = m.runSecurityTransition(ctx, SecurityTransitionEnrollment, func(tx *sql.Tx) error {
		if err := requireActiveManagementAdministrator(ctx, tx, actorUserID); err != nil {
			return err
		}
		if err := requireRecentAdministratorStepUp(ctx, tx, actorUserID, actorSessionID, now); err != nil {
			return err
		}
		target, err := m.administratorUserInvitationTarget(ctx, tx, actorSessionID, actionReference)
		if err != nil {
			return err
		}
		replaced, err := tx.ExecContext(ctx, `
			UPDATE user_enrollment_tokens SET revoked_at = ?
			WHERE user_id = ? AND purpose = 'enrollment'
			  AND used_at IS NULL AND revoked_at IS NULL`, now, target.ID)
		if err != nil {
			return fmt.Errorf("revoke prior administrator invitations: %w", err)
		}
		replacedCount, err := replaced.RowsAffected()
		if err != nil {
			return fmt.Errorf("count replaced administrator invitations: %w", err)
		}
		token := EnrollmentToken{
			ID: tokenID, Token: rawToken, UserID: target.ID, CreatedBy: actorUserID,
			Purpose: EnrollmentTokenPurposeEnrollment, CreatedAt: now, ExpiresAt: now.Add(lifetime),
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_enrollment_tokens (
				id, user_id, created_by, token_hash, purpose, created_at, expires_at
			) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			token.ID, token.UserID, token.CreatedBy, hashToken(token.Token), token.Purpose,
			token.CreatedAt, token.ExpiresAt,
		); err != nil {
			return fmt.Errorf("insert rotated administrator invitation: %w", err)
		}
		metadata, err := enrollmentTokenEventJSON(token.ID, token.Purpose, replacedCount)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
				success, reason, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, actorUserID, target.ID, actorSessionID,
			AuthEventEnrollmentIssued, AuthEventReasonAdministratorAction, metadata,
		); err != nil {
			return fmt.Errorf("record rotated administrator invitation: %w", err)
		}
		invitation = &AdministratorUserInvitation{
			Name: target.Name,
			User: AdministratorUserSummary{
				ID: target.ID, Username: target.Username, Email: target.Email,
				Status: UserStatusPending, UserType: UserTypeWebmail,
			},
			Token: token,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return invitation, nil
}
