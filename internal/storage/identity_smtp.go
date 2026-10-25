package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
)

// ErrIdentitySMTPNotSupported is an own SMTP server on an identity whose
// account can't keep a copy of what it sends that way: an Outlook account,
// or an identity from the provider.
var ErrIdentitySMTPNotSupported = errors.New("this identity can't send through its own server")

// identitySMTPColumns are the columns migrateV128ToV129 adds.
var identitySMTPColumns = []struct{ name, definition string }{
	{"smtp_host", `TEXT NOT NULL DEFAULT ''`},
	{"smtp_port", `INTEGER NOT NULL DEFAULT 0`},
	{"smtp_tls_mode", `TEXT NOT NULL DEFAULT ''`},
	{"smtp_username", `TEXT NOT NULL DEFAULT ''`},
	{"encrypted_smtp_password", `BLOB`},
}

// migrateV128ToV129 lets an identity send through its own SMTP server. Every
// identity starts without one and keeps sending the account's way.
func migrateV128ToV129(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 129 {
		return nil
	}
	exists, err := tableExistsTx(tx, "account_identities")
	if err != nil {
		return err
	}
	if exists {
		for _, column := range identitySMTPColumns {
			present, err := columnExistsTx(tx, "account_identities", column.name)
			if err != nil {
				return err
			}
			if !present {
				if _, err := tx.Exec(`ALTER TABLE account_identities ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
					return err
				}
			}
		}
	}
	if err := markSchemaVersion(tx, 129); err != nil {
		return err
	}
	return tx.Commit()
}

// NormalizeIdentitySMTP checks an identity's own SMTP server. An identity
// without a host has none, and its other SMTP fields are cleared.
func NormalizeIdentitySMTP(identity *models.Identity) error {
	identity.SMTPHost = strings.TrimSpace(identity.SMTPHost)
	identity.SMTPUsername = strings.TrimSpace(identity.SMTPUsername)
	identity.SMTPTLSMode = strings.ToLower(strings.TrimSpace(identity.SMTPTLSMode))
	if identity.SMTPHost == "" {
		identity.SMTPPort, identity.SMTPTLSMode, identity.SMTPUsername = 0, "", ""
		return nil
	}
	if strings.ContainsAny(identity.SMTPHost, " /\\\t@") {
		return fmt.Errorf("%q is not a server name", identity.SMTPHost)
	}
	if identity.SMTPPort < 1 || identity.SMTPPort > 65535 {
		return fmt.Errorf("the SMTP port must be between 1 and 65535")
	}
	switch identity.SMTPTLSMode {
	case "tls", "starttls", "plaintext":
	default:
		return fmt.Errorf("choose TLS, STARTTLS or no encryption for the SMTP server")
	}
	return nil
}

// checkIdentitySMTPAllowed refuses an own SMTP server where Gofer could not
// keep a copy of the message in the account's Sent folder.
func checkIdentitySMTPAllowed(ctx context.Context, tx *sql.Tx, accountID string) error {
	var provider string
	if err := tx.QueryRowContext(ctx, `SELECT provider FROM accounts WHERE id = ?`, accountID).Scan(&provider); err != nil {
		return err
	}
	if provider == providers.ProviderOutlook {
		return ErrIdentitySMTPNotSupported
	}
	return nil
}

// SetAccountIdentitySMTPPasswordGuarded stores the encrypted password of a
// manual identity's own SMTP server; nil removes it.
func (db *DB) SetAccountIdentitySMTPPasswordGuarded(ctx context.Context, userID, accountID, identityID string, encrypted []byte, guard func(*sql.Tx) error) error {
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
	var source, host string
	err = tx.QueryRowContext(ctx, `
		SELECT i.source, i.smtp_host FROM account_identities i JOIN accounts a ON a.id = i.account_id
		WHERE a.user_id = ? AND a.id = ? AND i.id = ? AND COALESCE(a.is_deleting, 0) = 0`, userID, accountID, identityID).Scan(&source, &host)
	if err != nil {
		return err
	}
	if source != models.IdentitySourceManual || host == "" {
		return ErrIdentitySMTPNotSupported
	}
	if _, err := tx.ExecContext(ctx, `UPDATE account_identities SET encrypted_smtp_password = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, encrypted, identityID); err != nil {
		return err
	}
	return tx.Commit()
}

// AccountIdentitySMTPPassword returns the owner of an identity's account and
// the encrypted password of the identity's own SMTP server, nil when none is
// stored.
func (db *DB) AccountIdentitySMTPPassword(ctx context.Context, accountID, identityID string) (string, []byte, error) {
	var userID string
	var encrypted []byte
	err := db.Read().QueryRowContext(ctx, `
		SELECT a.user_id, i.encrypted_smtp_password FROM account_identities i JOIN accounts a ON a.id = i.account_id
		WHERE a.id = ? AND i.id = ?`, accountID, identityID).Scan(&userID, &encrypted)
	return userID, encrypted, err
}
