package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	netmail "net/mail"
	"slices"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/google/uuid"
)

// ErrProviderIdentity is a change only the provider may make, such as deleting
// or readdressing an identity it reported.
var ErrProviderIdentity = errors.New("this identity is managed by the provider")

// ErrIdentityAddressNotAllowed is an address added by hand that the
// account's provider would not send as: any address on Gmail, which sends
// only from its own send-as list, and on JMAP one outside the domains the
// server reported a whole-domain identity for.
var ErrIdentityAddressNotAllowed = errors.New("the provider does not let this account send as that address")

// The whole-domain identities a provider reported are kept as a sync state
// of the account: domains, space-separated, in the cursor.
const (
	identityStateProvider = "identities"
	identityStateScope    = "catch_all_domains"
)

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func catchAllIdentityDomains(ctx context.Context, q queryRower, accountID string) ([]string, error) {
	var cursor string
	err := q.QueryRowContext(ctx, `SELECT cursor FROM label_sync_state WHERE account_id = ? AND provider_type = ? AND scope = ?`,
		accountID, identityStateProvider, identityStateScope).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return strings.Fields(cursor), err
}

// CatchAllIdentityDomains returns the domains the provider of one of the
// owner's accounts lets it send from at any address.
func (db *DB) CatchAllIdentityDomains(ctx context.Context, userID, accountID string) ([]string, error) {
	var owned int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE user_id = ? AND id = ?`, userID, accountID).Scan(&owned); err != nil || owned == 0 {
		return nil, err
	}
	return catchAllIdentityDomains(ctx, db.Read(), accountID)
}

func checkManualIdentityAddress(ctx context.Context, tx *sql.Tx, accountID, email string) error {
	var provider string
	if err := tx.QueryRowContext(ctx, `SELECT provider FROM accounts WHERE id = ?`, accountID).Scan(&provider); err != nil {
		return err
	}
	domains, err := catchAllIdentityDomains(ctx, tx, accountID)
	if err != nil {
		return err
	}
	// The rule's service only matters to what the compose form says.
	if !providers.AccountFromRule(provider, "", domains).Accepts(email) {
		return ErrIdentityAddressNotAllowed
	}
	return nil
}

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
		smtp_host TEXT NOT NULL DEFAULT '',
		smtp_port INTEGER NOT NULL DEFAULT 0,
		smtp_tls_mode TEXT NOT NULL DEFAULT '',
		smtp_username TEXT NOT NULL DEFAULT '',
		encrypted_smtp_password BLOB,
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

const accountIdentityColumns = `i.id, i.account_id, i.name, i.email, i.reply_to, COALESCE(i.signature_id, ''), i.source, i.provider_identity_id, i.sort_order,
	i.smtp_host, i.smtp_port, i.smtp_tls_mode, i.smtp_username, i.encrypted_smtp_password IS NOT NULL`

func scanAccountIdentity(scan func(...any) error) (models.Identity, error) {
	var identity models.Identity
	err := scan(&identity.ID, &identity.AccountID, &identity.Name, &identity.Email, &identity.ReplyTo, &identity.SignatureID, &identity.Source, &identity.ProviderIdentityID, &identity.SortOrder,
		&identity.SMTPHost, &identity.SMTPPort, &identity.SMTPTLSMode, &identity.SMTPUsername, &identity.HasSMTPPassword)
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

// ListAccountIdentitiesInternal returns an account's identities for work that
// already holds the account, such as sending, without an owner check.
func (db *DB) ListAccountIdentitiesInternal(ctx context.Context, accountID string) ([]models.Identity, error) {
	return queryAccountIdentities(ctx, db.Read(), `
		SELECT `+accountIdentityColumns+`
		FROM account_identities i
		WHERE i.account_id = ?
		ORDER BY i.sort_order, i.created_at, i.id`, accountID)
}

func (db *DB) attachAccountIdentities(ctx context.Context, userID string, accounts []models.Account) error {
	identities, err := db.ListUserIdentities(ctx, userID)
	if err != nil {
		return fmt.Errorf("list identities: %w", err)
	}
	for i := range accounts {
		for _, identity := range identities {
			if identity.AccountID == accounts[i].ID {
				accounts[i].Identities = append(accounts[i].Identities, identity)
			}
		}
	}
	return nil
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
	if err := NormalizeIdentitySMTP(&identity); err != nil {
		return models.Identity{}, err
	}

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
	if identity.HasOwnSMTP() {
		if err := checkIdentitySMTPAllowed(ctx, tx, identity.AccountID); err != nil {
			return models.Identity{}, err
		}
	}

	if identity.ID == "" {
		// An address sent through its own server needs no say from the
		// account's provider.
		if !identity.HasOwnSMTP() {
			if err := checkManualIdentityAddress(ctx, tx, identity.AccountID, identity.Email); err != nil {
				return models.Identity{}, err
			}
		}
		identity.ID = "idn_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO account_identities (id, account_id, name, email, reply_to, signature_id, source, sort_order, smtp_host, smtp_port, smtp_tls_mode, smtp_username)
			VALUES (?, ?, ?, ?, ?, ?, 'manual', (SELECT COALESCE(MAX(sort_order), -1) + 1 FROM account_identities WHERE account_id = ?), ?, ?, ?, ?)`,
			identity.ID, identity.AccountID, identity.Name, identity.Email, identity.ReplyTo, signature, identity.AccountID,
			identity.SMTPHost, identity.SMTPPort, identity.SMTPTLSMode, identity.SMTPUsername); err != nil {
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
			if identity.HasOwnSMTP() {
				return models.Identity{}, ErrIdentitySMTPNotSupported
			}
			identity.Name, identity.Email, identity.ReplyTo = current.Name, current.Email, current.ReplyTo
		} else if !identity.HasOwnSMTP() && (!strings.EqualFold(identity.Email, current.Email) || current.HasOwnSMTP()) {
			if err := checkManualIdentityAddress(ctx, tx, identity.AccountID, identity.Email); err != nil {
				return models.Identity{}, err
			}
		}
		// A stored password only ever goes to the server and login it was
		// given for.
		keepPassword := identity.HasOwnSMTP() && strings.EqualFold(identity.SMTPHost, current.SMTPHost) && identity.SMTPUsername == current.SMTPUsername
		if _, err := tx.ExecContext(ctx, `
			UPDATE account_identities SET name = ?, email = ?, reply_to = ?, signature_id = ?,
				smtp_host = ?, smtp_port = ?, smtp_tls_mode = ?, smtp_username = ?,
				encrypted_smtp_password = CASE WHEN ? THEN encrypted_smtp_password END,
				updated_at = CURRENT_TIMESTAMP
			WHERE id = ? AND account_id = ?`,
			identity.Name, identity.Email, identity.ReplyTo, signature,
			identity.SMTPHost, identity.SMTPPort, identity.SMTPTLSMode, identity.SMTPUsername, keepPassword,
			identity.ID, identity.AccountID); err != nil {
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
//
// Reported addresses Gofer cannot send as an identity are left out: the
// account's own address, whole-domain ones such as *@example.com, ones that
// are not an address, and ones a manual identity already has.
func (db *DB) ReplaceProviderIdentities(ctx context.Context, accountID string, reported []models.Identity) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var accountEmail string
	if err := tx.QueryRowContext(ctx, `SELECT email_address FROM accounts WHERE id = ?`, accountID).Scan(&accountEmail); err != nil {
		return err
	}
	taken := map[string]bool{strings.ToLower(strings.TrimSpace(accountEmail)): true}
	rows, err := tx.QueryContext(ctx, `SELECT email FROM account_identities WHERE account_id = ? AND source = 'manual'`, accountID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			rows.Close()
			return err
		}
		taken[strings.ToLower(email)] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var domains []string
	for _, identity := range reported {
		if domain, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(identity.Email)), "*@"); ok && domain != "" && !strings.ContainsAny(domain, "@ \t") && !slices.Contains(domains, domain) {
			domains = append(domains, domain)
		}
	}
	slices.Sort(domains)
	if len(domains) == 0 {
		_, err = tx.ExecContext(ctx, `DELETE FROM label_sync_state WHERE account_id = ? AND provider_type = ? AND scope = ?`, accountID, identityStateProvider, identityStateScope)
	} else {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO label_sync_state (account_id, provider_type, scope, cursor, last_success_at, updated_at)
			VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
			ON CONFLICT(account_id, provider_type, scope) DO UPDATE SET
				cursor = excluded.cursor, last_success_at = excluded.last_success_at, updated_at = excluded.updated_at`,
			accountID, identityStateProvider, identityStateScope, strings.Join(domains, " "))
	}
	if err != nil {
		return fmt.Errorf("save whole-domain identities: %w", err)
	}
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
			// A bad Reply-To alone does not cost the address.
			if email, replyTo, err = NormalizeIdentityAddresses(identity.Email, ""); err != nil {
				continue
			}
		}
		if taken[strings.ToLower(email)] {
			continue
		}
		taken[strings.ToLower(email)] = true
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
