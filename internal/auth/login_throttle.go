package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	loginThrottleAction           = "local_password_login"
	loginThrottleCleanupBatchSize = 500
	minimumBucketHashKeyBytes     = 32
)

type loginThrottleBucketKind string

const (
	loginThrottleBucketIdentifier loginThrottleBucketKind = "identifier"
	loginThrottleBucketSource     loginThrottleBucketKind = "source"
	loginThrottleBucketInstance   loginThrottleBucketKind = "instance"
)

type loginThrottlePolicy struct {
	delayStartsAt int64
	baseDelay     time.Duration
	maximumDelay  time.Duration
	retention     time.Duration
}

var loginThrottlePolicies = map[loginThrottleBucketKind]loginThrottlePolicy{
	loginThrottleBucketIdentifier: {
		delayStartsAt: 5,
		baseDelay:     time.Second,
		maximumDelay:  15 * time.Minute,
		retention:     24 * time.Hour,
	},
	loginThrottleBucketSource: {
		delayStartsAt: 20,
		baseDelay:     time.Second,
		maximumDelay:  5 * time.Minute,
		retention:     time.Hour,
	},
	loginThrottleBucketInstance: {
		delayStartsAt: 100,
		baseDelay:     time.Second,
		maximumDelay:  time.Minute,
		retention:     10 * time.Minute,
	},
}

// LoginThrottleDecision intentionally exposes no bucket identity or failure
// count so callers can give the same response for every submitted identifier.
type LoginThrottleDecision struct {
	Throttled bool
	RetryAt   time.Time
}

type loginThrottleBucket struct {
	hash   string
	policy loginThrottlePolicy
}

// CheckLoginThrottle reports whether a local-password attempt may proceed.
// The source must be derived from a trusted transport peer by the caller; this
// layer deliberately does not interpret proxy forwarding headers.
func (m *Manager) CheckLoginThrottle(ctx context.Context, identifier, source string) (LoginThrottleDecision, error) {
	buckets, err := m.loginThrottleBuckets(identifier, source)
	if err != nil {
		return LoginThrottleDecision{}, err
	}
	now := m.clock.Now().UTC()
	tx, err := m.db.Read().BeginTx(ctx, nil)
	if err != nil {
		return LoginThrottleDecision{}, fmt.Errorf("begin login throttle check: %w", err)
	}
	defer tx.Rollback()

	retryAt := time.Time{}
	for _, bucket := range buckets {
		var blockedUntil sql.NullTime
		var expiresAt time.Time
		err := tx.QueryRowContext(ctx, `
			SELECT blocked_until, expires_at
			FROM auth_throttle
			WHERE bucket_hash = ? AND action = ?`, bucket.hash, loginThrottleAction,
		).Scan(&blockedUntil, &expiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return LoginThrottleDecision{}, fmt.Errorf("read login throttle bucket: %w", err)
		}
		if !expiresAt.After(now) || !blockedUntil.Valid || !blockedUntil.Time.After(now) {
			continue
		}
		if blockedUntil.Time.After(retryAt) {
			retryAt = blockedUntil.Time
		}
	}
	if err := tx.Commit(); err != nil {
		return LoginThrottleDecision{}, fmt.Errorf("commit login throttle check: %w", err)
	}
	return newLoginThrottleDecision(now, retryAt), nil
}

// RecordLoginFailure updates the identifier, source, and instance buckets in
// one transaction and returns the next generic retry decision.
func (m *Manager) RecordLoginFailure(ctx context.Context, identifier, source string) (LoginThrottleDecision, error) {
	buckets, err := m.loginThrottleBuckets(identifier, source)
	if err != nil {
		return LoginThrottleDecision{}, err
	}
	now := m.clock.Now().UTC()
	retryAt := time.Time{}
	err = m.runSecurityTransition(ctx, SecurityTransitionLoginThrottle, func(tx *sql.Tx) error {
		for _, bucket := range buckets {
			blockedUntil, err := recordLoginThrottleFailure(ctx, tx, bucket, now)
			if err != nil {
				return err
			}
			if blockedUntil.After(retryAt) {
				retryAt = blockedUntil
			}
		}
		return nil
	})
	if err != nil {
		return LoginThrottleDecision{}, err
	}
	return newLoginThrottleDecision(now, retryAt), nil
}

// RecordLoginSuccess clears only the identifier bucket. Shared source and
// instance abuse history cannot be erased by presenting one valid credential.
func (m *Manager) RecordLoginSuccess(ctx context.Context, identifier string) error {
	bucketHash, err := m.loginThrottleBucketHash(loginThrottleBucketIdentifier, normalizeLoginIdentifier(identifier))
	if err != nil {
		return err
	}
	if _, err := m.db.Write().ExecContext(ctx,
		`DELETE FROM auth_throttle WHERE bucket_hash = ? AND action = ?`,
		bucketHash, loginThrottleAction,
	); err != nil {
		return fmt.Errorf("clear successful login throttle bucket: %w", err)
	}
	return nil
}

// CleanupExpiredLoginThrottle removes a bounded batch of expired buckets.
func (m *Manager) CleanupExpiredLoginThrottle(ctx context.Context) error {
	now := m.clock.Now().UTC()
	_, err := m.db.Write().ExecContext(ctx, `
		DELETE FROM auth_throttle
		WHERE bucket_hash IN (
			SELECT bucket_hash FROM auth_throttle
			WHERE expires_at <= ?
			ORDER BY expires_at, bucket_hash
			LIMIT ?
		)`, now, loginThrottleCleanupBatchSize)
	if err != nil {
		return fmt.Errorf("clean expired login throttle buckets: %w", err)
	}
	return nil
}

func (m *Manager) loginThrottleBuckets(identifier, source string) ([]loginThrottleBucket, error) {
	values := []struct {
		kind  loginThrottleBucketKind
		value string
	}{
		{kind: loginThrottleBucketIdentifier, value: normalizeLoginIdentifier(identifier)},
		{kind: loginThrottleBucketSource, value: strings.TrimSpace(source)},
		{kind: loginThrottleBucketInstance, value: "gofer"},
	}
	buckets := make([]loginThrottleBucket, 0, len(values))
	for _, value := range values {
		hash, err := m.loginThrottleBucketHash(value.kind, value.value)
		if err != nil {
			return nil, err
		}
		buckets = append(buckets, loginThrottleBucket{
			hash:   hash,
			policy: loginThrottlePolicies[value.kind],
		})
	}
	return buckets, nil
}

func (m *Manager) loginThrottleBucketHash(kind loginThrottleBucketKind, value string) (string, error) {
	if len(m.bucketHashKey) < minimumBucketHashKeyBytes {
		return "", errors.New("login throttle bucket hash key must contain at least 32 bytes")
	}
	mac := hmac.New(sha256.New, m.bucketHashKey)
	for _, part := range []string{"gofer-auth-throttle-v1", loginThrottleAction, string(kind), value} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = mac.Write(length[:])
		_, _ = mac.Write([]byte(part))
	}
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func recordLoginThrottleFailure(ctx context.Context, tx *sql.Tx, bucket loginThrottleBucket, now time.Time) (time.Time, error) {
	var failureCount int64
	var firstAttemptAt, expiresAt time.Time
	var blockedUntil sql.NullTime
	err := tx.QueryRowContext(ctx, `
		SELECT failure_count, first_attempt_at, blocked_until, expires_at
		FROM auth_throttle
		WHERE bucket_hash = ? AND action = ?`, bucket.hash, loginThrottleAction,
	).Scan(&failureCount, &firstAttemptAt, &blockedUntil, &expiresAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, fmt.Errorf("read login throttle failure bucket: %w", err)
	}
	if errors.Is(err, sql.ErrNoRows) || !expiresAt.After(now) {
		failureCount = 0
		firstAttemptAt = now
		blockedUntil = sql.NullTime{}
	}
	if failureCount < int64(^uint64(0)>>1) {
		failureCount++
	}

	nextBlockedUntil := time.Time{}
	if delay := loginThrottleDelay(bucket.policy, failureCount); delay > 0 {
		nextBlockedUntil = now.Add(delay)
	}
	if blockedUntil.Valid && blockedUntil.Time.After(nextBlockedUntil) {
		nextBlockedUntil = blockedUntil.Time
	}
	expiresAt = now.Add(bucket.policy.retention)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO auth_throttle (
			bucket_hash, action, failure_count, first_attempt_at,
			last_attempt_at, blocked_until, expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(bucket_hash) DO UPDATE SET
			action = excluded.action,
			failure_count = excluded.failure_count,
			first_attempt_at = excluded.first_attempt_at,
			last_attempt_at = excluded.last_attempt_at,
			blocked_until = excluded.blocked_until,
			expires_at = excluded.expires_at`,
		bucket.hash, loginThrottleAction, failureCount, firstAttemptAt,
		now, nullableThrottleTime(nextBlockedUntil), expiresAt,
	)
	if err != nil {
		return time.Time{}, fmt.Errorf("write login throttle failure bucket: %w", err)
	}
	return nextBlockedUntil, nil
}

func loginThrottleDelay(policy loginThrottlePolicy, failureCount int64) time.Duration {
	if failureCount < policy.delayStartsAt {
		return 0
	}
	delay := policy.baseDelay
	for steps := failureCount - policy.delayStartsAt; steps > 0 && delay < policy.maximumDelay; steps-- {
		if delay > policy.maximumDelay/2 {
			return policy.maximumDelay
		}
		delay *= 2
	}
	if delay > policy.maximumDelay {
		return policy.maximumDelay
	}
	return delay
}

func newLoginThrottleDecision(now, retryAt time.Time) LoginThrottleDecision {
	if retryAt.After(now) {
		return LoginThrottleDecision{Throttled: true, RetryAt: retryAt}
	}
	return LoginThrottleDecision{}
}

func nullableThrottleTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}
