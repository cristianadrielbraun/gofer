package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestPreAuthChallengeIsHashOnlyPurposeAndOriginBound(t *testing.T) {
	now := time.Date(2026, time.August, 4, 13, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"challenge-id"},
		tokens: []string{"raw-pre-auth-token", "raw-oidc-nonce"},
	})
	challenge, err := manager.CreatePreAuthChallenge(t.Context(), PreAuthChallengeOptions{
		Purpose:     ChallengePurposeFederatedLogin,
		Origin:      "HTTPS://Gofer.Example:443/",
		Lifetime:    10 * time.Minute,
		MaxAttempts: 3,
		IssueNonce:  true,
	})
	if err != nil {
		t.Fatalf("CreatePreAuthChallenge() error = %v", err)
	}
	if challenge.Token != "raw-pre-auth-token" || challenge.Nonce != "raw-oidc-nonce" || challenge.Origin != "https://gofer.example" || challenge.ExpiresAt != now.Add(10*time.Minute) {
		t.Fatalf("created challenge = %#v", challenge)
	}
	var storedHash, nonceHash, origin string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT challenge_hash, nonce_hash, origin FROM auth_challenges WHERE id = ?`, challenge.ID).Scan(&storedHash, &nonceHash, &origin); err != nil {
		t.Fatalf("query stored challenge: %v", err)
	}
	if storedHash != hashToken(challenge.Token) || storedHash == challenge.Token || nonceHash != hashToken(challenge.Nonce) || nonceHash == challenge.Nonce || origin != challenge.Origin {
		t.Fatalf("stored challenge = hash:%q nonce:%q origin:%q", storedHash, nonceHash, origin)
	}
	if consumed, err := manager.ConsumePreAuthChallenge(t.Context(), challenge.Token, challenge.Nonce, ChallengePurposeLogin, challenge.Origin); !errors.Is(err, ErrPreAuthChallengeInvalid) || consumed != nil {
		t.Fatalf("cross-purpose consume = %#v, %v", consumed, err)
	}
	if consumed, err := manager.ConsumePreAuthChallenge(t.Context(), challenge.Token, challenge.Nonce, challenge.Purpose, "https://other.example"); !errors.Is(err, ErrPreAuthChallengeInvalid) || consumed != nil {
		t.Fatalf("cross-origin consume = %#v, %v", consumed, err)
	}
	if consumed, err := manager.ConsumePreAuthChallenge(t.Context(), challenge.Token, "wrong-nonce", challenge.Purpose, challenge.Origin); !errors.Is(err, ErrPreAuthChallengeInvalid) || consumed != nil {
		t.Fatalf("wrong-nonce consume = %#v, %v", consumed, err)
	}
	consumed, err := manager.ConsumePreAuthChallenge(t.Context(), challenge.Token, challenge.Nonce, challenge.Purpose, "https://GOFER.example:443")
	if err != nil {
		t.Fatalf("ConsumePreAuthChallenge() error = %v", err)
	}
	if consumed.Token != "" || consumed.Nonce != "" || consumed.ConsumedAt == nil || consumed.Attempts != 2 {
		t.Fatalf("consumed challenge = %#v", consumed)
	}
	if replay, err := manager.ConsumePreAuthChallenge(t.Context(), challenge.Token, challenge.Nonce, challenge.Purpose, challenge.Origin); !errors.Is(err, ErrPreAuthChallengeInvalid) || replay != nil {
		t.Fatalf("replayed challenge = %#v, %v", replay, err)
	}
}

func TestPreAuthChallengeExpiryAndBoundedFailuresFailClosed(t *testing.T) {
	now := time.Date(2026, time.August, 4, 13, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{
		ids:    []string{"failure-id", "expiry-id"},
		tokens: []string{"failure-token", "expiry-token"},
	})
	failure, err := manager.CreatePreAuthChallenge(t.Context(), PreAuthChallengeOptions{
		Purpose: ChallengePurposeMFA, Origin: "https://gofer.example", MaxAttempts: 2,
	})
	if err != nil {
		t.Fatalf("CreatePreAuthChallenge(failure) error = %v", err)
	}
	if terminal, err := manager.RecordPreAuthChallengeFailure(t.Context(), failure.Token, failure.Purpose, failure.Origin); err != nil || terminal {
		t.Fatalf("first failure = terminal:%t error:%v", terminal, err)
	}
	if terminal, err := manager.RecordPreAuthChallengeFailure(t.Context(), failure.Token, failure.Purpose, failure.Origin); err != nil || !terminal {
		t.Fatalf("second failure = terminal:%t error:%v", terminal, err)
	}
	if consumed, err := manager.ConsumePreAuthChallenge(t.Context(), failure.Token, "", failure.Purpose, failure.Origin); !errors.Is(err, ErrPreAuthChallengeInvalid) || consumed != nil {
		t.Fatalf("consume after terminal failure = %#v, %v", consumed, err)
	}

	expiring, err := manager.CreatePreAuthChallenge(t.Context(), PreAuthChallengeOptions{
		Purpose: ChallengePurposeRecovery, Origin: "https://gofer.example", Lifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("CreatePreAuthChallenge(expiry) error = %v", err)
	}
	clock.now = expiring.ExpiresAt
	if consumed, err := manager.ConsumePreAuthChallenge(t.Context(), expiring.Token, "", expiring.Purpose, expiring.Origin); !errors.Is(err, ErrPreAuthChallengeInvalid) || consumed != nil {
		t.Fatalf("consume at expiry = %#v, %v", consumed, err)
	}
	var consumedAt bool
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT consumed_at IS NOT NULL FROM auth_challenges WHERE id = ?`, expiring.ID).Scan(&consumedAt); err != nil || !consumedAt {
		t.Fatalf("expired challenge consumed = %t, %v", consumedAt, err)
	}
}

func TestPreAuthChallengeRejectsInvalidContractsWithoutPersistence(t *testing.T) {
	manager := newDeterministicManager(t, &fixedClock{}, &deterministicTokenGenerator{})
	tests := []PreAuthChallengeOptions{
		{Purpose: ChallengePurpose("unknown"), Origin: "https://gofer.example"},
		{Purpose: ChallengePurposeLogin, Origin: "https://gofer.example/path"},
		{Purpose: ChallengePurposeLogin, Origin: "javascript://gofer.example"},
		{Purpose: ChallengePurposeLogin, Origin: "https://gofer.example", Lifetime: maximumPreAuthLifetime + time.Second},
		{Purpose: ChallengePurposeLogin, Origin: "https://gofer.example", MaxAttempts: maximumPreAuthAttempts + 1},
	}
	for _, options := range tests {
		if challenge, err := manager.CreatePreAuthChallenge(t.Context(), options); err == nil || challenge != nil {
			t.Fatalf("CreatePreAuthChallenge(%#v) = %#v, %v, want rejection", options, challenge, err)
		}
	}
	var count int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_challenges`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("challenge count after invalid contracts = %d, %v", count, err)
	}
}

func TestGetActivePreAuthChallengeRequiresMatchingLiveFlow(t *testing.T) {
	now := time.Date(2026, time.August, 5, 8, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{
		ids:    []string{"challenge-id"},
		tokens: []string{"challenge-token"},
	})
	challenge, err := manager.CreatePreAuthChallenge(t.Context(), PreAuthChallengeOptions{
		Purpose:  ChallengePurposeMFA,
		Origin:   "https://gofer.example",
		Lifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("CreatePreAuthChallenge() error = %v", err)
	}
	found, err := manager.GetActivePreAuthChallenge(t.Context(), challenge.Token, ChallengePurposeMFA, "https://gofer.example")
	if err != nil || found == nil || found.ID != challenge.ID {
		t.Fatalf("GetActivePreAuthChallenge() = %#v, %v", found, err)
	}
	for _, lookup := range []struct {
		token   string
		purpose ChallengePurpose
		origin  string
	}{
		{token: "wrong-token", purpose: ChallengePurposeMFA, origin: "https://gofer.example"},
		{token: challenge.Token, purpose: ChallengePurposeLogin, origin: "https://gofer.example"},
		{token: challenge.Token, purpose: ChallengePurposeMFA, origin: "https://other.example"},
	} {
		if found, err := manager.GetActivePreAuthChallenge(t.Context(), lookup.token, lookup.purpose, lookup.origin); err != nil || found != nil {
			t.Fatalf("mismatched GetActivePreAuthChallenge() = %#v, %v", found, err)
		}
	}
	clock.now = challenge.ExpiresAt
	if found, err := manager.GetActivePreAuthChallenge(t.Context(), challenge.Token, ChallengePurposeMFA, "https://gofer.example"); err != nil || found != nil {
		t.Fatalf("expired GetActivePreAuthChallenge() = %#v, %v", found, err)
	}
}

func TestPreAuthChallengeSessionBindingRequiresActiveOwningSession(t *testing.T) {
	now := time.Date(2026, time.August, 4, 13, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"session-id", "challenge-id", "mismatch-id", "revoked-id"},
		tokens: []string{"session-token", "challenge-token", "mismatch-token", "revoked-token"},
	})
	insertActiveUser(t, manager, "user-one", false, now)
	insertActiveUser(t, manager, "user-two", false, now)
	session, err := manager.CreateSession(t.Context(), "user-one", "test-agent")
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}

	challenge, err := manager.CreatePreAuthChallenge(t.Context(), PreAuthChallengeOptions{
		SessionID: session.ID, Purpose: ChallengePurposeStepUp, Origin: "https://gofer.example",
	})
	if err != nil {
		t.Fatalf("CreatePreAuthChallenge() error = %v", err)
	}
	if challenge.UserID != "user-one" || challenge.SessionID != session.ID {
		t.Fatalf("inferred challenge binding = user:%q session:%q", challenge.UserID, challenge.SessionID)
	}
	if challenge, err := manager.CreatePreAuthChallenge(t.Context(), PreAuthChallengeOptions{
		UserID: "user-two", SessionID: session.ID, Purpose: ChallengePurposeStepUp, Origin: "https://gofer.example",
	}); !errors.Is(err, ErrSessionNotActive) || challenge != nil {
		t.Fatalf("cross-user session binding = %#v, %v", challenge, err)
	}

	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE sessions SET revoked_at = ?, revocation_reason = ? WHERE id = ?`,
		now, SessionRevocationLogout, session.ID,
	); err != nil {
		t.Fatalf("revoke session: %v", err)
	}
	if challenge, err := manager.CreatePreAuthChallenge(t.Context(), PreAuthChallengeOptions{
		SessionID: session.ID, Purpose: ChallengePurposeStepUp, Origin: "https://gofer.example",
	}); !errors.Is(err, ErrSessionNotActive) || challenge != nil {
		t.Fatalf("revoked session binding = %#v, %v", challenge, err)
	}
}

func TestConcurrentPreAuthConsumptionSucceedsOnce(t *testing.T) {
	now := time.Date(2026, time.August, 4, 13, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"challenge-id"}, tokens: []string{"challenge-token"},
	})
	challenge, err := manager.CreatePreAuthChallenge(t.Context(), PreAuthChallengeOptions{
		Purpose: ChallengePurposeStepUp, Origin: "https://gofer.example",
	})
	if err != nil {
		t.Fatalf("CreatePreAuthChallenge() error = %v", err)
	}
	start := make(chan struct{})
	errorsCh := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := manager.ConsumePreAuthChallenge(t.Context(), challenge.Token, "", challenge.Purpose, challenge.Origin)
			errorsCh <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsCh)
	successes, rejected := 0, 0
	for err := range errorsCh {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrPreAuthChallengeInvalid) {
			rejected++
		} else {
			t.Fatalf("ConsumePreAuthChallenge() error = %v", err)
		}
	}
	if successes != 1 || rejected != 1 {
		t.Fatalf("concurrent consumption = success:%d rejected:%d, want 1/1", successes, rejected)
	}
}

func TestPreAuthCleanupUsesBoundedBatches(t *testing.T) {
	now := time.Date(2026, time.August, 4, 13, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{})
	tx, err := manager.db.Write().BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin seed transaction: %v", err)
	}
	for i := range preAuthCleanupBatchSize + 1 {
		value := fmt.Sprintf("expired-challenge-%d", i)
		if _, err := tx.ExecContext(t.Context(), `
			INSERT INTO auth_challenges (
				id, challenge_hash, purpose, origin, attempts, max_attempts, created_at, expires_at
			) VALUES (?, ?, 'login', 'https://gofer.example', 0, 1, ?, ?)`,
			value, hashToken(value), now.Add(-time.Hour), now.Add(-time.Second)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert expired challenge %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit expired challenges: %v", err)
	}
	if err := manager.CleanupPreAuthChallenges(t.Context()); err != nil {
		t.Fatalf("CleanupPreAuthChallenges() error = %v", err)
	}
	var count int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_challenges`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("challenge count after cleanup = %d, %v; want 1", count, err)
	}
}

func TestPreAuthCookieContract(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "loopback HTTP", true: "remote HTTPS"}[secure], func(t *testing.T) {
			recorder := httptest.NewRecorder()
			SetPreAuthCookie(recorder, "pre-auth-token", secure, 10*time.Minute)
			cookies := recorder.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("SetPreAuthCookie() cookies = %#v", cookies)
			}
			cookie := cookies[0]
			if cookie.Name != preAuthCookieName || cookie.Value != "pre-auth-token" || cookie.Path != "/" || cookie.MaxAge != 600 || !cookie.HttpOnly || cookie.Secure != secure || cookie.SameSite != http.SameSiteLaxMode {
				t.Fatalf("pre-auth cookie = %#v", cookie)
			}
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.AddCookie(cookie)
			if token := GetPreAuthToken(request); token != "pre-auth-token" {
				t.Fatalf("GetPreAuthToken() = %q", token)
			}

			recorder = httptest.NewRecorder()
			ClearPreAuthCookie(recorder, secure)
			cleared := recorder.Result().Cookies()[0]
			if cleared.Name != preAuthCookieName || cleared.MaxAge != -1 || cleared.Secure != secure {
				t.Fatalf("cleared pre-auth cookie = %#v", cleared)
			}
		})
	}
	if !PreAuthTokensMatch("pre-auth-token", "pre-auth-token") || PreAuthTokensMatch("pre-auth-token", "different-token") || PreAuthTokensMatch("", "") {
		t.Fatal("PreAuthTokensMatch() did not enforce exact non-empty matching")
	}
}
