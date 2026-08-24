package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	mfaContinuationDraftVersion = 1
	mfaContinuationDraftKey     = "gofer/auth/mfa-continuation-draft/v1"
)

var ErrMFAContinuationInvalid = errors.New("multi-factor continuation is invalid")

type mfaContinuationDraft struct {
	Version          int                  `json:"version"`
	AuthVersion      int64                `json:"auth_version"`
	PrimaryMethod    AuthenticationMethod `json:"primary_method"`
	PrimaryAssurance AssuranceLevel       `json:"primary_assurance"`
	Enrollment       *mfaEnrollmentDraft  `json:"enrollment,omitempty"`
}

func validMFAContinuationDraft(draft *mfaContinuationDraft) bool {
	if draft == nil || draft.Version != mfaContinuationDraftVersion || draft.AuthVersion < 1 ||
		draft.PrimaryAssurance != AssuranceLevelSingleFactor || !validMFAEnrollmentDraft(draft.Enrollment) {
		return false
	}
	switch draft.PrimaryMethod {
	case AuthenticationMethodPassword,
		AuthenticationMethodFederatedGoogle,
		AuthenticationMethodFederatedMicrosoft,
		AuthenticationMethodFederatedOIDC:
		return true
	default:
		return false
	}
}

func sameMFAContinuationDraft(left, right *mfaContinuationDraft) bool {
	return validMFAContinuationDraft(left) && validMFAContinuationDraft(right) &&
		left.Version == right.Version && left.AuthVersion == right.AuthVersion &&
		left.PrimaryMethod == right.PrimaryMethod && left.PrimaryAssurance == right.PrimaryAssurance &&
		sameMFAEnrollmentDraft(left.Enrollment, right.Enrollment)
}

func (m *Manager) prepareMFAContinuation(userID string, authVersion int64, primaryMethod AuthenticationMethod, origin string, now time.Time) (*PreAuthChallenge, *mfaContinuationDraft, error) {
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil {
		return nil, nil, err
	}
	draft := &mfaContinuationDraft{
		Version: mfaContinuationDraftVersion, AuthVersion: authVersion,
		PrimaryMethod: primaryMethod, PrimaryAssurance: AssuranceLevelSingleFactor,
	}
	if !validMFAContinuationDraft(draft) || strings.TrimSpace(userID) == "" {
		return nil, nil, fmt.Errorf("invalid multi-factor continuation state")
	}
	id, err := m.tokens.ID()
	if err != nil {
		return nil, nil, fmt.Errorf("generate MFA challenge ID: %w", err)
	}
	token, err := m.tokens.Token(32)
	if err != nil {
		return nil, nil, fmt.Errorf("generate MFA challenge token: %w", err)
	}
	challenge := &PreAuthChallenge{
		ID: id, Token: token, UserID: strings.TrimSpace(userID), Purpose: ChallengePurposeMFA,
		Origin: canonicalOrigin, MaxAttempts: defaultPreAuthMaxAttempts,
		CreatedAt: now, ExpiresAt: now.Add(defaultPreAuthLifetime),
	}
	payload, err := m.encryptMFAContinuationDraft(challenge, draft)
	if err != nil {
		return nil, nil, err
	}
	challenge.PayloadCiphertext = payload
	return challenge, draft, nil
}

func (m *Manager) insertMFAContinuation(ctx context.Context, tx *sql.Tx, challenge *PreAuthChallenge, now time.Time) error {
	if challenge == nil || challenge.Purpose != ChallengePurposeMFA || len(challenge.PayloadCiphertext) == 0 {
		return fmt.Errorf("multi-factor continuation challenge is incomplete")
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE auth_challenges
		SET consumed_at = COALESCE(consumed_at, ?), payload_ciphertext = NULL
		WHERE user_id = ? AND purpose = ? AND consumed_at IS NULL`,
		now, challenge.UserID, ChallengePurposeMFA,
	); err != nil {
		return fmt.Errorf("terminate replaced MFA challenge: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO auth_challenges (
			id, user_id, session_id, challenge_hash, nonce_hash, purpose,
			origin, attempts, max_attempts, payload_ciphertext, created_at, expires_at
		) VALUES (?, ?, NULL, ?, NULL, ?, ?, 0, ?, ?, ?, ?)`,
		challenge.ID, challenge.UserID, hashToken(challenge.Token), challenge.Purpose,
		challenge.Origin, challenge.MaxAttempts, challenge.PayloadCiphertext,
		challenge.CreatedAt, challenge.ExpiresAt,
	); err != nil {
		return fmt.Errorf("insert MFA challenge: %w", err)
	}
	return nil
}

func (m *Manager) GetActiveMFAChallenge(ctx context.Context, token, origin string) (*PreAuthChallenge, error) {
	challenge, draft, err := m.readMFAContinuation(ctx, token, origin)
	if err != nil {
		if errors.Is(err, ErrMFAContinuationInvalid) {
			return nil, nil
		}
		return nil, err
	}
	policy, err := m.loadAuthenticationPolicy(ctx, m.db.Read(), challenge.UserID, draft.AuthVersion)
	if errors.Is(err, ErrUserNotActive) || (err == nil && !policy.RequiresMFA) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return challenge, nil
}

func (m *Manager) readMFAContinuation(ctx context.Context, token, origin string) (*PreAuthChallenge, *mfaContinuationDraft, error) {
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, nil, ErrMFAContinuationInvalid
	}
	tx, err := m.db.Read().BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin MFA continuation read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	challenge, draft, err := m.currentMFAContinuation(ctx, tx, token, canonicalOrigin, m.clock.Now().UTC())
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit MFA continuation read: %w", err)
	}
	return challenge, draft, nil
}

func (m *Manager) currentMFAContinuation(ctx context.Context, tx *sql.Tx, token, origin string, now time.Time) (*PreAuthChallenge, *mfaContinuationDraft, error) {
	if strings.TrimSpace(token) == "" {
		return nil, nil, ErrMFAContinuationInvalid
	}
	challenge, err := scanPreAuthChallenge(tx.QueryRowContext(ctx, preAuthChallengeSelect+`
		WHERE challenge_hash = ? AND purpose = ? AND origin = ?
		  AND session_id IS NULL AND consumed_at IS NULL AND expires_at > ?
		  AND attempts < max_attempts AND payload_ciphertext IS NOT NULL`,
		hashToken(token), ChallengePurposeMFA, origin, now,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrMFAContinuationInvalid
	}
	if err != nil {
		return nil, nil, fmt.Errorf("load MFA continuation: %w", err)
	}
	draft, err := m.decryptMFAContinuationDraft(challenge, challenge.PayloadCiphertext)
	if err != nil || !validMFAContinuationDraft(draft) {
		return nil, nil, ErrMFAContinuationInvalid
	}
	return challenge, draft, nil
}

func (m *Manager) mfaContinuationAEAD() (cipher.AEAD, error) {
	if len(m.bucketHashKey) < minimumBucketHashKeyBytes {
		return nil, fmt.Errorf("MFA continuation encryption key is unavailable")
	}
	deriver := hmac.New(sha256.New, m.bucketHashKey)
	_, _ = deriver.Write([]byte(mfaContinuationDraftKey))
	block, err := aes.NewCipher(deriver.Sum(nil))
	if err != nil {
		return nil, fmt.Errorf("create MFA continuation cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func (m *Manager) encryptMFAContinuationDraft(challenge *PreAuthChallenge, draft *mfaContinuationDraft) ([]byte, error) {
	if challenge == nil || !validMFAContinuationDraft(draft) {
		return nil, fmt.Errorf("multi-factor continuation draft is invalid")
	}
	plaintext, err := json.Marshal(draft)
	if err != nil {
		return nil, fmt.Errorf("encode MFA continuation draft: %w", err)
	}
	aead, err := m.mfaContinuationAEAD()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate MFA continuation nonce: %w", err)
	}
	payload := []byte{mfaContinuationDraftVersion}
	payload = append(payload, nonce...)
	return aead.Seal(payload, nonce, plaintext, mfaContinuationDraftAAD(challenge)), nil
}

func (m *Manager) decryptMFAContinuationDraft(challenge *PreAuthChallenge, payload []byte) (*mfaContinuationDraft, error) {
	aead, err := m.mfaContinuationAEAD()
	if err != nil {
		return nil, err
	}
	if challenge == nil || len(payload) < 1+aead.NonceSize()+aead.Overhead() || payload[0] != mfaContinuationDraftVersion {
		return nil, ErrMFAContinuationInvalid
	}
	nonce := payload[1 : 1+aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, payload[1+aead.NonceSize():], mfaContinuationDraftAAD(challenge))
	if err != nil {
		return nil, ErrMFAContinuationInvalid
	}
	var draft mfaContinuationDraft
	if err := json.Unmarshal(plaintext, &draft); err != nil {
		return nil, ErrMFAContinuationInvalid
	}
	return &draft, nil
}

func mfaContinuationDraftAAD(challenge *PreAuthChallenge) []byte {
	return []byte(challenge.ID + "\x00" + challenge.UserID + "\x00" + challenge.Origin + "\x00" + string(challenge.Purpose))
}

func mfaFactorEventMetadata(factor string, primary AuthenticationMethod, repairRequired bool) (string, error) {
	metadata := struct {
		Factor         string               `json:"factor"`
		Primary        AuthenticationMethod `json:"primary"`
		RepairRequired bool                 `json:"repair_required,omitempty"`
	}{Factor: factor, Primary: primary, RepairRequired: repairRequired}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode MFA event metadata: %w", err)
	}
	return string(encoded), nil
}
