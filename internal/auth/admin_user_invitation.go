package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
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
		if err := requireActiveAdministrator(ctx, tx, actorUserID); err != nil {
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
				status, auth_version, mfa_required, is_admin, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, 'pending', 1, 0, 0, ?, ?)`,
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
