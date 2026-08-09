package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	setupRecoveryCodeCount     = 10
	setupRecoveryCodeByteCount = 15
	setupRecoveryCodeGroupSize = 4
	setupRecoveryCodeDomain    = "gofer setup recovery code\x00"
	setupRecoveryBatchDomain   = "gofer setup recovery batch\x00"
)

var (
	ErrSetupTOTPConfirmationRequired      = errors.New("a verified setup TOTP draft is required")
	ErrSetupRecoveryBatchRequired         = errors.New("a setup recovery-code batch is required")
	ErrSetupRecoveryBatchAlreadyGenerated = errors.New("a setup recovery-code batch has already been generated")
	ErrSetupRecoveryStateChanged          = errors.New("the setup recovery-code state changed")
	setupRecoveryCodeEncoding             = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)
)

type SetupRecoveryState struct {
	BatchID      string
	Generated    bool
	Acknowledged bool
}

type SetupRecoveryBatch struct {
	SetupRecoveryState
	Codes []string
}

type SetupRecoveryValidationError struct {
	Fields map[string]string
}

func (e *SetupRecoveryValidationError) Error() string {
	return "setup owner recovery-code acknowledgement is invalid"
}

// GetSetupRecoveryState reports only non-secret batch state. Plaintext recovery
// codes are returned exactly once by GenerateSetupRecoveryCodes and are never
// recoverable from the encrypted setup draft.
func (m *Manager) GetSetupRecoveryState(ctx context.Context, token, origin string) (*SetupRecoveryState, error) {
	ownerState, err := m.GetSetupOwnerState(ctx, token, origin)
	if err != nil {
		return nil, err
	}
	if ownerState.Draft == nil || ownerState.DraftStale || ownerState.Draft.PasswordHash == "" {
		return nil, ErrSetupPasswordDraftRequired
	}
	if ownerState.Draft.TOTPSecret == "" || ownerState.Draft.TOTPConfirmedStep == nil {
		return nil, ErrSetupTOTPConfirmationRequired
	}
	if !validSetupRecoveryDraft(ownerState.Draft) {
		return nil, fmt.Errorf("setup recovery-code draft is invalid")
	}
	return setupRecoveryState(ownerState.Draft), nil
}

// GenerateSetupRecoveryCodes creates a new high-entropy batch and returns the
// plaintext only after its hashes have been committed to the challenge-bound
// draft. An existing batch is replaced only when replace is explicitly true.
func (m *Manager) GenerateSetupRecoveryCodes(ctx context.Context, token, origin string, replace bool) (*SetupRecoveryBatch, error) {
	preflight, err := m.GetSetupOwnerState(ctx, token, origin)
	if err != nil {
		return nil, err
	}
	if preflight.Draft == nil || preflight.DraftStale || preflight.Draft.PasswordHash == "" {
		return nil, ErrSetupPasswordDraftRequired
	}
	if preflight.Draft.TOTPSecret == "" || preflight.Draft.TOTPConfirmedStep == nil {
		return nil, ErrSetupTOTPConfirmationRequired
	}
	if !validSetupRecoveryDraft(preflight.Draft) {
		return nil, fmt.Errorf("setup recovery-code draft is invalid")
	}
	if preflight.Draft.RecoveryBatchID != "" && !replace {
		return nil, ErrSetupRecoveryBatchAlreadyGenerated
	}

	randomMaterial, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate setup recovery-code material: %w", err)
	}
	batch, hashes, err := deriveSetupRecoveryBatch(randomMaterial)
	if err != nil {
		return nil, err
	}
	if replace && subtle.ConstantTimeCompare([]byte(batch.BatchID), []byte(preflight.Draft.RecoveryBatchID)) == 1 {
		return nil, fmt.Errorf("replace setup recovery-code batch: generated batch did not change")
	}

	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, ErrSetupAccessInvalid
	}
	now := m.clock.Now().UTC()
	err = m.runSecurityTransition(ctx, SecurityTransitionSetup, func(tx *sql.Tx) error {
		challenge, draft, _, err := m.currentSetupSecurityDraft(ctx, tx, token, canonicalOrigin, now)
		if err != nil {
			return err
		}
		if draft.TOTPSecret == "" || draft.TOTPConfirmedStep == nil {
			return ErrSetupTOTPConfirmationRequired
		}
		if !sameSetupSecurityDraft(draft, preflight.Draft) {
			return ErrSetupRecoveryStateChanged
		}
		if draft.RecoveryBatchID != "" && !replace {
			return ErrSetupRecoveryBatchAlreadyGenerated
		}
		draft.RecoveryBatchID = batch.BatchID
		draft.RecoveryCodeHashes = hashes
		draft.RecoveryAcknowledged = false
		if err := m.storeSetupRecoveryDraft(ctx, tx, challenge, draft, token, canonicalOrigin, now); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return batch, nil
}

// AcknowledgeSetupRecoveryCodes binds the acknowledgement to the exact current
// batch so a stale page cannot approve codes that were replaced elsewhere.
func (m *Manager) AcknowledgeSetupRecoveryCodes(ctx context.Context, token, origin, batchID string, saved bool) (*SetupRecoveryState, error) {
	if !saved {
		return nil, &SetupRecoveryValidationError{Fields: map[string]string{
			"saved": "Confirm that you saved the recovery codes before continuing.",
		}}
	}
	preflight, err := m.GetSetupOwnerState(ctx, token, origin)
	if err != nil {
		return nil, err
	}
	if preflight.Draft == nil || preflight.DraftStale || preflight.Draft.PasswordHash == "" {
		return nil, ErrSetupPasswordDraftRequired
	}
	if preflight.Draft.TOTPSecret == "" || preflight.Draft.TOTPConfirmedStep == nil {
		return nil, ErrSetupTOTPConfirmationRequired
	}
	if !validSetupRecoveryDraft(preflight.Draft) || preflight.Draft.RecoveryBatchID == "" {
		return nil, ErrSetupRecoveryBatchRequired
	}
	if subtle.ConstantTimeCompare([]byte(batchID), []byte(preflight.Draft.RecoveryBatchID)) != 1 {
		return nil, staleSetupRecoveryBatchError()
	}

	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, ErrSetupAccessInvalid
	}
	now := m.clock.Now().UTC()
	var acknowledged *SetupRecoveryState
	err = m.runSecurityTransition(ctx, SecurityTransitionSetup, func(tx *sql.Tx) error {
		challenge, draft, _, err := m.currentSetupSecurityDraft(ctx, tx, token, canonicalOrigin, now)
		if err != nil {
			return err
		}
		if draft.TOTPSecret == "" || draft.TOTPConfirmedStep == nil {
			return ErrSetupTOTPConfirmationRequired
		}
		if !validSetupRecoveryDraft(draft) || draft.RecoveryBatchID == "" {
			return ErrSetupRecoveryBatchRequired
		}
		if !sameSetupRecoveryIdentityDraft(draft, preflight.Draft) || subtle.ConstantTimeCompare([]byte(batchID), []byte(draft.RecoveryBatchID)) != 1 {
			return staleSetupRecoveryBatchError()
		}
		if !draft.RecoveryAcknowledged {
			draft.RecoveryAcknowledged = true
			if err := m.storeSetupRecoveryDraft(ctx, tx, challenge, draft, token, canonicalOrigin, now); err != nil {
				return err
			}
		}
		acknowledged = setupRecoveryState(draft)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return acknowledged, nil
}

func deriveSetupRecoveryBatch(randomMaterial string) (*SetupRecoveryBatch, []string, error) {
	if strings.TrimSpace(randomMaterial) == "" {
		return nil, nil, fmt.Errorf("generate setup recovery codes: random material is empty")
	}
	batchID := hashToken(setupRecoveryBatchDomain + randomMaterial)
	codes := make([]string, 0, setupRecoveryCodeCount)
	hashes := make([]string, 0, setupRecoveryCodeCount)
	seen := make(map[string]struct{}, setupRecoveryCodeCount)
	for index := 0; index < setupRecoveryCodeCount; index++ {
		mac := hmac.New(sha256.New, []byte(randomMaterial))
		_, _ = mac.Write([]byte(setupRecoveryCodeDomain))
		var encodedIndex [4]byte
		binary.BigEndian.PutUint32(encodedIndex[:], uint32(index))
		_, _ = mac.Write(encodedIndex[:])
		canonical := setupRecoveryCodeEncoding.EncodeToString(mac.Sum(nil)[:setupRecoveryCodeByteCount])
		if _, duplicate := seen[canonical]; duplicate {
			return nil, nil, fmt.Errorf("generate setup recovery codes: duplicate code")
		}
		seen[canonical] = struct{}{}
		codes = append(codes, formatSetupRecoveryCode(canonical))
		hashes = append(hashes, hashToken(canonical))
	}
	return &SetupRecoveryBatch{
		SetupRecoveryState: SetupRecoveryState{BatchID: batchID, Generated: true},
		Codes:              codes,
	}, hashes, nil
}

func formatSetupRecoveryCode(canonical string) string {
	groups := make([]string, 0, (len(canonical)+setupRecoveryCodeGroupSize-1)/setupRecoveryCodeGroupSize)
	for len(canonical) > setupRecoveryCodeGroupSize {
		groups = append(groups, canonical[:setupRecoveryCodeGroupSize])
		canonical = canonical[setupRecoveryCodeGroupSize:]
	}
	if canonical != "" {
		groups = append(groups, canonical)
	}
	return strings.Join(groups, "-")
}

func setupRecoveryState(draft *SetupOwnerDraft) *SetupRecoveryState {
	generated := draft != nil && draft.RecoveryBatchID != "" && len(draft.RecoveryCodeHashes) == setupRecoveryCodeCount
	state := &SetupRecoveryState{Generated: generated}
	if generated {
		state.BatchID = draft.RecoveryBatchID
		state.Acknowledged = draft.RecoveryAcknowledged
	}
	return state
}

func clearSetupRecoveryDraft(draft *SetupOwnerDraft) {
	if draft == nil {
		return
	}
	draft.RecoveryBatchID = ""
	draft.RecoveryCodeHashes = nil
	draft.RecoveryAcknowledged = false
}

func validSetupRecoveryDraft(draft *SetupOwnerDraft) bool {
	if draft == nil {
		return false
	}
	if draft.RecoveryBatchID == "" {
		return len(draft.RecoveryCodeHashes) == 0 && !draft.RecoveryAcknowledged
	}
	if !isLowerHexHash(draft.RecoveryBatchID) || len(draft.RecoveryCodeHashes) != setupRecoveryCodeCount {
		return false
	}
	seen := make(map[string]struct{}, len(draft.RecoveryCodeHashes))
	for _, codeHash := range draft.RecoveryCodeHashes {
		if !isLowerHexHash(codeHash) {
			return false
		}
		if _, duplicate := seen[codeHash]; duplicate {
			return false
		}
		seen[codeHash] = struct{}{}
	}
	return true
}

func isLowerHexHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func sameSetupRecoveryIdentityDraft(left, right *SetupOwnerDraft) bool {
	if !sameSetupSecurityDraftIgnoringRecovery(left, right) || !sameRecoveryCodeHashes(left, right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left.RecoveryBatchID), []byte(right.RecoveryBatchID)) == 1
}

func sameRecoveryCodeHashes(left, right *SetupOwnerDraft) bool {
	if left == nil || right == nil || len(left.RecoveryCodeHashes) != len(right.RecoveryCodeHashes) {
		return false
	}
	result := 1
	for index := range left.RecoveryCodeHashes {
		result &= subtle.ConstantTimeCompare([]byte(left.RecoveryCodeHashes[index]), []byte(right.RecoveryCodeHashes[index]))
	}
	return result == 1
}

func staleSetupRecoveryBatchError() error {
	return &SetupRecoveryValidationError{Fields: map[string]string{
		"form": "That recovery-code batch is no longer current. Review the latest batch before continuing.",
	}}
}

func (m *Manager) storeSetupRecoveryDraft(ctx context.Context, tx *sql.Tx, challenge *PreAuthChallenge, draft *SetupOwnerDraft, token, origin string, now time.Time) error {
	payload, err := m.encryptSetupOwnerDraft(challenge, draft)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE auth_challenges SET payload_ciphertext = ?
		WHERE id = ? AND challenge_hash = ? AND purpose = ? AND origin = ?
		  AND user_id IS NULL AND session_id IS NULL AND consumed_at IS NULL
		  AND expires_at > ? AND attempts < max_attempts`,
		payload, challenge.ID, hashToken(token), ChallengePurposeEnrollment, origin, now,
	)
	if err != nil {
		return fmt.Errorf("store setup recovery-code draft: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read setup recovery-code draft update result: %w", err)
	}
	if changed != 1 {
		return ErrSetupAccessInvalid
	}
	return nil
}
