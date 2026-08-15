package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func insertGoogleLinkSession(t *testing.T, manager *Manager, userID, sessionID, rawToken string, stepUpAt, now time.Time) {
	t.Helper()
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method,
			assurance_level, user_agent, authenticated_at, last_used_at,
			idle_expires_at, absolute_expires_at, step_up_at, step_up_method, created_at
		) VALUES (?, ?, ?, 1, 'password', 'multi_factor', 'Security Browser', ?, ?, ?, ?, ?, 'password', ?)`,
		sessionID, userID, hashToken(rawToken), now, now,
		now.Add(time.Hour), now.Add(24*time.Hour), stepUpAt, now,
	); err != nil {
		t.Fatalf("insert Google-link session: %v", err)
	}
}

func prepareGoogleIdentityLink(
	t *testing.T,
	ids []string,
	stepUpAt time.Time,
) (*Manager, *fixedClock, string) {
	t.Helper()
	now := time.Date(2026, time.August, 15, 10, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{
		ids: ids,
		tokens: []string{
			"link-state", "link-nonce", "replacement-state", "replacement-nonce",
			"final-state", "final-nonce",
		},
	})
	insertActiveUser(t, manager, "person", false, now)
	insertGoogleLinkSession(t, manager, "person", "person-session", "person-session-token", stepUpAt, now)
	configureGoogleOAuthTest(manager)
	return manager, clock, "person-session-token"
}

func TestBeginGoogleIdentityLinkRequiresRecentStepUpAndBindsChallengeToSession(t *testing.T) {
	now := time.Date(2026, time.August, 15, 10, 0, 0, 0, time.UTC)
	manager, _, sessionToken := prepareGoogleIdentityLink(
		t, []string{"rejected-challenge-id", "challenge-id", "replacement-challenge-id"}, now.Add(-time.Hour),
	)

	if start, err := manager.BeginGoogleIdentityLink(t.Context(), sessionToken); !errors.Is(err, ErrRecentStepUpRequired) || start != nil {
		t.Fatalf("BeginGoogleIdentityLink(stale) = %#v, %v", start, err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE sessions SET step_up_at = ? WHERE id = 'person-session'`, now,
	); err != nil {
		t.Fatalf("refresh session step-up: %v", err)
	}
	start, err := manager.BeginGoogleIdentityLink(t.Context(), sessionToken)
	if err != nil {
		t.Fatalf("BeginGoogleIdentityLink(fresh) error = %v", err)
	}
	challenge, draft, _, err := manager.currentGoogleAuthorizationChallenge(
		t.Context(), manager.db.Read(), start.Challenge.Token, ChallengePurposeFederatedLink,
	)
	if err != nil {
		t.Fatalf("load Google identity-link challenge: %v", err)
	}
	if challenge.UserID != "person" || challenge.SessionID != "person-session" || challenge.Purpose != ChallengePurposeFederatedLink {
		t.Fatalf("linked challenge binding = %#v", challenge)
	}
	if !validGoogleLoginDraft(draft) || strings.Contains(string(challenge.PayloadCiphertext), draft.CodeVerifier) {
		t.Fatal("Google identity-link PKCE verifier was missing or stored in plaintext")
	}
	replacement, err := manager.BeginGoogleIdentityLink(t.Context(), sessionToken)
	if err != nil {
		t.Fatalf("BeginGoogleIdentityLink(replacement) error = %v", err)
	}
	if replacement.Challenge.Token == start.Challenge.Token {
		t.Fatal("replacement Google identity-link challenge reused state")
	}
	var total, active, consumed int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*),
		       SUM(CASE WHEN consumed_at IS NULL THEN 1 ELSE 0 END),
		       SUM(CASE WHEN consumed_at IS NOT NULL THEN 1 ELSE 0 END)
		FROM auth_challenges WHERE purpose = ?`, ChallengePurposeFederatedLink,
	).Scan(&total, &active, &consumed); err != nil {
		t.Fatalf("read replacement Google identity-link challenges: %v", err)
	}
	if total != 2 || active != 1 || consumed != 1 {
		t.Fatalf("replacement Google identity-link challenges = total:%d active:%d consumed:%d", total, active, consumed)
	}
}

func TestCompleteGoogleIdentityLinkPersistsExactIdentityAndRedactedAuditEvent(t *testing.T) {
	now := time.Date(2026, time.August, 15, 10, 0, 0, 0, time.UTC)
	manager, _, sessionToken := prepareGoogleIdentityLink(
		t, []string{"challenge-id", "identity-id", "event-id"}, now,
	)
	start, err := manager.BeginGoogleIdentityLink(t.Context(), sessionToken)
	if err != nil {
		t.Fatalf("BeginGoogleIdentityLink() error = %v", err)
	}
	manager.googleIDTokenVerifier = googleIDTokenVerifierFunc(func(context.Context, string) (*GoogleIDTokenClaims, error) {
		return &GoogleIDTokenClaims{
			Subject: "google-subject", Nonce: start.Challenge.Nonce,
			Email: "person@gmail.example", EmailVerified: true,
		}, nil
	})

	identity, err := manager.CompleteGoogleIdentityLink(
		googleOAuthTestContext(t), start.Challenge.Token, sessionToken, "authorization-code", "Link Browser",
	)
	if err != nil || identity == nil {
		t.Fatalf("CompleteGoogleIdentityLink() = %#v, %v", identity, err)
	}
	if identity.ID != "identity-id" || identity.Provider != googleIdentityProvider ||
		identity.Issuer != googleLoginIssuer || identity.Email != "person@gmail.example" || !identity.EmailVerified {
		t.Fatalf("linked identity = %#v", identity)
	}
	identities, err := manager.ListFederatedIdentities(t.Context(), "person")
	if err != nil || len(identities) != 1 || identities[0].ID != identity.ID {
		t.Fatalf("ListFederatedIdentities() = %#v, %v", identities, err)
	}
	var eventType AuthEventType
	var success int
	var metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT event_type, success, metadata_json FROM auth_events WHERE id = 'event-id'`,
	).Scan(&eventType, &success, &metadata); err != nil {
		t.Fatalf("read Google identity-link event: %v", err)
	}
	if eventType != AuthEventIdentityLinked || success != 1 || metadata != `{"provider":"google","result":"linked"}` {
		t.Fatalf("identity-link event = type:%q success:%d metadata:%q", eventType, success, metadata)
	}
	if strings.Contains(metadata, "google-subject") || strings.Contains(metadata, "person@gmail.example") {
		t.Fatalf("identity-link event leaked identity data: %q", metadata)
	}
	var consumed bool
	var payload []byte
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT consumed_at IS NOT NULL, payload_ciphertext FROM auth_challenges WHERE id = 'challenge-id'`,
	).Scan(&consumed, &payload); err != nil {
		t.Fatalf("read consumed identity-link challenge: %v", err)
	}
	if !consumed || payload != nil {
		t.Fatalf("identity-link challenge = consumed:%t payload:%x", consumed, payload)
	}
}

func TestCompleteGoogleIdentityLinkCannotReassignIdentityFromAnotherUser(t *testing.T) {
	now := time.Date(2026, time.August, 15, 10, 0, 0, 0, time.UTC)
	manager, _, sessionToken := prepareGoogleIdentityLink(
		t, []string{"challenge-id", "unused-identity-id", "event-id"}, now,
	)
	insertActiveUser(t, manager, "owner", false, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_identities (
			id, user_id, provider, issuer, subject, email, email_verified, created_at, linked_at
		) VALUES ('owner-identity', 'owner', ?, ?, 'google-subject', 'owner@example.com', 1, ?, ?)`,
		googleIdentityProvider, googleLoginIssuer, now, now,
	); err != nil {
		t.Fatalf("insert owner identity: %v", err)
	}
	start, err := manager.BeginGoogleIdentityLink(t.Context(), sessionToken)
	if err != nil {
		t.Fatalf("BeginGoogleIdentityLink() error = %v", err)
	}
	manager.googleIDTokenVerifier = googleIDTokenVerifierFunc(func(context.Context, string) (*GoogleIDTokenClaims, error) {
		return &GoogleIDTokenClaims{
			Subject: "google-subject", Nonce: start.Challenge.Nonce,
			Email: "attacker-controlled@example.com", EmailVerified: true,
		}, nil
	})

	identity, err := manager.CompleteGoogleIdentityLink(
		googleOAuthTestContext(t), start.Challenge.Token, sessionToken, "authorization-code", "Link Browser",
	)
	if !errors.Is(err, ErrFederatedIdentityConflict) || identity != nil {
		t.Fatalf("CompleteGoogleIdentityLink(conflict) = %#v, %v", identity, err)
	}
	var ownerID, email string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT user_id, email FROM auth_identities WHERE id = 'owner-identity'`,
	).Scan(&ownerID, &email); err != nil {
		t.Fatalf("read conflicting identity: %v", err)
	}
	if ownerID != "owner" || email != "owner@example.com" {
		t.Fatalf("conflicting identity changed = owner:%q email:%q", ownerID, email)
	}
	var success int
	var metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT success, metadata_json FROM auth_events WHERE id = 'event-id'`,
	).Scan(&success, &metadata); err != nil {
		t.Fatalf("read conflict event: %v", err)
	}
	if success != 0 || metadata != `{"provider":"google","result":"conflict"}` {
		t.Fatalf("conflict event = success:%d metadata:%q", success, metadata)
	}
}

func TestCompleteGoogleIdentityLinkIsIdempotentForSameUser(t *testing.T) {
	now := time.Date(2026, time.August, 15, 10, 0, 0, 0, time.UTC)
	manager, _, sessionToken := prepareGoogleIdentityLink(
		t, []string{"challenge-id", "unused-identity-id", "event-id"}, now,
	)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_identities (
			id, user_id, provider, issuer, subject, email, email_verified, created_at, linked_at
		) VALUES ('existing-identity', 'person', ?, ?, 'google-subject',
		          'old-address@example.com', 1, ?, ?)`,
		googleIdentityProvider, googleLoginIssuer, now.Add(-time.Hour), now.Add(-time.Hour),
	); err != nil {
		t.Fatalf("insert existing same-user identity: %v", err)
	}
	start, err := manager.BeginGoogleIdentityLink(t.Context(), sessionToken)
	if err != nil {
		t.Fatalf("BeginGoogleIdentityLink() error = %v", err)
	}
	manager.googleIDTokenVerifier = googleIDTokenVerifierFunc(func(context.Context, string) (*GoogleIDTokenClaims, error) {
		return &GoogleIDTokenClaims{
			Subject: "google-subject", Nonce: start.Challenge.Nonce,
			Email: "current-address@example.com", EmailVerified: true,
		}, nil
	})

	identity, err := manager.CompleteGoogleIdentityLink(
		googleOAuthTestContext(t), start.Challenge.Token, sessionToken, "authorization-code", "Link Browser",
	)
	if err != nil || identity == nil || identity.ID != "existing-identity" || identity.Email != "current-address@example.com" {
		t.Fatalf("CompleteGoogleIdentityLink(existing) = %#v, %v", identity, err)
	}
	var identityCount int
	var metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_identities`).Scan(&identityCount); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT metadata_json FROM auth_events WHERE id = 'event-id'`,
	).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	if identityCount != 1 || metadata != `{"provider":"google","result":"already_linked"}` {
		t.Fatalf("same-user relink = identities:%d metadata:%q", identityCount, metadata)
	}
}

func TestCompleteGoogleIdentityLinkRollsBackIdentityWhenAuditWriteFails(t *testing.T) {
	now := time.Date(2026, time.August, 15, 10, 0, 0, 0, time.UTC)
	manager, _, sessionToken := prepareGoogleIdentityLink(
		t, []string{"challenge-id", "identity-id", "event-id"}, now,
	)
	start, err := manager.BeginGoogleIdentityLink(t.Context(), sessionToken)
	if err != nil {
		t.Fatalf("BeginGoogleIdentityLink() error = %v", err)
	}
	manager.googleIDTokenVerifier = googleIDTokenVerifierFunc(func(context.Context, string) (*GoogleIDTokenClaims, error) {
		return &GoogleIDTokenClaims{
			Subject: "google-subject", Nonce: start.Challenge.Nonce,
			Email: "person@example.com", EmailVerified: true,
		}, nil
	})
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_google_identity_link_event
		BEFORE INSERT ON auth_events
		WHEN NEW.id = 'event-id'
		BEGIN
			SELECT RAISE(ABORT, 'forced audit failure');
		END`); err != nil {
		t.Fatalf("create audit failure trigger: %v", err)
	}

	identity, err := manager.CompleteGoogleIdentityLink(
		googleOAuthTestContext(t), start.Challenge.Token, sessionToken, "authorization-code", "Link Browser",
	)
	if err == nil || identity != nil {
		t.Fatalf("CompleteGoogleIdentityLink(audit failure) = %#v, %v", identity, err)
	}
	var identities, events int
	var consumed bool
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_identities`).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT consumed_at IS NOT NULL FROM auth_challenges WHERE id = 'challenge-id'`,
	).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if identities != 0 || events != 0 || !consumed {
		t.Fatalf("audit rollback = identities:%d events:%d challenge-consumed:%t", identities, events, consumed)
	}
}

func TestCompleteGoogleIdentityLinkRequiresOriginalFreshSession(t *testing.T) {
	now := time.Date(2026, time.August, 15, 10, 0, 0, 0, time.UTC)
	manager, _, sessionToken := prepareGoogleIdentityLink(
		t, []string{"challenge-id", "identity-id", "event-id"}, now,
	)
	insertActiveUser(t, manager, "other", false, now)
	insertGoogleLinkSession(t, manager, "other", "other-session", "other-session-token", now, now)
	start, err := manager.BeginGoogleIdentityLink(t.Context(), sessionToken)
	if err != nil {
		t.Fatalf("BeginGoogleIdentityLink() error = %v", err)
	}
	manager.googleIDTokenVerifier = googleIDTokenVerifierFunc(func(context.Context, string) (*GoogleIDTokenClaims, error) {
		return &GoogleIDTokenClaims{
			Subject: "google-subject", Nonce: start.Challenge.Nonce,
			Email: "person@example.com", EmailVerified: true,
		}, nil
	})

	if identity, err := manager.CompleteGoogleIdentityLink(
		googleOAuthTestContext(t), start.Challenge.Token, "other-session-token", "authorization-code", "Link Browser",
	); !errors.Is(err, ErrSecuritySessionInvalid) || identity != nil {
		t.Fatalf("CompleteGoogleIdentityLink(other session) = %#v, %v", identity, err)
	}
	var identityCount int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_identities`).Scan(&identityCount); err != nil {
		t.Fatal(err)
	}
	if identityCount != 0 {
		t.Fatalf("identity count after session mismatch = %d", identityCount)
	}

	manager2, _, sessionToken2 := prepareGoogleIdentityLink(
		t, []string{"challenge-id", "identity-id", "event-id"}, now,
	)
	start2, err := manager2.BeginGoogleIdentityLink(t.Context(), sessionToken2)
	if err != nil {
		t.Fatalf("BeginGoogleIdentityLink(second) error = %v", err)
	}
	manager2.googleIDTokenVerifier = googleIDTokenVerifierFunc(func(context.Context, string) (*GoogleIDTokenClaims, error) {
		return &GoogleIDTokenClaims{
			Subject: "google-subject", Nonce: start2.Challenge.Nonce,
			Email: "person@example.com", EmailVerified: true,
		}, nil
	})
	if _, err := manager2.db.Write().ExecContext(t.Context(), `
		UPDATE sessions SET step_up_at = ? WHERE id = 'person-session'`, now.Add(-time.Hour),
	); err != nil {
		t.Fatalf("make original session step-up stale: %v", err)
	}
	if identity, err := manager2.CompleteGoogleIdentityLink(
		googleOAuthTestContext(t), start2.Challenge.Token, sessionToken2, "authorization-code", "Link Browser",
	); !errors.Is(err, ErrRecentStepUpRequired) || identity != nil {
		t.Fatalf("CompleteGoogleIdentityLink(stale step-up) = %#v, %v", identity, err)
	}
}
