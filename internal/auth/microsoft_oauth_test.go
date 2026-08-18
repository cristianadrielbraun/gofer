package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
)

const microsoftTestTenantID = "11111111-2222-3333-4444-555555555555"

type microsoftIDTokenVerifierFunc func(context.Context, string) (*MicrosoftIDTokenClaims, error)

func (verify microsoftIDTokenVerifierFunc) Verify(ctx context.Context, rawIDToken string) (*MicrosoftIDTokenClaims, error) {
	return verify(ctx, rawIDToken)
}

func configureMicrosoftOAuthTest(manager *Manager) {
	manager.config.MicrosoftLoginTenant = "common"
	manager.config.MicrosoftLoginClient = &oauth2.Config{
		ClientID: "microsoft-login-client",
		Endpoint: oauth2.Endpoint{
			AuthURL: "https://login.example/authorize", TokenURL: "https://login.example/token",
		},
	}
}

func microsoftOAuthTestContext(t *testing.T) context.Context {
	t.Helper()
	client := &http.Client{Transport: oauthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"access_token":"access","token_type":"Bearer","id_token":"signed-id-token"}`)),
			Request:    request,
		}, nil
	})}
	return context.WithValue(t.Context(), oauth2.HTTPClient, client)
}

func TestLoadConfigSeparatesMicrosoftLoginFromOutlookMailboxOAuth(t *testing.T) {
	t.Setenv("GOFER_AUTH_ENABLED", "true")
	t.Setenv("MICROSOFT_OAUTH_CLIENT_ID", "outlook-mailbox-client")
	t.Setenv("MICROSOFT_OAUTH_CLIENT_SECRET", "outlook-mailbox-secret")
	t.Setenv("GOFER_MICROSOFT_LOGIN_CLIENT_ID", "application-login-client")
	t.Setenv("GOFER_MICROSOFT_LOGIN_CLIENT_SECRET", "application-login-secret")
	t.Setenv("GOFER_MICROSOFT_LOGIN_TENANT", "organizations")

	cfg := LoadConfig("https://gofer.example")
	if cfg.MicrosoftLoginClient == nil || cfg.MicrosoftLoginTenant != "organizations" {
		t.Fatalf("Microsoft application-login config = %#v", cfg)
	}
	if cfg.MicrosoftLoginClient.ClientID != "application-login-client" || cfg.MicrosoftLoginClient.ClientSecret != "application-login-secret" {
		t.Fatalf("Microsoft application-login client = %#v", cfg.MicrosoftLoginClient)
	}
	if cfg.MicrosoftLoginClient.RedirectURL != "https://gofer.example/auth/microsoft/callback" {
		t.Fatalf("Microsoft application-login redirect = %q", cfg.MicrosoftLoginClient.RedirectURL)
	}
	wantScopes := []string{"openid", "profile", "email"}
	if !slices.Equal(cfg.MicrosoftLoginClient.Scopes, wantScopes) {
		t.Fatalf("Microsoft application-login scopes = %#v, want %#v", cfg.MicrosoftLoginClient.Scopes, wantScopes)
	}
	for _, scope := range cfg.MicrosoftLoginClient.Scopes {
		if !slices.Contains(wantScopes, scope) {
			t.Fatalf("Microsoft application login requested resource/offline scope %q", scope)
		}
	}
}

func TestMicrosoftApplicationLoginAuthorizationUsesPKCEStateNonceAndIdentityOnlyScopes(t *testing.T) {
	manager := newDeterministicManager(t, &fixedClock{now: time.Date(2026, time.August, 18, 4, 0, 0, 0, time.UTC)}, &deterministicTokenGenerator{
		ids: []string{"microsoft-challenge"}, tokens: []string{"state-value", "nonce-value"},
	})
	configureMicrosoftOAuthTest(manager)
	manager.config.MicrosoftLoginClient.Scopes = []string{
		"openid", "profile", "email", "offline_access", "https://graph.microsoft.com/Mail.Read",
	}

	start, err := manager.BeginMicrosoftLogin(t.Context())
	if err != nil {
		t.Fatalf("BeginMicrosoftLogin() error = %v", err)
	}
	parsed, err := url.Parse(start.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("state") != "state-value" || query.Get("nonce") != "nonce-value" || query.Get("scope") != "openid profile email" {
		t.Fatalf("Microsoft authorization query = %q", query.Encode())
	}
	if query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("Microsoft authorization omitted S256 PKCE: %q", query.Encode())
	}
	challenge, draft, _, err := manager.currentMicrosoftAuthorizationChallenge(
		t.Context(), manager.db.Read(), start.Challenge.Token, ChallengePurposeFederatedLogin,
	)
	if err != nil {
		t.Fatal(err)
	}
	if query.Get("code_challenge") != oauth2.S256ChallengeFromVerifier(draft.CodeVerifier) {
		t.Fatal("Microsoft PKCE challenge does not match the encrypted verifier")
	}
	if strings.Contains(string(challenge.PayloadCiphertext), draft.CodeVerifier) || strings.Contains(string(challenge.PayloadCiphertext), "code_verifier") {
		t.Fatalf("Microsoft challenge persisted plaintext PKCE data: %q", challenge.PayloadCiphertext)
	}
}

func TestMaintainedMicrosoftIDTokenVerifierRejectsInvalidStandardAndTenantClaims(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer := microsoftTenantIssuer(microsoftTestTenantID)
	verifier := &maintainedMicrosoftIDTokenVerifier{
		verifier: oidc.NewVerifier(
			issuer, &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}},
			&oidc.Config{ClientID: "microsoft-login-client"},
		),
		expectedIssuer: issuer, expectedTenant: microsoftTestTenantID,
	}
	now := time.Now().UTC()
	baseClaims := jwt.MapClaims{
		"iss": issuer, "aud": "microsoft-login-client", "sub": "microsoft-subject", "tid": microsoftTestTenantID,
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(5 * time.Minute).Unix(), "nonce": "nonce-value",
		"email": "person@outlook.example", "preferred_username": "mutable-login@example.com", "name": "Person",
	}
	tests := []struct {
		name   string
		mutate func(jwt.MapClaims)
		key    *rsa.PrivateKey
		valid  bool
	}{
		{name: "valid", key: key, valid: true},
		{name: "wrong signature", key: otherKey},
		{name: "wrong issuer", key: key, mutate: func(claims jwt.MapClaims) {
			claims["iss"] = microsoftTenantIssuer("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
		}},
		{name: "wrong audience", key: key, mutate: func(claims jwt.MapClaims) { claims["aud"] = "other-client" }},
		{name: "expired", key: key, mutate: func(claims jwt.MapClaims) { claims["exp"] = now.Add(-time.Minute).Unix() }},
		{name: "tenant issuer mismatch", key: key, mutate: func(claims jwt.MapClaims) { claims["tid"] = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := jwt.MapClaims{}
			for name, value := range baseClaims {
				claims[name] = value
			}
			if test.mutate != nil {
				test.mutate(claims)
			}
			raw, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(test.key)
			if err != nil {
				t.Fatal(err)
			}
			got, err := verifier.Verify(t.Context(), raw)
			if test.valid {
				if err != nil || got == nil || got.Subject != "microsoft-subject" || got.Nonce != "nonce-value" || got.DisplayEmail() != "person@outlook.example" {
					t.Fatalf("Verify(valid) = %#v, %v", got, err)
				}
				return
			}
			if err == nil || got != nil {
				t.Fatalf("Verify(invalid) = %#v, %v", got, err)
			}
		})
	}
}

func TestMicrosoftTenantAudiencePolicy(t *testing.T) {
	for _, test := range []struct {
		name, configured, allowed, token string
		want                             bool
	}{
		{name: "common organization", configured: "common", token: microsoftTestTenantID, want: true},
		{name: "common consumer", configured: "common", token: microsoftLoginConsumerTenantID, want: true},
		{name: "organizations organization", configured: "organizations", token: microsoftTestTenantID, want: true},
		{name: "organizations consumer", configured: "organizations", token: microsoftLoginConsumerTenantID},
		{name: "consumers consumer", configured: "consumers", token: microsoftLoginConsumerTenantID, want: true},
		{name: "consumers organization", configured: "consumers", token: microsoftTestTenantID},
		{name: "exact tenant", configured: microsoftTestTenantID, allowed: microsoftTestTenantID, token: microsoftTestTenantID, want: true},
		{name: "different exact tenant", configured: microsoftTestTenantID, allowed: microsoftTestTenantID, token: microsoftLoginConsumerTenantID},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := microsoftLoginTenantAllows(test.configured, test.allowed, test.token); got != test.want {
				t.Fatalf("microsoftLoginTenantAllows() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestMicrosoftApplicationLoginUsesExactSubjectWithoutCreatingOutlookMailbox(t *testing.T) {
	now := time.Date(2026, time.August, 18, 4, 15, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"challenge-id", "session-id"}, tokens: []string{"state-token", "nonce-token", "session-token"},
	})
	configureMicrosoftOAuthTest(manager)
	insertActiveUser(t, manager, "person", false, now)
	issuer := microsoftTenantIssuer(microsoftTestTenantID)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_identities (
			id, user_id, provider, issuer, subject, email, email_verified, created_at, linked_at
		) VALUES ('microsoft-identity', 'person', ?, ?, 'microsoft-subject', 'old@example.com', 0, ?, ?)`,
		microsoftIdentityProvider, issuer, now, now,
	); err != nil {
		t.Fatal(err)
	}
	start, err := manager.BeginMicrosoftLogin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	manager.microsoftIDTokenVerifier = microsoftIDTokenVerifierFunc(func(context.Context, string) (*MicrosoftIDTokenClaims, error) {
		return &MicrosoftIDTokenClaims{
			Issuer: issuer, Subject: "microsoft-subject", TenantID: microsoftTestTenantID,
			Nonce: start.Challenge.Nonce, Email: "new@outlook.example", Name: "Provider Name",
		}, nil
	})
	user, result, err := manager.HandleMicrosoftCallback(
		microsoftOAuthTestContext(t), start.Challenge.Token, "authorization-code", "Microsoft Browser",
	)
	if err != nil || user == nil || user.ID != "person" || result == nil || result.Session == nil ||
		result.Session.AuthenticationMethod != AuthenticationMethodFederatedMicrosoft {
		t.Fatalf("HandleMicrosoftCallback() = user:%#v result:%#v error:%v", user, result, err)
	}
	for table, want := range map[string]int{"accounts": 0, "oauth_accounts": 0, "sessions": 1} {
		var count int
		if err := manager.db.Read().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s rows = %d, %v; want %d", table, count, err, want)
		}
	}
	var email string
	var verified int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT email, email_verified FROM auth_identities WHERE id = 'microsoft-identity'`,
	).Scan(&email, &verified); err != nil {
		t.Fatal(err)
	}
	if email != "new@outlook.example" || verified != 0 {
		t.Fatalf("Microsoft display metadata = email:%q verified:%d", email, verified)
	}
}

func TestMicrosoftApplicationLoginRejectsUnknownIdentityEvenWhenDisplayEmailMatches(t *testing.T) {
	now := time.Date(2026, time.August, 18, 4, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"challenge-id"}, tokens: []string{"state-token", "nonce-token"},
	})
	configureMicrosoftOAuthTest(manager)
	insertActiveUser(t, manager, "person", false, now)
	start, err := manager.BeginMicrosoftLogin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	manager.microsoftIDTokenVerifier = microsoftIDTokenVerifierFunc(func(context.Context, string) (*MicrosoftIDTokenClaims, error) {
		return &MicrosoftIDTokenClaims{
			Issuer: microsoftTenantIssuer(microsoftTestTenantID), Subject: "unlinked-subject",
			TenantID: microsoftTestTenantID, Nonce: start.Challenge.Nonce, Email: "person@example.com",
		}, nil
	})
	_, _, err = manager.HandleMicrosoftCallback(
		microsoftOAuthTestContext(t), start.Challenge.Token, "authorization-code", "Microsoft Browser",
	)
	if got := FederatedLoginReason(err); got != FederatedLoginFailureIdentityUnknown {
		t.Fatalf("failure reason = %q, want %q (error %v)", got, FederatedLoginFailureIdentityUnknown, err)
	}
	var identities, sessions int
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_identities`).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if identities != 0 || sessions != 0 {
		t.Fatalf("closed-registration state = identities:%d sessions:%d", identities, sessions)
	}
}

func TestMicrosoftApplicationLoginRejectsInvalidVerifiedClaimsAndConsumesChallenge(t *testing.T) {
	issuer := microsoftTenantIssuer(microsoftTestTenantID)
	for _, test := range []struct {
		name       string
		claims     *MicrosoftIDTokenClaims
		verifyErr  error
		wantReason FederatedLoginFailureReason
	}{
		{name: "invalid ID token", verifyErr: errors.New("signature rejected"), wantReason: FederatedLoginFailureIDTokenInvalid},
		{name: "wrong nonce", claims: &MicrosoftIDTokenClaims{Issuer: issuer, Subject: "subject", TenantID: microsoftTestTenantID, Nonce: "wrong"}, wantReason: FederatedLoginFailureNonceInvalid},
		{name: "missing subject", claims: &MicrosoftIDTokenClaims{Issuer: issuer, TenantID: microsoftTestTenantID, Nonce: "nonce-value"}, wantReason: FederatedLoginFailureSubjectInvalid},
		{name: "missing tenant", claims: &MicrosoftIDTokenClaims{Issuer: issuer, Subject: "subject", Nonce: "nonce-value"}, wantReason: FederatedLoginFailureSubjectInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, time.August, 18, 4, 45, 0, 0, time.UTC)
			manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
				ids: []string{"challenge-id"}, tokens: []string{"state-value", "nonce-value"},
			})
			configureMicrosoftOAuthTest(manager)
			manager.microsoftIDTokenVerifier = microsoftIDTokenVerifierFunc(func(context.Context, string) (*MicrosoftIDTokenClaims, error) {
				return test.claims, test.verifyErr
			})
			start, err := manager.BeginMicrosoftLogin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = manager.HandleMicrosoftCallback(
				microsoftOAuthTestContext(t), start.Challenge.Token, "authorization-code", "Browser",
			)
			if got := FederatedLoginReason(err); got != test.wantReason {
				t.Fatalf("failure reason = %q, want %q (error %v)", got, test.wantReason, err)
			}
			var attempts int
			var consumed bool
			var payload []byte
			if err := manager.db.Read().QueryRow(`
				SELECT attempts, consumed_at IS NOT NULL, payload_ciphertext FROM auth_challenges WHERE id = ?`,
				start.Challenge.ID,
			).Scan(&attempts, &consumed, &payload); err != nil {
				t.Fatal(err)
			}
			if attempts != 1 || !consumed || payload != nil {
				t.Fatalf("rejected challenge = attempts:%d consumed:%t payload:%x", attempts, consumed, payload)
			}
		})
	}
}

func prepareMicrosoftIdentityLink(t *testing.T, ids []string, tokens []string) (*Manager, string, time.Time) {
	t.Helper()
	now := time.Date(2026, time.August, 18, 5, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{ids: ids, tokens: tokens})
	insertActiveUser(t, manager, "person", false, now)
	insertGoogleLinkSession(t, manager, "person", "person-session", "person-session-token", now, now)
	configureMicrosoftOAuthTest(manager)
	return manager, "person-session-token", now
}

func TestMicrosoftIdentityLinkAndUnlinkUseExactSubjectAndPreserveMailboxBoundary(t *testing.T) {
	manager, sessionToken, now := prepareMicrosoftIdentityLink(
		t,
		[]string{"challenge-id", "identity-id", "link-event", "rotated-session", "unlink-event"},
		[]string{"link-state", "link-nonce", "rotated-session-token"},
	)
	insertFederatedIdentityTestPassword(t, manager, "person", now)
	start, err := manager.BeginMicrosoftIdentityLink(t.Context(), sessionToken)
	if err != nil {
		t.Fatal(err)
	}
	issuer := microsoftTenantIssuer(microsoftTestTenantID)
	manager.microsoftIDTokenVerifier = microsoftIDTokenVerifierFunc(func(context.Context, string) (*MicrosoftIDTokenClaims, error) {
		return &MicrosoftIDTokenClaims{
			Issuer: issuer, Subject: "microsoft-subject", TenantID: microsoftTestTenantID,
			Nonce: start.Challenge.Nonce, PreferredUsername: "person@microsoft.example",
		}, nil
	})
	identity, err := manager.CompleteMicrosoftIdentityLink(
		microsoftOAuthTestContext(t), start.Challenge.Token, sessionToken, "authorization-code", "Link Browser",
	)
	if err != nil || identity == nil || identity.ID != "identity-id" || identity.Provider != microsoftIdentityProvider ||
		identity.Issuer != issuer || identity.Email != "person@microsoft.example" || identity.EmailVerified {
		t.Fatalf("CompleteMicrosoftIdentityLink() = %#v, %v", identity, err)
	}
	var metadata string
	if err := manager.db.Read().QueryRow(`SELECT metadata_json FROM auth_events WHERE id = 'link-event'`).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	if metadata != `{"provider":"microsoft","result":"linked"}` || strings.Contains(metadata, "microsoft-subject") || strings.Contains(metadata, "person@microsoft.example") {
		t.Fatalf("Microsoft identity-link audit metadata = %q", metadata)
	}
	rotated, err := manager.UnlinkMicrosoftIdentity(t.Context(), sessionToken, identity.ID, "Unlink Browser")
	if err != nil || rotated == nil || rotated.Token != "rotated-session-token" {
		t.Fatalf("UnlinkMicrosoftIdentity() = %#v, %v", rotated, err)
	}
	for table, want := range map[string]int{"auth_identities": 0, "accounts": 0, "oauth_accounts": 0} {
		var count int
		if err := manager.db.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s rows after unlink = %d, %v; want %d", table, count, err, want)
		}
	}
}

func TestMicrosoftIdentityLinkRejectsCrossUserSubjectConflict(t *testing.T) {
	manager, sessionToken, now := prepareMicrosoftIdentityLink(
		t,
		[]string{"challenge-id", "unused-identity", "conflict-event"},
		[]string{"link-state", "link-nonce"},
	)
	insertActiveUser(t, manager, "owner", false, now)
	issuer := microsoftTenantIssuer(microsoftTestTenantID)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_identities (
			id, user_id, provider, issuer, subject, email, email_verified, created_at, linked_at
		) VALUES ('owner-identity', 'owner', ?, ?, 'shared-subject', 'owner@example.com', 0, ?, ?)`,
		microsoftIdentityProvider, issuer, now, now,
	); err != nil {
		t.Fatal(err)
	}
	start, err := manager.BeginMicrosoftIdentityLink(t.Context(), sessionToken)
	if err != nil {
		t.Fatal(err)
	}
	manager.microsoftIDTokenVerifier = microsoftIDTokenVerifierFunc(func(context.Context, string) (*MicrosoftIDTokenClaims, error) {
		return &MicrosoftIDTokenClaims{
			Issuer: issuer, Subject: "shared-subject", TenantID: microsoftTestTenantID,
			Nonce: start.Challenge.Nonce, Email: "changed@example.com",
		}, nil
	})
	identity, err := manager.CompleteMicrosoftIdentityLink(
		microsoftOAuthTestContext(t), start.Challenge.Token, sessionToken, "authorization-code", "Link Browser",
	)
	if identity != nil || !errors.Is(err, ErrFederatedIdentityConflict) {
		t.Fatalf("CompleteMicrosoftIdentityLink(conflict) = %#v, %v", identity, err)
	}
	var ownerID, email string
	if err := manager.db.Read().QueryRow(`
		SELECT user_id, email FROM auth_identities WHERE id = 'owner-identity'`,
	).Scan(&ownerID, &email); err != nil {
		t.Fatal(err)
	}
	if ownerID != "owner" || email != "owner@example.com" {
		t.Fatalf("conflicting Microsoft identity changed = owner:%q email:%q", ownerID, email)
	}
	var success int
	var metadata string
	if err := manager.db.Read().QueryRow(`
		SELECT success, metadata_json FROM auth_events WHERE id = 'conflict-event'`,
	).Scan(&success, &metadata); err != nil {
		t.Fatal(err)
	}
	if success != 0 || metadata != `{"provider":"microsoft","result":"conflict"}` || strings.Contains(metadata, "shared-subject") {
		t.Fatalf("Microsoft conflict audit = success:%d metadata:%q", success, metadata)
	}
}

func TestMicrosoftIdentityCountsAsPrimaryOnlyWhileProviderConfigured(t *testing.T) {
	manager, _, now := prepareMicrosoftIdentityLink(t, nil, nil)
	issuer := microsoftTenantIssuer(microsoftTestTenantID)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_identities (
			id, user_id, provider, issuer, subject, email, email_verified, created_at, linked_at
		) VALUES ('only-microsoft', 'person', ?, ?, 'subject', 'person@example.com', 0, ?, ?)`,
		microsoftIdentityProvider, issuer, now, now,
	); err != nil {
		t.Fatal(err)
	}
	identities, err := manager.ListFederatedIdentities(t.Context(), "person")
	if err != nil || len(identities) != 1 || identities[0].CanUnlink || identities[0].UnlinkReason == "" {
		t.Fatalf("last Microsoft identity summary = %#v, %v", identities, err)
	}
	manager.config.MicrosoftLoginClient = nil
	insertFederatedIdentityTestPassword(t, manager, "person", now)
	identities, err = manager.ListFederatedIdentities(t.Context(), "person")
	if err != nil || len(identities) != 1 || !identities[0].CanUnlink {
		t.Fatalf("Microsoft identity with local password summary = %#v, %v", identities, err)
	}
}
