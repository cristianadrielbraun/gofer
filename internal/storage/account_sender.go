package storage

import (
	"context"
	"database/sql"
	"strings"
	"unicode/utf8"
)

// maxAccountSenderName bounds the sender name; From headers fold long names
// but nobody signs with more.
const maxAccountSenderName = 200

// migrateV143ToV144 lets an account send under a name other than its own,
// which only labels it in Gofer, and start new messages from one of its
// identities. Both start empty: the account name and address, as before.
func migrateV143ToV144(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 144 {
		return nil
	}
	exists, err := tableExistsTx(tx, "accounts")
	if err != nil {
		return err
	}
	if exists {
		for _, column := range []string{"sender_name", "default_identity_id"} {
			present, err := columnExistsTx(tx, "accounts", column)
			if err != nil {
				return err
			}
			if !present {
				if _, err := tx.Exec(`ALTER TABLE accounts ADD COLUMN ` + column + ` TEXT NOT NULL DEFAULT ''`); err != nil {
					return err
				}
			}
		}
	}
	if err := markSchemaVersion(tx, 144); err != nil {
		return err
	}
	return tx.Commit()
}

// normalizeAccountSenderName trims a sender name and keeps it to one line,
// since it goes into a From header.
func normalizeAccountSenderName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if utf8.RuneCountInString(name) > maxAccountSenderName {
		name = string([]rune(name)[:maxAccountSenderName])
	}
	return name
}

// SetAccountSenderNameGuarded sets the name one of the owner's accounts sends
// its own address under; empty sends it under the account name. An unknown
// account is sql.ErrNoRows.
func (db *DB) SetAccountSenderNameGuarded(ctx context.Context, userID, accountID, name string, guard func(*sql.Tx) error) error {
	return db.updateAccountGuarded(ctx, guard, `UPDATE accounts SET sender_name = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0`, normalizeAccountSenderName(name), accountID, userID)
}

// SetAccountDefaultIdentityGuarded makes identityID the identity new messages
// of one of the owner's accounts start from, or the account's own address
// when it is empty. An unknown account or identity is sql.ErrNoRows.
func (db *DB) SetAccountDefaultIdentityGuarded(ctx context.Context, userID, accountID, identityID string, guard func(*sql.Tx) error) error {
	identityID = strings.TrimSpace(identityID)
	if identityID == "" {
		return db.updateAccountGuarded(ctx, guard, `UPDATE accounts SET default_identity_id = '', updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0`, accountID, userID)
	}
	return db.updateAccountGuarded(ctx, guard, `UPDATE accounts SET default_identity_id = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0
		  AND EXISTS (SELECT 1 FROM account_identities i WHERE i.id = ? AND i.account_id = accounts.id)`,
		identityID, accountID, userID, identityID)
}

// updateAccountGuarded runs one update of an accounts row inside guard,
// returning sql.ErrNoRows when it matched no row.
func (db *DB) updateAccountGuarded(ctx context.Context, guard func(*sql.Tx) error, query string, args ...any) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if guard != nil {
		if err := guard(tx); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed == 0 {
		return sql.ErrNoRows
	}
	if guard != nil {
		if err := guard(tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// clearMissingDefaultIdentity forgets an account's default identity once that
// identity is gone, so new messages start from the account's own address.
func clearMissingDefaultIdentity(ctx context.Context, tx *sql.Tx, accountID string) error {
	_, err := tx.ExecContext(ctx, `UPDATE accounts SET default_identity_id = ''
		WHERE id = ? AND default_identity_id != ''
		  AND NOT EXISTS (SELECT 1 FROM account_identities i WHERE i.id = accounts.default_identity_id AND i.account_id = accounts.id)`, accountID)
	return err
}
