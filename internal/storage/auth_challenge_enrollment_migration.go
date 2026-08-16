package storage

import (
	"database/sql"
	"fmt"
)

const authChallengesV84Table = `CREATE TABLE %s (
	id TEXT PRIMARY KEY,
	user_id TEXT REFERENCES users(id) ON DELETE CASCADE,
	session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE,
	challenge_hash TEXT NOT NULL UNIQUE CHECK (length(challenge_hash) = 64 AND challenge_hash NOT GLOB '*[^0-9a-f]*'),
	nonce_hash TEXT UNIQUE CHECK (nonce_hash IS NULL OR (length(nonce_hash) = 64 AND nonce_hash NOT GLOB '*[^0-9a-f]*')),
	purpose TEXT NOT NULL CHECK (purpose IN ('login', 'mfa', 'enrollment', 'recovery', 'step_up', 'federated_login', 'federated_link', 'federated_enrollment')),
	origin TEXT NOT NULL CHECK (length(origin) <= 2048),
	attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
	max_attempts INTEGER NOT NULL DEFAULT 1 CHECK (max_attempts > 0),
	payload_ciphertext BLOB,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	expires_at DATETIME NOT NULL,
	consumed_at DATETIME,
	CHECK (attempts <= max_attempts),
	CHECK (trim(origin) <> '' OR consumed_at IS NOT NULL)
)`

func migrateV83ToV84(tx *sql.Tx) error {
	exists, err := tableExistsTx(tx, "auth_challenges")
	if err != nil {
		return err
	}
	if !exists {
		if _, err := tx.Exec(fmt.Sprintf(authChallengesV84Table, "auth_challenges")); err != nil {
			return fmt.Errorf("create v84 authentication challenges: %w", err)
		}
		if err := createAuthChallengeV80Indexes(tx); err != nil {
			return err
		}
		return markSchemaVersion(tx, 84)
	}
	hasSessions, err := tableExistsTx(tx, "sessions")
	if err != nil {
		return err
	}
	if !hasSessions {
		var challengeCount int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM auth_challenges`).Scan(&challengeCount); err != nil {
			return fmt.Errorf("count authentication challenges without session schema: %w", err)
		}
		if challengeCount != 0 {
			return fmt.Errorf("cannot rebuild %d authentication challenges without session schema", challengeCount)
		}
		if _, err := tx.Exec(`DROP TABLE auth_challenges`); err != nil {
			return fmt.Errorf("drop empty partial authentication challenges: %w", err)
		}
		if _, err := tx.Exec(fmt.Sprintf(authChallengesV84Table, "auth_challenges")); err != nil {
			return fmt.Errorf("rebuild empty partial authentication challenges: %w", err)
		}
		if err := createAuthChallengeV80Indexes(tx); err != nil {
			return err
		}
		return markSchemaVersion(tx, 84)
	}
	if exists, err := tableExistsTx(tx, "auth_challenges_v83_old"); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("temporary v83 authentication challenge table already exists")
	}
	if _, err := tx.Exec(`ALTER TABLE auth_challenges RENAME TO auth_challenges_v83_old`); err != nil {
		return fmt.Errorf("rename v83 authentication challenges: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(authChallengesV84Table, "auth_challenges")); err != nil {
		return fmt.Errorf("create v84 authentication challenges: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO auth_challenges (
			id, user_id, session_id, challenge_hash, nonce_hash, purpose, origin,
			attempts, max_attempts, payload_ciphertext, created_at, expires_at, consumed_at
		)
		SELECT id, user_id, session_id, challenge_hash, nonce_hash, purpose, origin,
		       attempts, max_attempts, payload_ciphertext, created_at, expires_at, consumed_at
		FROM auth_challenges_v83_old`); err != nil {
		return fmt.Errorf("copy v83 authentication challenges: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE auth_challenges_v83_old`); err != nil {
		return fmt.Errorf("drop v83 authentication challenges: %w", err)
	}
	if err := createAuthChallengeV80Indexes(tx); err != nil {
		return err
	}
	if err := foreignKeyCheckTx(tx); err != nil {
		return err
	}
	return markSchemaVersion(tx, 84)
}
