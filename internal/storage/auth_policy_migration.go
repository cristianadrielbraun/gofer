package storage

import (
	"database/sql"
	"fmt"
	"strings"
)

const authSystemStateV82Schema = `CREATE TABLE auth_system_state (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	initialized INTEGER NOT NULL DEFAULT 0 CHECK (initialized IN (0, 1)),
	owner_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
	initialized_at DATETIME,
	setup_token_hash TEXT,
	setup_expires_at DATETIME,
	setup_attempts INTEGER NOT NULL DEFAULT 0 CHECK (setup_attempts >= 0),
	setup_rotated_at DATETIME,
	cutover_version INTEGER NOT NULL DEFAULT 0 CHECK (cutover_version >= 0),
	mfa_policy TEXT NOT NULL DEFAULT 'administrators' CHECK (mfa_policy IN ('administrators', 'all_users')),
	security_policy_updated_at DATETIME,
	security_policy_updated_by TEXT REFERENCES users(id) ON DELETE SET NULL
)`

func migrateV81ToV82(tx *sql.Tx) error {
	hasState, err := tableExistsTx(tx, "auth_system_state")
	if err != nil {
		return err
	}
	if !hasState {
		if _, err := tx.Exec(authSystemStateV82Schema); err != nil {
			return fmt.Errorf("create authentication system state with MFA policy: %w", err)
		}
	} else {
		if _, err := tx.Exec(`ALTER TABLE auth_system_state RENAME TO auth_system_state_v81_old`); err != nil {
			return fmt.Errorf("rename v81 authentication system state: %w", err)
		}
		if _, err := tx.Exec(authSystemStateV82Schema); err != nil {
			return fmt.Errorf("create v82 authentication system state: %w", err)
		}

		columns := []string{
			"id", "initialized", "owner_user_id", "initialized_at", "setup_token_hash",
			"setup_expires_at", "setup_attempts", "setup_rotated_at", "cutover_version",
			"mfa_policy", "security_policy_updated_at", "security_policy_updated_by",
		}
		fallbacks := map[string]string{
			"id": "1", "initialized": "0", "owner_user_id": "NULL", "initialized_at": "NULL",
			"setup_token_hash": "NULL", "setup_expires_at": "NULL", "setup_attempts": "0",
			"setup_rotated_at": "NULL", "cutover_version": "0", "mfa_policy": "'administrators'",
			"security_policy_updated_at": "NULL", "security_policy_updated_by": "NULL",
		}
		expressions := make([]string, 0, len(columns))
		for _, column := range columns {
			exists, err := columnExistsTx(tx, "auth_system_state_v81_old", column)
			if err != nil {
				return err
			}
			if exists {
				expressions = append(expressions, column)
				continue
			}
			expressions = append(expressions, fallbacks[column])
		}
		if _, err := tx.Exec(`INSERT INTO auth_system_state (` + strings.Join(columns, ", ") + `)
			SELECT ` + strings.Join(expressions, ", ") + ` FROM auth_system_state_v81_old`); err != nil {
			return fmt.Errorf("copy v81 authentication system state: %w", err)
		}
		if _, err := tx.Exec(`DROP TABLE auth_system_state_v81_old`); err != nil {
			return fmt.Errorf("drop v81 authentication system state: %w", err)
		}
	}

	if err := foreignKeyCheckTx(tx); err != nil {
		return err
	}
	return markSchemaVersion(tx, 82)
}
