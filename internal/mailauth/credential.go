package mailauth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	mailboxCredentialKeyVersion = 1
	mailboxCredentialKeyContext = "gofer/mailauth/oauth-account-credential/v1"
	minimumMailboxCredentialKey = 32
)

type oauthCredentialContext struct {
	ID                string
	AccountID         string
	Provider          string
	ProviderAccountID string
}

type legacyOAuthCredential struct {
	Context      oauthCredentialContext
	AccessToken  string
	RefreshToken string
}

func (m *Service) mailboxCredentialAEAD() (cipher.AEAD, error) {
	if m == nil || len(m.credentialKey) < minimumMailboxCredentialKey {
		return nil, fmt.Errorf("mailbox credential encryption key is unavailable")
	}
	deriver := hmac.New(sha256.New, m.credentialKey)
	_, _ = deriver.Write([]byte(mailboxCredentialKeyContext))
	block, err := aes.NewCipher(deriver.Sum(nil))
	if err != nil {
		return nil, fmt.Errorf("create mailbox credential cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create mailbox credential AEAD: %w", err)
	}
	return aead, nil
}

func (m *Service) encryptOAuthToken(credential oauthCredentialContext, kind, token string) ([]byte, error) {
	if token == "" {
		return nil, nil
	}
	aead, err := m.mailboxCredentialAEAD()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate mailbox credential nonce: %w", err)
	}
	payload := []byte{mailboxCredentialKeyVersion}
	payload = append(payload, nonce...)
	payload = aead.Seal(payload, nonce, []byte(token), oauthTokenAAD(credential, kind))
	return payload, nil
}

func (m *Service) decryptOAuthToken(credential oauthCredentialContext, kind string, payload []byte, keyVersion int) (string, error) {
	if len(payload) == 0 {
		return "", nil
	}
	if keyVersion != mailboxCredentialKeyVersion {
		return "", fmt.Errorf("unsupported mailbox credential key version %d", keyVersion)
	}
	aead, err := m.mailboxCredentialAEAD()
	if err != nil {
		return "", err
	}
	if len(payload) < 1+aead.NonceSize()+aead.Overhead() || int(payload[0]) != keyVersion {
		return "", fmt.Errorf("mailbox credential payload is invalid")
	}
	nonce := payload[1 : 1+aead.NonceSize()]
	plaintext, err := aead.Open(nil, nonce, payload[1+aead.NonceSize():], oauthTokenAAD(credential, kind))
	if err != nil {
		return "", fmt.Errorf("authenticate mailbox credential: %w", err)
	}
	return string(plaintext), nil
}

func oauthTokenAAD(credential oauthCredentialContext, kind string) []byte {
	fields := []string{
		credential.ID,
		credential.AccountID,
		credential.Provider,
		credential.ProviderAccountID,
		kind,
	}
	size := 4 * len(fields)
	for _, field := range fields {
		size += len(field)
	}
	payload := make([]byte, 0, size)
	var length [4]byte
	for _, field := range fields {
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		payload = append(payload, length[:]...)
		payload = append(payload, field...)
	}
	return payload
}

// SecureOAuthCredentials encrypts every legacy plaintext mailbox grant in one
// transaction and verifies that every encrypted row can be authenticated with
// the current application secret. It must run before mailbox workers start.
func (m *Service) SecureOAuthCredentials(ctx context.Context) error {
	if _, err := m.mailboxCredentialAEAD(); err != nil {
		return err
	}
	tx, err := m.db.Write().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mailbox credential encryption: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, account_id, provider, provider_account_id, access_token, refresh_token
		FROM oauth_accounts WHERE key_version IS NULL ORDER BY id`)
	if err != nil {
		return fmt.Errorf("load plaintext mailbox credentials: %w", err)
	}
	legacy := []legacyOAuthCredential{}
	for rows.Next() {
		var credential legacyOAuthCredential
		if err := rows.Scan(
			&credential.Context.ID, &credential.Context.AccountID,
			&credential.Context.Provider, &credential.Context.ProviderAccountID,
			&credential.AccessToken, &credential.RefreshToken,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scan plaintext mailbox credential: %w", err)
		}
		legacy = append(legacy, credential)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("load plaintext mailbox credentials: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close plaintext mailbox credentials: %w", err)
	}

	for _, credential := range legacy {
		accessCiphertext, err := m.encryptOAuthToken(credential.Context, "access", credential.AccessToken)
		if err != nil {
			return err
		}
		refreshCiphertext, err := m.encryptOAuthToken(credential.Context, "refresh", credential.RefreshToken)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE oauth_accounts
			SET access_token = '', refresh_token = '',
			    access_token_ciphertext = ?, refresh_token_ciphertext = ?, key_version = ?
			WHERE id = ? AND account_id = ? AND key_version IS NULL`,
			accessCiphertext, refreshCiphertext, mailboxCredentialKeyVersion,
			credential.Context.ID, credential.Context.AccountID,
		)
		if err != nil {
			return fmt.Errorf("encrypt mailbox credential %q: %w", credential.Context.ID, err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return fmt.Errorf("encrypt mailbox credential %q: row changed concurrently", credential.Context.ID)
		}
	}

	if err := m.verifyOAuthCredentialsInTransaction(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit mailbox credential encryption: %w", err)
	}
	return nil
}

func (m *Service) verifyOAuthCredentialsInTransaction(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, account_id, provider, provider_account_id,
		       access_token, refresh_token,
		       access_token_ciphertext, refresh_token_ciphertext, key_version
		FROM oauth_accounts ORDER BY id`)
	if err != nil {
		return fmt.Errorf("verify mailbox credentials: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var credential oauthCredentialContext
		var plaintextAccess, plaintextRefresh string
		var accessCiphertext, refreshCiphertext []byte
		var keyVersion sql.NullInt64
		if err := rows.Scan(
			&credential.ID, &credential.AccountID, &credential.Provider, &credential.ProviderAccountID,
			&plaintextAccess, &plaintextRefresh, &accessCiphertext, &refreshCiphertext, &keyVersion,
		); err != nil {
			return fmt.Errorf("scan mailbox credential verification row: %w", err)
		}
		if plaintextAccess != "" || plaintextRefresh != "" || !keyVersion.Valid {
			return fmt.Errorf("mailbox credential %q remains in plaintext", credential.ID)
		}
		if _, err := m.decryptOAuthToken(credential, "access", accessCiphertext, int(keyVersion.Int64)); err != nil {
			return fmt.Errorf("verify mailbox access credential %q: %w", credential.ID, err)
		}
		if _, err := m.decryptOAuthToken(credential, "refresh", refreshCiphertext, int(keyVersion.Int64)); err != nil {
			return fmt.Errorf("verify mailbox refresh credential %q: %w", credential.ID, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("verify mailbox credentials: %w", err)
	}
	return nil
}

func (m *Service) loadOAuthCredentialContext(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, credentialID string) (oauthCredentialContext, error) {
	var credential oauthCredentialContext
	err := queryer.QueryRowContext(ctx, `
		SELECT id, account_id, provider, provider_account_id
		FROM oauth_accounts WHERE id = ?`, credentialID,
	).Scan(&credential.ID, &credential.AccountID, &credential.Provider, &credential.ProviderAccountID)
	if err != nil {
		return credential, fmt.Errorf("load mailbox credential context: %w", err)
	}
	return credential, nil
}
