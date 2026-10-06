package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// User stores reuse the current repository schema during the routing transition.
// Authentication tables stay empty and reject writes. Only an owner profile
// stub is copied for local foreign keys; it cannot authenticate anyone.
var centralAuthenticationTables = []string{
	"sessions", "auth_identities", "password_credentials", "webauthn_credentials",
	"webauthn_users", "totp_credentials", "recovery_codes", "auth_challenges",
	"auth_throttle", "auth_events", "user_enrollment_tokens", "auth_system_state",
}

func initializeUserStore(ctx context.Context, db *DB, owner userStoreOwner) error {
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE gofer_user_store (
		singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
		layout_version INTEGER NOT NULL CHECK (layout_version = 1),
		user_id TEXT NOT NULL CHECK (user_id != '')
	)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO gofer_user_store VALUES (1, 1, ?)`, owner.id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO users (id, username, username_normalized, name, avatar_url)
		VALUES (?, ?, ?, ?, ?)`, owner.id, owner.username, owner.normalized, owner.name, owner.avatar); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		CREATE TRIGGER gofer_store_owner_insert BEFORE INSERT ON users
		WHEN NEW.id IS NOT (SELECT user_id FROM gofer_user_store WHERE singleton = 1)
		BEGIN SELECT RAISE(ABORT, 'wrong user database'); END;
		CREATE TRIGGER gofer_store_owner_update BEFORE UPDATE OF id, user_type, is_admin ON users
		WHEN NEW.id IS NOT OLD.id OR NEW.user_type != 'webmail' OR NEW.is_admin != 0
		BEGIN SELECT RAISE(ABORT, 'user database owner is immutable'); END;
		CREATE TRIGGER gofer_store_owner_delete BEFORE DELETE ON users
		BEGIN SELECT RAISE(ABORT, 'user database owner is immutable'); END;
		CREATE TRIGGER gofer_store_identity_update BEFORE UPDATE ON gofer_user_store
		BEGIN SELECT RAISE(ABORT, 'user database identity is immutable'); END;
		CREATE TRIGGER gofer_store_identity_delete BEFORE DELETE ON gofer_user_store
		BEGIN SELECT RAISE(ABORT, 'user database identity is immutable'); END;
	`); err != nil {
		return err
	}

	centralTables := append(append([]string(nil), centralAuthenticationTables...), "web_push_subscriptions")
	for _, table := range centralTables {
		for _, action := range []string{"INSERT", "UPDATE", "DELETE"} {
			sql := fmt.Sprintf(`CREATE TRIGGER %s BEFORE %s ON %s
				BEGIN SELECT RAISE(ABORT, 'system records belong in the system database'); END`,
				quoteStoreIdentifier("gofer_store_central_"+table+"_"+strings.ToLower(action)), action, quoteStoreIdentifier(table))
			if _, err := tx.ExecContext(ctx, sql); err != nil {
				return err
			}
		}
	}
	// Guard direct ownership columns as well as foreign keys. This also rejects
	// NULL account owners inherited from the historical shared schema.
	tables, err := userOwnedTables(ctx, tx)
	if err != nil {
		return err
	}
	for _, table := range tables {
		for _, action := range []string{"INSERT", "UPDATE"} {
			sql := fmt.Sprintf(`CREATE TRIGGER %s BEFORE %s ON %s
				WHEN NEW.user_id IS NOT (SELECT user_id FROM gofer_user_store WHERE singleton = 1)
				BEGIN SELECT RAISE(ABORT, 'wrong user database'); END`,
				quoteStoreIdentifier("gofer_store_owner_"+table+"_"+strings.ToLower(action)), action, quoteStoreIdentifier(table))
			if _, err := tx.ExecContext(ctx, sql); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func userOwnedTables(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var owned []string
	for _, name := range names {
		hasUser, err := columnExistsTx(tx, name, "user_id")
		if err != nil {
			return nil, err
		}
		if hasUser {
			owned = append(owned, name)
		}
	}
	return owned, nil
}

func quoteStoreIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
