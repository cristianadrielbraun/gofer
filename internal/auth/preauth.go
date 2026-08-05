package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrPreAuthChallengeInvalid = errors.New("pre-authentication challenge is invalid")

const (
	preAuthCookieName         = "gofer_pre_auth"
	defaultPreAuthLifetime    = 10 * time.Minute
	maximumPreAuthLifetime    = 15 * time.Minute
	defaultPreAuthMaxAttempts = 3
	maximumPreAuthAttempts    = 10
	preAuthConsumedRetention  = 24 * time.Hour
	preAuthCleanupBatchSize   = 500
)

const preAuthChallengeSelect = `SELECT id, COALESCE(user_id, ''), COALESCE(session_id, ''),
	purpose, origin, attempts, max_attempts, payload_ciphertext, created_at, expires_at, consumed_at
	FROM auth_challenges`

func scanPreAuthChallenge(row rowScanner) (*PreAuthChallenge, error) {
	challenge := &PreAuthChallenge{}
	var consumedAt sql.NullTime
	if err := row.Scan(
		&challenge.ID, &challenge.UserID, &challenge.SessionID, &challenge.Purpose,
		&challenge.Origin, &challenge.Attempts, &challenge.MaxAttempts,
		&challenge.PayloadCiphertext, &challenge.CreatedAt, &challenge.ExpiresAt, &consumedAt,
	); err != nil {
		return nil, err
	}
	if consumedAt.Valid {
		challenge.ConsumedAt = &consumedAt.Time
	}
	return challenge, nil
}

func (m *Manager) CreatePreAuthChallenge(ctx context.Context, options PreAuthChallengeOptions) (*PreAuthChallenge, error) {
	if !options.Purpose.Valid() {
		return nil, fmt.Errorf("invalid authentication challenge purpose %q", options.Purpose)
	}
	origin, err := canonicalAuthOrigin(options.Origin)
	if err != nil {
		return nil, err
	}
	lifetime := options.Lifetime
	if lifetime == 0 {
		lifetime = defaultPreAuthLifetime
	}
	if lifetime < time.Second || lifetime > maximumPreAuthLifetime {
		return nil, fmt.Errorf("pre-authentication challenge lifetime must be between one second and %s", maximumPreAuthLifetime)
	}
	maxAttempts := options.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = defaultPreAuthMaxAttempts
	}
	if maxAttempts < 1 || maxAttempts > maximumPreAuthAttempts {
		return nil, fmt.Errorf("pre-authentication challenge attempts must be between 1 and %d", maximumPreAuthAttempts)
	}
	now := m.clock.Now().UTC()
	userID := strings.TrimSpace(options.UserID)
	sessionID := strings.TrimSpace(options.SessionID)
	id, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate pre-authentication challenge ID: %w", err)
	}
	token, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate pre-authentication token: %w", err)
	}
	nonce := ""
	if options.IssueNonce {
		nonce, err = m.tokens.Token(32)
		if err != nil {
			return nil, fmt.Errorf("generate pre-authentication nonce: %w", err)
		}
	}
	challenge := &PreAuthChallenge{
		ID:          id,
		Token:       token,
		Nonce:       nonce,
		UserID:      userID,
		SessionID:   sessionID,
		Purpose:     options.Purpose,
		Origin:      origin,
		MaxAttempts: maxAttempts,
		CreatedAt:   now,
		ExpiresAt:   now.Add(lifetime),
	}
	if sessionID != "" {
		err := m.db.Write().QueryRowContext(ctx, `
			INSERT INTO auth_challenges (
				id, user_id, session_id, challenge_hash, nonce_hash, purpose, origin,
				attempts, max_attempts, created_at, expires_at
			)
			SELECT ?, sessions.user_id, sessions.id, ?, ?, ?, ?, 0, ?, ?, ?
			FROM sessions
			JOIN users ON users.id = sessions.user_id
			WHERE sessions.id = ? AND sessions.revoked_at IS NULL
			  AND sessions.idle_expires_at > ? AND sessions.absolute_expires_at > ?
			  AND users.status = 'active' AND users.auth_version = sessions.auth_version
			  AND (? = '' OR sessions.user_id = ?)
			RETURNING user_id`,
			challenge.ID, hashToken(token), nullableTokenHash(nonce), challenge.Purpose, challenge.Origin,
			challenge.MaxAttempts, challenge.CreatedAt, challenge.ExpiresAt,
			sessionID, now, now, userID, userID,
		).Scan(&challenge.UserID)
		if err == sql.ErrNoRows {
			return nil, ErrSessionNotActive
		}
		if err != nil {
			return nil, fmt.Errorf("insert session-bound pre-authentication challenge: %w", err)
		}
	} else if _, err := m.db.Write().ExecContext(ctx, `
			INSERT INTO auth_challenges (
				id, user_id, session_id, challenge_hash, nonce_hash, purpose, origin,
				attempts, max_attempts, created_at, expires_at
			) VALUES (?, ?, NULL, ?, ?, ?, ?, 0, ?, ?, ?)`,
		challenge.ID, nullableIdentifier(challenge.UserID), hashToken(token), nullableTokenHash(nonce),
		challenge.Purpose, challenge.Origin, challenge.MaxAttempts, challenge.CreatedAt, challenge.ExpiresAt,
	); err != nil {
		return nil, fmt.Errorf("insert pre-authentication challenge: %w", err)
	}
	return challenge, nil
}

func (m *Manager) ConsumePreAuthChallenge(ctx context.Context, token, nonce string, purpose ChallengePurpose, origin string) (*PreAuthChallenge, error) {
	if strings.TrimSpace(token) == "" || !purpose.Valid() {
		return nil, ErrPreAuthChallengeInvalid
	}
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil {
		return nil, ErrPreAuthChallengeInvalid
	}
	now := m.clock.Now().UTC()
	challenge, err := scanPreAuthChallenge(m.db.Write().QueryRowContext(ctx, `
		UPDATE auth_challenges
		SET attempts = attempts + 1, consumed_at = ?
		WHERE challenge_hash = ? AND purpose = ? AND origin = ?
		  AND ((nonce_hash IS NULL AND ? = '') OR nonce_hash = ?)
		  AND consumed_at IS NULL AND expires_at > ? AND attempts < max_attempts
		RETURNING id, COALESCE(user_id, ''), COALESCE(session_id, ''), purpose, origin,
		          attempts, max_attempts, payload_ciphertext, created_at, expires_at, consumed_at`,
		now, hashToken(token), purpose, canonicalOrigin, nonce, hashToken(nonce), now,
	))
	if err == sql.ErrNoRows {
		_, _ = m.recordPreAuthChallengeFailure(ctx, token, purpose, canonicalOrigin, now)
		_, _ = m.db.Write().ExecContext(ctx, `
			UPDATE auth_challenges SET consumed_at = COALESCE(consumed_at, ?)
			WHERE challenge_hash = ? AND purpose = ? AND origin = ? AND expires_at <= ?`,
			now, hashToken(token), purpose, canonicalOrigin, now)
		return nil, ErrPreAuthChallengeInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("consume pre-authentication challenge: %w", err)
	}
	return challenge, nil
}

func (m *Manager) RecordPreAuthChallengeFailure(ctx context.Context, token string, purpose ChallengePurpose, origin string) (bool, error) {
	if strings.TrimSpace(token) == "" || !purpose.Valid() {
		return false, ErrPreAuthChallengeInvalid
	}
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil {
		return false, ErrPreAuthChallengeInvalid
	}
	now := m.clock.Now().UTC()
	return m.recordPreAuthChallengeFailure(ctx, token, purpose, canonicalOrigin, now)
}

func (m *Manager) recordPreAuthChallengeFailure(ctx context.Context, token string, purpose ChallengePurpose, canonicalOrigin string, now time.Time) (bool, error) {
	var consumedAt sql.NullTime
	err := m.db.Write().QueryRowContext(ctx, `
		UPDATE auth_challenges
		SET attempts = attempts + 1,
		    consumed_at = CASE WHEN attempts + 1 >= max_attempts THEN ? ELSE NULL END
		WHERE challenge_hash = ? AND purpose = ? AND origin = ?
		  AND consumed_at IS NULL AND expires_at > ? AND attempts < max_attempts
		RETURNING consumed_at`,
		now, hashToken(token), purpose, canonicalOrigin, now,
	).Scan(&consumedAt)
	if err == sql.ErrNoRows {
		return false, ErrPreAuthChallengeInvalid
	}
	if err != nil {
		return false, fmt.Errorf("record pre-authentication challenge failure: %w", err)
	}
	return consumedAt.Valid, nil
}

func (m *Manager) TerminatePreAuthChallenge(ctx context.Context, token string, purpose ChallengePurpose, origin string) error {
	if strings.TrimSpace(token) == "" || !purpose.Valid() {
		return ErrPreAuthChallengeInvalid
	}
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil {
		return ErrPreAuthChallengeInvalid
	}
	now := m.clock.Now().UTC()
	result, err := m.db.Write().ExecContext(ctx, `
		UPDATE auth_challenges
		SET attempts = CASE WHEN attempts < max_attempts THEN attempts + 1 ELSE attempts END,
		    consumed_at = COALESCE(consumed_at, ?)
		WHERE challenge_hash = ? AND purpose = ? AND origin = ?`,
		now, hashToken(token), purpose, canonicalOrigin)
	if err != nil {
		return fmt.Errorf("terminate pre-authentication challenge: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrPreAuthChallengeInvalid
	}
	return nil
}

func (m *Manager) CleanupPreAuthChallenges(ctx context.Context) error {
	now := m.clock.Now().UTC()
	consumedBefore := now.Add(-preAuthConsumedRetention)
	_, err := m.db.Write().ExecContext(ctx, `
		DELETE FROM auth_challenges
		WHERE id IN (
			SELECT id FROM auth_challenges
			WHERE expires_at <= ? OR (consumed_at IS NOT NULL AND consumed_at <= ?)
			ORDER BY COALESCE(consumed_at, expires_at), id
			LIMIT ?
		)`, now, consumedBefore, preAuthCleanupBatchSize)
	return err
}

func SetPreAuthCookie(w http.ResponseWriter, token string, secure bool, lifetime time.Duration) {
	maxAge := int(lifetime / time.Second)
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     preAuthCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func ClearPreAuthCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     preAuthCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func GetPreAuthToken(r *http.Request) string {
	cookie, err := r.Cookie(preAuthCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func PreAuthTokensMatch(left, right string) bool {
	if left == "" || right == "" || len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func canonicalAuthOrigin(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("invalid canonical authentication origin")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("invalid canonical authentication origin")
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("canonical authentication origin must not contain a path, query, or fragment")
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "" {
		return "", fmt.Errorf("invalid canonical authentication origin")
	}
	port := parsed.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	}
	origin := scheme + "://" + host
	if len(origin) > 2048 {
		return "", fmt.Errorf("canonical authentication origin is too long")
	}
	return origin, nil
}

func nullableIdentifier(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableTokenHash(token string) any {
	if token == "" {
		return nil
	}
	return hashToken(token)
}
