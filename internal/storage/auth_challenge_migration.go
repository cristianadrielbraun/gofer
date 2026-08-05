package storage

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const authChallengesV80Table = `CREATE TABLE %s (
	id TEXT PRIMARY KEY,
	user_id TEXT REFERENCES users(id) ON DELETE CASCADE,
	session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE,
	challenge_hash TEXT NOT NULL UNIQUE CHECK (length(challenge_hash) = 64 AND challenge_hash NOT GLOB '*[^0-9a-f]*'),
	nonce_hash TEXT UNIQUE CHECK (nonce_hash IS NULL OR (length(nonce_hash) = 64 AND nonce_hash NOT GLOB '*[^0-9a-f]*')),
	purpose TEXT NOT NULL CHECK (purpose IN ('login', 'mfa', 'enrollment', 'recovery', 'step_up', 'federated_login')),
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

type authChallengeV79Record struct {
	id                string
	userID            sql.NullString
	sessionID         sql.NullString
	challengeHash     string
	purpose           string
	attempts          int
	maxAttempts       int
	payloadCiphertext []byte
	createdAt         time.Time
	expiresAt         time.Time
	consumedAt        sql.NullTime
}

func migrateV79ToV80(tx *sql.Tx) error {
	hasChallenges, err := tableExistsTx(tx, "auth_challenges")
	if err != nil {
		return err
	}
	if !hasChallenges {
		if _, err := tx.Exec(fmt.Sprintf(authChallengesV80Table, "auth_challenges")); err != nil {
			return fmt.Errorf("create v80 authentication challenges: %w", err)
		}
		if err := createAuthChallengeV80Indexes(tx); err != nil {
			return err
		}
		return markSchemaVersion(tx, 80)
	}
	hasOrigin, err := columnExistsTx(tx, "auth_challenges", "origin")
	if err != nil {
		return err
	}
	hasNonce, err := columnExistsTx(tx, "auth_challenges", "nonce_hash")
	if err != nil {
		return err
	}
	if hasOrigin && !hasNonce {
		return fmt.Errorf("origin-bound authentication challenge schema is missing nonce hash storage")
	}
	if hasOrigin {
		if err := createAuthChallengeV80Indexes(tx); err != nil {
			return err
		}
		return markSchemaVersion(tx, 80)
	}
	if exists, err := tableExistsTx(tx, "auth_challenges_v79_old"); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("temporary authentication challenge migration table already exists")
	}

	rows, err := tx.Query(`
		SELECT id, user_id, session_id, challenge_hash, purpose, attempts, max_attempts,
		       payload_ciphertext, created_at, expires_at, consumed_at
		FROM auth_challenges ORDER BY id`)
	if err != nil {
		return fmt.Errorf("read v79 authentication challenges: %w", err)
	}
	var challenges []authChallengeV79Record
	for rows.Next() {
		var challenge authChallengeV79Record
		if err := rows.Scan(
			&challenge.id, &challenge.userID, &challenge.sessionID, &challenge.challengeHash,
			&challenge.purpose, &challenge.attempts, &challenge.maxAttempts,
			&challenge.payloadCiphertext, &challenge.createdAt, &challenge.expiresAt, &challenge.consumedAt,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scan v79 authentication challenge: %w", err)
		}
		challenges = append(challenges, challenge)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read v79 authentication challenges: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	var invalidatedAt string
	if err := tx.QueryRow(`SELECT CURRENT_TIMESTAMP`).Scan(&invalidatedAt); err != nil {
		return fmt.Errorf("read challenge invalidation time: %w", err)
	}
	if _, err := tx.Exec(`ALTER TABLE auth_challenges RENAME TO auth_challenges_v79_old`); err != nil {
		return fmt.Errorf("rename v79 authentication challenges: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(authChallengesV80Table, "auth_challenges")); err != nil {
		return fmt.Errorf("create origin-bound authentication challenges: %w", err)
	}
	for _, challenge := range challenges {
		challengeHash := canonicalChallengeHash(challenge.challengeHash)
		var consumedAt any = invalidatedAt
		if challenge.consumedAt.Valid {
			consumedAt = challenge.consumedAt.Time
		}
		if _, err := tx.Exec(`
			INSERT INTO auth_challenges (
				id, user_id, session_id, challenge_hash, nonce_hash, purpose, origin, attempts,
				max_attempts, payload_ciphertext, created_at, expires_at, consumed_at
			) VALUES (?, ?, ?, ?, NULL, ?, '', ?, ?, ?, ?, ?, ?)`,
			challenge.id, nullableString(challenge.userID), nullableString(challenge.sessionID),
			challengeHash, challenge.purpose, challenge.attempts, challenge.maxAttempts,
			challenge.payloadCiphertext, challenge.createdAt, challenge.expiresAt, consumedAt,
		); err != nil {
			return fmt.Errorf("migrate authentication challenge %q: %w", challenge.id, err)
		}
	}
	if _, err := tx.Exec(`DROP TABLE auth_challenges_v79_old`); err != nil {
		return fmt.Errorf("drop v79 authentication challenges: %w", err)
	}
	if err := createAuthChallengeV80Indexes(tx); err != nil {
		return err
	}
	if err := foreignKeyCheckTx(tx); err != nil {
		return err
	}
	return markSchemaVersion(tx, 80)
}

func canonicalChallengeHash(value string) string {
	decoded, err := hex.DecodeString(value)
	if err == nil && len(decoded) == 32 && value == strings.ToLower(value) {
		return value
	}
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func createAuthChallengeV80Indexes(tx *sql.Tx) error {
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS idx_auth_challenges_active ON auth_challenges(purpose, expires_at) WHERE consumed_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_auth_challenges_user ON auth_challenges(user_id, purpose)`,
		`CREATE INDEX IF NOT EXISTS idx_auth_challenges_cleanup ON auth_challenges(consumed_at, expires_at)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func nullableString(value sql.NullString) any {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return nil
	}
	return value.String
}
