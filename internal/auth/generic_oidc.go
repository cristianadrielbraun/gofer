package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"unicode"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	oidcLoginDefaultName         = "OpenID Connect"
	oidcLoginDraftVersion        = 1
	oidcLoginDraftKey            = "gofer/auth/oidc-login-draft/v1"
	oidcLoginCallbackPath        = "/auth/oidc/callback"
	oidcApplicationOpenIDScope   = "openid"
	oidcApplicationProfileScope  = "profile"
	oidcApplicationEmailScope    = "email"
	maximumOIDCIDTokenLength     = 64 * 1024
	maximumOIDCIdentityFieldSize = 8 * 1024
	maximumOIDCSubjectLength     = 255
	maximumOIDCLoginNameLength   = 64
)

type OIDCIDTokenClaims struct {
	Issuer            string
	Subject           string
	Nonce             string
	Email             string
	EmailVerified     bool
	PreferredUsername string
	Name              string
}

// DisplayEmail is provider metadata only. Generic OIDC identity ownership is
// always keyed by the verified issuer and subject, never by this value.
func (claims *OIDCIDTokenClaims) DisplayEmail() string {
	if claims == nil {
		return ""
	}
	if email := strings.TrimSpace(claims.Email); email != "" {
		return email
	}
	return strings.TrimSpace(claims.PreferredUsername)
}

func (claims *OIDCIDTokenClaims) DisplayEmailVerified() bool {
	return claims != nil && strings.TrimSpace(claims.Email) != "" && claims.EmailVerified
}

type OIDCIDTokenVerifier interface {
	Verify(context.Context, string) (*OIDCIDTokenClaims, error)
}

type OIDCLoginStart struct {
	Challenge        *PreAuthChallenge
	AuthorizationURL string
}

type oidcLoginDraft struct {
	Version      int    `json:"version"`
	CodeVerifier string `json:"code_verifier"`
}

type secureOIDCHTTPContextKey struct{}

type maintainedOIDCIDTokenVerifier struct {
	verifier       *oidc.IDTokenVerifier
	expectedIssuer string
}

type oidcProviderMetadata struct {
	Issuer           string `json:"issuer"`
	JSONWebKeySetURL string `json:"jwks_uri"`
}

func (verifier *maintainedOIDCIDTokenVerifier) Verify(ctx context.Context, rawIDToken string) (*OIDCIDTokenClaims, error) {
	if verifier == nil || verifier.verifier == nil || len(rawIDToken) == 0 || len(rawIDToken) > maximumOIDCIDTokenLength {
		return nil, fmt.Errorf("OIDC ID token is invalid")
	}
	idToken, err := verifier.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("verify OIDC ID token: %w", err)
	}
	var providerClaims struct {
		Email             string `json:"email"`
		EmailVerified     bool   `json:"email_verified"`
		PreferredUsername string `json:"preferred_username"`
		Name              string `json:"name"`
	}
	if err := idToken.Claims(&providerClaims); err != nil {
		return nil, fmt.Errorf("decode verified OIDC ID token claims: %w", err)
	}
	claims := &OIDCIDTokenClaims{
		Issuer: idToken.Issuer, Subject: idToken.Subject, Nonce: idToken.Nonce,
		Email: providerClaims.Email, EmailVerified: providerClaims.EmailVerified,
		PreferredUsername: providerClaims.PreferredUsername, Name: providerClaims.Name,
	}
	if claims.Issuer != verifier.expectedIssuer || !boundedOIDCIdentityClaims(claims) {
		return nil, fmt.Errorf("OIDC ID token claims are invalid")
	}
	return claims, nil
}

type discoveredOIDCIDTokenVerifier struct {
	issuer   string
	clientID string
	mu       sync.Mutex
	verifier OIDCIDTokenVerifier
}

func newDiscoveredOIDCIDTokenVerifier(issuer, clientID string) OIDCIDTokenVerifier {
	return &discoveredOIDCIDTokenVerifier{
		issuer: normalizeOIDCLoginIssuer(issuer), clientID: strings.TrimSpace(clientID),
	}
}

func (verifier *discoveredOIDCIDTokenVerifier) Verify(ctx context.Context, rawIDToken string) (*OIDCIDTokenClaims, error) {
	ctx = withSecureOIDCHTTPClient(ctx)
	verifier.mu.Lock()
	if verifier.verifier == nil {
		provider, err := oidc.NewProvider(ctx, verifier.issuer)
		if err != nil {
			verifier.mu.Unlock()
			return nil, fmt.Errorf("discover OIDC provider: %w", err)
		}
		if _, err := validateDiscoveredOIDCProvider(provider, verifier.issuer); err != nil {
			verifier.mu.Unlock()
			return nil, err
		}
		verifier.verifier = &maintainedOIDCIDTokenVerifier{
			verifier:       provider.VerifierContext(ctx, &oidc.Config{ClientID: verifier.clientID}),
			expectedIssuer: verifier.issuer,
		}
	}
	maintainedVerifier := verifier.verifier
	verifier.mu.Unlock()
	return maintainedVerifier.Verify(ctx, rawIDToken)
}

func normalizeOIDCLoginIssuer(raw string) string {
	issuer := strings.TrimSpace(raw)
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	return issuer
}

func normalizeOIDCLoginName(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		return oidcLoginDefaultName
	}
	if len(name) > maximumOIDCLoginNameLength {
		return oidcLoginDefaultName
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return oidcLoginDefaultName
		}
	}
	return name
}

func boundedOIDCIdentityClaims(claims *OIDCIDTokenClaims) bool {
	if claims == nil || !validOIDCSubject(claims.Subject) {
		return false
	}
	for _, value := range []string{
		claims.Issuer, claims.Nonce, claims.Email, claims.PreferredUsername, claims.Name,
	} {
		if len(value) > maximumOIDCIdentityFieldSize {
			return false
		}
	}
	return true
}

func validOIDCSubject(subject string) bool {
	if len(subject) == 0 || len(subject) > maximumOIDCSubjectLength {
		return false
	}
	for index := 0; index < len(subject); index++ {
		if subject[index] < 0x20 || subject[index] > 0x7e {
			return false
		}
	}
	return true
}

func validOIDCEndpoint(raw string) bool {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && endpoint.Scheme == "https" && endpoint.Host != "" && endpoint.User == nil && endpoint.Fragment == ""
}

func withSecureOIDCHTTPClient(ctx context.Context) context.Context {
	if hardened, _ := ctx.Value(secureOIDCHTTPContextKey{}).(bool); hardened {
		return ctx
	}
	client, _ := ctx.Value(oauth2.HTTPClient).(*http.Client)
	if client == nil {
		client = http.DefaultClient
	}
	hardened := *client
	previousRedirectPolicy := client.CheckRedirect
	hardened.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if request == nil || request.URL == nil || request.URL.Scheme != "https" {
			return fmt.Errorf("OIDC redirect must remain on HTTPS")
		}
		if previousRedirectPolicy != nil {
			return previousRedirectPolicy(request, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("OIDC redirect limit exceeded")
		}
		return nil
	}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &hardened)
	return context.WithValue(ctx, secureOIDCHTTPContextKey{}, true)
}

func validateDiscoveredOIDCProvider(provider *oidc.Provider, expectedIssuer string) (oauth2.Endpoint, error) {
	if provider == nil {
		return oauth2.Endpoint{}, fmt.Errorf("discovered OIDC provider is unavailable")
	}
	var metadata oidcProviderMetadata
	if err := provider.Claims(&metadata); err != nil {
		return oauth2.Endpoint{}, fmt.Errorf("decode OIDC discovery metadata: %w", err)
	}
	endpoint := provider.Endpoint()
	if metadata.Issuer != expectedIssuer || !validOIDCEndpoint(metadata.JSONWebKeySetURL) ||
		!validOIDCEndpoint(endpoint.AuthURL) || !validOIDCEndpoint(endpoint.TokenURL) {
		return oauth2.Endpoint{}, fmt.Errorf("discovered OIDC application-login metadata is invalid")
	}
	return endpoint, nil
}

func (m *Manager) oidcOAuthClient(ctx context.Context) (*oauth2.Config, error) {
	if !m.HasOIDCLogin() {
		return nil, fmt.Errorf("OIDC application login is not configured")
	}
	client := *m.config.OIDCLoginClient
	client.Scopes = []string{oidcApplicationOpenIDScope, oidcApplicationProfileScope, oidcApplicationEmailScope}
	if client.Endpoint.AuthURL != "" || client.Endpoint.TokenURL != "" {
		if !validOIDCEndpoint(client.Endpoint.AuthURL) || !validOIDCEndpoint(client.Endpoint.TokenURL) {
			return nil, fmt.Errorf("OIDC application-login endpoints are invalid")
		}
		return &client, nil
	}
	ctx = withSecureOIDCHTTPClient(ctx)

	m.oidcEndpointMu.Lock()
	defer m.oidcEndpointMu.Unlock()
	if m.oidcEndpoint == nil {
		provider, err := oidc.NewProvider(ctx, m.config.OIDCLoginIssuer)
		if err != nil {
			return nil, fmt.Errorf("discover OIDC application-login provider: %w", err)
		}
		endpoint, err := validateDiscoveredOIDCProvider(provider, m.config.OIDCLoginIssuer)
		if err != nil {
			return nil, err
		}
		m.oidcEndpoint = &endpoint
	}
	client.Endpoint = *m.oidcEndpoint
	return &client, nil
}

func (m *Manager) BeginOIDCLogin(ctx context.Context) (*OIDCLoginStart, error) {
	return m.beginOIDCAuthorization(ctx, ChallengePurposeFederatedLogin, "")
}

func (m *Manager) BeginOIDCIdentityLink(ctx context.Context, sessionToken string) (*OIDCLoginStart, error) {
	return m.beginOIDCAuthorization(ctx, ChallengePurposeFederatedLink, sessionToken)
}

func (m *Manager) beginOIDCAuthorization(ctx context.Context, purpose ChallengePurpose, sessionToken string) (*OIDCLoginStart, error) {
	if !m.HasOIDCLogin() || m.db == nil {
		return nil, fmt.Errorf("OIDC application login is not configured")
	}
	if purpose != ChallengePurposeFederatedLogin && purpose != ChallengePurposeFederatedLink {
		return nil, fmt.Errorf("invalid OIDC authorization purpose %q", purpose)
	}
	client, err := m.oidcOAuthClient(ctx)
	if err != nil {
		return nil, err
	}
	origin, err := canonicalAuthOrigin(m.config.BaseURL)
	if err != nil {
		return nil, err
	}
	now := m.clock.Now().UTC()
	id, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate OIDC login challenge ID: %w", err)
	}
	state, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate OIDC login state: %w", err)
	}
	nonce, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate OIDC login nonce: %w", err)
	}
	draft := &oidcLoginDraft{Version: oidcLoginDraftVersion, CodeVerifier: oauth2.GenerateVerifier()}
	challenge := &PreAuthChallenge{
		ID: id, Token: state, Nonce: nonce, Purpose: purpose, Origin: origin,
		MaxAttempts: 1, CreatedAt: now, ExpiresAt: now.Add(defaultPreAuthLifetime),
	}
	if purpose == ChallengePurposeFederatedLink {
		session, err := m.GetSessionByToken(ctx, sessionToken)
		if err != nil {
			return nil, fmt.Errorf("load OIDC identity-link session: %w", err)
		}
		if session == nil {
			return nil, ErrSecuritySessionInvalid
		}
		if err := m.requireRecentSecurityStepUp(ctx, session, now); err != nil {
			return nil, err
		}
		challenge.UserID = session.UserID
		challenge.SessionID = session.ID
	}
	payload, err := m.encryptOIDCLoginDraft(challenge, draft)
	if err != nil {
		return nil, err
	}
	challenge.PayloadCiphertext = payload
	if purpose == ChallengePurposeFederatedLink {
		err = m.runSecurityTransition(ctx, SecurityTransitionIdentityChange, func(tx *sql.Tx) error {
			current, err := currentSecuritySession(ctx, tx, sessionToken, now, true)
			if err != nil {
				return err
			}
			if current.ID != challenge.SessionID || current.UserID != challenge.UserID {
				return ErrSecuritySessionInvalid
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE auth_challenges SET consumed_at = COALESCE(consumed_at, ?), payload_ciphertext = NULL
				WHERE session_id = ? AND purpose = ? AND consumed_at IS NULL`,
				now, current.ID, ChallengePurposeFederatedLink,
			); err != nil {
				return fmt.Errorf("replace OIDC identity-link challenge: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO auth_challenges (
					id, user_id, session_id, challenge_hash, nonce_hash, purpose, origin,
					attempts, max_attempts, payload_ciphertext, created_at, expires_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
				challenge.ID, current.UserID, current.ID, hashToken(challenge.Token), hashToken(challenge.Nonce),
				challenge.Purpose, challenge.Origin, challenge.MaxAttempts, challenge.PayloadCiphertext,
				challenge.CreatedAt, challenge.ExpiresAt,
			); err != nil {
				return fmt.Errorf("insert OIDC identity-link challenge: %w", err)
			}
			return nil
		})
	} else {
		_, err = m.db.Write().ExecContext(ctx, `
			INSERT INTO auth_challenges (
				id, user_id, session_id, challenge_hash, nonce_hash, purpose, origin,
				attempts, max_attempts, payload_ciphertext, created_at, expires_at
			) VALUES (?, NULL, NULL, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
			challenge.ID, hashToken(challenge.Token), hashToken(challenge.Nonce), challenge.Purpose,
			challenge.Origin, challenge.MaxAttempts, challenge.PayloadCiphertext,
			challenge.CreatedAt, challenge.ExpiresAt,
		)
		if err != nil {
			err = fmt.Errorf("insert OIDC login challenge: %w", err)
		}
	}
	if err != nil {
		return nil, err
	}
	authorizationURL := oidcApplicationAuthorizationURL(
		client, challenge.Token, oauth2.S256ChallengeOption(draft.CodeVerifier), oidc.Nonce(challenge.Nonce),
	)
	return &OIDCLoginStart{Challenge: challenge, AuthorizationURL: authorizationURL}, nil
}

func oidcApplicationAuthorizationURL(client *oauth2.Config, state string, options ...oauth2.AuthCodeOption) string {
	config := *client
	config.Scopes = []string{oidcApplicationOpenIDScope, oidcApplicationProfileScope, oidcApplicationEmailScope}
	return config.AuthCodeURL(state, options...)
}

func (m *Manager) IsCanonicalOIDCCallbackRequest(request *http.Request) bool {
	if request == nil || request.Method != http.MethodGet || request.URL == nil || request.URL.Path != oidcLoginCallbackPath {
		return false
	}
	baseURL, err := url.Parse(m.config.BaseURL)
	if err != nil || baseURL.Host == "" || !strings.EqualFold(request.Host, baseURL.Host) {
		return false
	}
	if request.URL.IsAbs() && (!strings.EqualFold(request.URL.Scheme, baseURL.Scheme) || !strings.EqualFold(request.URL.Host, baseURL.Host)) {
		return false
	}
	return true
}

func (m *Manager) HandleOIDCCallback(ctx context.Context, challengeToken, code, userAgent string) (*User, *PrimaryAuthenticationResult, error) {
	_, _, claims, err := m.verifyOIDCCallback(ctx, challengeToken, ChallengePurposeFederatedLogin, code)
	if err != nil {
		return nil, nil, err
	}
	if _, err := m.ConsumePreAuthChallenge(ctx, challengeToken, claims.Nonce, ChallengePurposeFederatedLogin, m.config.BaseURL); err != nil {
		return nil, nil, federatedLoginError(FederatedLoginFailureChallengeInvalid)
	}
	return m.authenticateOIDCIdentity(ctx, claims, userAgent)
}

type oidcChallengeQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (m *Manager) currentOIDCAuthorizationChallenge(ctx context.Context, queryer oidcChallengeQueryer, token string, purpose ChallengePurpose) (*PreAuthChallenge, *oidcLoginDraft, string, error) {
	if strings.TrimSpace(token) == "" || (purpose != ChallengePurposeFederatedLogin && purpose != ChallengePurposeFederatedLink) {
		return nil, nil, "", ErrPreAuthChallengeInvalid
	}
	origin, err := canonicalAuthOrigin(m.config.BaseURL)
	if err != nil {
		return nil, nil, "", ErrPreAuthChallengeInvalid
	}
	challenge, err := scanPreAuthChallenge(queryer.QueryRowContext(ctx, preAuthChallengeSelect+`
		WHERE challenge_hash = ? AND purpose = ? AND origin = ?
		  AND nonce_hash IS NOT NULL AND consumed_at IS NULL
		  AND expires_at > ? AND attempts < max_attempts AND payload_ciphertext IS NOT NULL`,
		hashToken(token), purpose, origin, m.clock.Now().UTC(),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, "", ErrPreAuthChallengeInvalid
	}
	if err != nil {
		return nil, nil, "", fmt.Errorf("load OIDC login challenge: %w", err)
	}
	if (purpose == ChallengePurposeFederatedLogin && (challenge.SessionID != "" || challenge.UserID != "")) ||
		(purpose == ChallengePurposeFederatedLink && (challenge.SessionID == "" || challenge.UserID == "")) {
		return nil, nil, "", ErrPreAuthChallengeInvalid
	}
	draft, err := m.decryptOIDCLoginDraft(challenge, challenge.PayloadCiphertext)
	if err != nil || !validOIDCLoginDraft(draft) {
		return nil, nil, "", ErrPreAuthChallengeInvalid
	}
	var nonceHash string
	if err := queryer.QueryRowContext(ctx, `SELECT nonce_hash FROM auth_challenges WHERE id = ?`, challenge.ID).Scan(&nonceHash); err != nil || nonceHash == "" {
		return nil, nil, "", ErrPreAuthChallengeInvalid
	}
	return challenge, draft, nonceHash, nil
}

func (m *Manager) GetOIDCCallbackPurpose(ctx context.Context, token string) (ChallengePurpose, error) {
	if strings.TrimSpace(token) == "" {
		return "", ErrPreAuthChallengeInvalid
	}
	origin, err := canonicalAuthOrigin(m.config.BaseURL)
	if err != nil {
		return "", ErrPreAuthChallengeInvalid
	}
	var purpose ChallengePurpose
	err = m.db.Read().QueryRowContext(ctx, `
		SELECT purpose FROM auth_challenges
		WHERE challenge_hash = ? AND purpose IN (?, ?) AND origin = ?`,
		hashToken(token), ChallengePurposeFederatedLogin, ChallengePurposeFederatedLink, origin,
	).Scan(&purpose)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrPreAuthChallengeInvalid
	}
	if err != nil {
		return "", fmt.Errorf("load OIDC callback purpose: %w", err)
	}
	return purpose, nil
}

func (m *Manager) TerminateOIDCAuthorizationChallenge(ctx context.Context, token string) error {
	purpose, err := m.GetOIDCCallbackPurpose(ctx, token)
	if err != nil {
		return err
	}
	return m.TerminatePreAuthChallenge(ctx, token, purpose, m.config.BaseURL)
}

func (m *Manager) verifyOIDCCallback(ctx context.Context, challengeToken string, purpose ChallengePurpose, code string) (*PreAuthChallenge, *oidcLoginDraft, *OIDCIDTokenClaims, error) {
	ctx = withSecureOIDCHTTPClient(ctx)
	challenge, draft, expectedNonceHash, err := m.currentOIDCAuthorizationChallenge(ctx, m.db.Read(), challengeToken, purpose)
	if err != nil {
		reason := FederatedLoginFailureChallengeInvalid
		if !errors.Is(err, ErrPreAuthChallengeInvalid) {
			reason = FederatedLoginFailureInternal
		}
		return nil, nil, nil, m.rejectOIDCAuthorization(ctx, challengeToken, purpose, reason)
	}
	client, err := m.oidcOAuthClient(ctx)
	if err != nil {
		return nil, nil, nil, m.rejectOIDCAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureInternal)
	}
	token, err := client.Exchange(ctx, code, oauth2.VerifierOption(draft.CodeVerifier))
	if err != nil {
		return nil, nil, nil, m.rejectOIDCAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureCodeExchange)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || strings.TrimSpace(rawIDToken) == "" {
		return nil, nil, nil, m.rejectOIDCAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureIDTokenMissing)
	}
	if m.oidcIDTokenVerifier == nil {
		return nil, nil, nil, m.rejectOIDCAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureInternal)
	}
	claims, err := m.oidcIDTokenVerifier.Verify(ctx, rawIDToken)
	if err != nil || !boundedOIDCIdentityClaims(claims) {
		return nil, nil, nil, m.rejectOIDCAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureIDTokenInvalid)
	}
	if claims.Nonce == "" || subtle.ConstantTimeCompare([]byte(hashToken(claims.Nonce)), []byte(expectedNonceHash)) != 1 {
		return nil, nil, nil, m.rejectOIDCAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureNonceInvalid)
	}
	if claims.Issuer != m.config.OIDCLoginIssuer || !validOIDCSubject(claims.Subject) {
		return nil, nil, nil, m.rejectOIDCAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureSubjectInvalid)
	}
	return challenge, draft, claims, nil
}

func (m *Manager) rejectOIDCAuthorization(ctx context.Context, token string, purpose ChallengePurpose, reason FederatedLoginFailureReason) error {
	if err := m.TerminatePreAuthChallenge(ctx, token, purpose, m.config.BaseURL); err != nil && !errors.Is(err, ErrPreAuthChallengeInvalid) {
		return federatedLoginError(FederatedLoginFailureInternal)
	}
	return federatedLoginError(reason)
}

func validOIDCLoginDraft(draft *oidcLoginDraft) bool {
	if draft == nil || draft.Version != oidcLoginDraftVersion || len(draft.CodeVerifier) < 43 || len(draft.CodeVerifier) > 128 {
		return false
	}
	for _, character := range draft.CodeVerifier {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("-._~", character) {
			continue
		}
		return false
	}
	return true
}

func (m *Manager) oidcLoginAEAD() (cipher.AEAD, error) {
	if len(m.bucketHashKey) < minimumBucketHashKeyBytes {
		return nil, fmt.Errorf("OIDC login challenge encryption key is unavailable")
	}
	deriver := hmac.New(sha256.New, m.bucketHashKey)
	_, _ = deriver.Write([]byte(oidcLoginDraftKey))
	block, err := aes.NewCipher(deriver.Sum(nil))
	if err != nil {
		return nil, fmt.Errorf("create OIDC login challenge cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func (m *Manager) encryptOIDCLoginDraft(challenge *PreAuthChallenge, draft *oidcLoginDraft) ([]byte, error) {
	if challenge == nil || !validOIDCLoginDraft(draft) {
		return nil, fmt.Errorf("OIDC login challenge draft is invalid")
	}
	plaintext, err := json.Marshal(draft)
	if err != nil {
		return nil, fmt.Errorf("encode OIDC login challenge draft: %w", err)
	}
	aead, err := m.oidcLoginAEAD()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate OIDC login challenge nonce: %w", err)
	}
	payload := []byte{oidcLoginDraftVersion}
	payload = append(payload, nonce...)
	return aead.Seal(payload, nonce, plaintext, oidcLoginDraftAAD(challenge)), nil
}

func (m *Manager) decryptOIDCLoginDraft(challenge *PreAuthChallenge, payload []byte) (*oidcLoginDraft, error) {
	aead, err := m.oidcLoginAEAD()
	if err != nil {
		return nil, err
	}
	if challenge == nil || len(payload) < 1+aead.NonceSize()+aead.Overhead() || payload[0] != oidcLoginDraftVersion {
		return nil, ErrPreAuthChallengeInvalid
	}
	nonce := payload[1 : 1+aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, payload[1+aead.NonceSize():], oidcLoginDraftAAD(challenge))
	if err != nil {
		return nil, ErrPreAuthChallengeInvalid
	}
	var draft oidcLoginDraft
	if err := json.Unmarshal(plaintext, &draft); err != nil {
		return nil, ErrPreAuthChallengeInvalid
	}
	return &draft, nil
}

func oidcLoginDraftAAD(challenge *PreAuthChallenge) []byte {
	return []byte(challenge.ID + "\x00" + string(challenge.Purpose) + "\x00" + challenge.Origin)
}

func sameOIDCLoginDraft(left, right *oidcLoginDraft) bool {
	return validOIDCLoginDraft(left) && validOIDCLoginDraft(right) &&
		left.Version == right.Version && left.CodeVerifier == right.CodeVerifier
}
