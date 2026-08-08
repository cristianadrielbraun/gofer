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
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	maximumSetupOwnerCandidates = 100
	setupOwnerDraftVersion      = 1
	setupOwnerDraftKeyContext   = "gofer/auth/setup-owner-draft/v1"
)

var (
	ErrSetupAccessInvalid      = errors.New("setup access is invalid or no longer active")
	ErrSetupOwnerBlocked       = errors.New("existing user records require local repair before owner setup can continue")
	ErrSetupOwnerDraftRequired = errors.New("a current owner profile draft is required")
)

type SetupOwnerMode string

const (
	SetupOwnerModeCreate   SetupOwnerMode = "create"
	SetupOwnerModeExisting SetupOwnerMode = "existing"
)

type SetupOwnerTopologyKind string

const (
	SetupOwnerTopologyFresh         SetupOwnerTopologyKind = "fresh"
	SetupOwnerTopologyLegacyDefault SetupOwnerTopologyKind = "legacy_default"
	SetupOwnerTopologyExisting      SetupOwnerTopologyKind = "existing"
)

type SetupOwnerCandidate struct {
	ID             string
	Email          string
	Username       string
	Name           string
	Status         UserStatus
	IsAdmin        bool
	MailboxCount   int64
	LegacySessions int64
	updatedAt      time.Time
}

type SetupOwnerTopology struct {
	Kind        SetupOwnerTopologyKind
	Candidates  []SetupOwnerCandidate
	fingerprint string
}

type SetupOwnerDraft struct {
	Mode                SetupOwnerMode `json:"mode"`
	TargetUserID        string         `json:"target_user_id,omitempty"`
	Name                string         `json:"name"`
	Username            string         `json:"username"`
	UsernameNormalized  string         `json:"username_normalized"`
	Email               string         `json:"email"`
	EmailNormalized     string         `json:"email_normalized"`
	TopologyFingerprint string         `json:"topology_fingerprint"`
	PasswordHash        string         `json:"password_hash,omitempty"`
}

type SetupOwnerState struct {
	Topology      SetupOwnerTopology
	Draft         *SetupOwnerDraft
	DraftStale    bool
	PasswordReady bool
}

type SetupOwnerDraftInput struct {
	Mode         SetupOwnerMode
	TargetUserID string
	Name         string
	Username     string
	Email        string
}

type SetupOwnerValidationError struct {
	Fields map[string]string
}

type SetupPasswordDraftInput struct {
	Password             string
	PasswordConfirmation string
}

type SetupPasswordValidationError struct {
	Fields map[string]string
}

func (e *SetupPasswordValidationError) Error() string {
	return "setup owner password is invalid"
}

func (e *SetupOwnerValidationError) Error() string {
	return "setup owner profile is invalid"
}

// GetSetupOwnerState returns the current migration choices and any encrypted
// owner draft belonging to the exact active setup challenge. It does not
// mutate user, role, credential, or setup-completion state.
func (m *Manager) GetSetupOwnerState(ctx context.Context, token, origin string) (*SetupOwnerState, error) {
	challenge, err := m.GetActiveSetupAccess(ctx, token, origin)
	if err != nil {
		return nil, err
	}
	if challenge == nil {
		return nil, ErrSetupAccessInvalid
	}
	topology, err := loadSetupOwnerTopology(ctx, m.db.Read())
	if err != nil {
		return nil, err
	}
	state := &SetupOwnerState{Topology: topology}
	if len(challenge.PayloadCiphertext) == 0 {
		return state, nil
	}
	draft, err := m.decryptSetupOwnerDraft(challenge, challenge.PayloadCiphertext)
	if err != nil {
		return nil, err
	}
	state.Draft = draft
	if draft.TopologyFingerprint != topology.fingerprint || validateSetupOwnerTarget(topology, draft.Mode, draft.TargetUserID) != nil {
		state.DraftStale = true
		return state, nil
	}
	state.PasswordReady = draft.PasswordHash != ""
	return state, nil
}

// SaveSetupOwnerDraft validates the selected existing user (or fresh-user
// choice), profile, and login identifiers against one current topology, then
// stores only an encrypted challenge-bound draft. No user row is changed.
func (m *Manager) SaveSetupOwnerDraft(ctx context.Context, token, origin string, input SetupOwnerDraftInput) (*SetupOwnerState, error) {
	prepared, validationErr := prepareSetupOwnerDraft(input)
	if validationErr != nil {
		return nil, validationErr
	}
	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, ErrSetupAccessInvalid
	}
	now := m.clock.Now().UTC()
	var savedState *SetupOwnerState
	err = m.runSecurityTransition(ctx, SecurityTransitionSetup, func(tx *sql.Tx) error {
		challenge, err := activeSetupAccessInTransaction(ctx, tx, token, canonicalOrigin, now)
		if err != nil {
			return err
		}
		topology, err := loadSetupOwnerTopology(ctx, tx)
		if err != nil {
			return err
		}
		if targetErr := validateSetupOwnerTarget(topology, prepared.Mode, prepared.TargetUserID); targetErr != nil {
			return targetErr
		}
		fieldErrors, err := setupOwnerIdentifierCollisions(ctx, tx, prepared)
		if err != nil {
			return err
		}
		if len(fieldErrors) > 0 {
			return &SetupOwnerValidationError{Fields: fieldErrors}
		}
		prepared.TopologyFingerprint = topology.fingerprint
		payload, err := m.encryptSetupOwnerDraft(challenge, prepared)
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
			return fmt.Errorf("store setup owner draft: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read setup owner draft update result: %w", err)
		}
		if changed != 1 {
			return ErrSetupAccessInvalid
		}
		savedState = &SetupOwnerState{Topology: topology, Draft: prepared}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return savedState, nil
}

// SaveSetupPasswordDraft validates and hashes an owner password, then replaces
// the encrypted setup payload while the exact owner draft and user topology
// remain current. It never writes password_credentials or any user row.
func (m *Manager) SaveSetupPasswordDraft(ctx context.Context, token, origin string, input SetupPasswordDraftInput) (*SetupOwnerState, error) {
	preflight, err := m.GetSetupOwnerState(ctx, token, origin)
	if err != nil {
		return nil, err
	}
	if preflight.Draft == nil || preflight.DraftStale {
		return nil, ErrSetupOwnerDraftRequired
	}
	if subtle.ConstantTimeCompare([]byte(input.Password), []byte(input.PasswordConfirmation)) != 1 {
		return nil, &SetupPasswordValidationError{Fields: map[string]string{
			"confirmation": "The password confirmation does not match.",
		}}
	}
	preparedPassword, err := PrepareNewPassword(input.Password, PasswordPolicyContext{
		Username: preflight.Draft.Username,
		Email:    preflight.Draft.Email,
	})
	if err != nil {
		return nil, &SetupPasswordValidationError{Fields: map[string]string{
			"password": err.Error(),
		}}
	}
	passwordHash, err := HashPassword(preparedPassword)
	if err != nil {
		return nil, fmt.Errorf("hash setup owner password: %w", err)
	}

	canonicalOrigin, err := canonicalAuthOrigin(origin)
	if err != nil || strings.TrimSpace(token) == "" {
		return nil, ErrSetupAccessInvalid
	}
	now := m.clock.Now().UTC()
	var savedState *SetupOwnerState
	err = m.runSecurityTransition(ctx, SecurityTransitionSetup, func(tx *sql.Tx) error {
		challenge, err := activeSetupAccessInTransaction(ctx, tx, token, canonicalOrigin, now)
		if err != nil {
			return err
		}
		if len(challenge.PayloadCiphertext) == 0 {
			return ErrSetupOwnerDraftRequired
		}
		draft, err := m.decryptSetupOwnerDraft(challenge, challenge.PayloadCiphertext)
		if err != nil {
			return err
		}
		topology, err := loadSetupOwnerTopology(ctx, tx)
		if err != nil {
			return err
		}
		if draft.TopologyFingerprint != topology.fingerprint || validateSetupOwnerTarget(topology, draft.Mode, draft.TargetUserID) != nil {
			return ErrSetupOwnerDraftRequired
		}
		fieldErrors, err := setupOwnerIdentifierCollisions(ctx, tx, draft)
		if err != nil {
			return err
		}
		if len(fieldErrors) > 0 {
			return ErrSetupOwnerDraftRequired
		}
		if !sameSetupOwnerProfile(draft, preflight.Draft) {
			return ErrSetupOwnerDraftRequired
		}
		draft.PasswordHash = passwordHash
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
			return fmt.Errorf("store setup owner password draft: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read setup owner password draft update result: %w", err)
		}
		if changed != 1 {
			return ErrSetupAccessInvalid
		}
		savedState = &SetupOwnerState{Topology: topology, Draft: draft, PasswordReady: true}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return savedState, nil
}

func sameSetupOwnerProfile(left, right *SetupOwnerDraft) bool {
	if left == nil || right == nil {
		return false
	}
	return left.Mode == right.Mode && left.TargetUserID == right.TargetUserID &&
		left.Name == right.Name && left.Username == right.Username &&
		left.UsernameNormalized == right.UsernameNormalized && left.Email == right.Email &&
		left.EmailNormalized == right.EmailNormalized && left.TopologyFingerprint == right.TopologyFingerprint
}

func prepareSetupOwnerDraft(input SetupOwnerDraftInput) (*SetupOwnerDraft, error) {
	fieldErrors := make(map[string]string)
	name, err := PrepareDisplayName(input.Name)
	if err != nil {
		fieldErrors["name"] = err.Error()
	}
	username, usernameNormalized, err := PrepareUsername(input.Username)
	if err != nil {
		fieldErrors["username"] = err.Error()
	}
	email, emailNormalized, err := PrepareEmail(input.Email)
	if err != nil {
		fieldErrors["email"] = err.Error()
	}
	if len(fieldErrors) > 0 {
		return nil, &SetupOwnerValidationError{Fields: fieldErrors}
	}
	return &SetupOwnerDraft{
		Mode: input.Mode, TargetUserID: strings.TrimSpace(input.TargetUserID),
		Name: name, Username: username, UsernameNormalized: usernameNormalized,
		Email: email, EmailNormalized: emailNormalized,
	}, nil
}

type setupOwnerQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type setupOwnerCandidateRecord struct {
	candidate                  SetupOwnerCandidate
	emailNormalized            string
	usernameNormalized         string
	passwords, passkeys, totps int64
	identities                 int64
}

type setupOwnerFingerprintRecord struct {
	Candidate          SetupOwnerCandidate `json:"candidate"`
	EmailNormalized    string              `json:"email_normalized"`
	UsernameNormalized string              `json:"username_normalized"`
	UpdatedAt          time.Time           `json:"updated_at"`
	Passwords          int64               `json:"passwords"`
	Passkeys           int64               `json:"passkeys"`
	TOTPs              int64               `json:"totps"`
	Identities         int64               `json:"identities"`
}

func loadSetupOwnerTopology(ctx context.Context, queryer setupOwnerQuerier) (SetupOwnerTopology, error) {
	rows, err := queryer.QueryContext(ctx, `
		SELECT users.id, users.email, COALESCE(users.email_normalized, ''),
		       COALESCE(users.username, ''), COALESCE(users.username_normalized, ''),
		       users.name, users.status, users.is_admin, users.updated_at,
		       (SELECT COUNT(*) FROM accounts WHERE accounts.user_id = users.id),
		       (SELECT COUNT(*) FROM sessions WHERE sessions.user_id = users.id AND sessions.authentication_method = 'legacy'),
		       (SELECT COUNT(*) FROM password_credentials WHERE password_credentials.user_id = users.id),
		       (SELECT COUNT(*) FROM webauthn_credentials WHERE webauthn_credentials.user_id = users.id AND webauthn_credentials.revoked_at IS NULL),
		       (SELECT COUNT(*) FROM totp_credentials WHERE totp_credentials.user_id = users.id AND totp_credentials.revoked_at IS NULL),
		       (SELECT COUNT(*) FROM auth_identities WHERE auth_identities.user_id = users.id)
		FROM users
		ORDER BY lower(trim(COALESCE(users.username, users.email))), users.id
		LIMIT ?`, maximumSetupOwnerCandidates+1)
	if err != nil {
		return SetupOwnerTopology{}, fmt.Errorf("list setup owner candidates: %w", err)
	}
	defer rows.Close()
	records := make([]setupOwnerCandidateRecord, 0)
	identifiers := make(map[string]string)
	for rows.Next() {
		var record setupOwnerCandidateRecord
		var isAdmin int
		if err := rows.Scan(
			&record.candidate.ID, &record.candidate.Email, &record.emailNormalized,
			&record.candidate.Username, &record.usernameNormalized, &record.candidate.Name,
			&record.candidate.Status, &isAdmin, &record.candidate.updatedAt,
			&record.candidate.MailboxCount, &record.candidate.LegacySessions,
			&record.passwords, &record.passkeys, &record.totps, &record.identities,
		); err != nil {
			return SetupOwnerTopology{}, fmt.Errorf("scan setup owner candidate: %w", err)
		}
		record.candidate.IsAdmin = isAdmin == 1
		for _, identifier := range []string{
			firstNonempty(record.emailNormalized, normalizeLoginIdentifier(record.candidate.Email)),
			firstNonempty(record.usernameNormalized, normalizeLoginIdentifier(record.candidate.Username)),
		} {
			if identifier == "" {
				continue
			}
			if existingID, found := identifiers[identifier]; found && existingID != record.candidate.ID {
				return SetupOwnerTopology{}, ErrSetupOwnerBlocked
			}
			identifiers[identifier] = record.candidate.ID
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return SetupOwnerTopology{}, fmt.Errorf("iterate setup owner candidates: %w", err)
	}
	if len(records) > maximumSetupOwnerCandidates {
		return SetupOwnerTopology{}, ErrSetupOwnerBlocked
	}
	topology := SetupOwnerTopology{Kind: SetupOwnerTopologyExisting, Candidates: make([]SetupOwnerCandidate, 0, len(records))}
	for _, record := range records {
		topology.Candidates = append(topology.Candidates, record.candidate)
	}
	if len(records) == 0 {
		topology.Kind = SetupOwnerTopologyFresh
	} else if len(records) == 1 && isLegacyDefaultOwnerCandidate(records[0]) {
		topology.Kind = SetupOwnerTopologyLegacyDefault
	}
	fingerprintRecords := make([]setupOwnerFingerprintRecord, 0, len(records))
	for _, record := range records {
		fingerprintRecords = append(fingerprintRecords, setupOwnerFingerprintRecord{
			Candidate: record.candidate, EmailNormalized: record.emailNormalized,
			UsernameNormalized: record.usernameNormalized, UpdatedAt: record.candidate.updatedAt,
			Passwords: record.passwords, Passkeys: record.passkeys, TOTPs: record.totps, Identities: record.identities,
		})
	}
	fingerprintPayload, err := json.Marshal(fingerprintRecords)
	if err != nil {
		return SetupOwnerTopology{}, fmt.Errorf("encode setup owner topology: %w", err)
	}
	fingerprint := sha256.Sum256(append([]byte(topology.Kind+"\x00"), fingerprintPayload...))
	topology.fingerprint = hex.EncodeToString(fingerprint[:])
	return topology, nil
}

func isLegacyDefaultOwnerCandidate(record setupOwnerCandidateRecord) bool {
	return record.candidate.ID == "default" &&
		normalizeLoginIdentifier(record.candidate.Email) == "local@gofer.local" &&
		strings.TrimSpace(record.candidate.Username) == "" &&
		strings.TrimSpace(record.candidate.Name) == "Local User" &&
		record.passwords == 0 && record.passkeys == 0 && record.totps == 0 && record.identities == 0
}

func validateSetupOwnerTarget(topology SetupOwnerTopology, mode SetupOwnerMode, targetUserID string) error {
	targetUserID = strings.TrimSpace(targetUserID)
	valid := false
	switch topology.Kind {
	case SetupOwnerTopologyFresh:
		valid = mode == SetupOwnerModeCreate && targetUserID == ""
	case SetupOwnerTopologyLegacyDefault:
		valid = mode == SetupOwnerModeExisting && targetUserID == "default"
	case SetupOwnerTopologyExisting:
		if mode == SetupOwnerModeCreate && targetUserID == "" {
			valid = true
		}
		if mode == SetupOwnerModeExisting && targetUserID != "" {
			for _, candidate := range topology.Candidates {
				if candidate.ID == targetUserID {
					valid = true
					break
				}
			}
		}
	}
	if valid {
		return nil
	}
	return &SetupOwnerValidationError{Fields: map[string]string{
		"target": "Choose which Gofer user should become the owner.",
	}}
}

func setupOwnerIdentifierCollisions(ctx context.Context, tx *sql.Tx, draft *SetupOwnerDraft) (map[string]string, error) {
	excludedID := draft.TargetUserID
	fields := make(map[string]string)
	for field, normalized := range map[string]string{
		"username": draft.UsernameNormalized,
		"email":    draft.EmailNormalized,
	} {
		var conflictingID string
		err := tx.QueryRowContext(ctx, `
			SELECT id FROM users
			WHERE id != ? AND (
				CASE
					WHEN trim(COALESCE(username_normalized, '')) != '' THEN username_normalized
					ELSE lower(trim(COALESCE(username, '')))
				END = ? OR
				CASE
					WHEN trim(COALESCE(email_normalized, '')) != '' THEN email_normalized
					ELSE lower(trim(email))
				END = ?
			)
			ORDER BY id LIMIT 1`, excludedID, normalized, normalized).Scan(&conflictingID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("check setup owner %s collision: %w", field, err)
		}
		fields[field] = "That " + field + " is already used by another Gofer user."
	}
	return fields, nil
}

func activeSetupAccessInTransaction(ctx context.Context, tx *sql.Tx, token, origin string, now time.Time) (*PreAuthChallenge, error) {
	challenge, err := scanPreAuthChallenge(tx.QueryRowContext(ctx, `
		SELECT challenge.id, '', '', challenge.purpose, challenge.origin,
		       challenge.attempts, challenge.max_attempts, challenge.payload_ciphertext,
		       challenge.created_at, challenge.expires_at, challenge.consumed_at
		FROM auth_challenges challenge
		JOIN auth_system_state setup ON setup.id = 1
		WHERE challenge.challenge_hash = ? AND challenge.purpose = ? AND challenge.origin = ?
		  AND challenge.user_id IS NULL AND challenge.session_id IS NULL
		  AND challenge.consumed_at IS NULL AND challenge.expires_at > ?
		  AND challenge.attempts < challenge.max_attempts
		  AND setup.initialized = 0 AND setup.setup_token_hash IS NOT NULL`,
		hashToken(token), ChallengePurposeEnrollment, origin, now,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSetupAccessInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("read setup owner access: %w", err)
	}
	return challenge, nil
}

func (m *Manager) setupOwnerAEAD() (cipher.AEAD, error) {
	if len(m.bucketHashKey) < minimumBucketHashKeyBytes {
		return nil, fmt.Errorf("setup owner draft encryption key is unavailable")
	}
	deriver := hmac.New(sha256.New, m.bucketHashKey)
	_, _ = deriver.Write([]byte(setupOwnerDraftKeyContext))
	key := deriver.Sum(nil)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create setup owner draft cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create setup owner draft AEAD: %w", err)
	}
	return aead, nil
}

func (m *Manager) encryptSetupOwnerDraft(challenge *PreAuthChallenge, draft *SetupOwnerDraft) ([]byte, error) {
	aead, err := m.setupOwnerAEAD()
	if err != nil {
		return nil, err
	}
	plaintext, err := json.Marshal(draft)
	if err != nil {
		return nil, fmt.Errorf("encode setup owner draft: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate setup owner draft nonce: %w", err)
	}
	payload := []byte{setupOwnerDraftVersion}
	payload = append(payload, nonce...)
	payload = aead.Seal(payload, nonce, plaintext, setupOwnerDraftAAD(challenge))
	return payload, nil
}

func (m *Manager) decryptSetupOwnerDraft(challenge *PreAuthChallenge, payload []byte) (*SetupOwnerDraft, error) {
	aead, err := m.setupOwnerAEAD()
	if err != nil {
		return nil, err
	}
	if len(payload) < 1+aead.NonceSize()+aead.Overhead() || payload[0] != setupOwnerDraftVersion {
		return nil, fmt.Errorf("setup owner draft payload is invalid")
	}
	nonce := payload[1 : 1+aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, payload[1+aead.NonceSize():], setupOwnerDraftAAD(challenge))
	if err != nil {
		return nil, fmt.Errorf("authenticate setup owner draft: %w", err)
	}
	var draft SetupOwnerDraft
	if err := json.Unmarshal(plaintext, &draft); err != nil {
		return nil, fmt.Errorf("decode setup owner draft: %w", err)
	}
	return &draft, nil
}

func setupOwnerDraftAAD(challenge *PreAuthChallenge) []byte {
	return []byte(challenge.ID + "\x00" + challenge.Origin + "\x00" + string(challenge.Purpose))
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
