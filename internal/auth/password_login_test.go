package auth

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

const passwordLoginTestPassword = "a safe local login passphrase"

func insertPasswordLoginUser(t *testing.T, manager *Manager, id, email, username string, status UserStatus, isAdmin, mfaRequired, mustChange bool, passwordHash string, now time.Time) {
	t.Helper()
	adminValue := 0
	if isAdmin {
		adminValue = 1
	}
	mfaValue := 0
	if mfaRequired {
		mfaValue = 1
	}
	mustChangeValue := 0
	if mustChange {
		mustChangeValue = 1
	}
	emailNormalized := normalizeLoginIdentifier(email)
	usernameNormalized := normalizeLoginIdentifier(username)
	var usernameValue, usernameNormalizedValue any
	if username != "" {
		usernameValue = username
		usernameNormalizedValue = usernameNormalized
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, email, email_normalized, username, username_normalized, name,
			status, auth_version, mfa_required, is_admin, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`,
		id, email, emailNormalized, usernameValue, usernameNormalizedValue, id,
		status, mfaValue, adminValue, now, now,
	); err != nil {
		t.Fatalf("insert password login user %q: %v", id, err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO password_credentials (
			user_id, password_hash, must_change, created_at, changed_at
		) VALUES (?, ?, ?, ?, ?)`, id, passwordHash, mustChangeValue, now, now,
	); err != nil {
		t.Fatalf("insert password credential %q: %v", id, err)
	}
}

func currentPasswordLoginHash(t *testing.T) string {
	t.Helper()
	hash, err := HashPassword(passwordLoginTestPassword)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	return hash
}

func TestAuthenticatePasswordCreatesSingleFactorSessionAtomically(t *testing.T) {
	now := time.Date(2026, time.August, 5, 17, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{
		ids:    []string{"password-session-id"},
		tokens: []string{"password-session-token"},
	})
	insertPasswordLoginUser(t, manager, "person", "Person@Example.com", "Person.Name", UserStatusActive, false, false, false, currentPasswordLoginHash(t), now.Add(-time.Hour))

	for attempt := 1; attempt < int(loginThrottlePolicies[loginThrottleBucketIdentifier].delayStartsAt); attempt++ {
		if _, err := manager.RecordLoginFailure(t.Context(), "person@example.com", fmt.Sprintf("source-%d", attempt)); err != nil {
			t.Fatalf("seed throttle failure %d: %v", attempt, err)
		}
	}
	result, err := manager.AuthenticatePassword(t.Context(), PasswordLoginOptions{
		Identifier: " PERSON@example.COM ",
		Password:   passwordLoginTestPassword,
		Source:     "success-source",
		UserAgent:  "  test browser  ",
	})
	if err != nil {
		t.Fatalf("AuthenticatePassword() error = %v", err)
	}
	if result == nil || result.Session == nil || result.PreAuthChallenge != nil {
		t.Fatalf("AuthenticatePassword() result = %#v", result)
	}
	session := result.Session
	if session.ID != "password-session-id" || session.Token != "password-session-token" || session.AuthenticationMethod != AuthenticationMethodPassword || session.AssuranceLevel != AssuranceLevelSingleFactor || session.UserAgent != "test browser" {
		t.Fatalf("password session = %#v", session)
	}
	stored, err := manager.GetSessionByToken(t.Context(), session.Token)
	if err != nil || stored == nil || stored.ID != session.ID {
		t.Fatalf("GetSessionByToken() = %#v, %v", stored, err)
	}
	var lastLoginAt time.Time
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT last_login_at FROM users WHERE id = 'person'`).Scan(&lastLoginAt); err != nil {
		t.Fatalf("query last_login_at: %v", err)
	}
	if !lastLoginAt.Equal(now) {
		t.Fatalf("last_login_at = %v, want %v", lastLoginAt, now)
	}
	identifierHash, err := manager.loginThrottleBucketHash(loginThrottleBucketIdentifier, "person@example.com")
	if err != nil {
		t.Fatal(err)
	}
	var identifierBuckets int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_throttle WHERE bucket_hash = ?`, identifierHash).Scan(&identifierBuckets); err != nil {
		t.Fatalf("count identifier throttle bucket: %v", err)
	}
	if identifierBuckets != 0 {
		t.Fatalf("identifier throttle buckets after success = %d, want 0", identifierBuckets)
	}
}

func TestAuthenticatePasswordReturnsGenericFailures(t *testing.T) {
	now := time.Date(2026, time.August, 5, 18, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		insertUser bool
		status     UserStatus
		mustChange bool
		password   string
	}{
		{name: "incorrect password", insertUser: true, status: UserStatusActive, password: "incorrect passphrase"},
		{name: "unknown identifier", password: "incorrect passphrase"},
		{name: "disabled user", insertUser: true, status: UserStatusDisabled, password: passwordLoginTestPassword},
		{name: "pending user", insertUser: true, status: UserStatusPending, password: passwordLoginTestPassword},
		{name: "must change", insertUser: true, status: UserStatusActive, mustChange: true, password: passwordLoginTestPassword},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := &fixedClock{now: now}
			manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{})
			if test.insertUser {
				insertPasswordLoginUser(t, manager, "person", "person@example.com", "person", test.status, false, false, test.mustChange, currentPasswordLoginHash(t), now)
			}
			result, err := manager.AuthenticatePassword(t.Context(), PasswordLoginOptions{
				Identifier: "person@example.com",
				Password:   test.password,
				Source:     "198.51.100.40",
			})
			if result != nil || !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("AuthenticatePassword() = %#v, %v, want ErrInvalidCredentials", result, err)
			}
			var sessionCount, throttleCount int
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&sessionCount); err != nil {
				t.Fatal(err)
			}
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_throttle`).Scan(&throttleCount); err != nil {
				t.Fatal(err)
			}
			if sessionCount != 0 || throttleCount != 3 {
				t.Fatalf("failure state = sessions:%d throttle buckets:%d, want 0 and 3", sessionCount, throttleCount)
			}
		})
	}
}

func TestAuthenticatePasswordDetectsAmbiguousCrossNamespaceIdentifier(t *testing.T) {
	now := time.Date(2026, time.August, 5, 18, 30, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{})
	hash := currentPasswordLoginHash(t)
	insertPasswordLoginUser(t, manager, "email-owner", "shared@example.com", "email-owner", UserStatusActive, false, false, false, hash, now)
	insertPasswordLoginUser(t, manager, "username-owner", "other@example.com", "shared@example.com", UserStatusActive, false, false, false, hash, now)

	result, err := manager.AuthenticatePassword(t.Context(), PasswordLoginOptions{
		Identifier: "shared@example.com",
		Password:   passwordLoginTestPassword,
		Source:     "198.51.100.41",
	})
	if result != nil || !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("AuthenticatePassword(ambiguous) = %#v, %v, want generic rejection", result, err)
	}
}

func TestAuthenticatePasswordReturnsPersistentThrottleDecision(t *testing.T) {
	now := time.Date(2026, time.August, 5, 19, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{})
	insertPasswordLoginUser(t, manager, "person", "person@example.com", "person", UserStatusActive, false, false, false, currentPasswordLoginHash(t), now)

	var err error
	for attempt := int64(1); attempt <= loginThrottlePolicies[loginThrottleBucketIdentifier].delayStartsAt; attempt++ {
		_, err = manager.AuthenticatePassword(t.Context(), PasswordLoginOptions{
			Identifier: "person@example.com",
			Password:   "incorrect passphrase",
			Source:     fmt.Sprintf("source-%d", attempt),
		})
	}
	var throttleError *LoginThrottleError
	if !errors.Is(err, ErrLoginThrottled) || !errors.As(err, &throttleError) {
		t.Fatalf("threshold error = %v, want LoginThrottleError", err)
	}
	if throttleError.RetryAfter != time.Second || !throttleError.RetryAt.Equal(now.Add(time.Second)) {
		t.Fatalf("throttle error = %#v", throttleError)
	}
	if _, err := manager.AuthenticatePassword(t.Context(), PasswordLoginOptions{
		Identifier: "person@example.com",
		Password:   passwordLoginTestPassword,
		Source:     "new-source",
	}); !errors.Is(err, ErrLoginThrottled) {
		t.Fatalf("correct password during backoff error = %v, want throttled", err)
	}
}

func TestAuthenticatePasswordCreatesMFAContinuationForPolicy(t *testing.T) {
	now := time.Date(2026, time.August, 5, 20, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		isAdmin     bool
		mfaRequired bool
	}{
		{name: "administrator", isAdmin: true},
		{name: "user policy", mfaRequired: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			clock := &fixedClock{now: now}
			manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{
				ids:    []string{"mfa-challenge-id"},
				tokens: []string{"mfa-challenge-token"},
			})
			insertPasswordLoginUser(t, manager, "person", "person@example.com", "person", UserStatusActive, test.isAdmin, test.mfaRequired, false, currentPasswordLoginHash(t), now)

			result, err := manager.AuthenticatePassword(t.Context(), PasswordLoginOptions{
				Identifier: "person",
				Password:   passwordLoginTestPassword,
				Source:     "198.51.100.42",
			})
			if err != nil || result == nil || result.Session != nil || result.PreAuthChallenge == nil {
				t.Fatalf("AuthenticatePassword(MFA) = %#v, %v", result, err)
			}
			challenge := result.PreAuthChallenge
			if challenge.Token != "mfa-challenge-token" || challenge.Purpose != ChallengePurposeMFA || challenge.UserID != "person" || challenge.Origin != "https://gofer.example" {
				t.Fatalf("MFA challenge = %#v", challenge)
			}
			var sessionCount int
			var lastLoginAt sql.NullTime
			var storedHash string
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&sessionCount); err != nil {
				t.Fatal(err)
			}
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT challenge_hash FROM auth_challenges WHERE id = ?`, challenge.ID).Scan(&storedHash); err != nil {
				t.Fatal(err)
			}
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT last_login_at FROM users WHERE id = 'person'`).Scan(&lastLoginAt); err != nil {
				t.Fatal(err)
			}
			if sessionCount != 0 || lastLoginAt.Valid || storedHash == challenge.Token || storedHash != hashToken(challenge.Token) {
				t.Fatalf("MFA persistence = sessions:%d lastLogin:%v hash:%q", sessionCount, lastLoginAt, storedHash)
			}
		})
	}
}

func TestAuthenticatePasswordUpgradesStaleHash(t *testing.T) {
	now := time.Date(2026, time.August, 5, 21, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{
		ids:    []string{"session-id"},
		tokens: []string{"session-token"},
	})
	staleParameters := currentPasswordHashParameters
	staleParameters.Iterations--
	staleHash, err := hashPassword(passwordLoginTestPassword, staleParameters, bytes.NewReader(make([]byte, staleParameters.SaltLength)))
	if err != nil {
		t.Fatalf("hashPassword(stale) error = %v", err)
	}
	insertPasswordLoginUser(t, manager, "person", "person@example.com", "person", UserStatusActive, false, false, false, staleHash, now)

	if _, err := manager.AuthenticatePassword(t.Context(), PasswordLoginOptions{
		Identifier: "person",
		Password:   passwordLoginTestPassword,
		Source:     "198.51.100.43",
	}); err != nil {
		t.Fatalf("AuthenticatePassword() error = %v", err)
	}
	var upgradedHash string
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT password_hash FROM password_credentials WHERE user_id = 'person'`).Scan(&upgradedHash); err != nil {
		t.Fatal(err)
	}
	matches, needsRehash, err := VerifyPassword(upgradedHash, passwordLoginTestPassword)
	if err != nil || !matches || needsRehash || upgradedHash == staleHash {
		t.Fatalf("upgraded hash = changed:%t matches:%t needsRehash:%t error:%v", upgradedHash != staleHash, matches, needsRehash, err)
	}
}

func TestAuthenticatePasswordCompletionRollsBackAtomically(t *testing.T) {
	now := time.Date(2026, time.August, 5, 22, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{
		ids:    []string{"session-id"},
		tokens: []string{"session-token"},
	})
	staleParameters := currentPasswordHashParameters
	staleParameters.Iterations--
	staleHash, err := hashPassword(passwordLoginTestPassword, staleParameters, bytes.NewReader(make([]byte, staleParameters.SaltLength)))
	if err != nil {
		t.Fatal(err)
	}
	insertPasswordLoginUser(t, manager, "person", "person@example.com", "person", UserStatusActive, false, false, false, staleHash, now.Add(-time.Hour))
	for attempt := 1; attempt < int(loginThrottlePolicies[loginThrottleBucketIdentifier].delayStartsAt); attempt++ {
		if _, err := manager.RecordLoginFailure(t.Context(), "person@example.com", fmt.Sprintf("source-%d", attempt)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_password_session
		BEFORE INSERT ON sessions
		BEGIN
			SELECT RAISE(ABORT, 'reject password session');
		END`); err != nil {
		t.Fatalf("create session failure trigger: %v", err)
	}

	if result, err := manager.AuthenticatePassword(t.Context(), PasswordLoginOptions{
		Identifier: "person@example.com",
		Password:   passwordLoginTestPassword,
		Source:     "success-source",
	}); err == nil || result != nil || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("AuthenticatePassword(forced rollback) = %#v, %v", result, err)
	}
	var storedHash string
	var lastLoginAt sql.NullTime
	var sessionCount, throttleCount, identifierThrottleCount int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT password_hash FROM password_credentials WHERE user_id = 'person'`).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT last_login_at FROM users WHERE id = 'person'`).Scan(&lastLoginAt); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&sessionCount); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_throttle`).Scan(&throttleCount); err != nil {
		t.Fatal(err)
	}
	identifierHash, err := manager.loginThrottleBucketHash(loginThrottleBucketIdentifier, "person@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_throttle WHERE bucket_hash = ?`, identifierHash).Scan(&identifierThrottleCount); err != nil {
		t.Fatal(err)
	}
	if storedHash != staleHash || lastLoginAt.Valid || sessionCount != 0 || throttleCount != 6 || identifierThrottleCount != 1 {
		t.Fatalf("rollback state = hashChanged:%t lastLogin:%v sessions:%d throttle:%d identifierThrottle:%d", storedHash != staleHash, lastLoginAt, sessionCount, throttleCount, identifierThrottleCount)
	}
}
