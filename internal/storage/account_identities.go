package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	netmail "net/mail"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/google/uuid"
)

// ErrProviderIdentity is a change only the provider may make, such as deleting
// or readdressing an identity it reported.
var ErrProviderIdentity = errors.New("this identity is managed by the provider")

// accountIdentitiesTableSQL matches the table in schema.sql.
const accountIdentitiesTableSQL = `
	CREATE TABLE IF NOT EXISTS account_identities (
		id TEXT PRIMARY KEY,
		account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
		name TEXT NOT NULL DEFAULT '',
		email TEXT NOT NULL CHECK (email != ''),
		reply_to TEXT NOT NULL DEFAULT '',
		signature_id TEXT REFERENCES signatures(id) ON DELETE SET NULL,
		source TEXT NOT NULL DEFAULT 'manual' CHECK (source IN ('manual', 'provider')),
		provider_identity_id TEXT NOT NULL DEFAULT '',
		sort_order INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		CHECK ((source = 'provider') = (provider_identity_id != ''))
	);
	CREATE INDEX IF NOT EXISTS idx_account_identities_account
		ON account_identities(account_id, sort_order);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_account_identities_provider
		ON account_identities(account_id, provider_identity_id) WHERE source = 'provider';`

// migrateV127ToV128 adds account_identities, the further From addresses of an
// account. It starts empty: every account keeps sending as its own address.
func migrateV127ToV128(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 128 {
		return nil
	}
	if _, err := tx.Exec(accountIdentitiesTableSQL); err != nil {
		return err
	}
	if err := markSchemaVersion(tx, 128); err != nil {
		return err
	}
	return tx.Commit()
}

const accountIdentityColumns = `i.id, i.account_id, i.name, i.email, i.reply_to, COALESCE(i.signature_id, ''), i.source, i.provider_identity_id, i.sort_order`

func scanAccountIdentity(scan func(...any) error) (models.Identity, error) {
	var identity models.Identity
	err := scan(&identity.ID, &identity.AccountID, &identity.Name, &identity.Email, &identity.ReplyTo, &identity.SignatureID, &identity.Source, &identity.ProviderIdentityID, &identity.SortOrder)
	return identity, err
}

func queryAccountIdentities(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, query string, args ...any) ([]models.Identity, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	identities := []models.Identity{}
	for rows.Next() {
		identity, err := scanAccountIdentity(rows.Scan)
		if err != nil {
			return nil, err
		}
		identities = append(identities, identity)
	}
	return identities, rows.Err()
}

// ListAccountIdentities returns the further identities of one of the owner's
// accounts, in their order.
func (db *DB) ListAccountIdentities(ctx context.Context, userID, accountID string) ([]models.Identity, error) {
	return queryAccountIdentities(ctx, db.Read(), `
		SELECT `+accountIdentityColumns+`
		FROM account_identities i JOIN accounts a ON a.id = i.account_id
		WHERE a.user_id = ? AND a.id = ?
		ORDER BY i.sort_order, i.created_at, i.id`, userID, accountID)
}

// ListUserIdentities returns the further identities of all the owner's
// accounts, in account order and then their own.
func (db *DB) ListUserIdentities(ctx context.Context, userID string) ([]models.Identity, error) {
	return queryAccountIdentities(ctx, db.Read(), `
		SELECT `+accountIdentityColumns+`
		FROM account_identities i JOIN accounts a ON a.id = i.account_id
		WHERE a.user_id = ? AND COALESCE(a.is_deleting, 0) = 0
		ORDER BY a.sort_order, a.created_at, a.id, i.sort_order, i.created_at, i.id`, userID)
}

// GetAccountIdentity returns one identity of one of the owner's accounts, or
// sql.ErrNoRows.
func (db *DB) GetAccountIdentity(ctx context.Context, userID, accountID, identityID string) (models.Identity, error) {
	return scanAccountIdentity(db.Read().QueryRowContext(ctx, `
		SELECT `+accountIdentityColumns+`
		FROM account_identities i JOIN accounts a ON a.id = i.account_id
		WHERE a.user_id = ? AND a.id = ? AND i.id = ?`, userID, accountID, identityID).Scan)
}

// NormalizeIdentityAddresses checks an identity's From address and Reply-To
// list and returns them in the form they are stored.
func NormalizeIdentityAddresses(email, replyTo string) (string, string, error) {
	email = strings.TrimSpace(email)
	parsed, err := netmail.ParseAddress(email)
	// A wildcard such as *@example.com stands for many addresses, not one.
	if err != nil || !strings.EqualFold(parsed.Address, email) || parsed.Name != "" || strings.HasPrefix(parsed.Address, "*@") {
		return "", "", fmt.Errorf("%q is not an email address", email)
	}
	replyTo = strings.TrimSpace(replyTo)
	if replyTo == "" {
		return parsed.Address, "", nil
	}
	list, err := netmail.ParseAddressList(replyTo)
	if err != nil || len(list) == 0 {
		return "", "", fmt.Errorf("the reply-to address is not valid")
	}
	parts := make([]string, len(list))
	for i, address := range list {
		parts[i] = address.String()
	}
	return parsed.Address, strings.Join(parts, ", "), nil
}

// SaveAccountIdentityGuarded creates or updates an identity of one of the
// owner's accounts and returns it as stored. A new identity is always manual
// and goes last. On a provider identity only the signature may change; any
// other difference is ErrProviderIdentity.
func (db *DB) SaveAccountIdentityGuarded(ctx context.Context, userID string, identity models.Identity, guard func(*sql.Tx) error) (models.Identity, error) {
	identity.Name = strings.TrimSpace(identity.Name)
	identity.SignatureID = strings.TrimSpace(identity.SignatureID)
	email, replyTo, err := NormalizeIdentityAddresses(identity.Email, identity.ReplyTo)
	if err != nil {
		return models.Identity{}, err
	}
	identity.Email, identity.ReplyTo = email, replyTo

	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return models.Identity{}, err
	}
	defer tx.Rollback()
	if guard != nil {
		if err := guard(tx); err != nil {
			return models.Identity{}, err
		}
	}
	var owned int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE user_id = ? AND id = ? AND COALESCE(is_deleting, 0) = 0`, userID, identity.AccountID).Scan(&owned); err != nil {
		return models.Identity{}, err
	}
	if owned == 0 {
		return models.Identity{}, sql.ErrNoRows
	}
	if identity.SignatureID != "" {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM signatures WHERE user_id = ? AND id = ?`, userID, identity.SignatureID).Scan(&owned); err != nil {
			return models.Identity{}, err
		}
		if owned == 0 {
			return models.Identity{}, sql.ErrNoRows
		}
	}
	signature := sql.NullString{String: identity.SignatureID, Valid: identity.SignatureID != ""}

	if identity.ID == "" {
		identity.ID = "idn_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO account_identities (id, account_id, name, email, reply_to, signature_id, source, sort_order)
			VALUES (?, ?, ?, ?, ?, ?, 'manual', (SELECT COALESCE(MAX(sort_order), -1) + 1 FROM account_identities WHERE account_id = ?))`,
			identity.ID, identity.AccountID, identity.Name, identity.Email, identity.ReplyTo, signature, identity.AccountID); err != nil {
			return models.Identity{}, fmt.Errorf("save identity: %w", err)
		}
	} else {
		current, err := scanAccountIdentity(tx.QueryRowContext(ctx, `
			SELECT `+accountIdentityColumns+` FROM account_identities i WHERE i.id = ? AND i.account_id = ?`, identity.ID, identity.AccountID).Scan)
		if err != nil {
			return models.Identity{}, err
		}
		if current.Source == models.IdentitySourceProvider {
			if identity.Name != current.Name || !strings.EqualFold(identity.Email, current.Email) || identity.ReplyTo != current.ReplyTo {
				return models.Identity{}, ErrProviderIdentity
			}
			identity.Name, identity.Email, identity.ReplyTo = current.Name, current.Email, current.ReplyTo
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE account_identities SET name = ?, email = ?, reply_to = ?, signature_id = ?, updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND account_id = ?`,
			identity.Name, identity.Email, identity.ReplyTo, signature, identity.ID, identity.AccountID); err != nil {
			return models.Identity{}, fmt.Errorf("save identity: %w", err)
		}
	}
	saved, err := scanAccountIdentity(tx.QueryRowContext(ctx, `
		SELECT `+accountIdentityColumns+` FROM account_identities i WHERE i.id = ?`, identity.ID).Scan)
	if err != nil {
		return models.Identity{}, err
	}
	if guard != nil {
		if err := guard(tx); err != nil {
			return models.Identity{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return models.Identity{}, err
	}
	return saved, nil
}

// DeleteAccountIdentityGuarded removes a manual identity of one of the owner's
// accounts. A provider identity is ErrProviderIdentity: it would come back on
// the next refresh.
func (db *DB) DeleteAccountIdentityGuarded(ctx context.Context, userID, accountID, identityID string, guard func(*sql.Tx) error) error {
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
	var source string
	err = tx.QueryRowContext(ctx, `
		SELECT i.source FROM account_identities i JOIN accounts a ON a.id = i.account_id
		WHERE a.user_id = ? AND a.id = ? AND i.id = ?`, userID, accountID, identityID).Scan(&source)
	if err != nil {
		return err
	}
	if source == models.IdentitySourceProvider {
		return ErrProviderIdentity
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_identities WHERE id = ? AND account_id = ?`, identityID, accountID); err != nil {
		return err
	}
	if guard != nil {
		if err := guard(tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetAccountIdentityOrderGuarded puts an account's identities in the order of
// ids, which must name each of them once.
func (db *DB) SetAccountIdentityOrderGuarded(ctx context.Context, userID, accountID string, ids []string, guard func(*sql.Tx) error) error {
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
	current, err := queryAccountIdentities(ctx, tx, `
		SELECT `+accountIdentityColumns+`
		FROM account_identities i JOIN accounts a ON a.id = i.account_id
		WHERE a.user_id = ? AND a.id = ?`, userID, accountID)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(current))
	for _, identity := range current {
		known[identity.ID] = true
	}
	if len(ids) != len(known) {
		return fmt.Errorf("identity order must name each identity once")
	}
	for i, id := range ids {
		if !known[id] {
			return fmt.Errorf("identity order must name each identity once")
		}
		delete(known, id)
		if _, err := tx.ExecContext(ctx, `UPDATE account_identities SET sort_order = ? WHERE id = ? AND account_id = ?`, i, id, accountID); err != nil {
			return err
		}
	}
	if guard != nil {
		if err := guard(tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplaceProviderIdentities makes an account's provider identities match the
// ones its provider reported, matched by ProviderIdentityID. Identities that
// are gone are removed; signatures chosen in Gofer are kept; new ones go
// last. Manual identities are untouched.
func (db *DB) ReplaceProviderIdentities(ctx context.Context, accountID string, reported []models.Identity) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	keep := make([]any, 0, len(reported)+1)
	keep = append(keep, accountID)
	placeholders := make([]string, 0, len(reported))
	for _, identity := range reported {
		providerID := strings.TrimSpace(identity.ProviderIdentityID)
		if providerID == "" {
			return fmt.Errorf("provider identity without an id")
		}
		email, replyTo, err := NormalizeIdentityAddresses(identity.Email, identity.ReplyTo)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO account_identities (id, account_id, name, email, reply_to, source, provider_identity_id, sort_order)
			VALUES (?, ?, ?, ?, ?, 'provider', ?, (SELECT COALESCE(MAX(sort_order), -1) + 1 FROM account_identities WHERE account_id = ?))
			ON CONFLICT(account_id, provider_identity_id) WHERE source = 'provider' DO UPDATE SET
				name = excluded.name,
				email = excluded.email,
				reply_to = excluded.reply_to,
				updated_at = CURRENT_TIMESTAMP
			WHERE name != excluded.name OR email != excluded.email OR reply_to != excluded.reply_to`,
			"idn_"+strings.ReplaceAll(uuid.NewString(), "-", ""), accountID, strings.TrimSpace(identity.Name), email, replyTo, providerID, accountID); err != nil {
			return fmt.Errorf("save provider identity: %w", err)
		}
		keep = append(keep, providerID)
		placeholders = append(placeholders, "?")
	}
	query := `DELETE FROM account_identities WHERE account_id = ? AND source = 'provider'`
	if len(placeholders) > 0 {
		query += ` AND provider_identity_id NOT IN (` + strings.Join(placeholders, ",") + `)`
	}
	if _, err := tx.ExecContext(ctx, query, keep...); err != nil {
		return err
	}
	return tx.Commit()
}
