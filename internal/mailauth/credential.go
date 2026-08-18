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
	"strings"
)

const (
	mailboxCredentialLegacyKeyVersion = 1
	mailboxCredentialKeyVersion       = 2
	mailboxCredentialKeyContext       = "gofer/mailauth/oauth-account-credential/v1"
	minimumMailboxCredentialKey       = 32
)

type oauthCredentialContext struct {
	ID                string
	AccountID         string
	Provider          string
	ProviderAccountID string
}

type legacyOAuthCredential struct {
	Context           oauthCredentialContext
	AccessToken       string
	RefreshToken      string
	AccessCiphertext  []byte
	RefreshCiphertext []byte
	KeyVersion        sql.NullInt64
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
	if keyVersion != mailboxCredentialLegacyKeyVersion && keyVersion != mailboxCredentialKeyVersion {
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
	ciphertext := payload[1+aead.NonceSize():]
	var authenticationErr error
	for _, aad := range oauthTokenAADForVersion(credential, kind, keyVersion) {
		var plaintext []byte
		plaintext, authenticationErr = aead.Open(nil, nonce, ciphertext, aad)
		if authenticationErr == nil {
			return string(plaintext), nil
		}
	}
	return "", fmt.Errorf("authenticate mailbox credential: %w", authenticationErr)
}

func oauthTokenAADForVersion(credential oauthCredentialContext, kind string, keyVersion int) [][]byte {
	if keyVersion == mailboxCredentialLegacyKeyVersion {
		// Two v1 encodings existed briefly during development. Accept both only
		// while startup rewrites the row to the unambiguous v2 encoding.
		return [][]byte{
			legacyOAuthTokenAAD(credential, kind),
			oauthTokenAAD(credential, kind),
		}
	}
	return [][]byte{oauthTokenAAD(credential, kind)}
}

func legacyOAuthTokenAAD(credential oauthCredentialContext, kind string) []byte {
	return []byte(strings.Join([]string{
		credential.ID,
		credential.AccountID,
		credential.Provider,
		credential.ProviderAccountID,
		kind,
	}, "\x00"))
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

// SecureOAuthCredentials encrypts every legacy plaintext mailbox grant and
// upgrades every v1 ciphertext in one transaction. It verifies that every row
// uses the current format and authenticates with the current application
// secret before commit. It must run before mailbox workers start.
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
		SELECT id, account_id, provider, provider_account_id,
		       access_token, refresh_token,
		       access_token_ciphertext, refresh_token_ciphertext, key_version
		FROM oauth_accounts
		WHERE key_version IS NULL OR key_version = ?
		ORDER BY id`, mailboxCredentialLegacyKeyVersion)
	if err != nil {
		return fmt.Errorf("load legacy mailbox credentials: %w", err)
	}
	legacy := []legacyOAuthCredential{}
	for rows.Next() {
		var credential legacyOAuthCredential
		if err := rows.Scan(
			&credential.Context.ID, &credential.Context.AccountID,
			&credential.Context.Provider, &credential.Context.ProviderAccountID,
			&credential.AccessToken, &credential.RefreshToken,
			&credential.AccessCiphertext, &credential.RefreshCiphertext, &credential.KeyVersion,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scan legacy mailbox credential: %w", err)
		}
		legacy = append(legacy, credential)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("load legacy mailbox credentials: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy mailbox credentials: %w", err)
	}

	for _, credential := range legacy {
		previousVersion := int64(0)
		if credential.KeyVersion.Valid {
			previousVersion = credential.KeyVersion.Int64
			if credential.AccessToken != "" || credential.RefreshToken != "" {
				return fmt.Errorf("mailbox credential %q mixes plaintext and ciphertext", credential.Context.ID)
			}
			var err error
			credential.AccessToken, err = m.decryptOAuthToken(
				credential.Context, "access", credential.AccessCiphertext, int(previousVersion),
			)
			if err != nil {
				return fmt.Errorf("decrypt legacy mailbox access credential %q: %w", credential.Context.ID, err)
			}
			credential.RefreshToken, err = m.decryptOAuthToken(
				credential.Context, "refresh", credential.RefreshCiphertext, int(previousVersion),
			)
			if err != nil {
				return fmt.Errorf("decrypt legacy mailbox refresh credential %q: %w", credential.Context.ID, err)
			}
		} else if credential.AccessCiphertext != nil || credential.RefreshCiphertext != nil {
			return fmt.Errorf("plaintext mailbox credential %q contains unexpected ciphertext", credential.Context.ID)
		}

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
			WHERE id = ? AND account_id = ? AND COALESCE(key_version, 0) = ?`,
			accessCiphertext, refreshCiphertext, mailboxCredentialKeyVersion,
			credential.Context.ID, credential.Context.AccountID, previousVersion,
		)
		if err != nil {
			return fmt.Errorf("upgrade mailbox credential %q: %w", credential.Context.ID, err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return fmt.Errorf("upgrade mailbox credential %q: row changed concurrently", credential.Context.ID)
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
		if keyVersion.Int64 != mailboxCredentialKeyVersion {
			return fmt.Errorf("mailbox credential %q has unsupported key version %d", credential.ID, keyVersion.Int64)
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
