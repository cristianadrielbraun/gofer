package auth

import (
	"context"
	"database/sql"
	"errors"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// ValidateUserStorageMigrationSecrets authenticates retained central credentials
// and challenge payloads with their original IDs and associated metadata. It does
// not initialize a Manager, run auth migrations, consume challenges, contact an
// identity provider or enable credentials. The caller holds the offline lock.
func ValidateUserStorageMigrationSecrets(ctx context.Context, source *storage.DB, key []byte) error {
	if ctx == nil || source == nil || len(key) != 32 {
		return errors.New("authentication migration verification requires source, context and application key")
	}
	codec := &Manager{bucketHashKey: key}
	tx, err := source.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT user_id,id,encrypted_seed,key_version FROM totp_credentials ORDER BY id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var owner, id string
		var ciphertext []byte
		var version int
		if err := rows.Scan(&owner, &id, &ciphertext, &version); err != nil {
			rows.Close()
			return err
		}
		if _, err := codec.decryptTOTPSeed(owner, id, ciphertext, version); err != nil {
			rows.Close()
			return errors.New("application key cannot authenticate a retained TOTP credential")
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT user_id,id,credential_id,public_key,credential_ciphertext,key_version FROM webauthn_credentials ORDER BY id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var owner, id string
		var credentialID, publicKey, ciphertext []byte
		var version sql.NullInt64
		if err := rows.Scan(&owner, &id, &credentialID, &publicKey, &ciphertext, &version); err != nil {
			rows.Close()
			return err
		}
		if ciphertext == nil && !version.Valid {
			continue // Legacy public metadata is sealed by ordinary startup later.
		}
		if ciphertext == nil || !version.Valid {
			rows.Close()
			return errors.New("retained passkey encryption metadata is incomplete")
		}
		credential, err := codec.decryptPasskeyCredential(owner, id, ciphertext, int(version.Int64))
		if err != nil || validatePasskeyCredentialBinding(credential, credentialID, publicKey) != nil {
			rows.Close()
			return errors.New("application key or lookup metadata cannot authenticate a retained passkey credential")
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil {
		return err
	}
	rows, err = tx.QueryContext(ctx, `SELECT id,coalesce(user_id,''),coalesce(session_id,''),purpose,origin,payload_ciphertext FROM auth_challenges WHERE payload_ciphertext IS NOT NULL ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var challenge PreAuthChallenge
		if err := rows.Scan(&challenge.ID, &challenge.UserID, &challenge.SessionID, &challenge.Purpose, &challenge.Origin, &challenge.PayloadCiphertext); err != nil {
			return err
		}
		if !migrationChallengeAuthenticates(codec, &challenge) {
			return errors.New("application key or original binding cannot authenticate a retained authentication challenge")
		}
	}
	return rows.Err()
}

func migrationChallengeAuthenticates(codec *Manager, challenge *PreAuthChallenge) bool {
	// Historical rows have no codec discriminator. Try the existing codecs using
	// the exact original AAD; distinct derived keys prevent accepting a different
	// format accidentally. Authentication does not authorize or revive a draft.
	payload := challenge.PayloadCiphertext
	if _, err := codec.decryptSetupOwnerDraft(challenge, payload); err == nil {
		return true
	}
	if _, err := codec.decryptRecoveryRepairDraft(challenge, payload); err == nil {
		return true
	}
	if _, err := codec.decryptPasskeyRegistrationDraft(challenge, payload); err == nil {
		return true
	}
	if _, err := codec.decryptPasskeyAssertionDraft(challenge, payload); err == nil {
		return true
	}
	if _, err := codec.decryptSecurityManagementDraft(challenge, payload); err == nil {
		return true
	}
	if _, err := codec.decryptMFAContinuationDraft(challenge, payload); err == nil {
		return true
	}
	if _, err := codec.decryptGoogleLoginDraft(challenge, payload); err == nil {
		return true
	}
	if _, err := codec.decryptMicrosoftLoginDraft(challenge, payload); err == nil {
		return true
	}
	if _, err := codec.decryptOIDCLoginDraft(challenge, payload); err == nil {
		return true
	}
	return false
}
