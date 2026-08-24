package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrLoginThrottled     = errors.New("login throttled")
	errPasswordStateMoved = errors.New("password login state changed")
)

type LoginThrottleError struct {
	RetryAt    time.Time
	RetryAfter time.Duration
}

func (err *LoginThrottleError) Error() string { return ErrLoginThrottled.Error() }
func (err *LoginThrottleError) Unwrap() error { return ErrLoginThrottled }

type PasswordLoginOptions struct {
	Identifier       string
	Password         string
	RequiredUserType UserType
	Source           string
	UserAgent        string
}

type PrimaryAuthenticationResult struct {
	Session               *Session
	PreAuthChallenge      *PreAuthChallenge
	MFAEnrollmentRequired bool
}

type PasswordLoginResult = PrimaryAuthenticationResult

type passwordLoginCandidate struct {
	userID       string
	status       UserStatus
	authVersion  int64
	mustChange   bool
	passwordHash string
	userType     UserType
}

// AuthenticatePassword completes a local password primary-factor attempt. A
// successful non-MFA login creates its session in the same transaction as a
// stale-hash upgrade, last-login update, and identifier-throttle reset. When
// MFA is required, that transaction creates only a pre-authentication flow.
func (m *Manager) AuthenticatePassword(ctx context.Context, options PasswordLoginOptions) (*PasswordLoginResult, error) {
	decision, err := m.CheckLoginThrottle(ctx, options.Identifier, options.Source)
	if err != nil {
		return nil, fmt.Errorf("check password login throttle: %w", err)
	}
	if decision.Throttled {
		return nil, m.loginThrottleError(decision)
	}

	candidate, err := m.findPasswordLoginCandidate(ctx, options.Identifier)
	if err != nil {
		return nil, err
	}
	passwordHash := ""
	if candidate != nil && options.RequiredUserType != "" && candidate.userType != options.RequiredUserType {
		candidate = nil
	}
	if candidate != nil {
		passwordHash = candidate.passwordHash
	}
	matches, replacementHash, verifyErr := VerifyAndRehashPassword(passwordHash, options.Password)
	if verifyErr != nil {
		// Corrupt stored parameters must not turn the matching identifier into a
		// cheap timing oracle. Execute the current dummy profile before returning
		// the same invalid-credential result used for every ordinary failure.
		if passwordHash != "" {
			_, _, _ = VerifyAndRehashPassword("", options.Password)
		}
		return nil, m.rejectPasswordLogin(ctx, options.Identifier, options.Source)
	}
	if !matches || candidate == nil || !candidate.status.AllowsAuthentication() || candidate.mustChange {
		return nil, m.rejectPasswordLogin(ctx, options.Identifier, options.Source)
	}

	policy, err := m.loadAuthenticationPolicy(ctx, m.db.Read(), candidate.userID, candidate.authVersion)
	if err != nil {
		if errors.Is(err, ErrUserNotActive) {
			return nil, m.rejectPasswordLogin(ctx, options.Identifier, options.Source)
		}
		return nil, err
	}
	result, err := m.completePasswordLogin(
		ctx, candidate, normalizeLoginIdentifier(options.Identifier), replacementHash,
		boundedUserAgent(options.UserAgent), policy,
	)
	if errors.Is(err, errPasswordStateMoved) {
		return nil, m.rejectPasswordLogin(ctx, options.Identifier, options.Source)
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (m *Manager) findPasswordLoginCandidate(ctx context.Context, identifier string) (*passwordLoginCandidate, error) {
	normalized := normalizeLoginIdentifier(identifier)
	rows, err := m.db.Read().QueryContext(ctx, `
		SELECT u.id, u.status, u.auth_version, p.must_change, p.password_hash, u.user_type
		FROM users u
		JOIN password_credentials p ON p.user_id = u.id
		WHERE u.username_normalized = ?
		ORDER BY u.id
		LIMIT 2`, normalized)
	if err != nil {
		return nil, fmt.Errorf("lookup password login candidate: %w", err)
	}
	defer rows.Close()

	var candidates []passwordLoginCandidate
	for rows.Next() {
		var candidate passwordLoginCandidate
		var mustChange int
		if err := rows.Scan(
			&candidate.userID, &candidate.status, &candidate.authVersion,
			&mustChange, &candidate.passwordHash, &candidate.userType,
		); err != nil {
			return nil, fmt.Errorf("scan password login candidate: %w", err)
		}
		candidate.mustChange = mustChange == 1
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate password login candidates: %w", err)
	}
	if len(candidates) != 1 {
		return nil, nil
	}
	return &candidates[0], nil
}

func (m *Manager) completePasswordLogin(ctx context.Context, candidate *passwordLoginCandidate, normalizedIdentifier, replacementHash, userAgent string, policy authenticationPolicy) (*PasswordLoginResult, error) {
	now := m.clock.Now().UTC()
	identifierBucketHash, err := m.loginThrottleBucketHash(loginThrottleBucketIdentifier, normalizedIdentifier)
	if err != nil {
		return nil, err
	}

	var session *Session
	var challenge *PreAuthChallenge
	var continuation *mfaContinuationDraft
	if policy.RequiresMFA {
		challenge, continuation, err = m.preparePrimaryMFAContinuation(
			ctx, m.db.Read(),
			candidate.userID, candidate.authVersion, AuthenticationMethodPassword,
			policy, m.config.BaseURL, now,
		)
		if err != nil {
			return nil, err
		}
	} else {
		id, err := m.tokens.ID()
		if err != nil {
			return nil, fmt.Errorf("generate password session ID: %w", err)
		}
		token, err := m.tokens.Token(32)
		if err != nil {
			return nil, fmt.Errorf("generate password session token: %w", err)
		}
		session = &Session{
			ID:                   id,
			UserID:               candidate.userID,
			Token:                token,
			AuthenticationMethod: AuthenticationMethodPassword,
			AssuranceLevel:       AssuranceLevelSingleFactor,
			UserAgent:            userAgent,
			AuthenticatedAt:      now,
			LastUsedAt:           now,
			IdleExpiresAt:        now.Add(sessionIdleLifetime),
			AbsoluteExpiresAt:    now.Add(sessionAbsoluteLifetime),
			StepUpAt:             &now,
			StepUpMethod:         AuthenticationMethodPassword,
			CreatedAt:            now,
		}
		if session.IdleExpiresAt.After(session.AbsoluteExpiresAt) {
			session.IdleExpiresAt = session.AbsoluteExpiresAt
		}
	}

	err = m.runSecurityTransition(ctx, SecurityTransitionLoginCompletion, func(tx *sql.Tx) error {
		var passwordHash, usernameNormalized string
		var status UserStatus
		var userType UserType
		var authVersion int64
		var mustChange int
		err := tx.QueryRowContext(ctx, `
			SELECT p.password_hash, u.status, u.auth_version,
			       p.must_change, u.username_normalized, u.user_type
			FROM users u
			JOIN password_credentials p ON p.user_id = u.id
			WHERE u.id = ?`, candidate.userID,
		).Scan(
			&passwordHash, &status, &authVersion,
			&mustChange, &usernameNormalized, &userType,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return errPasswordStateMoved
		}
		if err != nil {
			return fmt.Errorf("recheck password login state: %w", err)
		}
		currentPolicy, err := queryAuthenticationPolicy(ctx, tx, candidate.userID, authVersion)
		if err != nil {
			if errors.Is(err, ErrUserNotActive) {
				return errPasswordStateMoved
			}
			return err
		}
		if passwordHash != candidate.passwordHash || !status.AllowsAuthentication() || mustChange == 1 || userType != candidate.userType ||
			currentPolicy != policy {
			return errPasswordStateMoved
		}
		if normalizedIdentifier != usernameNormalized {
			return errPasswordStateMoved
		}

		if replacementHash != "" {
			changed, err := tx.ExecContext(ctx, `
				UPDATE password_credentials
				SET password_hash = ?, changed_at = ?
				WHERE user_id = ? AND password_hash = ?`,
				replacementHash, now, candidate.userID, candidate.passwordHash,
			)
			if err != nil {
				return fmt.Errorf("upgrade password hash: %w", err)
			}
			rowsAffected, err := changed.RowsAffected()
			if err != nil {
				return fmt.Errorf("count upgraded password hashes: %w", err)
			}
			if rowsAffected != 1 {
				return errPasswordStateMoved
			}
		}

		if challenge == nil {
			if _, err := tx.ExecContext(ctx, `
				UPDATE users SET last_login_at = ?, updated_at = ?
				WHERE id = ? AND status = 'active'`, now, now, candidate.userID); err != nil {
				return fmt.Errorf("update password login timestamp: %w", err)
			}
		}

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM auth_throttle WHERE bucket_hash = ? AND action = ?`,
			identifierBucketHash, loginThrottleAction,
		); err != nil {
			return fmt.Errorf("clear password login throttle: %w", err)
		}

		if challenge != nil {
			if err := m.requireMFAContinuationAuthenticatorState(ctx, tx, challenge.UserID, continuation); err != nil {
				return errPasswordStateMoved
			}
			return m.insertMFAContinuation(ctx, tx, challenge, now)
		}
		if !currentPolicy.allowsAssurance(session.AssuranceLevel) {
			return ErrAuthenticationPolicyNotSatisfied
		}

		session.AuthVersion = authVersion
		_, err = tx.ExecContext(ctx, `
			INSERT INTO sessions (
				id, user_id, token_hash, auth_version, authentication_method,
				assurance_level, user_agent, authenticated_at, last_used_at,
				idle_expires_at, absolute_expires_at, step_up_at, step_up_method, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			session.ID, session.UserID, hashToken(session.Token), session.AuthVersion,
			session.AuthenticationMethod, session.AssuranceLevel, session.UserAgent,
			session.AuthenticatedAt, session.LastUsedAt, session.IdleExpiresAt,
			session.AbsoluteExpiresAt, session.StepUpAt, session.StepUpMethod, session.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("insert password session: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &PasswordLoginResult{
		Session: session, PreAuthChallenge: challenge,
		MFAEnrollmentRequired: continuation != nil && continuation.Enrollment != nil,
	}, nil
}

func (m *Manager) rejectPasswordLogin(ctx context.Context, identifier, source string) error {
	decision, err := m.RecordLoginFailure(ctx, identifier, source)
	if err != nil {
		return fmt.Errorf("record rejected password login: %w", err)
	}
	if decision.Throttled {
		return m.loginThrottleError(decision)
	}
	return ErrInvalidCredentials
}

func (m *Manager) loginThrottleError(decision LoginThrottleDecision) error {
	now := m.clock.Now().UTC()
	retryAfter := decision.RetryAt.Sub(now)
	if retryAfter < 0 {
		retryAfter = 0
	}
	return &LoginThrottleError{RetryAt: decision.RetryAt, RetryAfter: retryAfter}
}

func boundedUserAgent(value string) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	for len(value) > 1024 {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}
