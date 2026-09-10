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
	"golang.org/x/oauth2"
)

const (
	googleLoginIssuer              = "https://accounts.google.com"
	googleLoginDraftVersion        = 1
	googleLoginDraftKey            = "gofer/auth/google-login-draft/v1"
	googleLoginCallbackPath        = "/auth/google/callback"
	maximumGoogleIDTokenLength     = 64 * 1024
	maximumGoogleIdentityFieldSize = 8 * 1024
	googleApplicationOpenIDScope   = "openid"
	googleApplicationEmailScope    = "email"
	googleApplicationProfileScope  = "profile"
)

type FederatedLoginFailureReason string

const (
	FederatedLoginFailureChallengeInvalid FederatedLoginFailureReason = "challenge_invalid"
	FederatedLoginFailureCodeExchange     FederatedLoginFailureReason = "code_exchange_failed"
	FederatedLoginFailureIDTokenMissing   FederatedLoginFailureReason = "id_token_missing"
	FederatedLoginFailureIDTokenInvalid   FederatedLoginFailureReason = "id_token_invalid"
	FederatedLoginFailureNonceInvalid     FederatedLoginFailureReason = "nonce_invalid"
	FederatedLoginFailureSubjectInvalid   FederatedLoginFailureReason = "subject_invalid"
	FederatedLoginFailureEmailUnverified  FederatedLoginFailureReason = "email_unverified"
	FederatedLoginFailureIdentityUnknown  FederatedLoginFailureReason = "identity_unknown"
	FederatedLoginFailureIdentityConflict FederatedLoginFailureReason = "identity_conflict"
	FederatedLoginFailurePolicyCompletion FederatedLoginFailureReason = "policy_completion_failed"
	FederatedLoginFailureInternal         FederatedLoginFailureReason = "internal_failure"
)

type FederatedLoginError struct {
	Reason FederatedLoginFailureReason
}

func (err *FederatedLoginError) Error() string {
	return "federated login failed: " + string(err.Reason)
}

func FederatedLoginReason(err error) FederatedLoginFailureReason {
	var loginErr *FederatedLoginError
	if errors.As(err, &loginErr) && loginErr.Reason != "" {
		return loginErr.Reason
	}
	return FederatedLoginFailureInternal
}

func federatedLoginError(reason FederatedLoginFailureReason) error {
	return &FederatedLoginError{Reason: reason}
}

type GoogleIDTokenClaims struct {
	Subject       string
	Nonce         string
	Email         string
	EmailVerified bool
	Name          string
	Picture       string
}

type GoogleIDTokenVerifier interface {
	Verify(context.Context, string) (*GoogleIDTokenClaims, error)
}

type GoogleLoginStart struct {
	Challenge        *PreAuthChallenge
	AuthorizationURL string
}

type googleLoginDraft struct {
	Version           int    `json:"version"`
	CodeVerifier      string `json:"code_verifier"`
	EnrollmentTokenID string `json:"enrollment_token_id,omitempty"`
}

type maintainedGoogleIDTokenVerifier struct {
	verifier *oidc.IDTokenVerifier
}

func (verifier *maintainedGoogleIDTokenVerifier) Verify(ctx context.Context, rawIDToken string) (*GoogleIDTokenClaims, error) {
	if verifier == nil || verifier.verifier == nil || len(rawIDToken) == 0 || len(rawIDToken) > maximumGoogleIDTokenLength {
		return nil, fmt.Errorf("Google ID token is invalid")
	}
	idToken, err := verifier.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("verify Google ID token: %w", err)
	}
	var providerClaims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
	}
	if err := idToken.Claims(&providerClaims); err != nil {
		return nil, fmt.Errorf("decode verified Google ID token claims: %w", err)
	}
	claims := &GoogleIDTokenClaims{
		Subject:       idToken.Subject,
		Nonce:         idToken.Nonce,
		Email:         providerClaims.Email,
		EmailVerified: providerClaims.EmailVerified,
		Name:          providerClaims.Name,
		Picture:       providerClaims.Picture,
	}
	if !boundedGoogleIdentityClaims(claims) {
		return nil, fmt.Errorf("Google ID token claims exceed allowed bounds")
	}
	return claims, nil
}

type discoveredGoogleIDTokenVerifier struct {
	clientID string
	mu       sync.Mutex
	verifier GoogleIDTokenVerifier
}

func newDiscoveredGoogleIDTokenVerifier(clientID string) GoogleIDTokenVerifier {
	return &discoveredGoogleIDTokenVerifier{clientID: strings.TrimSpace(clientID)}
}

func (verifier *discoveredGoogleIDTokenVerifier) Verify(ctx context.Context, rawIDToken string) (*GoogleIDTokenClaims, error) {
	verifier.mu.Lock()
	if verifier.verifier == nil {
		provider, err := oidc.NewProvider(ctx, googleLoginIssuer)
		if err != nil {
			verifier.mu.Unlock()
			return nil, fmt.Errorf("discover Google OIDC provider: %w", err)
		}
		verifier.verifier = &maintainedGoogleIDTokenVerifier{verifier: provider.VerifierContext(ctx, &oidc.Config{ClientID: verifier.clientID})}
	}
	maintainedVerifier := verifier.verifier
	verifier.mu.Unlock()
	return maintainedVerifier.Verify(ctx, rawIDToken)
}

func boundedGoogleIdentityClaims(claims *GoogleIDTokenClaims) bool {
	if claims == nil {
		return false
	}
	for _, value := range []string{claims.Subject, claims.Nonce, claims.Email, claims.Name, claims.Picture} {
		if len(value) > maximumGoogleIdentityFieldSize {
			return false
		}
	}
	return true
}

func (m *Manager) BeginGoogleLogin(ctx context.Context) (*GoogleLoginStart, error) {
	return m.beginGoogleAuthorization(ctx, ChallengePurposeFederatedLogin, "")
}

func (m *Manager) BeginGoogleIdentityLink(ctx context.Context, sessionToken string) (*GoogleLoginStart, error) {
	return m.beginGoogleAuthorization(ctx, ChallengePurposeFederatedLink, sessionToken)
}

func (m *Manager) beginGoogleAuthorization(ctx context.Context, purpose ChallengePurpose, sessionToken string) (*GoogleLoginStart, error) {
	if !m.HasGoogleLogin() || m.db == nil {
		return nil, fmt.Errorf("Google application login is not configured")
	}
	if purpose != ChallengePurposeFederatedLogin && purpose != ChallengePurposeFederatedLink {
		return nil, fmt.Errorf("invalid Google authorization purpose %q", purpose)
	}
	var linkSession *Session
	if purpose == ChallengePurposeFederatedLink {
		session, err := m.GetSessionByToken(ctx, sessionToken)
		if err != nil {
			return nil, fmt.Errorf("load Google identity-link session: %w", err)
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
		return nil, fmt.Errorf("generate Google login challenge ID: %w", err)
	}
	state, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate Google login state: %w", err)
	}
	nonce, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate Google login nonce: %w", err)
	}
	draft := &googleLoginDraft{Version: googleLoginDraftVersion, CodeVerifier: oauth2.GenerateVerifier()}
	challenge := &PreAuthChallenge{
		ID: id, Token: state, Nonce: nonce, Purpose: purpose,
		Origin: origin, MaxAttempts: 1, CreatedAt: now, ExpiresAt: now.Add(defaultPreAuthLifetime),
	}
	if purpose == ChallengePurposeFederatedLink {
		challenge.UserID = linkSession.UserID
		challenge.SessionID = linkSession.ID
	}
	payload, err := m.encryptGoogleLoginDraft(challenge, draft)
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
				return fmt.Errorf("replace Google identity-link challenge: %w", err)
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
				return fmt.Errorf("insert Google identity-link challenge: %w", err)
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
			err = fmt.Errorf("insert Google login challenge: %w", err)
		}
	}
	if err != nil {
		return nil, err
	}
	authorizationURL := googleApplicationAuthorizationURL(
		m.config.GoogleLoginClient,
		challenge.Token,
		oauth2.S256ChallengeOption(draft.CodeVerifier),
		oidc.Nonce(challenge.Nonce),
	)
	return &GoogleLoginStart{Challenge: challenge, AuthorizationURL: authorizationURL}, nil
}

func googleApplicationAuthorizationURL(client *oauth2.Config, state string, options ...oauth2.AuthCodeOption) string {
	config := *client
	config.Scopes = []string{
		googleApplicationOpenIDScope,
		googleApplicationEmailScope,
		googleApplicationProfileScope,
	}
	return config.AuthCodeURL(state, options...)
}

func (m *Manager) IsCanonicalGoogleCallbackRequest(request *http.Request) bool {
	if request == nil || request.Method != http.MethodGet || request.URL == nil || request.URL.Path != googleLoginCallbackPath {
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

func (m *Manager) HandleGoogleCallback(ctx context.Context, challengeToken, code, userAgent string) (*User, *PrimaryAuthenticationResult, error) {
	_, _, claims, err := m.verifyGoogleCallback(ctx, challengeToken, ChallengePurposeFederatedLogin, code)
	if err != nil {
		return nil, nil, err
	}
	user, result, err := m.authenticateGoogleIdentity(ctx, claims, userAgent, m.consumeFederatedLoginForEvent(ctx, challengeToken, claims.Nonce))
	if err != nil {
		var failure *FederatedLoginError
		if errors.As(err, &failure) && failure.Reason == FederatedLoginFailureIdentityUnknown {
			return nil, nil, m.rejectGoogleAuthorization(ctx, challengeToken, ChallengePurposeFederatedLogin, failure.Reason)
		}
		return nil, nil, err
	}
	return user, result, nil
}

func (m *Manager) currentGoogleLoginChallenge(ctx context.Context, token string) (*PreAuthChallenge, *googleLoginDraft, string, error) {
	return m.currentGoogleAuthorizationChallenge(ctx, m.db.Read(), token, ChallengePurposeFederatedLogin)
}

type googleChallengeQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (m *Manager) currentGoogleAuthorizationChallenge(ctx context.Context, queryer googleChallengeQueryer, token string, purpose ChallengePurpose) (*PreAuthChallenge, *googleLoginDraft, string, error) {
	if strings.TrimSpace(token) == "" {
		return nil, nil, "", ErrPreAuthChallengeInvalid
	}
	if purpose != ChallengePurposeFederatedLogin && purpose != ChallengePurposeFederatedLink && purpose != ChallengePurposeFederatedEnrollment {
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
		return nil, nil, "", fmt.Errorf("load Google login challenge: %w", err)
	}
	if (purpose == ChallengePurposeFederatedLogin && (challenge.SessionID != "" || challenge.UserID != "")) ||
		(purpose == ChallengePurposeFederatedLink && (challenge.SessionID == "" || challenge.UserID == "")) ||
		(purpose == ChallengePurposeFederatedEnrollment && (challenge.SessionID != "" || challenge.UserID == "")) {
		return nil, nil, "", ErrPreAuthChallengeInvalid
	}
	draft, err := m.decryptGoogleLoginDraft(challenge, challenge.PayloadCiphertext)
	if err != nil || !validGoogleLoginDraftForPurpose(draft, purpose) {
		return nil, nil, "", ErrPreAuthChallengeInvalid
	}
	var nonceHash string
	if err := queryer.QueryRowContext(ctx, `SELECT nonce_hash FROM auth_challenges WHERE id = ?`, challenge.ID).Scan(&nonceHash); err != nil || nonceHash == "" {
		return nil, nil, "", ErrPreAuthChallengeInvalid
	}
	return challenge, draft, nonceHash, nil
}

func (m *Manager) GetGoogleCallbackPurpose(ctx context.Context, token string) (ChallengePurpose, error) {
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
		WHERE challenge_hash = ? AND purpose IN (?, ?, ?) AND origin = ?`,
		hashToken(token), ChallengePurposeFederatedLogin, ChallengePurposeFederatedLink, ChallengePurposeFederatedEnrollment,
		origin,
	).Scan(&purpose)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrPreAuthChallengeInvalid
	}
	if err != nil {
		return "", fmt.Errorf("load Google callback purpose: %w", err)
	}
	return purpose, nil
}

func (m *Manager) TerminateGoogleAuthorizationChallenge(ctx context.Context, token string) error {
	purpose, err := m.GetGoogleCallbackPurpose(ctx, token)
	if err != nil {
		return err
	}
	return m.endFederatedAuthorization(ctx, token, purpose, AuthenticationMethodFederatedGoogle)
}

func (m *Manager) verifyGoogleCallback(ctx context.Context, challengeToken string, purpose ChallengePurpose, code string) (*PreAuthChallenge, *googleLoginDraft, *GoogleIDTokenClaims, error) {
	challenge, draft, expectedNonceHash, err := m.currentGoogleAuthorizationChallenge(ctx, m.db.Read(), challengeToken, purpose)
	if err != nil {
		reason := FederatedLoginFailureChallengeInvalid
		if !errors.Is(err, ErrPreAuthChallengeInvalid) {
			reason = FederatedLoginFailureInternal
		}
		return nil, nil, nil, m.rejectGoogleAuthorization(ctx, challengeToken, purpose, reason)
	}
	token, err := m.config.GoogleLoginClient.Exchange(ctx, code, oauth2.VerifierOption(draft.CodeVerifier))
	if err != nil {
		return nil, nil, nil, m.rejectGoogleAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureCodeExchange)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || strings.TrimSpace(rawIDToken) == "" {
		return nil, nil, nil, m.rejectGoogleAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureIDTokenMissing)
	}
	if m.googleIDTokenVerifier == nil {
		return nil, nil, nil, m.rejectGoogleAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureInternal)
	}
	claims, err := m.googleIDTokenVerifier.Verify(ctx, rawIDToken)
	if err != nil || !boundedGoogleIdentityClaims(claims) {
		return nil, nil, nil, m.rejectGoogleAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureIDTokenInvalid)
	}
	if claims.Nonce == "" || subtle.ConstantTimeCompare([]byte(hashToken(claims.Nonce)), []byte(expectedNonceHash)) != 1 {
		return nil, nil, nil, m.rejectGoogleAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureNonceInvalid)
	}
	if strings.TrimSpace(claims.Subject) == "" {
		return nil, nil, nil, m.rejectGoogleAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureSubjectInvalid)
	}
	if !claims.EmailVerified || strings.TrimSpace(claims.Email) == "" {
		return nil, nil, nil, m.rejectGoogleAuthorization(ctx, challengeToken, purpose, FederatedLoginFailureEmailUnverified)
	}
	return challenge, draft, claims, nil
}

func (m *Manager) rejectGoogleAuthorization(ctx context.Context, token string, purpose ChallengePurpose, reason FederatedLoginFailureReason) error {
	return m.rejectFederatedAuthorization(ctx, token, purpose, AuthenticationMethodFederatedGoogle, reason)
}

func validGoogleLoginDraft(draft *googleLoginDraft) bool {
	if draft == nil || draft.Version != googleLoginDraftVersion || len(draft.CodeVerifier) < 43 || len(draft.CodeVerifier) > 128 {
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

func validGoogleLoginDraftForPurpose(draft *googleLoginDraft, purpose ChallengePurpose) bool {
	if !validGoogleLoginDraft(draft) {
		return false
	}
	switch purpose {
	case ChallengePurposeFederatedLogin, ChallengePurposeFederatedLink:
		return draft.EnrollmentTokenID == ""
	case ChallengePurposeFederatedEnrollment:
		return strings.TrimSpace(draft.EnrollmentTokenID) != ""
	default:
		return false
	}
}

func (m *Manager) googleLoginAEAD() (cipher.AEAD, error) {
	if len(m.bucketHashKey) < minimumBucketHashKeyBytes {
		return nil, fmt.Errorf("Google login challenge encryption key is unavailable")
	}
	deriver := hmac.New(sha256.New, m.bucketHashKey)
	_, _ = deriver.Write([]byte(googleLoginDraftKey))
	block, err := aes.NewCipher(deriver.Sum(nil))
	if err != nil {
		return nil, fmt.Errorf("create Google login challenge cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func (m *Manager) encryptGoogleLoginDraft(challenge *PreAuthChallenge, draft *googleLoginDraft) ([]byte, error) {
	if challenge == nil || !validGoogleLoginDraft(draft) {
		return nil, fmt.Errorf("Google login challenge draft is invalid")
	}
	plaintext, err := json.Marshal(draft)
	if err != nil {
		return nil, fmt.Errorf("encode Google login challenge draft: %w", err)
	}
	aead, err := m.googleLoginAEAD()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate Google login challenge nonce: %w", err)
	}
	payload := []byte{googleLoginDraftVersion}
	payload = append(payload, nonce...)
	return aead.Seal(payload, nonce, plaintext, googleLoginDraftAAD(challenge)), nil
}

func (m *Manager) decryptGoogleLoginDraft(challenge *PreAuthChallenge, payload []byte) (*googleLoginDraft, error) {
	aead, err := m.googleLoginAEAD()
	if err != nil {
		return nil, err
	}
	if challenge == nil || len(payload) < 1+aead.NonceSize()+aead.Overhead() || payload[0] != googleLoginDraftVersion {
		return nil, ErrPreAuthChallengeInvalid
	}
	nonce := payload[1 : 1+aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, payload[1+aead.NonceSize():], googleLoginDraftAAD(challenge))
	if err != nil {
		return nil, ErrPreAuthChallengeInvalid
	}
	var draft googleLoginDraft
	if err := json.Unmarshal(plaintext, &draft); err != nil {
		return nil, ErrPreAuthChallengeInvalid
	}
	return &draft, nil
}

func googleLoginDraftAAD(challenge *PreAuthChallenge) []byte {
	return []byte(challenge.ID + "\x00" + challenge.Origin + "\x00" + string(challenge.Purpose))
}
