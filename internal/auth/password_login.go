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
	Identifier string
	Password   string
	Source     string
	UserAgent  string
}

type PasswordLoginResult struct {
	Session          *Session
	PreAuthChallenge *PreAuthChallenge
}

type passwordLoginCandidate struct {
	userID       string
	status       UserStatus
	mfaRequired  bool
	isAdmin      bool
	mustChange   bool
	passwordHash string
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

	requiresMFA := candidate.mfaRequired || candidate.isAdmin
	result, err := m.completePasswordLogin(
		ctx, candidate, normalizeLoginIdentifier(options.Identifier), replacementHash,
		boundedUserAgent(options.UserAgent), requiresMFA,
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
		SELECT u.id, u.status, u.mfa_required, u.is_admin,
		       p.must_change, p.password_hash
		FROM users u
		JOIN password_credentials p ON p.user_id = u.id
		WHERE u.email_normalized = ? OR u.username_normalized = ?
		ORDER BY u.id
		LIMIT 2`, normalized, normalized)
	if err != nil {
		return nil, fmt.Errorf("lookup password login candidate: %w", err)
	}
	defer rows.Close()

	var candidates []passwordLoginCandidate
	for rows.Next() {
		var candidate passwordLoginCandidate
		var mfaRequired, isAdmin, mustChange int
		if err := rows.Scan(
			&candidate.userID, &candidate.status, &mfaRequired, &isAdmin,
			&mustChange, &candidate.passwordHash,
		); err != nil {
			return nil, fmt.Errorf("scan password login candidate: %w", err)
		}
		candidate.mfaRequired = mfaRequired == 1
		candidate.isAdmin = isAdmin == 1
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

func (m *Manager) completePasswordLogin(ctx context.Context, candidate *passwordLoginCandidate, normalizedIdentifier, replacementHash, userAgent string, requiresMFA bool) (*PasswordLoginResult, error) {
	now := m.clock.Now().UTC()
	identifierBucketHash, err := m.loginThrottleBucketHash(loginThrottleBucketIdentifier, normalizedIdentifier)
	if err != nil {
		return nil, err
	}

	var session *Session
	var challenge *PreAuthChallenge
	if requiresMFA {
		origin, err := canonicalAuthOrigin(m.config.BaseURL)
		if err != nil {
			return nil, err
		}
		id, err := m.tokens.ID()
		if err != nil {
			return nil, fmt.Errorf("generate password MFA challenge ID: %w", err)
		}
		token, err := m.tokens.Token(32)
		if err != nil {
			return nil, fmt.Errorf("generate password MFA challenge token: %w", err)
		}
		challenge = &PreAuthChallenge{
			ID:          id,
			Token:       token,
			UserID:      candidate.userID,
			Purpose:     ChallengePurposeMFA,
			Origin:      origin,
			MaxAttempts: defaultPreAuthMaxAttempts,
			CreatedAt:   now,
			ExpiresAt:   now.Add(defaultPreAuthLifetime),
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
			CreatedAt:            now,
		}
		if session.IdleExpiresAt.After(session.AbsoluteExpiresAt) {
			session.IdleExpiresAt = session.AbsoluteExpiresAt
		}
	}

	err = m.runSecurityTransition(ctx, SecurityTransitionLoginCompletion, func(tx *sql.Tx) error {
		var passwordHash, emailNormalized, usernameNormalized string
		var status UserStatus
		var authVersion int64
		var mfaRequired, isAdmin, mustChange int
		err := tx.QueryRowContext(ctx, `
			SELECT p.password_hash, u.status, u.auth_version, u.mfa_required,
			       u.is_admin, p.must_change, COALESCE(u.email_normalized, ''),
			       COALESCE(u.username_normalized, '')
			FROM users u
			JOIN password_credentials p ON p.user_id = u.id
			WHERE u.id = ?`, candidate.userID,
		).Scan(
			&passwordHash, &status, &authVersion, &mfaRequired,
			&isAdmin, &mustChange, &emailNormalized, &usernameNormalized,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return errPasswordStateMoved
		}
		if err != nil {
			return fmt.Errorf("recheck password login state: %w", err)
		}
		currentRequiresMFA := mfaRequired == 1 || isAdmin == 1
		if passwordHash != candidate.passwordHash || !status.AllowsAuthentication() || mustChange == 1 || currentRequiresMFA != requiresMFA {
			return errPasswordStateMoved
		}
		if normalizedIdentifier != emailNormalized && normalizedIdentifier != usernameNormalized {
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
			_, err := tx.ExecContext(ctx, `
				INSERT INTO auth_challenges (
					id, user_id, session_id, challenge_hash, nonce_hash, purpose,
					origin, attempts, max_attempts, created_at, expires_at
				) VALUES (?, ?, NULL, ?, NULL, ?, ?, 0, ?, ?, ?)`,
				challenge.ID, challenge.UserID, hashToken(challenge.Token), challenge.Purpose,
				challenge.Origin, challenge.MaxAttempts, challenge.CreatedAt, challenge.ExpiresAt,
			)
			if err != nil {
				return fmt.Errorf("insert password MFA challenge: %w", err)
			}
			return nil
		}

		session.AuthVersion = authVersion
		_, err = tx.ExecContext(ctx, `
			INSERT INTO sessions (
				id, user_id, token_hash, auth_version, authentication_method,
				assurance_level, user_agent, authenticated_at, last_used_at,
				idle_expires_at, absolute_expires_at, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			session.ID, session.UserID, hashToken(session.Token), session.AuthVersion,
			session.AuthenticationMethod, session.AssuranceLevel, session.UserAgent,
			session.AuthenticatedAt, session.LastUsedAt, session.IdleExpiresAt,
			session.AbsoluteExpiresAt, session.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("insert password session: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &PasswordLoginResult{Session: session, PreAuthChallenge: challenge}, nil
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
