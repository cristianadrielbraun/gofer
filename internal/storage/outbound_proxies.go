package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Account proxy choices other than a saved proxy's id. An account's proxy is
// AccountProxyDefault (the administrator's default proxy, or a direct
// connection when there is none), AccountProxyDirect, or a proxy's id.
const (
	AccountProxyDefault = ""
	AccountProxyDirect  = "direct"
)

// ErrOutboundProxyNotFound means no saved proxy has the id.
var ErrOutboundProxyNotFound = errors.New("proxy not found")

// ErrOutboundProxyNameTaken means another saved proxy has the name.
var ErrOutboundProxyNameTaken = errors.New("another proxy already has this name")

// OutboundProxy is a proxy accounts can connect through: one an
// administrator saved, for everyone or for chosen people, or one a person
// saved for their own accounts. Its password stays encrypted here; only the
// config package can read it.
type OutboundProxy struct {
	ID                string
	Name              string
	Type              string
	Host              string
	Port              int
	Username          string
	EncryptedPassword []byte
	// IsDefault is set on the administrator's default proxy only.
	IsDefault bool
	// OwnerUserID is the person who saved the proxy for their own accounts,
	// or "" for a proxy the administrator saved.
	OwnerUserID string
	// Everyone is set when an administrator's proxy is for everyone rather
	// than only for the people chosen for it.
	Everyone bool
}

// outboundProxiesTableSQLv141 is the table as schema 141 made it, before
// proxies had owners and people; schema 142 rebuilds it.
const outboundProxiesTableSQLv141 = `CREATE TABLE IF NOT EXISTS outbound_proxies (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE CHECK (name <> ''),
    type TEXT NOT NULL CHECK (type IN ('http', 'socks5')),
    host TEXT NOT NULL CHECK (host <> ''),
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    username TEXT NOT NULL DEFAULT '',
    encrypted_password BLOB,
    is_default INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0, 1)),
    created_by TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

const outboundProxiesDefaultIndexSQL = `CREATE UNIQUE INDEX IF NOT EXISTS idx_outbound_proxies_default
    ON outbound_proxies(is_default) WHERE is_default = 1`

// migrateV140ToV141 adds saved outbound proxies and each account's choice of
// one. Every account starts on the default, and there is no default proxy, so
// connections stay direct until an administrator sets one.
func migrateV140ToV141(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 141 {
		return nil
	}
	for _, statement := range []string{outboundProxiesTableSQLv141, outboundProxiesDefaultIndexSQL} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	exists, err := tableExistsTx(tx, "accounts")
	if err != nil {
		return err
	}
	if exists {
		present, err := columnExistsTx(tx, "accounts", "proxy_id")
		if err != nil {
			return err
		}
		if !present {
			if _, err := tx.Exec(`ALTER TABLE accounts ADD COLUMN proxy_id TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
	}
	if err := markSchemaVersion(tx, 141); err != nil {
		return err
	}
	return tx.Commit()
}

// The proxies table from schema 142: a proxy's name is unique among its
// owner's, and only an administrator's proxy can be the default.
const outboundProxiesTableSQL = `CREATE TABLE IF NOT EXISTS outbound_proxies (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL CHECK (name <> ''),
    type TEXT NOT NULL CHECK (type IN ('http', 'socks5')),
    host TEXT NOT NULL CHECK (host <> ''),
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    username TEXT NOT NULL DEFAULT '',
    encrypted_password BLOB,
    is_default INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0, 1)),
    owner_user_id TEXT REFERENCES users(id) ON DELETE CASCADE,
    everyone INTEGER NOT NULL DEFAULT 1 CHECK (everyone IN (0, 1)),
    created_by TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CHECK (owner_user_id IS NULL OR is_default = 0)
)`

const outboundProxiesNameIndexSQL = `CREATE UNIQUE INDEX IF NOT EXISTS idx_outbound_proxies_name
    ON outbound_proxies(COALESCE(owner_user_id, ''), name)`

const outboundProxyUsersTableSQL = `CREATE TABLE IF NOT EXISTS outbound_proxy_users (
    proxy_id TEXT NOT NULL REFERENCES outbound_proxies(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (proxy_id, user_id)
)`

const outboundProxyPolicyTableSQL = `CREATE TABLE IF NOT EXISTS outbound_proxy_policy (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    people_add INTEGER NOT NULL DEFAULT 0 CHECK (people_add IN (0, 1))
)`

// migrateV141ToV142 lets an administrator's proxy be for chosen people, and
// people save proxies of their own when the administrator allows it. Every
// proxy saved so far is the administrator's and stays for everyone. The
// table is rebuilt so names are unique per owner, renaming in legacy mode as
// the other rebuilds do, so triggers of databases tested for old upgrades
// aren't checked again.
func migrateV141ToV142(db *sql.DB) (returnErr error) {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var version int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 142 {
		return nil
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA legacy_alter_table = ON`); err != nil {
		_, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
		return fmt.Errorf("enable legacy alter table mode: %w", err)
	}
	defer func() {
		_, legacyErr := conn.ExecContext(ctx, `PRAGMA legacy_alter_table = OFF`)
		_, foreignKeysErr := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)
		if returnErr == nil {
			if legacyErr != nil {
				returnErr = fmt.Errorf("disable legacy alter table mode: %w", legacyErr)
			} else if foreignKeysErr != nil {
				returnErr = fmt.Errorf("restore foreign keys: %w", foreignKeysErr)
			}
		}
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(outboundProxiesTableSQLv141); err != nil {
		return err
	}
	present, err := columnExistsTx(tx, "outbound_proxies", "owner_user_id")
	if err != nil {
		return err
	}
	if !present {
		for _, statement := range []string{
			`DROP INDEX IF EXISTS idx_outbound_proxies_default`,
			strings.Replace(outboundProxiesTableSQL, "outbound_proxies (", "outbound_proxies_v142 (", 1),
			`INSERT INTO outbound_proxies_v142 (id, name, type, host, port, username, encrypted_password, is_default, created_by, created_at, updated_at)
			 SELECT id, name, type, host, port, username, encrypted_password, is_default, created_by, created_at, updated_at FROM outbound_proxies`,
			`DROP TABLE outbound_proxies`,
			`ALTER TABLE outbound_proxies_v142 RENAME TO outbound_proxies`,
		} {
			if _, err := tx.Exec(statement); err != nil {
				return err
			}
		}
	}
	for _, statement := range []string{outboundProxiesNameIndexSQL, outboundProxiesDefaultIndexSQL, outboundProxyUsersTableSQL, outboundProxyPolicyTableSQL} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	if err := markSchemaVersion(tx, 142); err != nil {
		return err
	}
	return tx.Commit()
}

const outboundProxyColumns = `id, name, type, host, port, username, encrypted_password, is_default, COALESCE(owner_user_id, ''), everyone`

func scanOutboundProxy(row interface{ Scan(...any) error }) (OutboundProxy, error) {
	var p OutboundProxy
	var isDefault, everyone int
	err := row.Scan(&p.ID, &p.Name, &p.Type, &p.Host, &p.Port, &p.Username, &p.EncryptedPassword, &isDefault, &p.OwnerUserID, &everyone)
	p.IsDefault = isDefault == 1
	p.Everyone = everyone == 1
	return p, err
}

func (db *DB) queryOutboundProxies(ctx context.Context, where string, args ...any) ([]OutboundProxy, error) {
	rows, err := db.read.QueryContext(ctx, `SELECT `+outboundProxyColumns+` FROM outbound_proxies WHERE `+where+` ORDER BY name COLLATE NOCASE, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var proxies []OutboundProxy
	for rows.Next() {
		p, err := scanOutboundProxy(rows)
		if err != nil {
			return nil, err
		}
		proxies = append(proxies, p)
	}
	return proxies, rows.Err()
}

// ListOutboundProxies returns the administrator's proxies in name order.
func (db *DB) ListOutboundProxies(ctx context.Context) ([]OutboundProxy, error) {
	return db.queryOutboundProxies(ctx, `owner_user_id IS NULL`)
}

// ListOwnOutboundProxies returns the proxies a person saved for their own
// accounts, in name order.
func (db *DB) ListOwnOutboundProxies(ctx context.Context, userID string) ([]OutboundProxy, error) {
	return db.queryOutboundProxies(ctx, `owner_user_id = ?`, userID)
}

// OutboundProxiesForUser returns the proxies a person's accounts may use: the
// administrator's that are for everyone or for them, and, while the
// administrator lets people add their own, their own.
func (db *DB) OutboundProxiesForUser(ctx context.Context, userID string) ([]OutboundProxy, error) {
	return db.queryOutboundProxies(ctx, `(owner_user_id IS NULL AND (everyone = 1
		OR EXISTS (SELECT 1 FROM outbound_proxy_users u WHERE u.proxy_id = outbound_proxies.id AND u.user_id = ?)))
		OR (owner_user_id = ? AND EXISTS (SELECT 1 FROM outbound_proxy_policy WHERE singleton = 1 AND people_add = 1))`, userID, userID)
}

// GetOutboundProxy returns the saved proxy with id, or
// ErrOutboundProxyNotFound.
func (db *DB) GetOutboundProxy(ctx context.Context, id string) (OutboundProxy, error) {
	p, err := scanOutboundProxy(db.read.QueryRowContext(ctx, `SELECT `+outboundProxyColumns+` FROM outbound_proxies WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return OutboundProxy{}, ErrOutboundProxyNotFound
	}
	return p, err
}

// OutboundProxyAvailableTo reports whether the person's accounts may use p.
// An empty userID is work for nobody in particular, which may use any of the
// administrator's proxies.
func (db *DB) OutboundProxyAvailableTo(ctx context.Context, p OutboundProxy, userID string) (bool, error) {
	if p.OwnerUserID != "" {
		if p.OwnerUserID != userID {
			return false, nil
		}
		return db.PeopleMayAddOutboundProxies(ctx)
	}
	if p.Everyone || userID == "" {
		return true, nil
	}
	var chosen bool
	err := db.read.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM outbound_proxy_users WHERE proxy_id = ? AND user_id = ?)`, p.ID, userID).Scan(&chosen)
	return chosen, err
}

// DefaultOutboundProxy returns the administrator's default proxy, and false
// when there is none.
func (db *DB) DefaultOutboundProxy(ctx context.Context) (OutboundProxy, bool, error) {
	p, err := scanOutboundProxy(db.read.QueryRowContext(ctx, `SELECT `+outboundProxyColumns+` FROM outbound_proxies WHERE is_default = 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return OutboundProxy{}, false, nil
	}
	if err != nil {
		return OutboundProxy{}, false, err
	}
	return p, true, nil
}

// OutboundProxyUsers returns the people an administrator's proxy is for, when
// it isn't for everyone.
func (db *DB) OutboundProxyUsers(ctx context.Context, proxyID string) ([]string, error) {
	rows, err := db.read.QueryContext(ctx, `SELECT user_id FROM outbound_proxy_users WHERE proxy_id = ? ORDER BY user_id`, proxyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		users = append(users, id)
	}
	return users, rows.Err()
}

// SetOutboundProxyEveryone makes an administrator's proxy for everyone, or
// only for the people chosen for it.
func (db *DB) SetOutboundProxyEveryone(ctx context.Context, proxyID string, everyone bool) error {
	value := 0
	if everyone {
		value = 1
	}
	result, err := db.write.ExecContext(ctx, `UPDATE outbound_proxies SET everyone = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND owner_user_id IS NULL`, value, proxyID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrOutboundProxyNotFound
	}
	return nil
}

// SetOutboundProxyUser chooses a person for an administrator's proxy, or
// takes them off it.
func (db *DB) SetOutboundProxyUser(ctx context.Context, proxyID, userID string, chosen bool) error {
	if _, err := db.GetOutboundProxy(ctx, proxyID); err != nil {
		return err
	}
	if !chosen {
		_, err := db.write.ExecContext(ctx, `DELETE FROM outbound_proxy_users WHERE proxy_id = ? AND user_id = ?`, proxyID, userID)
		return err
	}
	_, err := db.write.ExecContext(ctx, `INSERT INTO outbound_proxy_users (proxy_id, user_id)
		SELECT id, ? FROM outbound_proxies WHERE id = ? AND owner_user_id IS NULL
		ON CONFLICT(proxy_id, user_id) DO NOTHING`, userID, proxyID)
	return err
}

// PeopleMayAddOutboundProxies reports whether the administrator lets people
// save proxies for their own accounts.
func (db *DB) PeopleMayAddOutboundProxies(ctx context.Context) (bool, error) {
	var allowed bool
	err := db.read.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM outbound_proxy_policy WHERE singleton = 1 AND people_add = 1)`).Scan(&allowed)
	return allowed, err
}

// SetPeopleMayAddOutboundProxies lets people save proxies for their own
// accounts, or stops them. Proxies people saved stay, unused, while they may
// not.
func (db *DB) SetPeopleMayAddOutboundProxies(ctx context.Context, allowed bool) error {
	value := 0
	if allowed {
		value = 1
	}
	_, err := db.write.ExecContext(ctx, `INSERT INTO outbound_proxy_policy (singleton, people_add) VALUES (1, ?)
		ON CONFLICT(singleton) DO UPDATE SET people_add = excluded.people_add`, value)
	return err
}

// NewOutboundProxyID returns an id for a proxy about to be created, so its
// password can be encrypted for that id before the row exists.
func NewOutboundProxyID() string { return uuid.NewString() }

// SaveOutboundProxy creates the proxy, or replaces the one with its id, which
// must have the same owner. The caller has validated it and encrypted its
// password for p.ID. Saving one as the default takes the default from any
// other; a person's own proxy can't be the default.
func (db *DB) SaveOutboundProxy(ctx context.Context, p OutboundProxy, createdBy string) error {
	p.ID = strings.TrimSpace(p.ID)
	if p.ID == "" || p.ID == AccountProxyDirect {
		return fmt.Errorf("proxy id %q is not valid", p.ID)
	}
	if p.OwnerUserID != "" {
		p.IsDefault, p.Everyone = false, false
	}
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owner sql.NullString
	switch err := tx.QueryRowContext(ctx, `SELECT owner_user_id FROM outbound_proxies WHERE id = ?`, p.ID).Scan(&owner); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case owner.String != p.OwnerUserID:
		return ErrOutboundProxyNotFound
	}
	var taken int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_proxies WHERE name = ? AND id != ? AND COALESCE(owner_user_id, '') = ?`,
		p.Name, p.ID, p.OwnerUserID).Scan(&taken); err != nil {
		return err
	}
	if taken > 0 {
		return ErrOutboundProxyNameTaken
	}
	if p.IsDefault {
		if _, err := tx.ExecContext(ctx, `UPDATE outbound_proxies SET is_default = 0, updated_at = CURRENT_TIMESTAMP WHERE is_default = 1 AND id != ?`, p.ID); err != nil {
			return err
		}
	}
	isDefault, everyone := 0, 0
	if p.IsDefault {
		isDefault = 1
	}
	if p.Everyone {
		everyone = 1
	}
	ownerValue := sql.NullString{String: p.OwnerUserID, Valid: p.OwnerUserID != ""}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbound_proxies (id, name, type, host, port, username, encrypted_password, is_default, owner_user_id, everyone, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name, type = excluded.type, host = excluded.host, port = excluded.port,
			username = excluded.username, encrypted_password = excluded.encrypted_password,
			is_default = excluded.is_default, updated_at = CURRENT_TIMESTAMP`,
		p.ID, p.Name, p.Type, p.Host, p.Port, p.Username, p.EncryptedPassword, isDefault, ownerValue, everyone, strings.TrimSpace(createdBy)); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteOutboundProxy removes a proxy of ownerUserID's, or of the
// administrator's when ownerUserID is "". Accounts that chose it are in other
// databases and keep its id; they connect as on the default until someone
// picks another.
func (db *DB) DeleteOutboundProxy(ctx context.Context, ownerUserID, id string) error {
	result, err := db.write.ExecContext(ctx, `DELETE FROM outbound_proxies WHERE id = ? AND COALESCE(owner_user_id, '') = ?`, id, ownerUserID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrOutboundProxyNotFound
	}
	return nil
}

// GetAccountProxyChoice returns the proxy the account chose: a proxy's id,
// AccountProxyDirect, or AccountProxyDefault.
func (db *DB) GetAccountProxyChoice(ctx context.Context, accountID string) (string, error) {
	var choice string
	err := db.read.QueryRowContext(ctx, `SELECT proxy_id FROM accounts WHERE id = ?`, accountID).Scan(&choice)
	return choice, err
}

// SetAccountProxyChoice records the proxy the owner's account connects
// through. The caller has checked that a proxy's id names a saved proxy.
func (db *DB) SetAccountProxyChoice(ctx context.Context, userID, accountID, choice string) error {
	result, err := db.write.ExecContext(ctx, `UPDATE accounts SET proxy_id = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND user_id = ? AND COALESCE(is_deleting, 0) = 0`, strings.TrimSpace(choice), accountID, userID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
