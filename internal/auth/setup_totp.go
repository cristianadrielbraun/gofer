package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrSetupPasswordDraftRequired = errors.New("a current owner password draft is required")
	ErrSetupTOTPDraftRequired     = errors.New("a current TOTP setup draft is required")
)

type SetupTOTPValidationError struct {
	Fields map[string]string
}

func (e *SetupTOTPValidationError) Error() string {
	return "setup owner TOTP code is invalid"
}

// GetSetupTOTPEnrollment returns display-only enrollment material for the
// encrypted seed already bound to the active setup challenge. It never creates
// a seed or writes permanent authenticator state.
func (m *Manager) GetSetupTOTPEnrollment(ctx context.Context, token, origin string) (*SetupTOTPEnrollment, error) {
	state, err := m.GetSetupOwnerState(ctx, token, origin)
	if err != nil {
		return nil, err
	}
	if state.Draft == nil || state.DraftStale || state.Draft.PasswordHash == "" {
		return nil, ErrSetupPasswordDraftRequired
	}
	if state.Draft.TOTPSecret == "" {
		return nil, ErrSetupTOTPDraftRequired
	}
	return setupTOTPEnrollment(state.Draft.EmailNormalized, state.Draft.TOTPSecret, state.TOTPReady)
}

// StartSetupTOTP creates or deliberately replaces a TOTP seed, then stores it
// only inside the encrypted challenge-bound owner draft. Passing replace=false
// preserves an existing seed so ordinary retries do not silently rotate it.
func (m *Manager) StartSetupTOTP(ctx context.Context, token, origin string, replace bool) (*SetupTOTPEnrollment, error) {
	preflight, err := m.GetSetupOwnerState(ctx, token, origin)
	if err != nil {
		return nil, err
	}
	if preflight.Draft == nil || preflight.DraftStale || preflight.Draft.PasswordHash == "" {
		return nil, ErrSetupPasswordDraftRequired
	}
	var newSecret string
	if preflight.Draft.TOTPSecret == "" || replace {
		randomMaterial, err := m.tokens.Token(32)
		if err != nil {
			return nil, fmt.Errorf("generate setup TOTP secret: %w", err)
		}
		key, err := newTOTPKey(preflight.Draft.EmailNormalized, randomMaterial)
		if err != nil {
			return nil, err
		}
		newSecret = key.Secret()
	}

	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, ErrSetupAccessInvalid
	}
	now := m.clock.Now().UTC()
	var savedDraft *SetupOwnerDraft
	err = m.runSecurityTransition(ctx, SecurityTransitionSetup, func(tx *sql.Tx) error {
		challenge, draft, _, err := m.currentSetupSecurityDraft(ctx, tx, token, canonicalOrigin, now)
		if err != nil {
			return err
		}
		if !sameSetupSecurityDraft(draft, preflight.Draft) {
			return ErrSetupPasswordDraftRequired
		}
		if draft.TOTPSecret != "" && !replace {
			savedDraft = draft
			return nil
		}
		draft.TOTPSecret = newSecret
		draft.TOTPConfirmedStep = nil
		payload, err := m.encryptSetupOwnerDraft(challenge, draft)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET payload_ciphertext = ?
			WHERE id = ? AND challenge_hash = ? AND purpose = ? AND origin = ?
			  AND user_id IS NULL AND session_id IS NULL AND consumed_at IS NULL
			  AND expires_at > ? AND attempts < max_attempts`,
			payload, challenge.ID, hashToken(token), ChallengePurposeEnrollment, canonicalOrigin, now,
		)
		if err != nil {
			return fmt.Errorf("store setup TOTP draft: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read setup TOTP draft update result: %w", err)
		}
		if changed != 1 {
			return ErrSetupAccessInvalid
		}
		savedDraft = draft
		return nil
	})
	if err != nil {
		return nil, err
	}
	return setupTOTPEnrollment(savedDraft.EmailNormalized, savedDraft.TOTPSecret, savedDraft.TOTPConfirmedStep != nil)
}

// ConfirmSetupTOTP accepts a code only from the current, immediately previous,
// or immediately next 30-second step. It records the exact accepted step in
// the encrypted draft for replay-safe persistence during final enrollment.
func (m *Manager) ConfirmSetupTOTP(ctx context.Context, token, origin, code string) (*SetupOwnerState, error) {
	preflight, err := m.GetSetupOwnerState(ctx, token, origin)
	if err != nil {
		return nil, err
	}
	if preflight.Draft == nil || preflight.DraftStale || preflight.Draft.PasswordHash == "" {
		return nil, ErrSetupPasswordDraftRequired
	}
	if preflight.Draft.TOTPSecret == "" {
		return nil, ErrSetupTOTPDraftRequired
	}
	now := m.clock.Now().UTC()
	matchedStep, valid, err := matchTOTPCode(preflight.Draft.TOTPSecret, code, now)
	if err != nil {
		return nil, err
	}

	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, ErrSetupAccessInvalid
	}
	invalid := false
	blocked := false
	var savedState *SetupOwnerState
	err = m.runSecurityTransition(ctx, SecurityTransitionSetup, func(tx *sql.Tx) error {
		challenge, draft, topology, err := m.currentSetupSecurityDraft(ctx, tx, token, canonicalOrigin, now)
		if err != nil {
			return err
		}
		if !sameSetupTOTPIdentityDraft(draft, preflight.Draft) || draft.TOTPSecret == "" {
			return ErrSetupTOTPDraftRequired
		}
		if draft.TOTPConfirmedStep != nil && matchedStep <= *draft.TOTPConfirmedStep {
			valid = false
		}
		if !valid {
			invalid = true
			var consumedAt sql.NullTime
			err := tx.QueryRowContext(ctx, `
				UPDATE auth_challenges
				SET attempts = attempts + 1,
				    consumed_at = CASE WHEN attempts + 1 >= max_attempts THEN ? ELSE NULL END
				WHERE id = ? AND challenge_hash = ? AND purpose = ? AND origin = ?
				  AND user_id IS NULL AND session_id IS NULL AND consumed_at IS NULL
				  AND expires_at > ? AND attempts < max_attempts
				RETURNING consumed_at`,
				now, challenge.ID, hashToken(token), ChallengePurposeEnrollment, canonicalOrigin, now,
			).Scan(&consumedAt)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrSetupAccessInvalid
			}
			if err != nil {
				return fmt.Errorf("record rejected setup TOTP code: %w", err)
			}
			blocked = consumedAt.Valid
			return nil
		}
		draft.TOTPConfirmedStep = &matchedStep
		payload, err := m.encryptSetupOwnerDraft(challenge, draft)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET payload_ciphertext = ?, attempts = 0
			WHERE id = ? AND challenge_hash = ? AND purpose = ? AND origin = ?
			  AND user_id IS NULL AND session_id IS NULL AND consumed_at IS NULL
			  AND expires_at > ? AND attempts < max_attempts`,
			payload, challenge.ID, hashToken(token), ChallengePurposeEnrollment, canonicalOrigin, now,
		)
		if err != nil {
			return fmt.Errorf("confirm setup TOTP draft: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read setup TOTP confirmation result: %w", err)
		}
		if changed != 1 {
			return ErrSetupAccessInvalid
		}
		savedState = &SetupOwnerState{
			Topology: topology, Draft: draft, PasswordReady: true,
			TOTPStarted: true, TOTPReady: true,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if blocked {
		return nil, ErrSetupAccessInvalid
	}
	if invalid {
		return nil, &SetupTOTPValidationError{Fields: map[string]string{
			"code": "That authenticator code is invalid or has already been used.",
		}}
	}
	return savedState, nil
}

func (m *Manager) currentSetupSecurityDraft(ctx context.Context, tx *sql.Tx, token, origin string, now time.Time) (*PreAuthChallenge, *SetupOwnerDraft, SetupOwnerTopology, error) {
	challenge, err := activeSetupAccessInTransaction(ctx, tx, token, origin, now)
	if err != nil {
		return nil, nil, SetupOwnerTopology{}, err
	}
	if len(challenge.PayloadCiphertext) == 0 {
		return nil, nil, SetupOwnerTopology{}, ErrSetupOwnerDraftRequired
	}
	draft, err := m.decryptSetupOwnerDraft(challenge, challenge.PayloadCiphertext)
	if err != nil {
		return nil, nil, SetupOwnerTopology{}, err
	}
	topology, err := loadSetupOwnerTopology(ctx, tx)
	if err != nil {
		return nil, nil, SetupOwnerTopology{}, err
	}
	if draft.TopologyFingerprint != topology.fingerprint || validateSetupOwnerTarget(topology, draft.Mode, draft.TargetUserID) != nil {
		return nil, nil, SetupOwnerTopology{}, ErrSetupOwnerDraftRequired
	}
	fieldErrors, err := setupOwnerIdentifierCollisions(ctx, tx, draft)
	if err != nil {
		return nil, nil, SetupOwnerTopology{}, err
	}
	if len(fieldErrors) > 0 {
		return nil, nil, SetupOwnerTopology{}, ErrSetupOwnerDraftRequired
	}
	if draft.PasswordHash == "" {
		return nil, nil, SetupOwnerTopology{}, ErrSetupPasswordDraftRequired
	}
	return challenge, draft, topology, nil
}

func sameSetupSecurityDraft(left, right *SetupOwnerDraft) bool {
	if left == nil || right == nil || !sameSetupOwnerProfile(left, right) {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(left.PasswordHash), []byte(right.PasswordHash)) != 1 ||
		subtle.ConstantTimeCompare([]byte(left.TOTPSecret), []byte(right.TOTPSecret)) != 1 {
		return false
	}
	if left.TOTPConfirmedStep == nil || right.TOTPConfirmedStep == nil {
		return left.TOTPConfirmedStep == nil && right.TOTPConfirmedStep == nil
	}
	return *left.TOTPConfirmedStep == *right.TOTPConfirmedStep
}

func sameSetupTOTPIdentityDraft(left, right *SetupOwnerDraft) bool {
	if left == nil || right == nil || !sameSetupOwnerProfile(left, right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left.PasswordHash), []byte(right.PasswordHash)) == 1 &&
		subtle.ConstantTimeCompare([]byte(left.TOTPSecret), []byte(right.TOTPSecret)) == 1
}
