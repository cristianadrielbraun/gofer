package config

import (
	"context"
	"database/sql"
	"errors"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// ValidateUserStorageMigrationPasswords authenticates every retained password,
// including disabled/deleting mailboxes and inactive CardDAV/CalDAV settings.
// It neither filters by runtime eligibility nor rewrites ciphertext or calls a
// provider. The offline coordinator must hold the source runtime lock.
func ValidateUserStorageMigrationPasswords(ctx context.Context, source *storage.DB, key []byte) error {
	if ctx == nil || source == nil || len(key) != 32 {
		return errors.New("password migration verification requires source, context and application key")
	}
	codec, err := NewAccountStore(nil, key)
	if err != nil {
		return err
	}
	tx, err := source.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT encrypted_password FROM accounts WHERE encrypted_password IS NOT NULL
 UNION ALL SELECT encrypted_smtp_password FROM accounts WHERE encrypted_smtp_password IS NOT NULL
 UNION ALL SELECT encrypted_password FROM account_contact_sync_configs WHERE encrypted_password IS NOT NULL
 UNION ALL SELECT encrypted_password FROM account_caldav_configs WHERE encrypted_password IS NOT NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ciphertext []byte
		if err := rows.Scan(&ciphertext); err != nil {
			return err
		}
		if _, err := codec.decrypt(ciphertext); err != nil {
			return errors.New("application key cannot authenticate a retained mailbox or DAV password")
		}
	}
	return rows.Err()
}
