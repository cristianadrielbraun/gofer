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
	"oauth_accounts", "oauth_account_flows",
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

	if err := ensureUserStoreGuards(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ensureUserStoreGuards installs the owner and boundary triggers. It is
// idempotent, so it also restores guards on tables a schema upgrade rebuilt
// and covers owned tables the upgrade added.
func ensureUserStoreGuards(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TRIGGER IF NOT EXISTS gofer_store_owner_insert BEFORE INSERT ON users
		WHEN NEW.id IS NOT (SELECT user_id FROM gofer_user_store WHERE singleton = 1)
		BEGIN SELECT RAISE(ABORT, 'wrong user database'); END;
		CREATE TRIGGER IF NOT EXISTS gofer_store_owner_update BEFORE UPDATE OF id, user_type, is_admin ON users
		WHEN NEW.id IS NOT OLD.id OR NEW.user_type != 'webmail' OR NEW.is_admin != 0
		BEGIN SELECT RAISE(ABORT, 'user database owner is immutable'); END;
		CREATE TRIGGER IF NOT EXISTS gofer_store_owner_delete BEFORE DELETE ON users
		BEGIN SELECT RAISE(ABORT, 'user database owner is immutable'); END;
		CREATE TRIGGER IF NOT EXISTS gofer_store_identity_update BEFORE UPDATE ON gofer_user_store
		BEGIN SELECT RAISE(ABORT, 'user database identity is immutable'); END;
		CREATE TRIGGER IF NOT EXISTS gofer_store_identity_delete BEFORE DELETE ON gofer_user_store
		BEGIN SELECT RAISE(ABORT, 'user database identity is immutable'); END;
	`); err != nil {
		return err
	}

	centralTables := append(append([]string(nil), centralAuthenticationTables...), "web_push_subscriptions")
	for _, table := range centralTables {
		if err := guardSystemTable(ctx, tx, table); err != nil {
			return err
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
			sql := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s BEFORE %s ON %s
				WHEN NEW.user_id IS NOT (SELECT user_id FROM gofer_user_store WHERE singleton = 1)
				BEGIN SELECT RAISE(ABORT, 'wrong user database'); END`,
				quoteStoreIdentifier("gofer_store_owner_"+table+"_"+strings.ToLower(action)), action, quoteStoreIdentifier(table))
			if _, err := tx.ExecContext(ctx, sql); err != nil {
				return err
			}
		}
	}
	return nil
}

func guardSystemTable(ctx context.Context, tx *sql.Tx, table string) error {
	for _, action := range []string{"INSERT", "UPDATE", "DELETE"} {
		query := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %s BEFORE %s ON %s
			BEGIN SELECT RAISE(ABORT, 'system records belong in the system database'); END`,
			quoteStoreIdentifier("gofer_store_central_"+table+"_"+strings.ToLower(action)), action, quoteStoreIdentifier(table))
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	return nil
}

// Older opt-in user stores did not guard mailbox OAuth tables. Add the guards
// when reopening an empty store; unexpected local grants require an explicit
// migration and are never silently deleted or copied.
func ensureUserOAuthBoundary(ctx context.Context, db *DB) error {
	var credentials, flows, guards int
	if err := db.Read().QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM oauth_accounts), (SELECT COUNT(*) FROM oauth_account_flows),
		(SELECT COUNT(*) FROM sqlite_schema WHERE type='trigger' AND name IN (
		 'gofer_store_central_oauth_accounts_insert','gofer_store_central_oauth_accounts_update','gofer_store_central_oauth_accounts_delete',
		 'gofer_store_central_oauth_account_flows_insert','gofer_store_central_oauth_account_flows_update','gofer_store_central_oauth_account_flows_delete'))`).Scan(&credentials, &flows, &guards); err != nil {
		return err
	}
	if credentials != 0 || flows != 0 {
		return fmt.Errorf("%w: mailbox OAuth records require central migration", ErrUserStoreIdentity)
	}
	if guards == 6 {
		return nil
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"oauth_accounts", "oauth_account_flows"} {
		if err := guardSystemTable(ctx, tx, table); err != nil {
			return err
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
