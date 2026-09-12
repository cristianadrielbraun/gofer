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
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/oauth2"
)

type oauthRoundTripFunc func(*http.Request) (*http.Response, error)

func (f oauthRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type googleIDTokenVerifierFunc func(context.Context, string) (*GoogleIDTokenClaims, error)

func (verify googleIDTokenVerifierFunc) Verify(ctx context.Context, rawIDToken string) (*GoogleIDTokenClaims, error) {
	return verify(ctx, rawIDToken)
}

func TestGoogleApplicationLoginUsesVerifiedIDTokenWithoutCreatingMailboxAccess(t *testing.T) {
	now := time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"challenge-id", "session-id"},
		tokens: []string{"state-token", "nonce-token", "session-token"},
	})
	insertActiveUser(t, manager, "person", false, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_identities (
			id, user_id, provider, issuer, subject, email, email_verified, created_at, linked_at
		) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		"google-identity", "person", googleIdentityProvider, googleLoginIssuer,
		"google-subject", "old-address@example.com", now, now,
	); err != nil {
		t.Fatalf("insert linked Google identity: %v", err)
	}
	manager.config.GoogleLoginClient = &oauth2.Config{
		ClientID:     "login-client",
		ClientSecret: "login-secret",
		RedirectURL:  "https://gofer.example/auth/google/login/callback",
		Scopes:       []string{"openid", "email", "profile"},
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://accounts.example/authorize",
			TokenURL: "https://accounts.example/token",
		},
	}

	client := &http.Client{Transport: oauthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://accounts.example/token" {
			t.Fatalf("unexpected OAuth request %s %s", request.Method, request.URL)
		}
		if err := request.ParseForm(); err != nil {
			t.Fatalf("parse token request: %v", err)
		}
		if request.Form.Get("code") != "authorization-code" || request.Form.Get("code_verifier") == "" {
			t.Fatalf("token request form = %q", request.Form.Encode())
		}
		body := `{"access_token":"application-access-token","refresh_token":"application-refresh-token","token_type":"Bearer","expires_in":3600,"id_token":"signed-id-token"}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    request,
		}, nil
	})}
	ctx := context.WithValue(t.Context(), oauth2.HTTPClient, client)
	start, err := manager.BeginGoogleLogin(ctx)
	if err != nil {
		t.Fatalf("BeginGoogleLogin() error = %v", err)
	}
	manager.googleIDTokenVerifier = googleIDTokenVerifierFunc(func(_ context.Context, rawIDToken string) (*GoogleIDTokenClaims, error) {
		if rawIDToken != "signed-id-token" {
			t.Fatalf("raw ID token = %q", rawIDToken)
		}
		return &GoogleIDTokenClaims{
			Subject: "google-subject", Nonce: start.Challenge.Nonce,
			Email: "person@example.com", EmailVerified: true,
			Name: "Person", Picture: "https://images.example/person.png",
		}, nil
	})

	user, result, err := manager.HandleGoogleCallback(ctx, start.Challenge.Token, "authorization-code", "Test Browser")
	if err != nil || user == nil || user.ID != "person" || result == nil || result.Session == nil {
		t.Fatalf("HandleGoogleCallback() = user:%#v result:%#v error:%v", user, result, err)
	}
	for table, want := range map[string]int{"oauth_accounts": 0, "accounts": 0, "sessions": 1} {
		var count int
		if err := manager.db.Read().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != want {
			t.Fatalf("%s rows = %d, want %d", table, count, want)
		}
	}
	var consumed bool
	var payload []byte
	if err := manager.db.Read().QueryRowContext(ctx, `
		SELECT consumed_at IS NOT NULL, payload_ciphertext FROM auth_challenges WHERE id = ?`,
		start.Challenge.ID,
	).Scan(&consumed, &payload); err != nil {
		t.Fatalf("read consumed Google challenge: %v", err)
	}
	if !consumed || payload != nil {
		t.Fatalf("consumed Google challenge = consumed:%t payload:%x", consumed, payload)
	}
	var identityEmail string
	var identityLastUsedAt *time.Time
	if err := manager.db.Read().QueryRowContext(ctx, `
		SELECT email, last_used_at FROM auth_identities WHERE id = ?`, "google-identity",
	).Scan(&identityEmail, &identityLastUsedAt); err != nil {
		t.Fatalf("read used Google identity: %v", err)
	}
	if identityEmail != "person@example.com" || identityLastUsedAt == nil || !identityLastUsedAt.Equal(now) {
		t.Fatalf("used Google identity = email:%q last-used:%v", identityEmail, identityLastUsedAt)
	}
}

func TestGoogleApplicationLoginRejectsUnlinkedIdentityEvenWhenEmailMatches(t *testing.T) {
	now := time.Date(2026, time.August, 14, 12, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"challenge-id"}, tokens: []string{"state-token", "nonce-token"},
	})
	insertActiveUser(t, manager, "person", false, now)
	configureGoogleOAuthTest(manager)

	start, err := manager.BeginGoogleLogin(t.Context())
	if err != nil {
		t.Fatalf("BeginGoogleLogin() error = %v", err)
	}
	manager.googleIDTokenVerifier = googleIDTokenVerifierFunc(func(context.Context, string) (*GoogleIDTokenClaims, error) {
		return &GoogleIDTokenClaims{
			Subject: "unlinked-subject", Nonce: start.Challenge.Nonce,
			Email: "person@example.com", EmailVerified: true,
		}, nil
	})

	_, _, err = manager.HandleGoogleCallback(
		googleOAuthTestContext(t), start.Challenge.Token, "authorization-code", "Test Browser",
	)
	if got := FederatedLoginReason(err); got != FederatedLoginFailureIdentityUnknown {
		t.Fatalf("failure reason = %q, want %q (error %v)", got, FederatedLoginFailureIdentityUnknown, err)
	}
	for table, want := range map[string]int{"users": 1, "auth_identities": 0, "sessions": 0} {
		var count int
		if err := manager.db.Read().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != want {
			t.Fatalf("%s rows = %d, want %d", table, count, want)
		}
	}
}

func TestGoogleApplicationLoginResolvesExactSubjectWithoutOverwritingUserProfile(t *testing.T) {
	now := time.Date(2026, time.August, 14, 13, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"challenge-id", "session-id"},
		tokens: []string{"state-token", "nonce-token", "session-token"},
	})
	insertActiveUser(t, manager, "owner", false, now)
	insertActiveUser(t, manager, "email-match", false, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_identities (
			id, user_id, provider, issuer, subject, email, email_verified, created_at, linked_at
		) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		"google-identity", "owner", googleIdentityProvider, googleLoginIssuer,
		"exact-subject", "old-google-address@example.com", now, now,
	); err != nil {
		t.Fatalf("insert linked Google identity: %v", err)
	}
	configureGoogleOAuthTest(manager)

	start, err := manager.BeginGoogleLogin(t.Context())
	if err != nil {
		t.Fatalf("BeginGoogleLogin() error = %v", err)
	}
	manager.googleIDTokenVerifier = googleIDTokenVerifierFunc(func(context.Context, string) (*GoogleIDTokenClaims, error) {
		return &GoogleIDTokenClaims{
			Subject: "exact-subject", Nonce: start.Challenge.Nonce,
			Email: "email-match@example.com", EmailVerified: true,
			Name: "Changed Google Name", Picture: "https://images.example/changed.png",
		}, nil
	})

	user, result, err := manager.HandleGoogleCallback(
		googleOAuthTestContext(t), start.Challenge.Token, "authorization-code", "Test Browser",
	)
	if err != nil || user == nil || user.ID != "owner" || result == nil || result.Session == nil {
		t.Fatalf("HandleGoogleCallback() = user:%#v result:%#v error:%v", user, result, err)
	}
	var ownerUsername, ownerName, ownerAvatar string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT username, name, avatar_url FROM users WHERE id = 'owner'`,
	).Scan(&ownerUsername, &ownerName, &ownerAvatar); err != nil {
		t.Fatalf("read owner profile: %v", err)
	}
	if ownerUsername != "owner" || ownerName != "owner" || ownerAvatar != "" {
		t.Fatalf("owner profile was overwritten = username:%q name:%q avatar:%q", ownerUsername, ownerName, ownerAvatar)
	}
	var identityEmail string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT email FROM auth_identities WHERE id = 'google-identity'`,
	).Scan(&identityEmail); err != nil {
		t.Fatalf("read Google identity email: %v", err)
	}
	if identityEmail != "email-match@example.com" {
		t.Fatalf("Google identity email = %q", identityEmail)
	}
}

func configureGoogleOAuthTest(manager *Manager) {
	manager.config.GoogleLoginClient = &oauth2.Config{
		ClientID: "login-client",
		Endpoint: oauth2.Endpoint{
			AuthURL: "https://accounts.example/authorize", TokenURL: "https://accounts.example/token",
		},
	}
}

func googleOAuthTestContext(t *testing.T) context.Context {
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

func TestGoogleApplicationLoginAuthorizationUsesPKCEStateNonceAndIdentityOnlyScopes(t *testing.T) {
	manager := newDeterministicManager(t, &fixedClock{now: time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)}, &deterministicTokenGenerator{
		ids:    []string{"challenge-id"},
		tokens: []string{"state-value", "nonce-value"},
	})
	manager.config.GoogleLoginClient = &oauth2.Config{
		ClientID: "login-client",
		Scopes:   []string{"openid", "email", "profile"},
		Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example/authorize"},
	}
	start, err := manager.BeginGoogleLogin(t.Context())
	if err != nil {
		t.Fatalf("BeginGoogleLogin() error = %v", err)
	}
	parsed, err := url.Parse(start.AuthorizationURL)
	if err != nil {
		t.Fatalf("parse authorization URL: %v", err)
	}
	query := parsed.Query()
	if query.Get("state") != "state-value" || query.Get("nonce") != "nonce-value" || query.Get("scope") != "openid email profile" {
		t.Fatalf("authorization query = %q", query.Encode())
	}
	if query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorization omitted S256 PKCE: %q", query.Encode())
	}
	if query.Get("access_type") != "" || query.Get("prompt") != "select_account" {
		t.Fatalf("application login must request account selection without offline access: %q", query.Encode())
	}

	challenge, draft, _, err := manager.currentGoogleLoginChallenge(t.Context(), start.Challenge.Token)
	if err != nil {
		t.Fatalf("currentGoogleLoginChallenge() error = %v", err)
	}
	if query.Get("code_challenge") != oauth2.S256ChallengeFromVerifier(draft.CodeVerifier) {
		t.Fatal("authorization PKCE challenge does not match the encrypted verifier")
	}
	if strings.Contains(string(challenge.PayloadCiphertext), draft.CodeVerifier) || strings.Contains(string(challenge.PayloadCiphertext), "code_verifier") {
		t.Fatalf("Google challenge persisted plaintext PKCE data: %q", challenge.PayloadCiphertext)
	}
}

func TestMaintainedGoogleIDTokenVerifierRejectsInvalidStandardClaims(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate alternate signing key: %v", err)
	}
	verifier := &maintainedGoogleIDTokenVerifier{verifier: oidc.NewVerifier(
		googleLoginIssuer,
		&oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&key.PublicKey}},
		&oidc.Config{ClientID: "login-client"},
	)}
	now := time.Now().UTC()
	baseClaims := jwt.MapClaims{
		"iss": googleLoginIssuer, "aud": "login-client", "sub": "google-subject",
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(5 * time.Minute).Unix(),
		"nonce": "nonce-value", "email": "person@example.com", "email_verified": true,
		"name": "Person", "picture": "https://images.example/person.png",
	}

	tests := []struct {
		name   string
		mutate func(jwt.MapClaims)
		key    *rsa.PrivateKey
		valid  bool
	}{
		{name: "valid", key: key, valid: true},
		{name: "wrong signature", key: otherKey},
		{name: "wrong issuer", key: key, mutate: func(claims jwt.MapClaims) { claims["iss"] = "https://issuer.example" }},
		{name: "wrong audience", key: key, mutate: func(claims jwt.MapClaims) { claims["aud"] = "other-client" }},
		{name: "expired", key: key, mutate: func(claims jwt.MapClaims) { claims["exp"] = now.Add(-time.Minute).Unix() }},
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
				t.Fatalf("sign ID token: %v", err)
			}
			got, err := verifier.Verify(t.Context(), raw)
			if test.valid {
				if err != nil || got == nil || got.Subject != "google-subject" || got.Nonce != "nonce-value" || !got.EmailVerified {
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

func TestGoogleApplicationLoginRejectsInvalidVerifiedIdentityClaims(t *testing.T) {
	tests := []struct {
		name       string
		claims     *GoogleIDTokenClaims
		verifyErr  error
		wantReason FederatedLoginFailureReason
	}{
		{name: "invalid ID token", verifyErr: errors.New("signature rejected"), wantReason: FederatedLoginFailureIDTokenInvalid},
		{name: "wrong nonce", claims: &GoogleIDTokenClaims{Subject: "subject", Nonce: "wrong", Email: "person@example.com", EmailVerified: true}, wantReason: FederatedLoginFailureNonceInvalid},
		{name: "missing subject", claims: &GoogleIDTokenClaims{Nonce: "nonce-value", Email: "person@example.com", EmailVerified: true}, wantReason: FederatedLoginFailureSubjectInvalid},
		{name: "unverified email", claims: &GoogleIDTokenClaims{Subject: "subject", Nonce: "nonce-value", Email: "person@example.com"}, wantReason: FederatedLoginFailureEmailUnverified},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, time.August, 14, 12, 0, 0, 0, time.UTC)
			manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
				ids: []string{"challenge-id"}, tokens: []string{"state-value", "nonce-value"},
			})
			manager.config.GoogleLoginClient = &oauth2.Config{
				ClientID: "login-client", Endpoint: oauth2.Endpoint{TokenURL: "https://accounts.example/token"},
			}
			manager.googleIDTokenVerifier = googleIDTokenVerifierFunc(func(context.Context, string) (*GoogleIDTokenClaims, error) {
				return test.claims, test.verifyErr
			})
			start, err := manager.BeginGoogleLogin(t.Context())
			if err != nil {
				t.Fatalf("BeginGoogleLogin() error = %v", err)
			}
			client := &http.Client{Transport: oauthRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"access_token":"access","token_type":"Bearer","id_token":"signed-id-token"}`)), Request: request,
				}, nil
			})}
			ctx := context.WithValue(t.Context(), oauth2.HTTPClient, client)
			_, _, err = manager.HandleGoogleCallback(ctx, start.Challenge.Token, "authorization-code", "Test Browser")
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
				t.Fatalf("read rejected challenge: %v", err)
			}
			if attempts != 1 || !consumed || payload != nil {
				t.Fatalf("rejected challenge = attempts:%d consumed:%t payload:%x", attempts, consumed, payload)
			}
		})
	}
}
