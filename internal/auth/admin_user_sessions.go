package auth

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/authz"
)

// ErrAdministratorUserSignOutTargetInvalid: only someone else, active and
// not being deleted, can be signed out by an administrator.
var ErrAdministratorUserSignOutTargetInvalid = errors.New("administrator user sign-out target is invalid")

type SignOutAdministratorUserOptions struct {
	ActorUserID    string
	ActorSessionID string
	TargetUserID   string
}

type SignOutAdministratorUserResult struct {
	// RevokedSessions is how many sessions ended; none means the person
	// wasn't signed in anywhere, and nothing was recorded.
	RevokedSessions int64
}

// SignOutAdministratorUser ends every session another active person has,
// without changing their credentials, status or auth version: they can sign
// in again straight away. It records the change when it ended any session.
func (m *Manager) SignOutAdministratorUser(ctx context.Context, options SignOutAdministratorUserOptions) (*SignOutAdministratorUserResult, error) {
	actorUserID := strings.TrimSpace(options.ActorUserID)
	actorSessionID := strings.TrimSpace(options.ActorSessionID)
	targetUserID := strings.TrimSpace(options.TargetUserID)
	if actorUserID == "" {
		return nil, ErrAdministratorRequired
	}
	if actorSessionID == "" {
		return nil, ErrRecentStepUpRequired
	}
	if targetUserID == "" {
		return nil, ErrAdministratorUserSignOutTargetInvalid
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate administrator sign-out event ID: %w", err)
	}

	now := m.clock.Now().UTC()
	result := &SignOutAdministratorUserResult{}
	err = m.runSecurityTransition(ctx, SecurityTransitionSessionRevocation, func(tx *sql.Tx) error {
		if err := requireAdministratorPermission(ctx, tx, actorUserID, actorSessionID, authz.UsersAccess, now); err != nil {
			return err
		}
		if targetUserID == actorUserID {
			return ErrAdministratorUserSignOutTargetInvalid
		}
		if err := requireAdministratorTargetPermission(ctx, tx, actorUserID, actorSessionID, targetUserID, now); err != nil {
			return err
		}
		var status UserStatus
		var deletionPending int
		var authVersion int64
		if err := tx.QueryRowContext(ctx, `SELECT status, deletion_pending, auth_version FROM users WHERE id = ?`, targetUserID).
			Scan(&status, &deletionPending, &authVersion); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrAdministratorUserSignOutTargetInvalid
			}
			return fmt.Errorf("load administrator sign-out target: %w", err)
		}
		if status != UserStatusActive || deletionPending != 0 {
			return ErrAdministratorUserSignOutTargetInvalid
		}

		// Sessions that expired, or belong to an older auth version, are
		// over already; only the ones still working count.
		revoked, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
			WHERE user_id = ? AND auth_version = ?
			  AND revoked_at IS NULL AND idle_expires_at > ? AND absolute_expires_at > ?`,
			now, actorUserID, SessionRevocationAdminAction, targetUserID, authVersion, now, now,
		)
		if err != nil {
			return fmt.Errorf("revoke administrator sign-out sessions: %w", err)
		}
		result.RevokedSessions, err = revoked.RowsAffected()
		if err != nil {
			return fmt.Errorf("count administrator sign-out sessions: %w", err)
		}
		if result.RevokedSessions == 0 {
			return nil
		}

		metadata, err := json.Marshal(struct {
			Scope           string `json:"scope"`
			RevokedSessions int64  `json:"revoked_sessions"`
		}{Scope: "all", RevokedSessions: result.RevokedSessions})
		if err != nil {
			return fmt.Errorf("encode administrator sign-out event: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
				success, reason, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, actorUserID, targetUserID, actorSessionID,
			AuthEventSessionRevoked, AuthEventReasonAdministratorAction, string(metadata),
		); err != nil {
			return fmt.Errorf("record administrator sign-out: %w", err)
		}
		return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
			Action:        "user.signed_out",
			Target:        audit.Target{Kind: auditTargetUser, ID: targetUserID},
			Metadata:      map[string]any{"revoked_sessions": result.RevokedSessions},
			CorrelationID: eventID,
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ErrAdministratorUserSessionGone means the session an administrator chose
// has ended, or was never one of the person's.
var ErrAdministratorUserSessionGone = errors.New("administrator user session is gone")

const administratorSessionActionReferenceContext = "gofer/auth/administrator-session-action-reference/v1"

// administratorSessionActionReference names one of a person's sessions to
// one administrator session, so a page never shows a session's ID and a
// reference is no use from another administrator session.
func (m *Manager) administratorSessionActionReference(actorSessionID, userID, sessionID string) (string, error) {
	return m.sessionActionReference(administratorSessionActionReferenceContext, actorSessionID, userID+"\x00"+sessionID)
}

// AdministratorUserSession is one place a person is signed in, as an
// administrator sees it.
type AdministratorUserSession struct {
	// Reference names the session to the administrator session that
	// listed it.
	Reference            string
	UserAgent            string
	AuthenticationMethod AuthenticationMethod
	AssuranceLevel       AssuranceLevel
	AuthenticatedAt      time.Time
	LastUsedAt           time.Time
	// ExpiresAt is when the session ends unless it's used again first.
	ExpiresAt time.Time
}

// ListAdministratorUserSessions returns the sessions that still sign
// userID in, most recently used first.
func (m *Manager) ListAdministratorUserSessions(ctx context.Context, actorUserID, actorSessionID, userID string) ([]AdministratorUserSession, error) {
	actorUserID, actorSessionID, userID = strings.TrimSpace(actorUserID), strings.TrimSpace(actorSessionID), strings.TrimSpace(userID)
	tx, err := m.db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin administrator user sessions: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := authz.Require(ctx, tx, actorUserID, authz.UsersRead); err != nil {
		return nil, err
	}
	now := m.clock.Now().UTC()
	rows, err := tx.QueryContext(ctx, `
		SELECT s.id, s.user_agent, s.authentication_method, s.assurance_level, s.authenticated_at,
		       s.last_used_at, s.idle_expires_at, s.absolute_expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.user_id = ? AND s.auth_version = u.auth_version
		  AND s.revoked_at IS NULL AND s.idle_expires_at > ? AND s.absolute_expires_at > ?
		ORDER BY s.last_used_at DESC, s.id`, userID, now, now)
	if err != nil {
		return nil, fmt.Errorf("list administrator user sessions: %w", err)
	}
	defer rows.Close()
	sessions := make([]AdministratorUserSession, 0)
	for rows.Next() {
		var session AdministratorUserSession
		var id string
		var idle, absolute time.Time
		if err := rows.Scan(&id, &session.UserAgent, &session.AuthenticationMethod, &session.AssuranceLevel,
			&session.AuthenticatedAt, &session.LastUsedAt, &idle, &absolute); err != nil {
			return nil, fmt.Errorf("scan administrator user session: %w", err)
		}
		session.ExpiresAt = idle
		if absolute.Before(idle) {
			session.ExpiresAt = absolute
		}
		if session.Reference, err = m.administratorSessionActionReference(actorSessionID, userID, id); err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate administrator user sessions: %w", err)
	}
	return sessions, nil
}

type SignOutAdministratorUserSessionOptions struct {
	ActorUserID    string
	ActorSessionID string
	TargetUserID   string
	// Reference is the session's, as ListAdministratorUserSessions gave it
	// to this administrator session.
	Reference string
}

type SignOutAdministratorUserSessionResult struct {
	// UserAgent is the ended session's browser, to say which it was.
	UserAgent string
}

// SignOutAdministratorUserSession ends one session of another active
// person, as signing them out everywhere does for all of them.
func (m *Manager) SignOutAdministratorUserSession(ctx context.Context, options SignOutAdministratorUserSessionOptions) (*SignOutAdministratorUserSessionResult, error) {
	actorUserID := strings.TrimSpace(options.ActorUserID)
	actorSessionID := strings.TrimSpace(options.ActorSessionID)
	targetUserID := strings.TrimSpace(options.TargetUserID)
	if actorUserID == "" {
		return nil, ErrAdministratorRequired
	}
	if actorSessionID == "" {
		return nil, ErrRecentStepUpRequired
	}
	if targetUserID == "" {
		return nil, ErrAdministratorUserSignOutTargetInvalid
	}
	if !canonicalSecuritySessionActionReference(options.Reference) {
		return nil, ErrAdministratorUserSessionGone
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate administrator session sign-out event ID: %w", err)
	}
	now := m.clock.Now().UTC()
	result := &SignOutAdministratorUserSessionResult{}
	err = m.runSecurityTransition(ctx, SecurityTransitionSessionRevocation, func(tx *sql.Tx) error {
		if err := requireAdministratorPermission(ctx, tx, actorUserID, actorSessionID, authz.UsersAccess, now); err != nil {
			return err
		}
		if targetUserID == actorUserID {
			return ErrAdministratorUserSignOutTargetInvalid
		}
		if err := requireAdministratorTargetPermission(ctx, tx, actorUserID, actorSessionID, targetUserID, now); err != nil {
			return err
		}
		var status UserStatus
		var deletionPending int
		var authVersion int64
		if err := tx.QueryRowContext(ctx, `SELECT status, deletion_pending, auth_version FROM users WHERE id = ?`, targetUserID).
			Scan(&status, &deletionPending, &authVersion); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrAdministratorUserSignOutTargetInvalid
			}
			return fmt.Errorf("load administrator session sign-out target: %w", err)
		}
		if status != UserStatusActive || deletionPending != 0 {
			return ErrAdministratorUserSignOutTargetInvalid
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT id, user_agent FROM sessions
			WHERE user_id = ? AND auth_version = ?
			  AND revoked_at IS NULL AND idle_expires_at > ? AND absolute_expires_at > ?`,
			targetUserID, authVersion, now, now)
		if err != nil {
			return fmt.Errorf("list administrator session sign-out candidates: %w", err)
		}
		targetSessionID := ""
		for rows.Next() {
			var id, userAgent string
			if err := rows.Scan(&id, &userAgent); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan administrator session sign-out candidate: %w", err)
			}
			reference, err := m.administratorSessionActionReference(actorSessionID, targetUserID, id)
			if err != nil {
				_ = rows.Close()
				return err
			}
			if hmac.Equal([]byte(reference), []byte(options.Reference)) {
				targetSessionID, result.UserAgent = id, userAgent
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate administrator session sign-out candidates: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close administrator session sign-out candidates: %w", err)
		}
		if targetSessionID == "" {
			return ErrAdministratorUserSessionGone
		}
		revoked, err := tx.ExecContext(ctx, `
			UPDATE sessions SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
			WHERE id = ? AND revoked_at IS NULL`,
			now, actorUserID, SessionRevocationAdminAction, targetSessionID)
		if err != nil {
			return fmt.Errorf("revoke administrator chosen session: %w", err)
		}
		if changed, err := revoked.RowsAffected(); err != nil {
			return fmt.Errorf("count administrator chosen session: %w", err)
		} else if changed != 1 {
			return ErrAdministratorUserSessionGone
		}
		metadata, err := json.Marshal(struct {
			Scope           string `json:"scope"`
			RevokedSessions int64  `json:"revoked_sessions"`
		}{Scope: "single", RevokedSessions: 1})
		if err != nil {
			return fmt.Errorf("encode administrator session sign-out event: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id, event_type,
				success, reason, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			eventID, now, actorUserID, targetUserID, actorSessionID,
			AuthEventSessionRevoked, AuthEventReasonAdministratorAction, string(metadata),
		); err != nil {
			return fmt.Errorf("record administrator session sign-out: %w", err)
		}
		return m.auditAdministratorChange(ctx, tx, actorUserID, actorSessionID, audit.Event{
			Action:        "user.session_signed_out",
			Target:        audit.Target{Kind: auditTargetUser, ID: targetUserID},
			Metadata:      map[string]any{"user_agent": boundedUserAgent(result.UserAgent)},
			CorrelationID: eventID,
		})
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
