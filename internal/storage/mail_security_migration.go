package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// migrateV114ToV115 lets plaintext exceptions name HTTP, for JMAP servers
// reached without TLS. SQLite cannot change a CHECK constraint in place, so
// the table is rebuilt with its rows.
func migrateV114ToV115(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 115 {
		return nil
	}
	exists, err := tableExistsTx(tx, "mail_security_exceptions")
	if err != nil {
		return err
	}
	if exists {
		for _, statement := range []string{
			`ALTER TABLE mail_security_exceptions RENAME TO mail_security_exceptions_v114`,
			`DROP INDEX IF EXISTS idx_mail_security_exceptions_lookup`,
			mailSecurityExceptionsTableSQL,
			`INSERT INTO mail_security_exceptions (id, kind, protocol, host, port, created_by, created_at)
			 SELECT id, kind, protocol, host, port, created_by, created_at FROM mail_security_exceptions_v114`,
			`DROP TABLE mail_security_exceptions_v114`,
			`CREATE INDEX IF NOT EXISTS idx_mail_security_exceptions_lookup
			 ON mail_security_exceptions(kind, protocol, host, port)`,
		} {
			if _, err := tx.Exec(statement); err != nil {
				return err
			}
		}
	}
	if err := markSchemaVersion(tx, 115); err != nil {
		return err
	}
	return tx.Commit()
}

// mailSecurityExceptionsTableSQL matches the table in schema.sql.
const mailSecurityExceptionsTableSQL = `CREATE TABLE mail_security_exceptions (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL CHECK (kind IN ('http_discovery', 'plaintext_transport', 'private_target')),
	protocol TEXT NOT NULL DEFAULT '',
	host TEXT NOT NULL CHECK (host <> ''),
	port INTEGER NOT NULL DEFAULT 0,
	created_by TEXT NOT NULL DEFAULT '',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	CHECK (
		(kind = 'http_discovery' AND protocol = '' AND port = 0)
		OR
		(kind = 'plaintext_transport' AND protocol IN ('http', 'imap', 'smtp') AND port BETWEEN 1 AND 65535)
		OR
		(kind = 'private_target' AND protocol IN ('http', 'https', 'imap', 'smtp', 'proxy') AND port BETWEEN 1 AND 65535)
	),
	UNIQUE(kind, protocol, host, port)
)`

// migrateV142ToV143 lets a private target exception approve a proxy that
// people may add for their own accounts on the local network. The table is
// rebuilt for its new CHECK, renaming in legacy mode so triggers of databases
// tested for old upgrades aren't checked again; a table made from the
// current schema already allows proxies.
func migrateV142ToV143(db *sql.DB) (returnErr error) {
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
	if version >= 143 {
		return nil
	}
	var definition string
	err = conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_schema WHERE type = 'table' AND name = 'mail_security_exceptions'`).Scan(&definition)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && strings.Contains(definition, "'proxy'")) {
		_, err := conn.ExecContext(ctx, `INSERT OR REPLACE INTO schema_version (version) VALUES (143)`)
		return err
	}
	if err != nil {
		return err
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
	for _, statement := range []string{
		`DROP INDEX IF EXISTS idx_mail_security_exceptions_lookup`,
		strings.Replace(mailSecurityExceptionsTableSQL, "mail_security_exceptions (", "mail_security_exceptions_v143 (", 1),
		`INSERT INTO mail_security_exceptions_v143 (id, kind, protocol, host, port, created_by, created_at)
		 SELECT id, kind, protocol, host, port, created_by, created_at FROM mail_security_exceptions`,
		`DROP TABLE mail_security_exceptions`,
		`ALTER TABLE mail_security_exceptions_v143 RENAME TO mail_security_exceptions`,
		`CREATE INDEX IF NOT EXISTS idx_mail_security_exceptions_lookup
		 ON mail_security_exceptions(kind, protocol, host, port)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	if err := markSchemaVersion(tx, 143); err != nil {
		return err
	}
	return tx.Commit()
}
