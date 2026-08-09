package auth

import (
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	totpLoginTestUserID = "totp-person"
	totpLoginTestOrigin = "https://gofer.example"
)

func prepareTOTPLoginManager(t *testing.T, now time.Time, tokens TokenGenerator) (*Manager, string, int64) {
	t.Helper()
	manager := newDeterministicManager(t, &fixedClock{now: now}, tokens)
	insertPasswordLoginUser(
		t, manager, totpLoginTestUserID, "totp@example.com", "totp-person",
		UserStatusActive, true, true, false, currentPasswordLoginHash(t), now.Add(-time.Hour),
	)
	key, err := newTOTPKey("totp@example.com", "deterministic TOTP login seed material")
	if err != nil {
		t.Fatal(err)
	}
	credentialID := "totp-credential"
	encryptedSeed, err := manager.encryptTOTPSeed(totpLoginTestUserID, credentialID, key.Secret())
	if err != nil {
		t.Fatal(err)
	}
	lastAcceptedStep := now.UTC().Unix()/totpPeriodSeconds - 2
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO totp_credentials (
			id, user_id, encrypted_seed, key_version, algorithm, digits, period,
			issuer, last_accepted_step, enabled, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		credentialID, totpLoginTestUserID, encryptedSeed, totpCredentialKeyVersion,
		totpAlgorithm, totpDigits, totpPeriodSeconds, totpIssuer, lastAcceptedStep, now,
	); err != nil {
		t.Fatal(err)
	}
	return manager, key.Secret(), lastAcceptedStep
}

func insertTOTPLoginChallenge(t *testing.T, manager *Manager, id, token string, now time.Time, maxAttempts int) {
	t.Helper()
	challenge := &PreAuthChallenge{
		ID: id, Token: token, UserID: totpLoginTestUserID,
		Purpose: ChallengePurposeMFA, Origin: totpLoginTestOrigin,
		MaxAttempts: maxAttempts, CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	payload, err := manager.encryptMFAContinuationDraft(challenge, &mfaContinuationDraft{
		Version: mfaContinuationDraftVersion, AuthVersion: 1,
		PrimaryMethod: AuthenticationMethodPassword, PrimaryAssurance: AssuranceLevelSingleFactor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_challenges (
			id, user_id, challenge_hash, purpose, origin, attempts,
			max_attempts, payload_ciphertext, created_at, expires_at
		) VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
		id, totpLoginTestUserID, hashToken(token), ChallengePurposeMFA,
		totpLoginTestOrigin, maxAttempts, payload, now, now.Add(10*time.Minute),
	); err != nil {
		t.Fatal(err)
	}
}

func completeTOTPLogin(t *testing.T, manager *Manager, token, code string) (*Session, error) {
	t.Helper()
	return manager.CompleteTOTPLogin(t.Context(), TOTPLoginOptions{
		Token: token, Code: code, Origin: totpLoginTestOrigin,
		Source: "198.51.100.44", UserAgent: "  TOTP Browser/1.0  ",
	})
}

func invalidTOTPCode(valid string) string {
	if valid[0] == '0' {
		return "1" + valid[1:]
	}
	return "0" + valid[1:]
}

func TestCompleteTOTPLoginCommitsReplayStepSessionAndRedactedEvent(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 0, 0, 0, time.UTC)
	manager, secret, _ := prepareTOTPLoginManager(t, now, &deterministicTokenGenerator{
		ids: []string{"totp-success-event", "totp-session-id"}, tokens: []string{"totp-session-token"},
	})
	insertTOTPLoginChallenge(t, manager, "totp-challenge", "totp-challenge-token", now, 3)
	insertTOTPLoginChallenge(t, manager, "parallel-challenge", "parallel-token", now, 3)
	code := setupTOTPCode(t, secret, now)

	session, err := completeTOTPLogin(t, manager, "totp-challenge-token", code)
	if err != nil {
		t.Fatalf("CompleteTOTPLogin() error = %v", err)
	}
	if session == nil || session.ID != "totp-session-id" || session.Token != "totp-session-token" ||
		session.UserID != totpLoginTestUserID || session.AuthenticationMethod != AuthenticationMethodPassword ||
		session.AssuranceLevel != AssuranceLevelMultiFactor || session.StepUpAt == nil ||
		session.StepUpMethod != AuthenticationMethodTOTP || session.UserAgent != "TOTP Browser/1.0" {
		t.Fatalf("TOTP session = %#v", session)
	}
	stored, err := manager.GetSessionByToken(t.Context(), session.Token)
	if err != nil || stored == nil || stored.ID != session.ID {
		t.Fatalf("stored TOTP session = %#v, %v", stored, err)
	}

	var acceptedStep int64
	var lastUsedAt, lastLoginAt time.Time
	if err := manager.db.Read().QueryRow(`
		SELECT last_accepted_step, last_used_at FROM totp_credentials WHERE id = 'totp-credential'`,
	).Scan(&acceptedStep, &lastUsedAt); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT last_login_at FROM users WHERE id = ?`, totpLoginTestUserID).Scan(&lastLoginAt); err != nil {
		t.Fatal(err)
	}
	if acceptedStep != now.Unix()/totpPeriodSeconds || !lastUsedAt.Equal(now) || !lastLoginAt.Equal(now) {
		t.Fatalf("TOTP login timestamps = step:%d credential:%v user:%v", acceptedStep, lastUsedAt, lastLoginAt)
	}
	var activeChallenges, consumedAttempts int
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges WHERE purpose = ? AND consumed_at IS NULL`, ChallengePurposeMFA).Scan(&activeChallenges); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT attempts FROM auth_challenges WHERE id = 'totp-challenge'`).Scan(&consumedAttempts); err != nil {
		t.Fatal(err)
	}
	if activeChallenges != 0 || consumedAttempts != 1 {
		t.Fatalf("consumed TOTP challenges = active:%d attempts:%d", activeChallenges, consumedAttempts)
	}

	var eventType, reason, userAgent, sourceHash, metadata string
	var success int
	if err := manager.db.Read().QueryRow(`
		SELECT event_type, success, reason, user_agent, source_hash, metadata_json
		FROM auth_events WHERE id = 'totp-success-event'`,
	).Scan(&eventType, &success, &reason, &userAgent, &sourceHash, &metadata); err != nil {
		t.Fatal(err)
	}
	if eventType != string(AuthEventLoginSucceeded) || success != 1 || reason != string(AuthEventReasonChallengeVerified) ||
		userAgent != "TOTP Browser/1.0" || sourceHash == "" || sourceHash == "198.51.100.44" ||
		metadata != `{"factor":"totp","primary":"password"}` {
		t.Fatalf("TOTP success event = type:%q success:%d reason:%q agent:%q source:%q metadata:%q", eventType, success, reason, userAgent, sourceHash, metadata)
	}
	for _, secretValue := range []string{secret, code, "totp-challenge-token", "totp-session-token", "198.51.100.44"} {
		if strings.Contains(metadata, secretValue) || strings.Contains(sourceHash, secretValue) {
			t.Fatalf("TOTP success event exposed secret %q", secretValue)
		}
	}
}

func TestCompleteTOTPLoginAcceptsNarrowWindowAndRejectsOutside(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 2, 0, 0, time.UTC)
	for _, offset := range []time.Duration{-30 * time.Second, 0, 30 * time.Second} {
		t.Run(offset.String(), func(t *testing.T) {
			manager, secret, _ := prepareTOTPLoginManager(t, now, &deterministicTokenGenerator{
				ids: []string{"window-event", "window-session"}, tokens: []string{"window-session-token"},
			})
			insertTOTPLoginChallenge(t, manager, "window-challenge", "window-challenge-token", now, 3)

			session, err := completeTOTPLogin(t, manager, "window-challenge-token", setupTOTPCode(t, secret, now.Add(offset)))
			if err != nil || session == nil {
				t.Fatalf("CompleteTOTPLogin(%s) = %#v, %v", offset, session, err)
			}
			var acceptedStep int64
			if err := manager.db.Read().QueryRow(`SELECT last_accepted_step FROM totp_credentials WHERE id = 'totp-credential'`).Scan(&acceptedStep); err != nil {
				t.Fatal(err)
			}
			wantStep := now.Add(offset).Unix() / totpPeriodSeconds
			if acceptedStep != wantStep {
				t.Fatalf("accepted TOTP step = %d, want %d", acceptedStep, wantStep)
			}
		})
	}

	manager, secret, originalStep := prepareTOTPLoginManager(t, now, &deterministicTokenGenerator{
		ids: []string{"outside-window-event"},
	})
	insertTOTPLoginChallenge(t, manager, "outside-window-challenge", "outside-window-token", now, 3)
	session, err := completeTOTPLogin(t, manager, "outside-window-token", setupTOTPCode(t, secret, now.Add(-2*totpPeriodSeconds*time.Second)))
	var validationError *TOTPLoginValidationError
	if session != nil || !errors.As(err, &validationError) || validationError.Terminal {
		t.Fatalf("outside-window TOTP = %#v, %T %v", session, err, err)
	}
	var acceptedStep, attempts int64
	if err := manager.db.Read().QueryRow(`SELECT last_accepted_step FROM totp_credentials WHERE id = 'totp-credential'`).Scan(&acceptedStep); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT attempts FROM auth_challenges WHERE id = 'outside-window-challenge'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if acceptedStep != originalStep || attempts != 1 {
		t.Fatalf("rejected TOTP state = step:%d attempts:%d", acceptedStep, attempts)
	}
}

func TestCompleteTOTPLoginRejectsReplayAndBoundsFailures(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 5, 0, 0, time.UTC)
	manager, secret, originalStep := prepareTOTPLoginManager(t, now, &deterministicTokenGenerator{
		ids: []string{"failure-event-1", "failure-event-2", "failure-event-3"},
	})
	insertTOTPLoginChallenge(t, manager, "failure-challenge", "failure-token", now, 3)
	invalidCode := invalidTOTPCode(setupTOTPCode(t, secret, now))

	for attempt := 1; attempt <= 3; attempt++ {
		session, err := completeTOTPLogin(t, manager, "failure-token", invalidCode)
		var validationError *TOTPLoginValidationError
		if session != nil || !errors.As(err, &validationError) || validationError.Terminal != (attempt == 3) {
			t.Fatalf("invalid TOTP attempt %d = %#v, %T %v", attempt, session, err, err)
		}
	}
	var attempts, sessions, events int
	var consumedAt sql.NullTime
	var acceptedStep int64
	if err := manager.db.Read().QueryRow(`SELECT attempts, consumed_at FROM auth_challenges WHERE id = 'failure-challenge'`).Scan(&attempts, &consumedAt); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT last_accepted_step FROM totp_credentials WHERE id = 'totp-credential'`).Scan(&acceptedStep); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE event_type = ? AND success = 0`, AuthEventLoginFailed).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || !consumedAt.Valid || acceptedStep != originalStep || sessions != 0 || events != 3 {
		t.Fatalf("bounded TOTP failures = attempts:%d consumed:%t step:%d sessions:%d events:%d", attempts, consumedAt.Valid, acceptedStep, sessions, events)
	}

	manager.tokens = &deterministicTokenGenerator{
		ids: []string{"success-event", "success-session", "replay-event"}, tokens: []string{"success-token"},
	}
	insertTOTPLoginChallenge(t, manager, "success-challenge", "success-challenge-token", now, 3)
	code := setupTOTPCode(t, secret, now)
	if session, err := completeTOTPLogin(t, manager, "success-challenge-token", code); err != nil || session == nil {
		t.Fatalf("successful replay prerequisite = %#v, %v", session, err)
	}
	insertTOTPLoginChallenge(t, manager, "replay-challenge", "replay-challenge-token", now, 3)
	if session, err := completeTOTPLogin(t, manager, "replay-challenge-token", code); session != nil {
		t.Fatalf("replayed TOTP created session %#v", session)
	} else {
		var validationError *TOTPLoginValidationError
		if !errors.As(err, &validationError) || validationError.Terminal {
			t.Fatalf("replayed TOTP error = %T %v", err, err)
		}
	}
}

func TestCompleteTOTPLoginPersistsThrottleAcrossChallenges(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 10, 0, 0, time.UTC)
	manager, secret, _ := prepareTOTPLoginManager(t, now, &deterministicTokenGenerator{
		ids: []string{"failure-1", "failure-2", "failure-3", "failure-4", "failure-5"},
	})
	insertTOTPLoginChallenge(t, manager, "throttle-challenge", "throttle-token", now, 10)
	invalidCode := invalidTOTPCode(setupTOTPCode(t, secret, now))
	for attempt := 1; attempt <= 4; attempt++ {
		if session, err := completeTOTPLogin(t, manager, "throttle-token", invalidCode); session != nil {
			t.Fatalf("invalid TOTP attempt %d created session %#v", attempt, session)
		} else {
			var validationError *TOTPLoginValidationError
			if !errors.As(err, &validationError) {
				t.Fatalf("invalid TOTP attempt %d error = %T %v", attempt, err, err)
			}
		}
	}
	if session, err := completeTOTPLogin(t, manager, "throttle-token", invalidCode); session != nil {
		t.Fatalf("throttling attempt created session %#v", session)
	} else {
		var throttleError *LoginThrottleError
		if !errors.As(err, &throttleError) || throttleError.RetryAfter != time.Second {
			t.Fatalf("fifth TOTP failure = %T %v", err, err)
		}
	}
	var attempts int
	if err := manager.db.Read().QueryRow(`SELECT attempts FROM auth_challenges WHERE id = 'throttle-challenge'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 5 {
		t.Fatalf("throttled challenge attempts = %d, want 5", attempts)
	}
	if session, err := completeTOTPLogin(t, manager, "throttle-token", invalidCode); session != nil {
		t.Fatalf("blocked TOTP attempt created session %#v", session)
	} else if !errors.Is(err, ErrLoginThrottled) {
		t.Fatalf("blocked TOTP attempt error = %T %v", err, err)
	}
	if err := manager.db.Read().QueryRow(`SELECT attempts FROM auth_challenges WHERE id = 'throttle-challenge'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 5 {
		t.Fatalf("pre-check throttling advanced challenge to %d attempts", attempts)
	}
}

func TestCompleteTOTPLoginHasOneWinnerUnderConcurrency(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 15, 0, 0, time.UTC)
	manager, secret, _ := prepareTOTPLoginManager(t, now, &deterministicTokenGenerator{
		ids: []string{"event-a", "event-b", "winner-session"}, tokens: []string{"winner-token"},
	})
	insertTOTPLoginChallenge(t, manager, "challenge-a", "token-a", now, 3)
	insertTOTPLoginChallenge(t, manager, "challenge-b", "token-b", now, 3)
	code := setupTOTPCode(t, secret, now)

	type result struct {
		session *Session
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, token := range []string{"token-a", "token-b"} {
		go func(token string) {
			ready.Done()
			<-start
			session, err := manager.CompleteTOTPLogin(t.Context(), TOTPLoginOptions{
				Token: token, Code: code, Origin: totpLoginTestOrigin,
				Source: "198.51.100.44", UserAgent: "parallel browser",
			})
			results <- result{session: session, err: err}
		}(token)
	}
	ready.Wait()
	close(start)
	first, second := <-results, <-results
	winners := 0
	losers := 0
	for _, outcome := range []result{first, second} {
		if outcome.err == nil && outcome.session != nil {
			winners++
		} else if outcome.session == nil && errors.Is(outcome.err, ErrTOTPLoginChallengeInvalid) {
			losers++
		} else {
			t.Fatalf("concurrent TOTP outcome = %#v, %T %v", outcome.session, outcome.err, outcome.err)
		}
	}
	var sessions, activeChallenges int
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges WHERE purpose = ? AND consumed_at IS NULL`, ChallengePurposeMFA).Scan(&activeChallenges); err != nil {
		t.Fatal(err)
	}
	if winners != 1 || losers != 1 || sessions != 1 || activeChallenges != 0 {
		t.Fatalf("concurrent TOTP = winners:%d losers:%d sessions:%d active:%d", winners, losers, sessions, activeChallenges)
	}
}

func TestCompleteTOTPLoginRollsBackWhenAuditPersistenceFails(t *testing.T) {
	now := time.Date(2026, time.August, 9, 12, 20, 0, 0, time.UTC)
	manager, secret, originalStep := prepareTOTPLoginManager(t, now, &deterministicTokenGenerator{
		ids: []string{"rejected-event", "rolled-back-session"}, tokens: []string{"rolled-back-token"},
	})
	insertTOTPLoginChallenge(t, manager, "rollback-challenge", "rollback-token", now, 3)
	if _, err := manager.db.Write().Exec(`
		CREATE TRIGGER reject_totp_login_event
		BEFORE INSERT ON auth_events
		BEGIN SELECT RAISE(ABORT, 'reject TOTP login event'); END`); err != nil {
		t.Fatal(err)
	}
	if session, err := completeTOTPLogin(t, manager, "rollback-token", setupTOTPCode(t, secret, now)); session != nil || err == nil {
		t.Fatalf("rolled-back TOTP login = %#v, %v", session, err)
	}
	var acceptedStep int64
	var attempts, sessions int
	var consumedAt sql.NullTime
	var lastLoginAt sql.NullTime
	if err := manager.db.Read().QueryRow(`SELECT last_accepted_step FROM totp_credentials WHERE id = 'totp-credential'`).Scan(&acceptedStep); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT attempts, consumed_at FROM auth_challenges WHERE id = 'rollback-challenge'`).Scan(&attempts, &consumedAt); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT last_login_at FROM users WHERE id = ?`, totpLoginTestUserID).Scan(&lastLoginAt); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if acceptedStep != originalStep || attempts != 0 || consumedAt.Valid || lastLoginAt.Valid || sessions != 0 {
		t.Fatalf("TOTP rollback = step:%d attempts:%d consumed:%t login:%v sessions:%d", acceptedStep, attempts, consumedAt.Valid, lastLoginAt, sessions)
	}
}
