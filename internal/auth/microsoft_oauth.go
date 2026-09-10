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

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

const (
	microsoftLoginDefaultTenant         = "common"
	microsoftLoginConsumerTenantID      = "9188040d-6c67-4c5b-b112-36a304b66dad"
	microsoftLoginDraftVersion          = 1
	microsoftLoginDraftKey              = "gofer/auth/microsoft-login-draft/v1"
	microsoftLoginCallbackPath          = "/auth/microsoft/callback"
	microsoftApplicationOpenIDScope     = "openid"
	microsoftApplicationProfileScope    = "profile"
	microsoftApplicationEmailScope      = "email"
	maximumMicrosoftIDTokenLength       = 64 * 1024
	maximumMicrosoftIdentityFieldLength = 8 * 1024
)

type MicrosoftIDTokenClaims struct {
	Issuer            string
	Subject           string
	TenantID          string
	Nonce             string
	Email             string
	PreferredUsername string
	Name              string
}

// DisplayEmail returns provider metadata only. Microsoft documents both email
// and preferred_username as mutable and unsuitable for authorization or user
// ownership, so callers must key identities exclusively by issuer and subject.
func (claims *MicrosoftIDTokenClaims) DisplayEmail() string {
	if claims == nil {
		return ""
	}
	if email := strings.TrimSpace(claims.Email); email != "" {
		return email
	}
	return strings.TrimSpace(claims.PreferredUsername)
}

type MicrosoftIDTokenVerifier interface {
	Verify(context.Context, string) (*MicrosoftIDTokenClaims, error)
}

type MicrosoftLoginStart struct {
	Challenge        *PreAuthChallenge
	AuthorizationURL string
}

type microsoftLoginDraft struct {
	Version      int    `json:"version"`
	CodeVerifier string `json:"code_verifier"`
}

type maintainedMicrosoftIDTokenVerifier struct {
	verifier       *oidc.IDTokenVerifier
	expectedIssuer string
	expectedTenant string
}

func (verifier *maintainedMicrosoftIDTokenVerifier) Verify(ctx context.Context, rawIDToken string) (*MicrosoftIDTokenClaims, error) {
	if verifier == nil || verifier.verifier == nil || len(rawIDToken) == 0 || len(rawIDToken) > maximumMicrosoftIDTokenLength {
		return nil, fmt.Errorf("Microsoft ID token is invalid")
	}
	idToken, err := verifier.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("verify Microsoft ID token: %w", err)
	}
	var providerClaims struct {
		TenantID          string `json:"tid"`
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		Name              string `json:"name"`
	}
	if err := idToken.Claims(&providerClaims); err != nil {
		return nil, fmt.Errorf("decode verified Microsoft ID token claims: %w", err)
	}
	claims := &MicrosoftIDTokenClaims{
		Issuer: idToken.Issuer, Subject: idToken.Subject, TenantID: providerClaims.TenantID,
		Nonce: idToken.Nonce, Email: providerClaims.Email,
		PreferredUsername: providerClaims.PreferredUsername, Name: providerClaims.Name,
	}
	if claims.Issuer != verifier.expectedIssuer || !strings.EqualFold(claims.TenantID, verifier.expectedTenant) ||
		!boundedMicrosoftIdentityClaims(claims) {
		return nil, fmt.Errorf("Microsoft ID token claims are invalid")
	}
	return claims, nil
}

type discoveredMicrosoftIDTokenVerifier struct {
	clientID         string
	configuredTenant string

	mu              sync.Mutex
	allowedTenantID string
	verifiers       map[string]MicrosoftIDTokenVerifier
}

func newDiscoveredMicrosoftIDTokenVerifier(clientID, tenant string) MicrosoftIDTokenVerifier {
	return &discoveredMicrosoftIDTokenVerifier{
		clientID: strings.TrimSpace(clientID), configuredTenant: normalizeMicrosoftLoginTenant(tenant),
		verifiers: make(map[string]MicrosoftIDTokenVerifier),
	}
}

func (verifier *discoveredMicrosoftIDTokenVerifier) Verify(ctx context.Context, rawIDToken string) (*MicrosoftIDTokenClaims, error) {
	tenantID, unverifiedIssuer, err := microsoftTokenRoutingClaims(rawIDToken)
	if err != nil {
		return nil, err
	}
	expectedIssuer := microsoftTenantIssuer(tenantID)
	if unverifiedIssuer != expectedIssuer {
		return nil, fmt.Errorf("Microsoft ID token issuer is invalid")
	}
	allowedTenantID, err := verifier.resolveAllowedTenantID(ctx)
	if err != nil {
		return nil, err
	}
	if !microsoftLoginTenantAllows(verifier.configuredTenant, allowedTenantID, tenantID) {
		return nil, fmt.Errorf("Microsoft ID token tenant is not allowed")
	}

	verifier.mu.Lock()
	maintainedVerifier := verifier.verifiers[tenantID]
	verifier.mu.Unlock()
	if maintainedVerifier == nil {
		provider, err := oidc.NewProvider(ctx, expectedIssuer)
		if err != nil {
			return nil, fmt.Errorf("discover Microsoft OIDC tenant: %w", err)
		}
		maintainedVerifier = &maintainedMicrosoftIDTokenVerifier{
			verifier:       provider.VerifierContext(ctx, &oidc.Config{ClientID: verifier.clientID}),
			expectedIssuer: expectedIssuer, expectedTenant: tenantID,
		}
		verifier.mu.Lock()
		if existing := verifier.verifiers[tenantID]; existing != nil {
			maintainedVerifier = existing
		} else {
			verifier.verifiers[tenantID] = maintainedVerifier
		}
		verifier.mu.Unlock()
	}
	return maintainedVerifier.Verify(ctx, rawIDToken)
}

func (verifier *discoveredMicrosoftIDTokenVerifier) resolveAllowedTenantID(ctx context.Context) (string, error) {
	tenant := verifier.configuredTenant
	if microsoftLoginTenantIsAudience(tenantOrDefault(tenant)) {
		return "", nil
	}
	if parsed, err := uuid.Parse(tenant); err == nil {
		return strings.ToLower(parsed.String()), nil
	}
	verifier.mu.Lock()
	allowedTenantID := verifier.allowedTenantID
	verifier.mu.Unlock()
	if allowedTenantID != "" {
		return allowedTenantID, nil
	}
	provider, err := oidc.NewProvider(ctx, microsoftLoginAuthority(tenant))
	if err != nil {
		return "", fmt.Errorf("discover configured Microsoft OIDC tenant: %w", err)
	}
	var metadata struct {
		Issuer string `json:"issuer"`
	}
	if err := provider.Claims(&metadata); err != nil {
		return "", fmt.Errorf("read configured Microsoft OIDC issuer: %w", err)
	}
	allowedTenantID, err = microsoftTenantIDFromIssuer(metadata.Issuer)
	if err != nil {
		return "", err
	}
	verifier.mu.Lock()
	verifier.allowedTenantID = allowedTenantID
	verifier.mu.Unlock()
	return allowedTenantID, nil
}

func microsoftTokenRoutingClaims(rawIDToken string) (string, string, error) {
	if len(rawIDToken) == 0 || len(rawIDToken) > maximumMicrosoftIDTokenLength {
		return "", "", fmt.Errorf("Microsoft ID token is invalid")
	}
	type routingClaims struct {
		jwt.RegisteredClaims
		TenantID string `json:"tid"`
	}
	claims := &routingClaims{}
	token, _, err := new(jwt.Parser).ParseUnverified(rawIDToken, claims)
	if err != nil || token == nil || token.Method == nil || token.Method.Alg() != "RS256" {
		return "", "", fmt.Errorf("read Microsoft ID token routing claims: invalid token")
	}
	parsedTenant, err := uuid.Parse(strings.TrimSpace(claims.TenantID))
	if err != nil {
		return "", "", fmt.Errorf("Microsoft ID token tenant is invalid")
	}
	tenantID := strings.ToLower(parsedTenant.String())
	return tenantID, strings.TrimSpace(claims.Issuer), nil
}

func normalizeMicrosoftLoginTenant(raw string) string {
	tenant := strings.TrimSpace(raw)
	if tenant == "" {
		return microsoftLoginDefaultTenant
	}
	lower := strings.ToLower(tenant)
	if microsoftLoginTenantIsAudience(lower) {
		return lower
	}
	if parsed, err := uuid.Parse(lower); err == nil {
		return strings.ToLower(parsed.String())
	}
	if len(lower) > 253 || strings.HasPrefix(lower, ".") || strings.HasSuffix(lower, ".") || strings.Contains(lower, "..") {
		return ""
	}
	for _, char := range lower {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' || char == '.' {
			continue
		}
		return ""
	}
	return lower
}

func tenantOrDefault(tenant string) string {
	if tenant == "" {
		return microsoftLoginDefaultTenant
	}
	return tenant
}

func microsoftLoginTenantIsAudience(tenant string) bool {
	switch tenant {
	case "common", "organizations", "consumers":
		return true
	default:
		return false
	}
}

func microsoftLoginTenantAllows(configuredTenant, allowedTenantID, tokenTenantID string) bool {
	switch configuredTenant {
	case "common":
		return true
	case "organizations":
		return !strings.EqualFold(tokenTenantID, microsoftLoginConsumerTenantID)
	case "consumers":
		return strings.EqualFold(tokenTenantID, microsoftLoginConsumerTenantID)
	default:
		return allowedTenantID != "" && strings.EqualFold(tokenTenantID, allowedTenantID)
	}
}

func microsoftLoginAuthority(tenant string) string {
	return "https://login.microsoftonline.com/" + tenantOrDefault(tenant) + "/v2.0"
}

func microsoftTenantIssuer(tenantID string) string {
	return "https://login.microsoftonline.com/" + strings.ToLower(tenantID) + "/v2.0"
}

func microsoftTenantIDFromIssuer(rawIssuer string) (string, error) {
	issuer, err := url.Parse(strings.TrimSpace(rawIssuer))
	if err != nil || issuer.Scheme != "https" || !strings.EqualFold(issuer.Host, "login.microsoftonline.com") || issuer.RawQuery != "" || issuer.Fragment != "" {
		return "", fmt.Errorf("configured Microsoft OIDC issuer is invalid")
	}
	parts := strings.Split(strings.Trim(issuer.Path, "/"), "/")
	if len(parts) != 2 || parts[1] != "v2.0" {
		return "", fmt.Errorf("configured Microsoft OIDC issuer is invalid")
	}
	tenantID, err := uuid.Parse(parts[0])
	if err != nil {
		return "", fmt.Errorf("configured Microsoft OIDC issuer tenant is invalid")
	}
	return strings.ToLower(tenantID.String()), nil
}

func microsoftLoginOAuthEndpoint(tenant string) oauth2.Endpoint {
	base := "https://login.microsoftonline.com/" + tenantOrDefault(tenant) + "/oauth2/v2.0"
	return oauth2.Endpoint{AuthURL: base + "/authorize", TokenURL: base + "/token"}
}

func boundedMicrosoftIdentityClaims(claims *MicrosoftIDTokenClaims) bool {
	if claims == nil {
		return false
	}
	for _, value := range []string{
		claims.Issuer, claims.Subject, claims.TenantID, claims.Nonce,
		claims.Email, claims.PreferredUsername, claims.Name,
	} {
		if len(value) > maximumMicrosoftIdentityFieldLength {
			return false
		}
	}
	return true
}

func (m *Manager) BeginMicrosoftLogin(ctx context.Context) (*MicrosoftLoginStart, error) {
	return m.beginMicrosoftAuthorization(ctx, ChallengePurposeFederatedLogin, "")
}

func (m *Manager) BeginMicrosoftIdentityLink(ctx context.Context, sessionToken string) (*MicrosoftLoginStart, error) {
	return m.beginMicrosoftAuthorization(ctx, ChallengePurposeFederatedLink, sessionToken)
}

func (m *Manager) beginMicrosoftAuthorization(ctx context.Context, purpose ChallengePurpose, sessionToken string) (*MicrosoftLoginStart, error) {
	if !m.HasMicrosoftLogin() || m.db == nil {
		return nil, fmt.Errorf("Microsoft application login is not configured")
	}
	if purpose != ChallengePurposeFederatedLogin && purpose != ChallengePurposeFederatedLink {
		return nil, fmt.Errorf("invalid Microsoft authorization purpose %q", purpose)
	}
	var linkSession *Session
	if purpose == ChallengePurposeFederatedLink {
		session, err := m.GetSessionByToken(ctx, sessionToken)
		if err != nil {
			return nil, fmt.Errorf("load Microsoft identity-link session: %w", err)
		}
		if err := m.requireWebmailSessionUser(ctx, session); err != nil {
			return nil, err
		}
		if err := m.requireRecentSecurityStepUp(ctx, session, m.clock.Now().UTC()); err != nil {
			return nil, err
		}
		linkSession = session
	}
	origin, err := canonicalAuthOrigin(m.config.BaseURL)
	if err != nil {
		return nil, err
	}
	now := m.clock.Now().UTC()
	id, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate Microsoft login challenge ID: %w", err)
	}
	state, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate Microsoft login state: %w", err)
	}
	nonce, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate Microsoft login nonce: %w", err)
	}
	draft := &microsoftLoginDraft{Version: microsoftLoginDraftVersion, CodeVerifier: oauth2.GenerateVerifier()}
	challenge := &PreAuthChallenge{
		ID: id, Token: state, Nonce: nonce, Purpose: purpose, Origin: origin,
		MaxAttempts: 1, CreatedAt: now, ExpiresAt: now.Add(defaultPreAuthLifetime),
	}
	if purpose == ChallengePurposeFederatedLink {
		challenge.UserID = linkSession.UserID
		challenge.SessionID = linkSession.ID
	}
	payload, err := m.encryptMicrosoftLoginDraft(challenge, draft)
	if err != nil {
		return nil, err
	}
	challenge.PayloadCiphertext = payload
	if purpose == ChallengePurposeFederatedLink {
		err = m.runSecurityTransition(ctx, SecurityTransitionIdentityChange, func(tx *sql.Tx) error {
			current, err := m.currentSecuritySession(ctx, tx, sessionToken, now, true)
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
				return fmt.Errorf("replace Microsoft identity-link challenge: %w", err)
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
				return fmt.Errorf("insert Microsoft identity-link challenge: %w", err)
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
			err = fmt.Errorf("insert Microsoft login challenge: %w", err)
		}
	}
	if err != nil {
		return nil, err
	}
	authorizationURL := microsoftApplicationAuthorizationURL(
		m.config.MicrosoftLoginClient, challenge.Token,
		oauth2.S256ChallengeOption(draft.CodeVerifier), oidc.Nonce(challenge.Nonce),
	)
	return &MicrosoftLoginStart{Challenge: challenge, AuthorizationURL: authorizationURL}, nil
}

func microsoftApplicationAuthorizationURL(client *oauth2.Config, state string, options ...oauth2.AuthCodeOption) string {
	config := *client
	config.Scopes = []string{
		microsoftApplicationOpenIDScope,
		microsoftApplicationProfileScope,
		microsoftApplicationEmailScope,
	}
	return config.AuthCodeURL(state, options...)
}

func (m *Manager) IsCanonicalMicrosoftCallbackRequest(request *http.Request) bool {
	if request == nil || request.Method != http.MethodGet || request.URL == nil || request.URL.Path != microsoftLoginCallbackPath {
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

func (m *Manager) HandleMicrosoftCallback(ctx context.Context, challengeToken, code, userAgent string) (*User, *PrimaryAuthenticationResult, error) {
	_, _, claims, err := m.verifyMicrosoftCallback(ctx, challengeToken, ChallengePurposeFederatedLogin, code)
	if err != nil {
		return nil, nil, err
	}
	user, result, err := m.authenticateMicrosoftIdentity(ctx, claims, userAgent, m.consumeFederatedLoginForEvent(ctx, challengeToken, claims.Nonce))
	if err != nil {
		var failure *FederatedLoginError
		if errors.As(err, &failure) && failure.Reason == FederatedLoginFailureIdentityUnknown {
			return nil, nil, m.rejectMicrosoftAuthorization(ctx, challengeToken, ChallengePurposeFederatedLogin, failure.Reason)
		}
		return nil, nil, err
	}
	return user, result, nil
}

type microsoftChallengeQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (m *Manager) currentMicrosoftAuthorizationChallenge(ctx context.Context, queryer microsoftChallengeQueryer, token string, purpose ChallengePurpose) (*PreAuthChallenge, *microsoftLoginDraft, string, error) {
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
		return nil, nil, "", fmt.Errorf("load Microsoft login challenge: %w", err)
	}
	if (purpose == ChallengePurposeFederatedLogin && (challenge.SessionID != "" || challenge.UserID != "")) ||
		(purpose == ChallengePurposeFederatedLink && (challenge.SessionID == "" || challenge.UserID == "")) {
		return nil, nil, "", ErrPreAuthChallengeInvalid
	}
	draft, err := m.decryptMicrosoftLoginDraft(challenge, challenge.PayloadCiphertext)
	if err != nil || !validMicrosoftLoginDraft(draft) {
		return nil, nil, "", ErrPreAuthChallengeInvalid
	}
	var nonceHash string
	if err := queryer.QueryRowContext(ctx, `SELECT nonce_hash FROM auth_challenges WHERE id = ?`, challenge.ID).Scan(&nonceHash); err != nil || nonceHash == "" {
		return nil, nil, "", ErrPreAuthChallengeInvalid
	}
	return challenge, draft, nonceHash, nil
}

func (m *Manager) GetMicrosoftCallbackPurpose(ctx context.Context, token string) (ChallengePurpose, error) {
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
		return "", fmt.Errorf("load Microsoft callback purpose: %w", err)
	}
	return purpose, nil
}

func (m *Manager) TerminateMicrosoftAuthorizationChallenge(ctx context.Context, token string) error {
	purpose, err := m.GetMicrosoftCallbackPurpose(ctx, token)
	if err != nil {
		return err
	}
	return m.endFederatedAuthorization(ctx, token, purpose, AuthenticationMethodFederatedMicrosoft)
}

func (m *Manager) verifyMicrosoftCallback(ctx context.Context, challengeToken string, purpose ChallengePurpose, code string) (*PreAuthChallenge, *microsoftLoginDraft, *MicrosoftIDTokenClaims, error) {
	challenge, draft, expectedNonceHash, err := m.currentMicrosoftAuthorizationChallenge(ctx, m.db.Read(), challengeToken, purpose)
	if err != nil {
		reason := FederatedLoginFailureChallengeInvalid
		if !errors.Is(err, ErrPreAuthChallengeInvalid) {
			reason = FederatedLoginFailureInternal
		}
		return nil, nil, nil, m.rejectMicrosoftAuthorization(ctx, challengeToken, purpose, reason)
	}
	token, err := m.config.MicrosoftLoginClient.Exchange(ctx, code, oauth2.VerifierOption(draft.CodeVerifier))
	if err != nil {
		return nil, nil, nil, m.rejectMicrosoftAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureCodeExchange)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || strings.TrimSpace(rawIDToken) == "" {
		return nil, nil, nil, m.rejectMicrosoftAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureIDTokenMissing)
	}
	if m.microsoftIDTokenVerifier == nil {
		return nil, nil, nil, m.rejectMicrosoftAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureInternal)
	}
	claims, err := m.microsoftIDTokenVerifier.Verify(ctx, rawIDToken)
	if err != nil || !boundedMicrosoftIdentityClaims(claims) {
		return nil, nil, nil, m.rejectMicrosoftAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureIDTokenInvalid)
	}
	if claims.Nonce == "" || subtle.ConstantTimeCompare([]byte(hashToken(claims.Nonce)), []byte(expectedNonceHash)) != 1 {
		return nil, nil, nil, m.rejectMicrosoftAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureNonceInvalid)
	}
	if strings.TrimSpace(claims.Issuer) == "" || strings.TrimSpace(claims.Subject) == "" || strings.TrimSpace(claims.TenantID) == "" {
		return nil, nil, nil, m.rejectMicrosoftAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureSubjectInvalid)
	}
	return challenge, draft, claims, nil
}

func (m *Manager) rejectMicrosoftAuthorization(ctx context.Context, token string, purpose ChallengePurpose, reason FederatedLoginFailureReason) error {
	return m.rejectFederatedAuthorization(ctx, token, purpose, AuthenticationMethodFederatedMicrosoft, reason)
}

func validMicrosoftLoginDraft(draft *microsoftLoginDraft) bool {
	if draft == nil || draft.Version != microsoftLoginDraftVersion || len(draft.CodeVerifier) < 43 || len(draft.CodeVerifier) > 128 {
		return false
	}
	for _, char := range draft.CodeVerifier {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("-._~", char) {
			continue
		}
		return false
	}
	return true
}

func (m *Manager) microsoftLoginAEAD() (cipher.AEAD, error) {
	if len(m.bucketHashKey) < minimumBucketHashKeyBytes {
		return nil, fmt.Errorf("Microsoft login challenge encryption key is unavailable")
	}
	deriver := hmac.New(sha256.New, m.bucketHashKey)
	_, _ = deriver.Write([]byte(microsoftLoginDraftKey))
	block, err := aes.NewCipher(deriver.Sum(nil))
	if err != nil {
		return nil, fmt.Errorf("create Microsoft login challenge cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func (m *Manager) encryptMicrosoftLoginDraft(challenge *PreAuthChallenge, draft *microsoftLoginDraft) ([]byte, error) {
	if challenge == nil || !validMicrosoftLoginDraft(draft) {
		return nil, fmt.Errorf("Microsoft login challenge draft is invalid")
	}
	plaintext, err := json.Marshal(draft)
	if err != nil {
		return nil, fmt.Errorf("encode Microsoft login challenge draft: %w", err)
	}
	aead, err := m.microsoftLoginAEAD()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate Microsoft login challenge nonce: %w", err)
	}
	payload := []byte{microsoftLoginDraftVersion}
	payload = append(payload, nonce...)
	return aead.Seal(payload, nonce, plaintext, microsoftLoginDraftAAD(challenge)), nil
}

func (m *Manager) decryptMicrosoftLoginDraft(challenge *PreAuthChallenge, payload []byte) (*microsoftLoginDraft, error) {
	aead, err := m.microsoftLoginAEAD()
	if err != nil {
		return nil, err
	}
	if challenge == nil || len(payload) < 1+aead.NonceSize()+aead.Overhead() || payload[0] != microsoftLoginDraftVersion {
		return nil, ErrPreAuthChallengeInvalid
	}
	nonce := payload[1 : 1+aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, payload[1+aead.NonceSize():], microsoftLoginDraftAAD(challenge))
	if err != nil {
		return nil, ErrPreAuthChallengeInvalid
	}
	var draft microsoftLoginDraft
	if err := json.Unmarshal(plaintext, &draft); err != nil {
		return nil, ErrPreAuthChallengeInvalid
	}
	return &draft, nil
}

func microsoftLoginDraftAAD(challenge *PreAuthChallenge) []byte {
	return []byte(challenge.ID + "\x00" + string(challenge.Purpose) + "\x00" + challenge.Origin)
}

func sameMicrosoftLoginDraft(left, right *microsoftLoginDraft) bool {
	return validMicrosoftLoginDraft(left) && validMicrosoftLoginDraft(right) &&
		left.Version == right.Version && left.CodeVerifier == right.CodeVerifier
}
