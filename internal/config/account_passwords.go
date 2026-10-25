package config

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"sync"
	"weak"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Stored passwords are bound to the user, mailbox and service they belong to,
// so a password copied into another account's row does not decrypt there.
const (
	accountPasswordVersion = 2
	accountPasswordContext = "gofer/account-password/v2"

	passwordKindIMAP    = "imap"
	passwordKindSMTP    = "smtp"
	passwordKindCardDAV = "carddav"
	passwordKindCalDAV  = "caldav"
	// An identity's own SMTP password is bound to the identity as well:
	// passwordKindIdentitySMTP + ":" + its id.
	passwordKindIdentitySMTP = "identity-smtp"

	// passwordsUpgradedMarker is written to the database header (SQLite's
	// application_id, otherwise unused by Gofer) once old-format passwords have
	// been upgraded, so the upgrade runs only once per database. An old-format
	// password written after that is never re-encrypted for its new row.
	passwordsUpgradedMarker = 0x47504632
)

func accountPasswordAAD(userID, accountID, kind string) []byte {
	var payload []byte
	for _, field := range []string{accountPasswordContext, userID, accountID, kind} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		payload = append(payload, length[:]...)
		payload = append(payload, field...)
	}
	return payload
}

func (s *AccountStore) encryptPassword(userID, accountID, kind, plaintext string) ([]byte, error) {
	nonce := make([]byte, s.aes.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	payload := []byte{accountPasswordVersion}
	payload = append(payload, nonce...)
	return s.aes.Seal(payload, nonce, []byte(plaintext), accountPasswordAAD(userID, accountID, kind)), nil
}

func (s *AccountStore) decryptPassword(userID, accountID, kind string, payload []byte) (string, error) {
	nonceSize := s.aes.NonceSize()
	if len(payload) < 1+nonceSize+s.aes.Overhead() || payload[0] != accountPasswordVersion {
		return "", fmt.Errorf("stored password has an unsupported format")
	}
	plaintext, err := s.aes.Open(nil, payload[1:1+nonceSize], payload[1+nonceSize:], accountPasswordAAD(userID, accountID, kind))
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}

// decryptLegacyPassword reads the format used before passwords were bound to
// their account. It is only used to upgrade stored rows.
func (s *AccountStore) decryptLegacyPassword(ciphertext []byte) (string, error) {
	nonceSize := s.aes.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	plaintext, err := s.aes.Open(nil, ciphertext[:nonceSize], ciphertext[nonceSize:], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(plaintext), nil
}

// authenticatesPassword reports whether the key can read a stored password in
// either format.
func (s *AccountStore) authenticatesPassword(userID, accountID, kind string, payload []byte) bool {
	if _, err := s.decryptPassword(userID, accountID, kind, payload); err == nil {
		return true
	}
	_, err := s.decryptLegacyPassword(payload)
	return err == nil
}

type storedPassword struct {
	table, column, kind string
	userID, accountID   string
	payload             []byte
}

var storedPasswordQueries = []struct {
	table, column, kind, query string
}{
	{"accounts", "encrypted_password", passwordKindIMAP, `SELECT user_id, id, encrypted_password FROM accounts WHERE encrypted_password IS NOT NULL`},
	{"accounts", "encrypted_smtp_password", passwordKindSMTP, `SELECT user_id, id, encrypted_smtp_password FROM accounts WHERE encrypted_smtp_password IS NOT NULL`},
	{"account_contact_sync_configs", "encrypted_password", passwordKindCardDAV, `SELECT user_id, account_id, encrypted_password FROM account_contact_sync_configs WHERE encrypted_password IS NOT NULL`},
	{"account_caldav_configs", "encrypted_password", passwordKindCalDAV, `SELECT user_id, account_id, encrypted_password FROM account_caldav_configs WHERE encrypted_password IS NOT NULL`},
}

func identitySMTPPasswordKind(identityID string) string {
	return passwordKindIdentitySMTP + ":" + identityID
}

func listStoredPasswords(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]storedPassword, error) {
	var out []storedPassword
	identities, err := listIdentitySMTPPasswords(ctx, q)
	if err != nil {
		return nil, err
	}
	out = append(out, identities...)
	for _, source := range storedPasswordQueries {
		rows, err := q.QueryContext(ctx, source.query)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			p := storedPassword{table: source.table, column: source.column, kind: source.kind}
			var userID sql.NullString
			if err := rows.Scan(&userID, &p.accountID, &p.payload); err != nil {
				rows.Close()
				return nil, err
			}
			p.userID = userID.String
			if len(p.payload) > 0 {
				out = append(out, p)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// listIdentitySMTPPasswords lists identities' own SMTP passwords. Databases
// from before schema 129 have none.
func listIdentitySMTPPasswords(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]storedPassword, error) {
	present, err := q.QueryContext(ctx, `SELECT 1 FROM pragma_table_info('account_identities') WHERE name = 'encrypted_smtp_password'`)
	if err != nil {
		return nil, err
	}
	hasColumn := present.Next()
	present.Close()
	if !hasColumn {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT a.user_id, i.account_id, i.id, i.encrypted_smtp_password
		FROM account_identities i JOIN accounts a ON a.id = i.account_id
		WHERE i.encrypted_smtp_password IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedPassword
	for rows.Next() {
		p := storedPassword{table: "account_identities", column: "encrypted_smtp_password"}
		var userID sql.NullString
		var identityID string
		if err := rows.Scan(&userID, &p.accountID, &identityID, &p.payload); err != nil {
			return nil, err
		}
		p.userID, p.kind = userID.String, identitySMTPPasswordKind(identityID)
		if len(p.payload) > 0 {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// SetIdentitySMTPPassword stores the password of a manual identity's own
// SMTP server, bound to that identity; an empty password removes it.
func (s *AccountStore) SetIdentitySMTPPassword(ctx context.Context, userID, accountID, identityID, password string, guard func(*sql.Tx) error) error {
	var encrypted []byte
	if password != "" {
		var err error
		if encrypted, err = s.encryptPassword(userID, accountID, identitySMTPPasswordKind(identityID), password); err != nil {
			return err
		}
	}
	return s.db.SetAccountIdentitySMTPPasswordGuarded(ctx, userID, accountID, identityID, encrypted, guard)
}

// DecryptIdentitySMTPPassword returns the password of an identity's own SMTP
// server, or "" when none is stored.
func (s *AccountStore) DecryptIdentitySMTPPassword(ctx context.Context, accountID, identityID string) (string, error) {
	userID, encrypted, err := s.db.AccountIdentitySMTPPassword(ctx, accountID, identityID)
	if err != nil {
		return "", fmt.Errorf("query identity smtp password: %w", err)
	}
	if encrypted == nil {
		return "", nil
	}
	return s.decryptPassword(userID, accountID, identitySMTPPasswordKind(identityID), encrypted)
}

// SecurePasswords rewrites passwords stored in the old format so they are bound
// to their account. It runs once per database; the old format is never
// accepted at runtime. A password the key cannot read is left as it is, since
// it can't be used either way.
func (s *AccountStore) SecurePasswords(ctx context.Context) error {
	// Check without the writer first; upgraded databases need no write.
	var marker int64
	if err := s.db.Read().QueryRowContext(ctx, `PRAGMA application_id`).Scan(&marker); err != nil {
		return err
	}
	if marker == passwordsUpgradedMarker {
		return nil
	}
	tx, err := s.db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&marker); err != nil {
		return err
	}
	if marker == passwordsUpgradedMarker {
		return nil
	}
	if marker != 0 {
		return fmt.Errorf("database header has an unexpected application id %d", marker)
	}
	passwords, err := listStoredPasswords(ctx, tx)
	if err != nil {
		return fmt.Errorf("load stored passwords: %w", err)
	}
	for _, p := range passwords {
		if _, err := s.decryptPassword(p.userID, p.accountID, p.kind, p.payload); err == nil {
			continue
		}
		plaintext, err := s.decryptLegacyPassword(p.payload)
		if err != nil {
			log.Printf("stored %s password for account %s can't be read with the current key", p.kind, p.accountID)
			continue
		}
		if p.table == "account_identities" {
			continue // Never stored in the old format.
		}
		upgraded, err := s.encryptPassword(p.userID, p.accountID, p.kind, plaintext)
		if err != nil {
			return err
		}
		idColumn := "account_id"
		if p.table == "accounts" {
			idColumn = "id"
		}
		query := fmt.Sprintf(`UPDATE %s SET %s = ? WHERE %s = ? AND %s = ?`, p.table, p.column, idColumn, p.column)
		if _, err := tx.ExecContext(ctx, query, upgraded, p.accountID, p.payload); err != nil {
			return fmt.Errorf("upgrade stored %s password for account %s: %w", p.kind, p.accountID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA application_id = %d`, passwordsUpgradedMarker)); err != nil {
		return err
	}
	return tx.Commit()
}

// securedStores remembers the open databases whose passwords this process has
// upgraded. A reopened database is checked again.
var securedStores sync.Map // weak.Pointer[storage.DB] -> bool

func (s *AccountStore) securePasswordsOnce(ctx context.Context, db *storage.DB) {
	if db == nil {
		return
	}
	key := weak.Make(db)
	if _, done := securedStores.Load(key); done {
		return
	}
	if err := s.SecurePasswords(ctx); err != nil {
		log.Printf("upgrade stored passwords: %v", err)
		return
	}
	securedStores.Store(key, true)
}
