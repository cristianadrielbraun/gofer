package auth

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type synchronizedRemovalTokenGenerator struct {
	mu             sync.Mutex
	firstPairReady chan struct{}
	firstIDCalls   int
	delegate       secureTokenGenerator
}

func newSynchronizedRemovalTokenGenerator() *synchronizedRemovalTokenGenerator {
	return &synchronizedRemovalTokenGenerator{firstPairReady: make(chan struct{})}
}

func (generator *synchronizedRemovalTokenGenerator) ID() (string, error) {
	generator.mu.Lock()
	generator.firstIDCalls++
	waitForPair := generator.firstIDCalls <= 2
	if generator.firstIDCalls == 2 {
		close(generator.firstPairReady)
	}
	ready := generator.firstPairReady
	generator.mu.Unlock()
	if waitForPair {
		<-ready
	}
	return generator.delegate.ID()
}

func (generator *synchronizedRemovalTokenGenerator) Token(byteCount int) (string, error) {
	return generator.delegate.Token(byteCount)
}

type concurrentRemovalOutcome struct {
	session *Session
	err     error
}

func runConcurrentAuthenticatorRemovals(
	t *testing.T,
	first func() (*Session, error),
	second func() (*Session, error),
) concurrentRemovalOutcome {
	t.Helper()
	start := make(chan struct{})
	outcomes := make(chan concurrentRemovalOutcome, 2)
	for _, remove := range []func() (*Session, error){first, second} {
		go func(remove func() (*Session, error)) {
			<-start
			session, err := remove()
			outcomes <- concurrentRemovalOutcome{session: session, err: err}
		}(remove)
	}
	close(start)

	results := make([]concurrentRemovalOutcome, 0, 2)
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for len(results) < 2 {
		select {
		case outcome := <-outcomes:
			results = append(results, outcome)
		case <-timer.C:
			t.Fatal("concurrent authenticator removals did not finish")
		}
	}

	var winner concurrentRemovalOutcome
	successes := 0
	failures := 0
	for _, outcome := range results {
		switch {
		case outcome.err == nil && outcome.session != nil:
			successes++
			winner = outcome
		case outcome.session == nil &&
			(errors.Is(outcome.err, ErrSecuritySessionInvalid) || errors.Is(outcome.err, ErrLastAuthenticator)):
			failures++
		default:
			t.Fatalf("unexpected concurrent removal outcome: session=%#v error=%v", outcome.session, outcome.err)
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("concurrent removal outcomes: successes=%d failures=%d results=%#v", successes, failures, results)
	}
	return winner
}

func insertConcurrentRemovalSession(
	t *testing.T,
	manager *Manager,
	userID string,
	sessionID string,
	rawToken string,
	now time.Time,
) {
	t.Helper()
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method,
			assurance_level, user_agent, authenticated_at, last_used_at,
			idle_expires_at, absolute_expires_at, step_up_at, step_up_method, created_at
		) VALUES (?, ?, ?, 1, 'passkey', 'phishing_resistant', 'Concurrent removal test',
			?, ?, ?, ?, ?, 'passkey', ?)`,
		sessionID, userID, hashToken(rawToken), now, now,
		now.Add(time.Hour), now.Add(24*time.Hour), now, now,
	); err != nil {
		t.Fatalf("insert concurrent removal session %q: %v", sessionID, err)
	}
}

func insertConcurrentRemovalPasskey(t *testing.T, manager *Manager, userID, passkeyID string, now time.Time) {
	t.Helper()
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO webauthn_credentials (
			id, user_id, credential_id, public_key, name, created_at,
			credential_ciphertext, key_version, rp_id
		) VALUES (?, ?, ?, x'02', 'Concurrent passkey', ?, x'03', 1, 'gofer.example')`,
		passkeyID, userID, []byte("credential-"+passkeyID), now,
	); err != nil {
		t.Fatalf("insert concurrent removal passkey %q: %v", passkeyID, err)
	}
}

func TestLastAuthenticatorProtectionSerializesConcurrentTOTPAndPasskeyRemoval(t *testing.T) {
	now := time.Date(2026, time.August, 31, 10, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, newSynchronizedRemovalTokenGenerator())
	insertActiveUser(t, manager, "person", false, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE users SET mfa_required = 1 WHERE id = 'person';
		INSERT INTO password_credentials (user_id, password_hash, created_at, changed_at)
		VALUES ('person', 'password-hash', ?, ?);
		INSERT INTO totp_credentials (
			id, user_id, encrypted_seed, key_version, enabled, created_at
		) VALUES ('person-totp', 'person', x'01', 1, 1, ?)`, now, now, now); err != nil {
		t.Fatalf("prepare TOTP/passkey removal race: %v", err)
	}
	insertConcurrentRemovalPasskey(t, manager, "person", "person-passkey", now)
	insertConcurrentRemovalSession(t, manager, "person", "totp-removal-session", "totp-removal-token", now)
	insertConcurrentRemovalSession(t, manager, "person", "passkey-removal-session", "passkey-removal-token", now)

	winner := runConcurrentAuthenticatorRemovals(t,
		func() (*Session, error) {
			return manager.DisableTOTP(t.Context(), "totp-removal-token", "TOTP removal")
		},
		func() (*Session, error) {
			return manager.RemovePasskey(t.Context(), "passkey-removal-token", "person-passkey", "Passkey removal")
		},
	)

	var activeTOTP, activePasskeys, authVersion, activeSessions, events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM totp_credentials
		WHERE user_id = 'person' AND enabled = 1 AND revoked_at IS NULL`).Scan(&activeTOTP); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM webauthn_credentials
		WHERE user_id = 'person' AND revoked_at IS NULL`).Scan(&activePasskeys); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT auth_version FROM users WHERE id = 'person'`).Scan(&authVersion); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sessions WHERE user_id = 'person' AND revoked_at IS NULL`).Scan(&activeSessions); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_events
		WHERE subject_user_id = 'person' AND event_type = ?`, AuthEventCredentialChanged).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if activeTOTP+activePasskeys != 1 || authVersion != 2 || activeSessions != 1 || events != 1 {
		t.Fatalf(
			"serialized TOTP/passkey removal state: TOTP=%d passkeys=%d auth_version=%d sessions=%d events=%d",
			activeTOTP, activePasskeys, authVersion, activeSessions, events,
		)
	}

	if activeTOTP == 1 {
		if rotated, err := manager.DisableTOTP(t.Context(), winner.session.Token, "Final TOTP removal"); !errors.Is(err, ErrLastAuthenticator) || rotated != nil {
			t.Fatalf("final TOTP removal = %#v, %v", rotated, err)
		}
	} else if rotated, err := manager.RemovePasskey(
		t.Context(), winner.session.Token, "person-passkey", "Final passkey removal",
	); !errors.Is(err, ErrLastAuthenticator) || rotated != nil {
		t.Fatalf("final passkey removal = %#v, %v", rotated, err)
	}
}

func TestLastSignInMethodProtectionSerializesConcurrentPasskeyAndIdentityRemoval(t *testing.T) {
	now := time.Date(2026, time.August, 31, 11, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, newSynchronizedRemovalTokenGenerator())
	insertActiveUser(t, manager, "person", false, now)
	configureGoogleOAuthTest(manager)
	insertConcurrentRemovalPasskey(t, manager, "person", "person-passkey", now)
	insertLinkedGoogleIdentity(t, manager, "person-google", "person", "person-subject", "person@example.com", now)
	insertConcurrentRemovalSession(t, manager, "person", "passkey-removal-session", "passkey-removal-token", now)
	insertConcurrentRemovalSession(t, manager, "person", "identity-removal-session", "identity-removal-token", now)

	winner := runConcurrentAuthenticatorRemovals(t,
		func() (*Session, error) {
			return manager.RemovePasskey(t.Context(), "passkey-removal-token", "person-passkey", "Passkey removal")
		},
		func() (*Session, error) {
			return manager.UnlinkGoogleIdentity(t.Context(), "identity-removal-token", "person-google", "Google removal")
		},
	)

	var activePasskeys, identities, authVersion, activeSessions, events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM webauthn_credentials
		WHERE user_id = 'person' AND revoked_at IS NULL`).Scan(&activePasskeys); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_identities WHERE user_id = 'person'`).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT auth_version FROM users WHERE id = 'person'`).Scan(&authVersion); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sessions WHERE user_id = 'person' AND revoked_at IS NULL`).Scan(&activeSessions); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_events
		WHERE subject_user_id = 'person' AND event_type IN (?, ?)`,
		AuthEventCredentialChanged, AuthEventIdentityUnlinked,
	).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if activePasskeys+identities != 1 || authVersion != 2 || activeSessions != 1 || events != 1 {
		t.Fatalf(
			"serialized passkey/identity removal state: passkeys=%d identities=%d auth_version=%d sessions=%d events=%d",
			activePasskeys, identities, authVersion, activeSessions, events,
		)
	}

	if activePasskeys == 1 {
		if rotated, err := manager.RemovePasskey(
			t.Context(), winner.session.Token, "person-passkey", "Final passkey removal",
		); !errors.Is(err, ErrLastAuthenticator) || rotated != nil {
			t.Fatalf("final passkey removal = %#v, %v", rotated, err)
		}
	} else if rotated, err := manager.UnlinkGoogleIdentity(
		t.Context(), winner.session.Token, "person-google", "Final Google removal",
	); !errors.Is(err, ErrLastAuthenticator) || rotated != nil {
		t.Fatalf("final Google removal = %#v, %v", rotated, err)
	}
}
