package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
)

const oidcTestIssuer = "https://identity.example/application"

type oidcIDTokenVerifierFunc func(context.Context, string) (*OIDCIDTokenClaims, error)

func (verify oidcIDTokenVerifierFunc) Verify(ctx context.Context, rawIDToken string) (*OIDCIDTokenClaims, error) {
	return verify(ctx, rawIDToken)
}

func configureOIDCTest(manager *Manager) {
	manager.config.OIDCLoginIssuer = oidcTestIssuer
	manager.config.OIDCLoginName = "Company SSO"
	manager.config.OIDCLoginClient = &oauth2.Config{
		ClientID: "oidc-login-client", ClientSecret: "oidc-login-secret",
		RedirectURL: "https://gofer.example/auth/oidc/callback",
		Endpoint: oauth2.Endpoint{
			AuthURL: "https://identity.example/authorize", TokenURL: "https://identity.example/token",
		},
	}
}

func oidcOAuthTestContext(t *testing.T) context.Context {
	t.Helper()
	client := &http.Client{Transport: oauthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:    io.NopCloser(strings.NewReader(`{"access_token":"access","token_type":"Bearer","id_token":"signed-id-token"}`)),
			Request: request,
		}, nil
	})}
	return context.WithValue(t.Context(), oauth2.HTTPClient, client)
}

func TestLoadConfigCreatesIdentityOnlyGenericOIDCLogin(t *testing.T) {
	t.Setenv("GOFER_AUTH_ENABLED", "true")
	t.Setenv("GOFER_OIDC_LOGIN_ISSUER", oidcTestIssuer)
	t.Setenv("GOFER_OIDC_LOGIN_CLIENT_ID", "application-login-client")
	t.Setenv("GOFER_OIDC_LOGIN_CLIENT_SECRET", "application-login-secret")
	t.Setenv("GOFER_OIDC_LOGIN_NAME", "Company SSO")

	cfg := LoadConfig("https://gofer.example")
	if cfg.OIDCLoginClient == nil || cfg.OIDCLoginIssuer != oidcTestIssuer || cfg.OIDCLoginName != "Company SSO" {
		t.Fatalf("OIDC application-login config = %#v", cfg)
	}
	if cfg.OIDCLoginClient.RedirectURL != "https://gofer.example/auth/oidc/callback" {
		t.Fatalf("OIDC redirect URL = %q", cfg.OIDCLoginClient.RedirectURL)
	}
	wantScopes := []string{"openid", "profile", "email"}
	if !slices.Equal(cfg.OIDCLoginClient.Scopes, wantScopes) {
		t.Fatalf("OIDC scopes = %#v, want %#v", cfg.OIDCLoginClient.Scopes, wantScopes)
	}
}

func TestLoadConfigRejectsUnsafeGenericOIDCIssuer(t *testing.T) {
	for _, issuer := range []string{
		"http://identity.example", "https://user@identity.example", "https://identity.example?tenant=one",
	} {
		t.Run(issuer, func(t *testing.T) {
			t.Setenv("GOFER_OIDC_LOGIN_ISSUER", issuer)
			t.Setenv("GOFER_OIDC_LOGIN_CLIENT_ID", "client")
			t.Setenv("GOFER_OIDC_LOGIN_CLIENT_SECRET", "secret")
			if cfg := LoadConfig("https://gofer.example"); cfg.OIDCLoginClient != nil {
				t.Fatalf("unsafe issuer enabled OIDC login: %#v", cfg)
			}
		})
	}
}

func TestGenericOIDCDiscoversAuthorizationAndTokenEndpoints(t *testing.T) {
	manager := newDeterministicManager(t, &fixedClock{now: time.Now()}, &deterministicTokenGenerator{})
	manager.config.OIDCLoginIssuer = oidcTestIssuer
	manager.config.OIDCLoginClient = &oauth2.Config{ClientID: "client", ClientSecret: "secret"}
	client := &http.Client{Transport: oauthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != oidcTestIssuer+"/.well-known/openid-configuration" {
			t.Fatalf("discovery URL = %q", request.URL.String())
		}
		body := `{"issuer":"` + oidcTestIssuer + `","authorization_endpoint":"https://identity.example/authorize","token_endpoint":"https://identity.example/token","jwks_uri":"https://identity.example/keys","subject_types_supported":["public"],"id_token_signing_alg_values_supported":["RS256"]}`
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(body)), Request: request,
		}, nil
	})}
	ctx := context.WithValue(t.Context(), oauth2.HTTPClient, client)
	config, err := manager.oidcOAuthClient(ctx)
	if err != nil {
		t.Fatalf("oidcOAuthClient() error = %v", err)
	}
	if config.Endpoint.AuthURL != "https://identity.example/authorize" || config.Endpoint.TokenURL != "https://identity.example/token" {
		t.Fatalf("discovered endpoints = %#v", config.Endpoint)
	}
	if !slices.Equal(config.Scopes, []string{"openid", "profile", "email"}) {
		t.Fatalf("discovered client scopes = %#v", config.Scopes)
	}
}

func TestGenericOIDCRejectsInsecureDiscoveredEndpoints(t *testing.T) {
	for _, test := range []struct {
		name         string
		authorizeURL string
		tokenURL     string
		keySetURL    string
	}{
		{
			name: "authorization endpoint", authorizeURL: "http://identity.example/authorize",
			tokenURL: "https://identity.example/token", keySetURL: "https://identity.example/keys",
		},
		{
			name: "token endpoint", authorizeURL: "https://identity.example/authorize",
			tokenURL: "http://identity.example/token", keySetURL: "https://identity.example/keys",
		},
		{
			name: "signing key endpoint", authorizeURL: "https://identity.example/authorize",
			tokenURL: "https://identity.example/token", keySetURL: "http://identity.example/keys",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := newDeterministicManager(t, &fixedClock{now: time.Now()}, &deterministicTokenGenerator{})
			manager.config.OIDCLoginIssuer = oidcTestIssuer
			manager.config.OIDCLoginClient = &oauth2.Config{ClientID: "client", ClientSecret: "secret"}
			client := &http.Client{Transport: oauthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				body := `{"issuer":"` + oidcTestIssuer + `","authorization_endpoint":"` + test.authorizeURL +
					`","token_endpoint":"` + test.tokenURL + `","jwks_uri":"` + test.keySetURL +
					`","subject_types_supported":["public"],"id_token_signing_alg_values_supported":["RS256"]}`
				return &http.Response{
					StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(body)), Request: request,
				}, nil
			})}
			ctx := context.WithValue(t.Context(), oauth2.HTTPClient, client)
			if config, err := manager.oidcOAuthClient(ctx); err == nil || config != nil {
				t.Fatalf("insecure discovered metadata accepted: %#v, %v", config, err)
			}
		})
	}
}

func TestGenericOIDCBlocksDiscoveryRedirectDowngrade(t *testing.T) {
	downgradedRequests := 0
	downgradeTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downgradedRequests++
		http.Error(w, "unexpected insecure discovery", http.StatusInternalServerError)
	}))
	defer downgradeTarget.Close()
	issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, downgradeTarget.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer issuer.Close()

	manager := newDeterministicManager(t, &fixedClock{now: time.Now()}, &deterministicTokenGenerator{})
	manager.config.OIDCLoginIssuer = issuer.URL
	manager.config.OIDCLoginClient = &oauth2.Config{ClientID: "client", ClientSecret: "secret"}
	ctx := context.WithValue(t.Context(), oauth2.HTTPClient, issuer.Client())
	if config, err := manager.oidcOAuthClient(ctx); err == nil || config != nil {
		t.Fatalf("discovery HTTPS downgrade accepted: %#v, %v", config, err)
	}
	if downgradedRequests != 0 {
		t.Fatalf("insecure discovery endpoint received %d requests", downgradedRequests)
	}
}

func TestGenericOIDCAuthorizationUsesPKCEStateNonceAndIdentityOnlyScopes(t *testing.T) {
	manager := newDeterministicManager(t, &fixedClock{now: time.Date(2026, time.August, 18, 6, 0, 0, 0, time.UTC)}, &deterministicTokenGenerator{
		ids: []string{"oidc-challenge"}, tokens: []string{"state-value", "nonce-value"},
	})
	configureOIDCTest(manager)
	manager.config.OIDCLoginClient.Scopes = []string{"openid", "profile", "email", "offline_access", "mail.read"}

	start, err := manager.BeginOIDCLogin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(start.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("state") != "state-value" || query.Get("nonce") != "nonce-value" || query.Get("scope") != "openid profile email" {
		t.Fatalf("OIDC authorization query = %q", query.Encode())
	}
	if query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("OIDC authorization omitted S256 PKCE: %q", query.Encode())
	}
	challenge, draft, _, err := manager.currentOIDCAuthorizationChallenge(
		t.Context(), manager.db.Read(), start.Challenge.Token, ChallengePurposeFederatedLogin,
	)
	if err != nil {
		t.Fatal(err)
	}
	if query.Get("code_challenge") != oauth2.S256ChallengeFromVerifier(draft.CodeVerifier) {
		t.Fatal("OIDC PKCE challenge does not match the encrypted verifier")
	}
	if strings.Contains(string(challenge.PayloadCiphertext), draft.CodeVerifier) || strings.Contains(string(challenge.PayloadCiphertext), "code_verifier") {
		t.Fatalf("OIDC challenge persisted plaintext PKCE data: %q", challenge.PayloadCiphertext)
	}
}

func TestMaintainedOIDCIDTokenVerifierRejectsInvalidStandardClaims(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &maintainedOIDCIDTokenVerifier{
		verifier: oidc.NewVerifier(
			oidcTestIssuer, &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}},
			&oidc.Config{ClientID: "oidc-login-client"},
		),
		expectedIssuer: oidcTestIssuer,
	}
	now := time.Now().UTC()
	baseClaims := jwt.MapClaims{
		"iss": oidcTestIssuer, "aud": "oidc-login-client", "sub": "stable-subject",
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(5 * time.Minute).Unix(), "nonce": "nonce-value",
		"email": "person@example.com", "email_verified": true, "preferred_username": "person", "name": "Person",
	}
	for _, test := range []struct {
		name   string
		mutate func(jwt.MapClaims)
		key    *rsa.PrivateKey
		valid  bool
	}{
		{name: "valid", key: key, valid: true},
		{name: "wrong signature", key: otherKey},
		{name: "wrong issuer", key: key, mutate: func(claims jwt.MapClaims) { claims["iss"] = "https://other.example" }},
		{name: "wrong audience", key: key, mutate: func(claims jwt.MapClaims) { claims["aud"] = "other-client" }},
		{name: "expired", key: key, mutate: func(claims jwt.MapClaims) { claims["exp"] = now.Add(-time.Minute).Unix() }},
		{name: "non ASCII subject", key: key, mutate: func(claims jwt.MapClaims) { claims["sub"] = "subject-ø" }},
	} {
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
				if err != nil || got == nil || got.Subject != "stable-subject" || !got.DisplayEmailVerified() {
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

func TestGenericOIDCLoginUsesExactIssuerSubjectWithoutCreatingMailbox(t *testing.T) {
	now := time.Date(2026, time.August, 18, 6, 15, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"challenge-id", "session-id"}, tokens: []string{"state-token", "nonce-token", "session-token"},
	})
	configureOIDCTest(manager)
	insertActiveUser(t, manager, "person", false, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_identities (
			id, user_id, provider, issuer, subject, email, email_verified, created_at, linked_at
		) VALUES ('oidc-identity', 'person', ?, ?, 'stable-subject', 'old@example.com', 0, ?, ?)`,
		oidcIdentityProvider, oidcTestIssuer, now, now,
	); err != nil {
		t.Fatal(err)
	}
	start, err := manager.BeginOIDCLogin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	manager.oidcIDTokenVerifier = oidcIDTokenVerifierFunc(func(context.Context, string) (*OIDCIDTokenClaims, error) {
		return &OIDCIDTokenClaims{
			Issuer: oidcTestIssuer, Subject: "stable-subject", Nonce: start.Challenge.Nonce,
			Email: "new@example.com", EmailVerified: true, Name: "Provider Name",
		}, nil
	})
	user, result, err := manager.HandleOIDCCallback(
		oidcOAuthTestContext(t), start.Challenge.Token, "authorization-code", "OIDC Browser",
	)
	if err != nil || user == nil || user.ID != "person" || result == nil || result.Session == nil ||
		result.Session.AuthenticationMethod != AuthenticationMethodFederatedOIDC {
		t.Fatalf("HandleOIDCCallback() = user:%#v result:%#v error:%v", user, result, err)
	}
	for table, want := range map[string]int{"accounts": 0, "oauth_accounts": 0, "sessions": 1} {
		var count int
		if err := manager.db.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s rows = %d, %v; want %d", table, count, err, want)
		}
	}
	var email string
	var verified int
	if err := manager.db.Read().QueryRow(`SELECT email, email_verified FROM auth_identities WHERE id = 'oidc-identity'`).Scan(&email, &verified); err != nil {
		t.Fatal(err)
	}
	if email != "new@example.com" || verified != 1 {
		t.Fatalf("OIDC display metadata = email:%q verified:%d", email, verified)
	}
}

func TestGenericOIDCLoginRejectsUnknownIdentityEvenWhenEmailMatches(t *testing.T) {
	now := time.Date(2026, time.August, 18, 6, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"challenge-id"}, tokens: []string{"state-token", "nonce-token"},
	})
	configureOIDCTest(manager)
	insertActiveUser(t, manager, "person", false, now)
	start, err := manager.BeginOIDCLogin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	manager.oidcIDTokenVerifier = oidcIDTokenVerifierFunc(func(context.Context, string) (*OIDCIDTokenClaims, error) {
		return &OIDCIDTokenClaims{
			Issuer: oidcTestIssuer, Subject: "unlinked-subject", Nonce: start.Challenge.Nonce,
			Email: "person@example.com", EmailVerified: true,
		}, nil
	})
	_, _, err = manager.HandleOIDCCallback(
		oidcOAuthTestContext(t), start.Challenge.Token, "authorization-code", "OIDC Browser",
	)
	if got := FederatedLoginReason(err); got != FederatedLoginFailureIdentityUnknown {
		t.Fatalf("failure reason = %q, want %q (error %v)", got, FederatedLoginFailureIdentityUnknown, err)
	}
	var identities, sessions int
	_ = manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_identities`).Scan(&identities)
	_ = manager.db.Read().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions)
	if identities != 0 || sessions != 0 {
		t.Fatalf("closed-registration state = identities:%d sessions:%d", identities, sessions)
	}
}

func TestGenericOIDCLoginRejectsInvalidClaimsAndConsumesChallenge(t *testing.T) {
	for _, test := range []struct {
		name       string
		claims     *OIDCIDTokenClaims
		verifyErr  error
		wantReason FederatedLoginFailureReason
	}{
		{name: "invalid ID token", verifyErr: errors.New("signature rejected"), wantReason: FederatedLoginFailureIDTokenInvalid},
		{name: "wrong nonce", claims: &OIDCIDTokenClaims{Issuer: oidcTestIssuer, Subject: "subject", Nonce: "wrong"}, wantReason: FederatedLoginFailureNonceInvalid},
		{name: "wrong issuer", claims: &OIDCIDTokenClaims{Issuer: "https://other.example", Subject: "subject", Nonce: "nonce-value"}, wantReason: FederatedLoginFailureSubjectInvalid},
		{name: "missing subject", claims: &OIDCIDTokenClaims{Issuer: oidcTestIssuer, Nonce: "nonce-value"}, wantReason: FederatedLoginFailureIDTokenInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, time.August, 18, 6, 45, 0, 0, time.UTC)
			manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
				ids: []string{"challenge-id"}, tokens: []string{"state-value", "nonce-value"},
			})
			configureOIDCTest(manager)
			manager.oidcIDTokenVerifier = oidcIDTokenVerifierFunc(func(context.Context, string) (*OIDCIDTokenClaims, error) {
				return test.claims, test.verifyErr
			})
			start, err := manager.BeginOIDCLogin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = manager.HandleOIDCCallback(
				oidcOAuthTestContext(t), start.Challenge.Token, "authorization-code", "Browser",
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

func TestGenericOIDCIdentityLinkAndUnlink(t *testing.T) {
	now := time.Date(2026, time.August, 18, 7, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"challenge-id", "identity-id", "link-event", "rotated-session", "unlink-event"},
		tokens: []string{"link-state", "link-nonce", "rotated-session-token"},
	})
	configureOIDCTest(manager)
	insertActiveUser(t, manager, "person", false, now)
	insertGoogleLinkSession(t, manager, "person", "person-session", "person-session-token", now, now)
	insertFederatedIdentityTestPassword(t, manager, "person", now)

	start, err := manager.BeginOIDCIdentityLink(t.Context(), "person-session-token")
	if err != nil {
		t.Fatal(err)
	}
	manager.oidcIDTokenVerifier = oidcIDTokenVerifierFunc(func(context.Context, string) (*OIDCIDTokenClaims, error) {
		return &OIDCIDTokenClaims{
			Issuer: oidcTestIssuer, Subject: "stable-subject", Nonce: start.Challenge.Nonce,
			Email: "person@example.com", EmailVerified: true,
		}, nil
	})
	identity, err := manager.CompleteOIDCIdentityLink(
		oidcOAuthTestContext(t), start.Challenge.Token, "person-session-token", "authorization-code", "Link Browser",
	)
	if err != nil || identity == nil || identity.Provider != oidcIdentityProvider || !identity.EmailVerified {
		t.Fatalf("CompleteOIDCIdentityLink() = %#v, %v", identity, err)
	}
	var metadata string
	if err := manager.db.Read().QueryRow(`SELECT metadata_json FROM auth_events WHERE id = 'link-event'`).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	if metadata != `{"provider":"oidc","result":"linked"}` || strings.Contains(metadata, "stable-subject") {
		t.Fatalf("OIDC link metadata = %q", metadata)
	}
	rotated, err := manager.UnlinkOIDCIdentity(t.Context(), "person-session-token", identity.ID, "Unlink Browser")
	if err != nil || rotated == nil || rotated.Token != "rotated-session-token" {
		t.Fatalf("UnlinkOIDCIdentity() = %#v, %v", rotated, err)
	}
	for table, want := range map[string]int{"auth_identities": 0, "accounts": 0, "oauth_accounts": 0} {
		var count int
		if err := manager.db.Read().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s rows after unlink = %d, %v; want %d", table, count, err, want)
		}
	}
}

func TestGenericOIDCIdentityLinkRejectsCrossUserSubjectConflict(t *testing.T) {
	now := time.Date(2026, time.August, 18, 7, 10, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"challenge-id", "unused-identity", "conflict-event"},
		tokens: []string{"link-state", "link-nonce"},
	})
	configureOIDCTest(manager)
	insertActiveUser(t, manager, "person", false, now)
	insertActiveUser(t, manager, "owner", false, now)
	insertGoogleLinkSession(t, manager, "person", "person-session", "person-session-token", now, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_identities (
			id, user_id, provider, issuer, subject, email, email_verified, created_at, linked_at
		) VALUES ('owner-identity', 'owner', ?, ?, 'shared-subject', 'owner@example.com', 1, ?, ?)`,
		oidcIdentityProvider, oidcTestIssuer, now, now,
	); err != nil {
		t.Fatal(err)
	}

	start, err := manager.BeginOIDCIdentityLink(t.Context(), "person-session-token")
	if err != nil {
		t.Fatal(err)
	}
	manager.oidcIDTokenVerifier = oidcIDTokenVerifierFunc(func(context.Context, string) (*OIDCIDTokenClaims, error) {
		return &OIDCIDTokenClaims{
			Issuer: oidcTestIssuer, Subject: "shared-subject", Nonce: start.Challenge.Nonce,
			Email: "changed@example.com", EmailVerified: true,
		}, nil
	})
	identity, err := manager.CompleteOIDCIdentityLink(
		oidcOAuthTestContext(t), start.Challenge.Token, "person-session-token", "authorization-code", "Link Browser",
	)
	if identity != nil || !errors.Is(err, ErrFederatedIdentityConflict) {
		t.Fatalf("CompleteOIDCIdentityLink(conflict) = %#v, %v", identity, err)
	}
	var ownerID, email string
	if err := manager.db.Read().QueryRow(`
		SELECT user_id, email FROM auth_identities WHERE id = 'owner-identity'`,
	).Scan(&ownerID, &email); err != nil {
		t.Fatal(err)
	}
	if ownerID != "owner" || email != "owner@example.com" {
		t.Fatalf("conflicting OIDC identity changed = owner:%q email:%q", ownerID, email)
	}
	var success int
	var metadata string
	if err := manager.db.Read().QueryRow(`
		SELECT success, metadata_json FROM auth_events WHERE id = 'conflict-event'`,
	).Scan(&success, &metadata); err != nil {
		t.Fatal(err)
	}
	if success != 0 || metadata != `{"provider":"oidc","result":"conflict"}` || strings.Contains(metadata, "shared-subject") {
		t.Fatalf("OIDC conflict audit = success:%d metadata:%q", success, metadata)
	}
}

func TestGenericOIDCIdentityCountsAsPrimaryOnlyForConfiguredIssuer(t *testing.T) {
	now := time.Date(2026, time.August, 18, 7, 15, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{})
	configureOIDCTest(manager)
	insertActiveUser(t, manager, "person", false, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_identities (
			id, user_id, provider, issuer, subject, email, email_verified, created_at, linked_at
		) VALUES ('only-oidc', 'person', ?, ?, 'subject', 'person@example.com', 1, ?, ?)`,
		oidcIdentityProvider, oidcTestIssuer, now, now,
	); err != nil {
		t.Fatal(err)
	}
	identities, err := manager.ListFederatedIdentities(t.Context(), "person")
	if err != nil || len(identities) != 1 || identities[0].CanUnlink || identities[0].UnlinkReason == "" {
		t.Fatalf("last OIDC identity summary = %#v, %v", identities, err)
	}
	manager.config.OIDCLoginIssuer = "https://different.example"
	insertFederatedIdentityTestPassword(t, manager, "person", now)
	identities, err = manager.ListFederatedIdentities(t.Context(), "person")
	if err != nil || len(identities) != 1 || !identities[0].CanUnlink {
		t.Fatalf("OIDC identity with local password summary = %#v, %v", identities, err)
	}
}
