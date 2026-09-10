package auth

import (
	"bytes"
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
	"unicode"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

const (
	passkeyRegistrationDraftVersion = 1
	passkeyRegistrationDraftKey     = "gofer/auth/passkey-registration-draft/v1"
	passkeyRegistrationMaxBodyBytes = 1 << 20
	passkeyNameMaximumRunes         = 64
	passkeyCredentialIDMaximumBytes = 1023
	passkeyPublicKeyMaximumBytes    = 16 << 10
	passkeyRecordMaximumBytes       = 1 << 20
)

var (
	ErrPasskeyNameInvalid         = errors.New("passkey name is invalid")
	ErrPasskeyRegistrationInvalid = errors.New("passkey registration is invalid or expired")
	ErrPasskeyDuplicate           = errors.New("that passkey is already registered")
	ErrPasskeyNotFound            = errors.New("passkey not found")
)

type PasskeyRegistrationValidationError struct{}

func (*PasskeyRegistrationValidationError) Error() string {
	return "passkey registration response is invalid"
}

type PasskeyRegistrationOptions struct {
	Challenge    *PreAuthChallenge
	CreationJSON json.RawMessage
}

type PasskeyCredentialSummary struct {
	ID             string
	Name           string
	Attachment     string
	Transports     []string
	BackupEligible bool
	BackupState    bool
	CreatedAt      time.Time
	LastUsedAt     *time.Time
	CanRemove      bool
	RemoveReason   string
}

type passkeyRegistrationDraft struct {
	Version     int             `json:"version"`
	AuthVersion int64           `json:"auth_version"`
	Name        string          `json:"name"`
	RPID        string          `json:"rp_id"`
	UserHandle  []byte          `json:"user_handle"`
	SessionJSON json.RawMessage `json:"session"`
}

func (m *Manager) StartPasskeyRegistration(ctx context.Context, sessionToken, origin, name string) (*PasskeyRegistrationOptions, error) {
	session, err := m.GetSessionByToken(ctx, sessionToken)
	if err != nil || session == nil {
		return nil, ErrSecuritySessionInvalid
	}
	now := m.clock.Now().UTC()
	if err := m.requireRecentSecurityStepUp(ctx, session, now); err != nil {
		return nil, err
	}
	name, err = normalizePasskeyName(name)
	if err != nil {
		return nil, err
	}
	canonicalOrigin, rpID, err := canonicalWebAuthnRelyingParty(origin)
	if err != nil {
		return nil, fmt.Errorf("validate passkey relying party: %w", err)
	}
	user, err := m.loadPasskeyUser(ctx, session.UserID, rpID)
	if err != nil {
		return nil, err
	}
	if user.AuthVersion != session.AuthVersion || !user.Status.AllowsAuthentication() {
		return nil, ErrSecuritySessionInvalid
	}
	if len(user.WebAuthn.ID) == 0 {
		user.WebAuthn.ID = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, user.WebAuthn.ID); err != nil {
			return nil, fmt.Errorf("generate WebAuthn user handle: %w", err)
		}
	}
	registration, err := m.passkeyRegistrationFactory(canonicalOrigin)
	if err != nil {
		return nil, err
	}
	creationJSON, sessionJSON, err := registration.Begin(user.WebAuthn)
	if err != nil {
		return nil, fmt.Errorf("begin passkey registration: %w", err)
	}
	if len(creationJSON) == 0 || len(sessionJSON) == 0 || len(sessionJSON) > passkeyRecordMaximumBytes {
		return nil, fmt.Errorf("WebAuthn registration adapter returned invalid state")
	}
	challengeID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate passkey registration challenge ID: %w", err)
	}
	challengeToken, err := m.tokens.Token(32)
	if err != nil {
		return nil, fmt.Errorf("generate passkey registration challenge token: %w", err)
	}
	challenge := &PreAuthChallenge{
		ID: challengeID, Token: challengeToken, UserID: session.UserID, SessionID: session.ID,
		Purpose: ChallengePurposeEnrollment, Origin: canonicalOrigin, MaxAttempts: 1,
		CreatedAt: now, ExpiresAt: now.Add(passkeyCeremonyLifetime),
	}
	draft := &passkeyRegistrationDraft{
		Version: passkeyRegistrationDraftVersion, AuthVersion: session.AuthVersion,
		Name: name, RPID: rpID, UserHandle: append([]byte(nil), user.WebAuthn.ID...),
		SessionJSON: append(json.RawMessage(nil), sessionJSON...),
	}
	err = m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		current, err := m.currentSecuritySession(ctx, tx, sessionToken, now, true)
		if err != nil || current.ID != session.ID || current.AuthVersion != session.AuthVersion {
			return ErrSecuritySessionInvalid
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET consumed_at = COALESCE(consumed_at, ?), payload_ciphertext = NULL
			WHERE user_id = ? AND session_id = ? AND purpose = ? AND consumed_at IS NULL`,
			now, current.UserID, current.ID, ChallengePurposeEnrollment,
		); err != nil {
			return fmt.Errorf("terminate prior credential enrollment: %w", err)
		}
		payload, err := m.encryptPasskeyRegistrationDraft(challenge, draft)
		if err != nil {
			return err
		}
		challenge.PayloadCiphertext = payload
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_challenges (
				id, user_id, session_id, challenge_hash, purpose, origin, attempts,
				max_attempts, payload_ciphertext, created_at, expires_at
			) VALUES (?, ?, ?, ?, ?, ?, 0, 1, ?, ?, ?)`,
			challenge.ID, challenge.UserID, challenge.SessionID, hashToken(challenge.Token),
			challenge.Purpose, challenge.Origin, challenge.PayloadCiphertext,
			challenge.CreatedAt, challenge.ExpiresAt,
		); err != nil {
			return fmt.Errorf("insert passkey registration challenge: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &PasskeyRegistrationOptions{Challenge: challenge, CreationJSON: creationJSON}, nil
}

func (m *Manager) FinishPasskeyRegistration(ctx context.Context, challengeToken, sessionToken, origin string, responseJSON []byte, userAgent string) (*PasskeyCredentialSummary, error) {
	if len(responseJSON) == 0 || len(responseJSON) > passkeyRegistrationMaxBodyBytes {
		return nil, &PasskeyRegistrationValidationError{}
	}
	canonicalOrigin, rpID, err := canonicalWebAuthnRelyingParty(origin)
	if err != nil {
		return nil, ErrPasskeyRegistrationInvalid
	}
	challenge, draft, session, err := m.readPasskeyRegistrationDraft(ctx, challengeToken, sessionToken, canonicalOrigin)
	if err != nil {
		return nil, err
	}
	if draft.RPID != rpID {
		return nil, ErrPasskeyRegistrationInvalid
	}
	loaded, err := m.loadPasskeyUser(ctx, session.UserID, rpID)
	if err != nil {
		return nil, err
	}
	if loaded.AuthVersion != session.AuthVersion {
		return nil, ErrPasskeyRegistrationInvalid
	}
	if len(loaded.WebAuthn.ID) == 0 {
		loaded.WebAuthn.ID = append([]byte(nil), draft.UserHandle...)
	} else if !bytes.Equal(loaded.WebAuthn.ID, draft.UserHandle) {
		return nil, ErrPasskeyRegistrationInvalid
	}
	registration, err := m.passkeyRegistrationFactory(canonicalOrigin)
	if err != nil {
		return nil, err
	}
	record, verifyErr := registration.Finish(loaded.WebAuthn, draft.SessionJSON, responseJSON)
	if verifyErr != nil {
		if err := m.consumeRejectedPasskeyRegistration(ctx, challengeToken, sessionToken, canonicalOrigin, userAgent); err != nil {
			return nil, err
		}
		return nil, &PasskeyRegistrationValidationError{}
	}
	if err := validatePasskeyRegistrationRecord(record); err != nil {
		if consumeErr := m.consumeRejectedPasskeyRegistration(ctx, challengeToken, sessionToken, canonicalOrigin, userAgent); consumeErr != nil {
			return nil, consumeErr
		}
		return nil, &PasskeyRegistrationValidationError{}
	}
	rowID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate passkey credential ID: %w", err)
	}
	eventID, err := m.tokens.ID()
	if err != nil {
		return nil, fmt.Errorf("generate passkey registration event ID: %w", err)
	}
	ciphertext, err := m.encryptPasskeyCredential(session.UserID, rowID, record.Record)
	if err != nil {
		return nil, err
	}
	transportsJSON, err := json.Marshal(record.Transports)
	if err != nil {
		return nil, fmt.Errorf("encode passkey transports: %w", err)
	}
	now := m.clock.Now().UTC()
	userAgent = boundedUserAgent(userAgent)
	summary := &PasskeyCredentialSummary{
		ID: rowID, Name: draft.Name, Attachment: record.Attachment,
		Transports: append([]string(nil), record.Transports...), BackupEligible: record.BackupEligible,
		BackupState: record.BackupState, CreatedAt: now,
	}
	duplicate := false
	err = m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		currentChallenge, currentDraft, current, err := m.currentPasskeyRegistrationDraft(
			ctx, tx, challengeToken, sessionToken, canonicalOrigin, now,
		)
		if err != nil || currentChallenge.ID != challenge.ID || current.ID != session.ID ||
			!samePasskeyRegistrationDraft(currentDraft, draft) {
			return ErrPasskeyRegistrationInvalid
		}
		var existing string
		err = tx.QueryRowContext(ctx, `SELECT user_id FROM webauthn_credentials WHERE credential_id = ?`, record.CredentialID).Scan(&existing)
		if err == nil {
			duplicate = true
			result, err := tx.ExecContext(ctx, `
				UPDATE auth_challenges SET attempts = max_attempts, consumed_at = ?, payload_ciphertext = NULL
				WHERE id = ? AND challenge_hash = ? AND consumed_at IS NULL AND expires_at > ?`,
				now, challenge.ID, hashToken(challengeToken), now,
			)
			if err != nil {
				return fmt.Errorf("consume duplicate passkey registration: %w", err)
			}
			changed, err := result.RowsAffected()
			if err != nil || changed != 1 {
				return ErrPasskeyRegistrationInvalid
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO auth_events (
					id, occurred_at, actor_user_id, subject_user_id, session_id,
					event_type, success, reason, user_agent, metadata_json
				) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, '{"action":"add","factor":"passkey","result":"duplicate"}')`,
				eventID, now, current.UserID, current.UserID, current.ID,
				AuthEventCredentialChanged, AuthEventReasonInvalidCredentials, userAgent,
			); err != nil {
				return fmt.Errorf("record duplicate passkey registration: %w", err)
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check duplicate passkey credential: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO webauthn_users (user_id, rp_id, user_handle, created_at)
			VALUES (?, ?, ?, ?)`, current.UserID, rpID, draft.UserHandle, now,
		); err != nil {
			return fmt.Errorf("persist WebAuthn user handle: %w", err)
		}
		var storedHandle []byte
		if err := tx.QueryRowContext(ctx, `
			SELECT user_handle FROM webauthn_users WHERE user_id = ? AND rp_id = ?`,
			current.UserID, rpID,
		).Scan(&storedHandle); err != nil || !bytes.Equal(storedHandle, draft.UserHandle) {
			return ErrPasskeyRegistrationInvalid
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO webauthn_credentials (
				id, user_id, credential_id, public_key, credential_ciphertext, key_version,
				sign_count, aaguid, transports, attachment, backup_eligible,
				backup_state, name, created_at, rp_id, flags, clone_warning
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			rowID, current.UserID, record.CredentialID, record.PublicKey, ciphertext,
			passkeyCredentialKeyVersion, record.SignCount, nullableBytes(record.AAGUID), string(transportsJSON),
			record.Attachment, boolInt(record.BackupEligible), boolInt(record.BackupState), draft.Name, now,
			rpID, record.Flags, boolInt(record.CloneWarning),
		); err != nil {
			return fmt.Errorf("persist passkey credential: %w", err)
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET attempts = max_attempts, consumed_at = ?, payload_ciphertext = NULL
			WHERE id = ? AND challenge_hash = ? AND consumed_at IS NULL AND expires_at > ?`,
			now, challenge.ID, hashToken(challengeToken), now,
		)
		if err != nil {
			return fmt.Errorf("consume passkey registration challenge: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return ErrPasskeyRegistrationInvalid
		}

		newAuthVersion, err := advanceSecurityAuthVersion(ctx, tx, current.UserID, current.AuthVersion, now)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
            UPDATE sessions SET revoked_at = ?, revoked_by = ?, revocation_reason = ?
            WHERE user_id = ? AND id != ? AND revoked_at IS NULL`,
			now, current.UserID, SessionRevocationCredentialReset, current.UserID, current.ID,
		); err != nil {
			return fmt.Errorf("revoke other sessions after passkey enrollment: %w", err)
		}
		// Registration verifies possession and user verification for the new key.
		// Preserve the current bearer while invalidating older login challenges.
		if _, err := tx.ExecContext(ctx, `
            UPDATE sessions SET auth_version = ?, assurance_level = ?, step_up_at = ?, step_up_method = ?
            WHERE id = ? AND revoked_at IS NULL`,
			newAuthVersion, AssuranceLevelPhishingResistant, now, AuthenticationMethodPasskey, current.ID,
		); err != nil {
			return fmt.Errorf("strengthen session after passkey enrollment: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, '{"action":"added","factor":"passkey"}')`,
			eventID, now, current.UserID, current.UserID, current.ID,
			AuthEventCredentialChanged, AuthEventReasonChallengeVerified, userAgent,
		); err != nil {
			return fmt.Errorf("record passkey registration: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if duplicate {
		return nil, ErrPasskeyDuplicate
	}
	return summary, nil
}

func (m *Manager) ListPasskeys(ctx context.Context, sessionToken string) ([]PasskeyCredentialSummary, error) {
	session, err := m.GetSessionByToken(ctx, sessionToken)
	if err != nil || session == nil {
		return nil, ErrSecuritySessionInvalid
	}
	return m.listPasskeysForUser(ctx, session.UserID)
}

func (m *Manager) RemovePasskey(ctx context.Context, sessionToken, passkeyID, userAgent string) (*Session, error) {
	session, err := m.GetSessionByToken(ctx, sessionToken)
	if err != nil || session == nil {
		return nil, ErrSecuritySessionInvalid
	}
	now := m.clock.Now().UTC()
	if err := m.requireRecentSecurityStepUp(ctx, session, now); err != nil {
		return nil, err
	}
	_, rpID, err := canonicalWebAuthnRelyingParty(m.config.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("validate passkey removal relying party: %w", err)
	}
	passkeyID = strings.TrimSpace(passkeyID)
	if passkeyID == "" {
		return nil, ErrPasskeyNotFound
	}
	newSessionID, newSessionToken, eventID, err := m.securityRotationMaterial()
	if err != nil {
		return nil, err
	}
	userAgent = boundedUserAgent(userAgent)
	var rotated *Session
	err = m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		current, err := m.currentSecuritySession(ctx, tx, sessionToken, now, true)
		if err != nil || current.ID != session.ID {
			return ErrSecuritySessionInvalid
		}
		var owned int
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM webauthn_credentials
			WHERE id = ? AND user_id = ? AND revoked_at IS NULL)`, passkeyID, current.UserID,
		).Scan(&owned); err != nil {
			return fmt.Errorf("load removable passkey: %w", err)
		}
		if owned != 1 {
			return ErrPasskeyNotFound
		}
		canRemove, err := canRemovePasskeyInTransaction(
			ctx, tx, current.UserID, passkeyID, rpID, m.configuredFederatedLoginAvailability(),
		)
		if err != nil {
			return err
		}
		if !canRemove {
			return ErrLastAuthenticator
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE webauthn_credentials SET revoked_at = ?
			WHERE id = ? AND user_id = ? AND revoked_at IS NULL`, now, passkeyID, current.UserID,
		)
		if err != nil {
			return fmt.Errorf("revoke passkey credential: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return ErrPasskeyNotFound
		}
		newAuthVersion, err := advanceSecurityAuthVersion(ctx, tx, current.UserID, current.AuthVersion, now)
		if err != nil {
			return err
		}
		if err := revokeSecuritySessions(ctx, tx, current.UserID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET consumed_at = COALESCE(consumed_at, ?), payload_ciphertext = NULL
			WHERE user_id = ? AND session_id IS NOT NULL AND consumed_at IS NULL`, now, current.UserID,
		); err != nil {
			return fmt.Errorf("terminate passkey-management challenges: %w", err)
		}
		rotated, err = insertRotatedSecuritySession(
			ctx, tx, current, newAuthVersion, newSessionID, newSessionToken, userAgent,
			current.StepUpMethod, now,
		)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, '{"action":"removed","factor":"passkey"}')`,
			eventID, now, current.UserID, current.UserID, rotated.ID,
			AuthEventCredentialChanged, AuthEventReasonChallengeVerified, userAgent,
		); err != nil {
			return fmt.Errorf("record passkey removal: %w", err)
		}
		return nil
	})
	return rotated, err
}

type loadedPasskeyUser struct {
	WebAuthn    passkeyUser
	AuthVersion int64
	Status      UserStatus
}

func (m *Manager) loadPasskeyUser(ctx context.Context, userID, rpID string) (*loadedPasskeyUser, error) {
	user, err := m.GetUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("load passkey user: %w", err)
	}
	if user == nil || !user.Status.AllowsAuthentication() {
		return nil, ErrSecuritySessionInvalid
	}
	var userHandle []byte
	if err := m.db.Read().QueryRowContext(ctx, `
		SELECT user_handle FROM webauthn_users WHERE user_id = ? AND rp_id = ?`, user.ID, rpID,
	).Scan(&userHandle); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load WebAuthn user handle: %w", err)
	}
	rows, err := m.db.Read().QueryContext(ctx, `
		SELECT id, credential_id, public_key, credential_ciphertext, key_version,
		       sign_count, COALESCE(aaguid, x''), transports, attachment,
		       backup_eligible, backup_state, flags, clone_warning
		FROM webauthn_credentials
		WHERE user_id = ? AND revoked_at IS NULL AND (rp_id = ? OR rp_id IS NULL)
		ORDER BY created_at, id`, user.ID, rpID)
	if err != nil {
		return nil, fmt.Errorf("load passkey credentials: %w", err)
	}
	defer rows.Close()
	credentials := make([]webauthn.Credential, 0)
	for rows.Next() {
		var rowID, transportsJSON, attachment string
		var credentialID, publicKey, ciphertext, aaguid []byte
		var keyVersion sql.NullInt64
		var signCount uint32
		var backupEligible, backupState, rawFlags, cloneWarning int
		if err := rows.Scan(
			&rowID, &credentialID, &publicKey, &ciphertext, &keyVersion, &signCount,
			&aaguid, &transportsJSON, &attachment, &backupEligible, &backupState, &rawFlags, &cloneWarning,
		); err != nil {
			return nil, fmt.Errorf("scan passkey credential: %w", err)
		}
		if keyVersion.Valid && len(ciphertext) > 0 {
			credential, err := m.decryptPasskeyCredential(user.ID, rowID, ciphertext, int(keyVersion.Int64))
			if err != nil {
				return nil, err
			}
			if err := validatePasskeyCredentialBinding(credential, credentialID, publicKey); err != nil {
				return nil, err
			}
			if byte(credential.Flags.ProtocolValue()) != byte(rawFlags) ||
				credential.Authenticator.CloneWarning != (cloneWarning == 1) {
				return nil, fmt.Errorf("passkey credential record does not match authenticator metadata")
			}
			credentials = append(credentials, *credential)
			continue
		}
		var transports []protocol.AuthenticatorTransport
		var rawTransports []string
		if err := json.Unmarshal([]byte(transportsJSON), &rawTransports); err != nil {
			return nil, fmt.Errorf("decode passkey transports: %w", err)
		}
		for _, transport := range rawTransports {
			transports = append(transports, protocol.AuthenticatorTransport(transport))
		}
		credentials = append(credentials, webauthn.Credential{
			ID: credentialID, PublicKey: publicKey, Transport: transports,
			Flags: webauthn.NewCredentialFlags(protocol.AuthenticatorFlags(byte(rawFlags))),
			Authenticator: webauthn.Authenticator{
				AAGUID: aaguid, SignCount: signCount,
				CloneWarning: cloneWarning == 1,
				Attachment:   protocol.AuthenticatorAttachment(attachment),
			},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate passkey credentials: %w", err)
	}
	name := strings.TrimSpace(user.Username)
	if name == "" {
		name = user.ID
	}
	displayName := strings.TrimSpace(user.Name)
	if displayName == "" {
		displayName = name
	}
	return &loadedPasskeyUser{
		WebAuthn:    passkeyUser{ID: userHandle, Name: name, DisplayName: displayName, Credentials: credentials},
		AuthVersion: user.AuthVersion, Status: user.Status,
	}, nil
}

func (m *Manager) listPasskeysForUser(ctx context.Context, userID string) ([]PasskeyCredentialSummary, error) {
	_, rpID, err := canonicalWebAuthnRelyingParty(m.config.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("validate passkey listing relying party: %w", err)
	}
	rows, err := m.db.Read().QueryContext(ctx, `
		SELECT id, name, attachment, transports, backup_eligible, backup_state,
		       created_at, last_used_at
		FROM webauthn_credentials
		WHERE user_id = ? AND revoked_at IS NULL
		ORDER BY created_at, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list passkeys: %w", err)
	}
	defer rows.Close()
	var passkeys []PasskeyCredentialSummary
	for rows.Next() {
		var passkey PasskeyCredentialSummary
		var transportsJSON string
		var backupEligible, backupState int
		var lastUsedAt sql.NullTime
		if err := rows.Scan(
			&passkey.ID, &passkey.Name, &passkey.Attachment, &transportsJSON,
			&backupEligible, &backupState, &passkey.CreatedAt, &lastUsedAt,
		); err != nil {
			return nil, fmt.Errorf("scan passkey summary: %w", err)
		}
		if err := json.Unmarshal([]byte(transportsJSON), &passkey.Transports); err != nil {
			return nil, fmt.Errorf("decode passkey summary transports: %w", err)
		}
		passkey.BackupEligible = backupEligible == 1
		passkey.BackupState = backupState == 1
		if lastUsedAt.Valid {
			value := lastUsedAt.Time
			passkey.LastUsedAt = &value
		}
		passkeys = append(passkeys, passkey)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate passkey summaries: %w", err)
	}
	for index := range passkeys {
		canRemove, err := m.canRemovePasskey(ctx, userID, passkeys[index].ID, rpID)
		if err != nil {
			return nil, err
		}
		passkeys[index].CanRemove = canRemove
		if !canRemove {
			policy, err := queryAuthenticationPolicy(ctx, m.db.Read(), userID, 0)
			if err != nil {
				return nil, err
			}
			if policy.MFAEnrollmentRequired {
				passkeys[index].RemoveReason = "Your administrator policy requires at least one MFA method to remain enabled. Add another TOTP authenticator app or passkey before removing this one."
			} else {
				passkeys[index].RemoveReason = "Keep at least one sign-in method available. Add another sign-in method before removing this passkey."
			}
		}
	}
	return passkeys, nil
}

func (m *Manager) canRemovePasskey(ctx context.Context, userID, passkeyID, rpID string) (bool, error) {
	tx, err := m.db.Read().BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	return canRemovePasskeyInTransaction(ctx, tx, userID, passkeyID, rpID, m.configuredFederatedLoginAvailability())
}

func canRemovePasskeyInTransaction(
	ctx context.Context,
	tx *sql.Tx,
	userID string,
	passkeyID string,
	rpID string,
	availability federatedLoginAvailability,
) (bool, error) {
	return canRemoveAuthenticatorInTransaction(
		ctx, tx, userID, rpID, availability, authenticatorRemoval{PasskeyID: passkeyID},
	)
}

func (m *Manager) readPasskeyRegistrationDraft(ctx context.Context, challengeToken, sessionToken, origin string) (*PreAuthChallenge, *passkeyRegistrationDraft, *Session, error) {
	tx, err := m.db.Read().BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("begin passkey registration read: %w", err)
	}
	defer tx.Rollback()
	challenge, draft, session, err := m.currentPasskeyRegistrationDraft(
		ctx, tx, challengeToken, sessionToken, origin, m.clock.Now().UTC(),
	)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, nil, fmt.Errorf("commit passkey registration read: %w", err)
	}
	return challenge, draft, session, nil
}

func (m *Manager) currentPasskeyRegistrationDraft(ctx context.Context, tx *sql.Tx, challengeToken, sessionToken, origin string, now time.Time) (*PreAuthChallenge, *passkeyRegistrationDraft, *Session, error) {
	if strings.TrimSpace(challengeToken) == "" || strings.TrimSpace(sessionToken) == "" {
		return nil, nil, nil, ErrPasskeyRegistrationInvalid
	}
	session, err := m.currentSecuritySession(ctx, tx, sessionToken, now, true)
	if err != nil {
		return nil, nil, nil, err
	}
	challenge, err := scanPreAuthChallenge(tx.QueryRowContext(ctx, preAuthChallengeSelect+`
		WHERE challenge_hash = ? AND purpose = ? AND origin = ?
		  AND user_id = ? AND session_id = ? AND consumed_at IS NULL
		  AND expires_at > ? AND attempts < max_attempts`,
		hashToken(challengeToken), ChallengePurposeEnrollment, origin,
		session.UserID, session.ID, now,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil, ErrPasskeyRegistrationInvalid
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load passkey registration challenge: %w", err)
	}
	draft, err := m.decryptPasskeyRegistrationDraft(challenge, challenge.PayloadCiphertext)
	if err != nil || draft.Version != passkeyRegistrationDraftVersion ||
		draft.AuthVersion != session.AuthVersion || draft.Name == "" || draft.RPID == "" ||
		len(draft.UserHandle) < 16 || len(draft.UserHandle) > 64 || len(draft.SessionJSON) == 0 {
		return nil, nil, nil, ErrPasskeyRegistrationInvalid
	}
	return challenge, draft, session, nil
}

func (m *Manager) consumeRejectedPasskeyRegistration(ctx context.Context, challengeToken, sessionToken, origin, userAgent string) error {
	eventID, err := m.tokens.ID()
	if err != nil {
		return fmt.Errorf("generate rejected passkey event ID: %w", err)
	}
	now := m.clock.Now().UTC()
	userAgent = boundedUserAgent(userAgent)
	return m.runSecurityTransition(ctx, SecurityTransitionCredentialChange, func(tx *sql.Tx) error {
		challenge, _, session, err := m.currentPasskeyRegistrationDraft(ctx, tx, challengeToken, sessionToken, origin, now)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE auth_challenges SET attempts = max_attempts, consumed_at = ?, payload_ciphertext = NULL
			WHERE id = ? AND challenge_hash = ? AND consumed_at IS NULL`,
			now, challenge.ID, hashToken(challengeToken),
		)
		if err != nil {
			return fmt.Errorf("consume rejected passkey registration: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return ErrPasskeyRegistrationInvalid
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO auth_events (
				id, occurred_at, actor_user_id, subject_user_id, session_id,
				event_type, success, reason, user_agent, metadata_json
			) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, '{"action":"add","factor":"passkey"}')`,
			eventID, now, session.UserID, session.UserID, session.ID,
			AuthEventCredentialChanged, AuthEventReasonInvalidCredentials, userAgent,
		); err != nil {
			return fmt.Errorf("record rejected passkey registration: %w", err)
		}
		return nil
	})
}

func normalizePasskeyName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || !utf8.ValidString(value) || utf8.RuneCountInString(value) > passkeyNameMaximumRunes {
		return "", ErrPasskeyNameInvalid
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "", ErrPasskeyNameInvalid
		}
	}
	return value, nil
}

func validatePasskeyRegistrationRecord(record *passkeyCredentialRecord) error {
	if record == nil || len(record.CredentialID) == 0 || len(record.CredentialID) > passkeyCredentialIDMaximumBytes ||
		len(record.PublicKey) == 0 || len(record.PublicKey) > passkeyPublicKeyMaximumBytes ||
		len(record.Record) == 0 || len(record.Record) > passkeyRecordMaximumBytes ||
		(len(record.AAGUID) != 0 && len(record.AAGUID) != 16) ||
		(record.BackupState && !record.BackupEligible) {
		return ErrPasskeyRegistrationInvalid
	}
	switch record.Attachment {
	case "", string(protocol.Platform), string(protocol.CrossPlatform):
	default:
		return ErrPasskeyRegistrationInvalid
	}
	flags := protocol.AuthenticatorFlags(record.Flags)
	if !flags.HasUserPresent() || !flags.HasUserVerified() ||
		flags.HasBackupEligible() != record.BackupEligible || flags.HasBackupState() != record.BackupState {
		return ErrPasskeyRegistrationInvalid
	}
	for _, transport := range record.Transports {
		switch protocol.AuthenticatorTransport(transport) {
		case protocol.USB, protocol.NFC, protocol.BLE, protocol.SmartCard, protocol.Hybrid, protocol.Internal:
		default:
			return ErrPasskeyRegistrationInvalid
		}
	}
	return nil
}

func (m *Manager) passkeyRegistrationDraftAEAD() (cipher.AEAD, error) {
	if len(m.bucketHashKey) < minimumBucketHashKeyBytes {
		return nil, fmt.Errorf("passkey registration encryption key is unavailable")
	}
	deriver := hmac.New(sha256.New, m.bucketHashKey)
	_, _ = deriver.Write([]byte(passkeyRegistrationDraftKey))
	block, err := aes.NewCipher(deriver.Sum(nil))
	if err != nil {
		return nil, fmt.Errorf("create passkey registration cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func (m *Manager) encryptPasskeyRegistrationDraft(challenge *PreAuthChallenge, draft *passkeyRegistrationDraft) ([]byte, error) {
	plaintext, err := json.Marshal(draft)
	if err != nil {
		return nil, fmt.Errorf("encode passkey registration draft: %w", err)
	}
	aead, err := m.passkeyRegistrationDraftAEAD()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate passkey registration nonce: %w", err)
	}
	payload := []byte{passkeyRegistrationDraftVersion}
	payload = append(payload, nonce...)
	return aead.Seal(payload, nonce, plaintext, passkeyRegistrationDraftAAD(challenge)), nil
}

func (m *Manager) decryptPasskeyRegistrationDraft(challenge *PreAuthChallenge, payload []byte) (*passkeyRegistrationDraft, error) {
	aead, err := m.passkeyRegistrationDraftAEAD()
	if err != nil {
		return nil, err
	}
	if len(payload) < 1+aead.NonceSize()+aead.Overhead() || payload[0] != passkeyRegistrationDraftVersion {
		return nil, ErrPasskeyRegistrationInvalid
	}
	nonce := payload[1 : 1+aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, payload[1+aead.NonceSize():], passkeyRegistrationDraftAAD(challenge))
	if err != nil {
		return nil, ErrPasskeyRegistrationInvalid
	}
	var draft passkeyRegistrationDraft
	if err := json.Unmarshal(plaintext, &draft); err != nil {
		return nil, ErrPasskeyRegistrationInvalid
	}
	return &draft, nil
}

func passkeyRegistrationDraftAAD(challenge *PreAuthChallenge) []byte {
	return []byte(challenge.ID + "\x00" + challenge.UserID + "\x00" + challenge.SessionID + "\x00" + challenge.Origin + "\x00" + string(challenge.Purpose))
}

func samePasskeyRegistrationDraft(left, right *passkeyRegistrationDraft) bool {
	return left != nil && right != nil && left.Version == right.Version &&
		left.AuthVersion == right.AuthVersion && left.Name == right.Name && left.RPID == right.RPID &&
		bytes.Equal(left.UserHandle, right.UserHandle) &&
		string(left.SessionJSON) == string(right.SessionJSON)
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
