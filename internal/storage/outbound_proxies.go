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

// OutboundProxy is a proxy an administrator saved for accounts to connect
// through. Its password stays encrypted here; only the config package can
// read it.
type OutboundProxy struct {
	ID                string
	Name              string
	Type              string
	Host              string
	Port              int
	Username          string
	EncryptedPassword []byte
	IsDefault         bool
}

const outboundProxiesTableSQL = `CREATE TABLE IF NOT EXISTS outbound_proxies (
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
	for _, statement := range []string{outboundProxiesTableSQL, outboundProxiesDefaultIndexSQL} {
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

const outboundProxyColumns = `id, name, type, host, port, username, encrypted_password, is_default`

func scanOutboundProxy(row interface{ Scan(...any) error }) (OutboundProxy, error) {
	var p OutboundProxy
	var isDefault int
	err := row.Scan(&p.ID, &p.Name, &p.Type, &p.Host, &p.Port, &p.Username, &p.EncryptedPassword, &isDefault)
	p.IsDefault = isDefault == 1
	return p, err
}

// ListOutboundProxies returns every saved proxy in name order.
func (db *DB) ListOutboundProxies(ctx context.Context) ([]OutboundProxy, error) {
	rows, err := db.read.QueryContext(ctx, `SELECT `+outboundProxyColumns+` FROM outbound_proxies ORDER BY name COLLATE NOCASE, id`)
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

// GetOutboundProxy returns the saved proxy with id, or
// ErrOutboundProxyNotFound.
func (db *DB) GetOutboundProxy(ctx context.Context, id string) (OutboundProxy, error) {
	p, err := scanOutboundProxy(db.read.QueryRowContext(ctx, `SELECT `+outboundProxyColumns+` FROM outbound_proxies WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return OutboundProxy{}, ErrOutboundProxyNotFound
	}
	return p, err
}

// DefaultOutboundProxy returns the default proxy, and false when there is
// none.
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

// NewOutboundProxyID returns an id for a proxy about to be created, so its
// password can be encrypted for that id before the row exists.
func NewOutboundProxyID() string { return uuid.NewString() }

// SaveOutboundProxy creates the proxy, or replaces the one with its id. The
// caller has validated it and encrypted its password for p.ID. Saving one as
// the default takes the default from any other.
func (db *DB) SaveOutboundProxy(ctx context.Context, p OutboundProxy, createdBy string) error {
	p.ID = strings.TrimSpace(p.ID)
	if p.ID == "" || p.ID == AccountProxyDirect {
		return fmt.Errorf("proxy id %q is not valid", p.ID)
	}
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var taken int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_proxies WHERE name = ? AND id != ?`, p.Name, p.ID).Scan(&taken); err != nil {
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
	isDefault := 0
	if p.IsDefault {
		isDefault = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO outbound_proxies (id, name, type, host, port, username, encrypted_password, is_default, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name, type = excluded.type, host = excluded.host, port = excluded.port,
			username = excluded.username, encrypted_password = excluded.encrypted_password,
			is_default = excluded.is_default, updated_at = CURRENT_TIMESTAMP`,
		p.ID, p.Name, p.Type, p.Host, p.Port, p.Username, p.EncryptedPassword, isDefault, strings.TrimSpace(createdBy)); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteOutboundProxy removes a saved proxy. Accounts that chose it are in
// other databases and keep its id; they connect as on the default until
// someone picks another.
func (db *DB) DeleteOutboundProxy(ctx context.Context, id string) error {
	result, err := db.write.ExecContext(ctx, `DELETE FROM outbound_proxies WHERE id = ?`, id)
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
