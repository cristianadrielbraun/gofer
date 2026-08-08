package auth

import (
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func insertSetupAccessState(t *testing.T, manager *Manager, token string, expiresAt time.Time, attempts int64) {
	t.Helper()
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (
			id, initialized, setup_token_hash, setup_expires_at, setup_attempts, cutover_version
		) VALUES (1, 0, ?, ?, ?, 0)`, hashToken(token), expiresAt, attempts); err != nil {
		t.Fatalf("insert setup access state: %v", err)
	}
}

func TestBeginSetupExchangesTokenForHashOnlyOriginBoundAccess(t *testing.T) {
	now := time.Date(2026, time.August, 8, 18, 0, 0, 0, time.UTC)
	setupToken := "private-initial-setup-token"
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"setup-access-id", "setup-success-event"}, tokens: []string{"setup-access-secret"},
	})
	insertSetupAccessState(t, manager, setupToken, now.Add(time.Hour), 4)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_challenges (
			id, challenge_hash, purpose, origin, attempts, max_attempts, created_at, expires_at
		) VALUES ('replaced-setup-access', ?, 'enrollment', 'https://gofer.example', 0, 3, ?, ?)`,
		hashToken("replaced-access-secret"), now.Add(-time.Minute), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	challenge, err := manager.BeginSetup(t.Context(), BeginSetupOptions{
		Token: setupToken, Origin: "https://GOFER.example:443", UserAgent: "  Setup Browser/1.0  ",
	})
	if err != nil {
		t.Fatalf("BeginSetup() error = %v", err)
	}
	if challenge.ID != "setup-access-id" || challenge.Token != "setup-access-secret" ||
		challenge.Purpose != ChallengePurposeEnrollment || challenge.Origin != "https://gofer.example" ||
		challenge.UserID != "" || challenge.SessionID != "" || challenge.MaxAttempts != defaultPreAuthMaxAttempts ||
		!challenge.CreatedAt.Equal(now) || !challenge.ExpiresAt.Equal(now.Add(setupAccessChallengeLifetime)) {
		t.Fatalf("setup access challenge = %#v", challenge)
	}

	var initialized int
	var setupHash string
	var attempts int64
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT initialized, setup_token_hash, setup_attempts
		FROM auth_system_state WHERE id = 1`,
	).Scan(&initialized, &setupHash, &attempts); err != nil {
		t.Fatal(err)
	}
	if initialized != 0 || setupHash != hashToken(setupToken) || attempts != 0 {
		t.Fatalf("setup state after verification = initialized:%d hash:%q attempts:%d", initialized, setupHash, attempts)
	}
	var storedChallengeHash, purpose, origin string
	var userID, sessionID sql.NullString
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT challenge_hash, purpose, origin, user_id, session_id
		FROM auth_challenges WHERE id = 'setup-access-id'`,
	).Scan(&storedChallengeHash, &purpose, &origin, &userID, &sessionID); err != nil {
		t.Fatal(err)
	}
	if storedChallengeHash != hashToken(challenge.Token) || storedChallengeHash == challenge.Token ||
		purpose != string(ChallengePurposeEnrollment) || origin != "https://gofer.example" || userID.Valid || sessionID.Valid {
		t.Fatalf("stored setup challenge = hash:%q purpose:%q origin:%q user:%#v session:%#v", storedChallengeHash, purpose, origin, userID, sessionID)
	}
	var replacedConsumed sql.NullTime
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT consumed_at FROM auth_challenges WHERE id = 'replaced-setup-access'`,
	).Scan(&replacedConsumed); err != nil || !replacedConsumed.Valid || !replacedConsumed.Time.Equal(now) {
		t.Fatalf("replaced setup challenge consumed = %#v, %v", replacedConsumed, err)
	}
	var eventType, reason, userAgent, metadata string
	var actorID, subjectID, eventSessionID sql.NullString
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT event_type, reason, user_agent, metadata_json,
		       actor_user_id, subject_user_id, session_id
		FROM auth_events WHERE id = 'setup-success-event'`,
	).Scan(&eventType, &reason, &userAgent, &metadata, &actorID, &subjectID, &eventSessionID); err != nil {
		t.Fatal(err)
	}
	if eventType != string(AuthEventSetupTokenVerified) || reason != string(AuthEventReasonChallengeVerified) ||
		userAgent != "Setup Browser/1.0" || actorID.Valid || subjectID.Valid || eventSessionID.Valid ||
		!strings.Contains(metadata, `"challenge_id":"setup-access-id"`) ||
		strings.Contains(metadata, setupToken) || strings.Contains(metadata, setupHash) || strings.Contains(metadata, challenge.Token) || strings.Contains(metadata, storedChallengeHash) {
		t.Fatalf("setup verification event = type:%q reason:%q agent:%q metadata:%q", eventType, reason, userAgent, metadata)
	}

	found, err := manager.GetActiveSetupAccess(t.Context(), challenge.Token, "https://gofer.example")
	if err != nil || found == nil || found.ID != challenge.ID || found.Token != "" {
		t.Fatalf("GetActiveSetupAccess() = %#v, %v", found, err)
	}
	if foreign, err := manager.GetActiveSetupAccess(t.Context(), challenge.Token, "https://other.example"); err != nil || foreign != nil {
		t.Fatalf("GetActiveSetupAccess(foreign origin) = %#v, %v", foreign, err)
	}
}

func TestBeginSetupFailuresAreGenericBoundedAndSecretFree(t *testing.T) {
	now := time.Date(2026, time.August, 8, 19, 0, 0, 0, time.UTC)
	setupToken := "valid-setup-token"
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{
			"bad-access-1", "bad-event-1", "bad-access-2", "bad-event-2",
			"blocked-access", "blocked-event",
		},
		tokens: []string{"unused-access-1", "unused-access-2", "unused-blocked-access"},
	})
	insertSetupAccessState(t, manager, setupToken, now.Add(time.Hour), 8)

	for index, submitted := range []string{"wrong-secret-one", "wrong-secret-two"} {
		challenge, err := manager.BeginSetup(t.Context(), BeginSetupOptions{
			Token: submitted, Origin: "https://gofer.example", UserAgent: "Failure Browser",
		})
		if challenge != nil || !errors.Is(err, ErrSetupTokenInvalid) || err.Error() != ErrSetupTokenInvalid.Error() {
			t.Fatalf("BeginSetup(wrong %d) = %#v, %v", index+1, challenge, err)
		}
	}
	var attempts int64
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT setup_attempts FROM auth_system_state WHERE id = 1`).Scan(&attempts); err != nil || attempts != maximumSetupTokenAttempts {
		t.Fatalf("bounded setup attempts = %d, %v", attempts, err)
	}

	challenge, err := manager.BeginSetup(t.Context(), BeginSetupOptions{
		Token: setupToken, Origin: "https://gofer.example", UserAgent: "Failure Browser",
	})
	if challenge != nil || !errors.Is(err, ErrSetupTokenInvalid) || err.Error() != ErrSetupTokenInvalid.Error() {
		t.Fatalf("BeginSetup(blocked correct token) = %#v, %v", challenge, err)
	}
	var challengeCount, eventCount int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_challenges`).Scan(&challengeCount); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if challengeCount != 0 || eventCount != 2 {
		t.Fatalf("rejected setup persistence = challenges:%d events:%d", challengeCount, eventCount)
	}
	rows, err := manager.db.Read().QueryContext(t.Context(), `
		SELECT success, reason, user_agent, metadata_json FROM auth_events ORDER BY occurred_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var sawBlocked bool
	for rows.Next() {
		var success int
		var reason, userAgent, metadata string
		if err := rows.Scan(&success, &reason, &userAgent, &metadata); err != nil {
			t.Fatal(err)
		}
		if success != 0 || reason != string(AuthEventReasonInvalidCredentials) || userAgent != "Failure Browser" ||
			strings.Contains(metadata, setupToken) || strings.Contains(metadata, hashToken(setupToken)) || strings.Contains(metadata, "wrong-secret") {
			t.Fatalf("rejected setup event = success:%d reason:%q agent:%q metadata:%q", success, reason, userAgent, metadata)
		}
		if strings.Contains(metadata, `"blocked":true`) {
			sawBlocked = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !sawBlocked {
		t.Fatal("final rejected setup event did not record the bounded lockout")
	}
}

func TestBeginSetupExpiredMissingAndBlockedStatesShareGenericFailure(t *testing.T) {
	now := time.Date(2026, time.August, 8, 20, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		insert func(*testing.T, *Manager)
	}{
		{name: "missing singleton"},
		{name: "missing token", insert: func(t *testing.T, manager *Manager) {
			if _, err := manager.db.Write().Exec(`INSERT INTO auth_system_state (id, initialized) VALUES (1, 0)`); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "expired", insert: func(t *testing.T, manager *Manager) {
			insertSetupAccessState(t, manager, "setup-secret", now, 0)
		}},
		{name: "blocked", insert: func(t *testing.T, manager *Manager) {
			insertSetupAccessState(t, manager, "setup-secret", now.Add(time.Hour), maximumSetupTokenAttempts)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
				ids: []string{"unused-challenge", "unused-event"}, tokens: []string{"unused-access-secret"},
			})
			if test.insert != nil {
				test.insert(t, manager)
			}
			challenge, err := manager.BeginSetup(t.Context(), BeginSetupOptions{
				Token: "setup-secret", Origin: "https://gofer.example",
			})
			if challenge != nil || !errors.Is(err, ErrSetupTokenInvalid) || err.Error() != ErrSetupTokenInvalid.Error() {
				t.Fatalf("BeginSetup(%s) = %#v, %v", test.name, challenge, err)
			}
			var challenges, events int
			if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&challenges); err != nil {
				t.Fatal(err)
			}
			if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events`).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if challenges != 0 || events != 0 {
				t.Fatalf("inactive setup state mutated = challenges:%d events:%d", challenges, events)
			}
		})
	}
}

func TestBeginSetupRejectsInitializedStateAndSetupAccessExpires(t *testing.T) {
	now := time.Date(2026, time.August, 8, 21, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{
		ids: []string{"setup-access", "setup-event"}, tokens: []string{"setup-access-secret"},
	})
	insertSetupAccessState(t, manager, "setup-secret", now.Add(time.Hour), 0)
	challenge, err := manager.BeginSetup(t.Context(), BeginSetupOptions{
		Token: "setup-secret", Origin: "https://gofer.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	clock.now = challenge.ExpiresAt
	if found, err := manager.GetActiveSetupAccess(t.Context(), challenge.Token, challenge.Origin); err != nil || found != nil {
		t.Fatalf("GetActiveSetupAccess(at expiry) = %#v, %v", found, err)
	}

	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE auth_system_state
		SET initialized = 1, setup_token_hash = NULL, setup_expires_at = NULL
		WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	manager.tokens = &deterministicTokenGenerator{
		ids: []string{"unused-initialized-access", "unused-initialized-event"}, tokens: []string{"unused-initialized-secret"},
	}
	result, err := manager.BeginSetup(t.Context(), BeginSetupOptions{
		Token: "setup-secret", Origin: "https://gofer.example",
	})
	if result != nil || !errors.Is(err, ErrSetupAlreadyInitialized) {
		t.Fatalf("BeginSetup(initialized) = %#v, %v", result, err)
	}
}

func TestBeginSetupRollsBackAttemptsAndChallengeWhenAuditFails(t *testing.T) {
	now := time.Date(2026, time.August, 8, 22, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		submitted string
	}{
		{name: "rejected", submitted: "wrong-secret"},
		{name: "accepted", submitted: "setup-secret"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
				ids: []string{"rollback-access", "rollback-event"}, tokens: []string{"rollback-access-secret"},
			})
			insertSetupAccessState(t, manager, "setup-secret", now.Add(time.Hour), 2)
			expectedChallenges := 0
			if test.name == "accepted" {
				expectedChallenges = 1
				if _, err := manager.db.Write().ExecContext(t.Context(), `
					INSERT INTO auth_challenges (
						id, challenge_hash, purpose, origin, attempts, max_attempts, created_at, expires_at
					) VALUES ('preserved-access', ?, 'enrollment', 'https://gofer.example', 0, 3, ?, ?)`,
					hashToken("preserved-access-secret"), now.Add(-time.Minute), now.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := manager.db.Write().ExecContext(t.Context(), `
				CREATE TRIGGER reject_setup_verification_event
				BEFORE INSERT ON auth_events
				BEGIN SELECT RAISE(ABORT, 'reject setup verification event'); END`); err != nil {
				t.Fatal(err)
			}
			result, err := manager.BeginSetup(t.Context(), BeginSetupOptions{
				Token: test.submitted, Origin: "https://gofer.example",
			})
			if result != nil || err == nil {
				t.Fatalf("BeginSetup(audit failure) = %#v, %v", result, err)
			}
			var attempts int64
			var challenges int
			if err := manager.db.Read().QueryRow(`SELECT setup_attempts FROM auth_system_state WHERE id = 1`).Scan(&attempts); err != nil {
				t.Fatal(err)
			}
			if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&challenges); err != nil {
				t.Fatal(err)
			}
			if attempts != 2 || challenges != expectedChallenges {
				t.Fatalf("audit rollback = attempts:%d challenges:%d", attempts, challenges)
			}
			if test.name == "accepted" {
				var consumed sql.NullTime
				if err := manager.db.Read().QueryRow(`SELECT consumed_at FROM auth_challenges WHERE id = 'preserved-access'`).Scan(&consumed); err != nil || consumed.Valid {
					t.Fatalf("replaced access was not preserved by rollback = %#v, %v", consumed, err)
				}
			}
		})
	}
}

func TestBeginSetupRandomnessFailuresDoNotMutateState(t *testing.T) {
	now := time.Date(2026, time.August, 8, 23, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		tokens *deterministicTokenGenerator
	}{
		{name: "challenge ID", tokens: &deterministicTokenGenerator{idErr: errors.New("random source failed")}},
		{name: "challenge token", tokens: &deterministicTokenGenerator{
			ids: []string{"unused-access-id"}, tokenErr: errors.New("random source failed"),
		}},
		{name: "event ID", tokens: &deterministicTokenGenerator{
			ids: []string{"unused-access-id"}, tokens: []string{"unused-access-token"},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := newDeterministicManager(t, &fixedClock{now: now}, test.tokens)
			insertSetupAccessState(t, manager, "setup-secret", now.Add(time.Hour), 3)
			result, err := manager.BeginSetup(t.Context(), BeginSetupOptions{
				Token: "setup-secret", Origin: "https://gofer.example",
			})
			if result != nil || err == nil {
				t.Fatalf("BeginSetup(randomness failure) = %#v, %v", result, err)
			}
			var attempts, challenges, events int
			if err := manager.db.Read().QueryRow(`SELECT setup_attempts FROM auth_system_state WHERE id = 1`).Scan(&attempts); err != nil {
				t.Fatal(err)
			}
			if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&challenges); err != nil {
				t.Fatal(err)
			}
			if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events`).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if attempts != 3 || challenges != 0 || events != 0 {
				t.Fatalf("randomness failure mutation = attempts:%d challenges:%d events:%d", attempts, challenges, events)
			}
		})
	}
}

func TestConcurrentSetupVerificationLeavesOneActiveContinuation(t *testing.T) {
	now := time.Date(2026, time.August, 8, 23, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertSetupAccessState(t, manager, "setup-secret", now.Add(time.Hour), 0)

	const attempts = 8
	type result struct {
		challenge *PreAuthChallenge
		err       error
	}
	results := make(chan result, attempts)
	var group sync.WaitGroup
	for index := 0; index < attempts; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			challenge, err := manager.BeginSetup(t.Context(), BeginSetupOptions{
				Token: "setup-secret", Origin: "https://gofer.example",
			})
			results <- result{challenge: challenge, err: err}
		}()
	}
	group.Wait()
	close(results)

	succeeded := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent BeginSetup() error = %v", result.err)
		}
		if result.challenge == nil {
			t.Fatal("concurrent BeginSetup() returned nil challenge")
		}
		succeeded++
	}
	var total, active int
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges WHERE purpose = 'enrollment' AND user_id IS NULL`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges WHERE purpose = 'enrollment' AND user_id IS NULL AND consumed_at IS NULL`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if succeeded != attempts || total != attempts || active != 1 {
		t.Fatalf("concurrent setup verification = succeeded:%d total:%d active:%d", succeeded, total, active)
	}
}
