package storage

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const sessionsV79Table = `CREATE TABLE %s (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE CHECK (length(token_hash) = 64 AND token_hash NOT GLOB '*[^0-9a-f]*'),
	auth_version INTEGER NOT NULL CHECK (auth_version > 0),
	authentication_method TEXT NOT NULL CHECK (authentication_method IN ('legacy', 'password', 'passkey', 'totp', 'recovery_code', 'federated_google', 'federated_microsoft', 'federated_oidc')),
	assurance_level TEXT NOT NULL CHECK (assurance_level IN ('legacy', 'single_factor', 'multi_factor', 'phishing_resistant')),
	user_agent TEXT NOT NULL DEFAULT '',
	authenticated_at DATETIME NOT NULL,
	last_used_at DATETIME NOT NULL,
	idle_expires_at DATETIME NOT NULL,
	absolute_expires_at DATETIME NOT NULL,
	step_up_at DATETIME,
	step_up_method TEXT NOT NULL DEFAULT '' CHECK (step_up_method IN ('', 'password', 'passkey', 'totp', 'recovery_code', 'federated_google', 'federated_microsoft', 'federated_oidc')),
	revoked_at DATETIME,
	revoked_by TEXT REFERENCES users(id) ON DELETE SET NULL,
	revocation_reason TEXT NOT NULL DEFAULT '' CHECK (revocation_reason IN ('', 'logout', 'user_disabled', 'user_status_changed', 'credential_reset', 'admin_action', 'expired', 'rotation', 'role_changed')),
	created_at DATETIME NOT NULL,
	CHECK (
		(revoked_at IS NULL AND revocation_reason = '')
		OR (revoked_at IS NOT NULL AND revocation_reason <> '')
	),
	CHECK (
		(step_up_at IS NULL AND step_up_method = '')
		OR (step_up_at IS NOT NULL AND step_up_method <> '')
	),
	CHECK (revoked_at IS NOT NULL OR revoked_by IS NULL)
)`

const authChallengesV79Table = `CREATE TABLE %s (
	id TEXT PRIMARY KEY,
	user_id TEXT REFERENCES users(id) ON DELETE CASCADE,
	session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE,
	challenge_hash TEXT NOT NULL UNIQUE CHECK (challenge_hash <> ''),
	purpose TEXT NOT NULL CHECK (purpose IN ('login', 'mfa', 'enrollment', 'recovery', 'step_up', 'federated_login')),
	attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
	max_attempts INTEGER NOT NULL DEFAULT 1 CHECK (max_attempts > 0),
	payload_ciphertext BLOB,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	expires_at DATETIME NOT NULL,
	consumed_at DATETIME,
	CHECK (attempts <= max_attempts)
)`

const authEventsV79Table = `CREATE TABLE %s (
	id TEXT PRIMARY KEY,
	occurred_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	actor_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
	subject_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
	session_id TEXT REFERENCES sessions(id) ON DELETE SET NULL,
	request_id TEXT NOT NULL DEFAULT '',
	event_type TEXT NOT NULL CHECK (event_type <> '' AND length(event_type) <= 64),
	success INTEGER NOT NULL CHECK (success IN (0, 1)),
	reason TEXT NOT NULL DEFAULT '' CHECK (length(reason) <= 64),
	user_agent TEXT NOT NULL DEFAULT '' CHECK (length(user_agent) <= 1024),
	source_hash TEXT NOT NULL DEFAULT '',
	metadata_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(metadata_json))
)`

type sessionV78Record struct {
	id                   string
	userID               string
	legacyToken          string
	tokenHash            string
	authVersion          int64
	authenticationMethod string
	assuranceLevel       string
	userAgent            string
	authenticatedAt      time.Time
	lastUsedAt           time.Time
	idleExpiresAt        sql.NullTime
	absoluteExpiresAt    sql.NullTime
	legacyExpiresAt      time.Time
	stepUpAt             sql.NullTime
	stepUpMethod         string
	revokedAt            sql.NullTime
	revokedBy            sql.NullString
	revocationReason     string
	createdAt            time.Time
}

func migrateV78ToV79(tx *sql.Tx) error {
	hasUsers, err := tableExistsTx(tx, "users")
	if err != nil {
		return err
	}
	if !hasUsers {
		return markSchemaVersion(tx, 79)
	}
	if err := migrateSessionsToV79(tx); err != nil {
		return err
	}
	if err := foreignKeyCheckTx(tx); err != nil {
		return err
	}
	return markSchemaVersion(tx, 79)
}

func migrateSessionsToV79(tx *sql.Tx) error {
	hasSessions, err := tableExistsTx(tx, "sessions")
	if err != nil {
		return err
	}
	if !hasSessions {
		if _, err := tx.Exec(fmt.Sprintf(sessionsV79Table, "sessions")); err != nil {
			return err
		}
		if err := createSessionV79Indexes(tx); err != nil {
			return err
		}
		if err := rebuildAuthChallengesForV79(tx, false); err != nil {
			return err
		}
		return rebuildAuthEventsForV79(tx, false)
	}
	hasLegacyToken, err := columnExistsTx(tx, "sessions", "token")
	if err != nil {
		return err
	}
	if !hasLegacyToken {
		hasLegacyExpiry, err := columnExistsTx(tx, "sessions", "expires_at")
		if err != nil {
			return err
		}
		if hasLegacyExpiry {
			return fmt.Errorf("sessions table has legacy expiry without legacy token storage")
		}
		return createSessionV79Indexes(tx)
	}

	rows, err := tx.Query(`
		SELECT id, user_id, token, COALESCE(token_hash, ''), auth_version,
		       authentication_method, assurance_level, user_agent,
		       authenticated_at, last_used_at, idle_expires_at,
		       absolute_expires_at, expires_at, step_up_at, step_up_method,
		       revoked_at, revoked_by, revocation_reason, created_at
		FROM sessions
		ORDER BY id`)
	if err != nil {
		return fmt.Errorf("read v78 sessions: %w", err)
	}
	var sessions []sessionV78Record
	seenHashes := make(map[string]string)
	for rows.Next() {
		var session sessionV78Record
		if err := rows.Scan(
			&session.id, &session.userID, &session.legacyToken, &session.tokenHash, &session.authVersion,
			&session.authenticationMethod, &session.assuranceLevel, &session.userAgent,
			&session.authenticatedAt, &session.lastUsedAt, &session.idleExpiresAt,
			&session.absoluteExpiresAt, &session.legacyExpiresAt, &session.stepUpAt, &session.stepUpMethod,
			&session.revokedAt, &session.revokedBy, &session.revocationReason, &session.createdAt,
		); err != nil {
			rows.Close()
			return fmt.Errorf("scan v78 session: %w", err)
		}
		decoded, err := hex.DecodeString(session.tokenHash)
		if err != nil || len(decoded) != 32 || session.tokenHash != strings.ToLower(session.tokenHash) {
			rows.Close()
			return fmt.Errorf("session %q has an invalid token hash", session.id)
		}
		if existingID, exists := seenHashes[session.tokenHash]; exists {
			rows.Close()
			return fmt.Errorf("session token hash collision between sessions %q and %q", existingID, session.id)
		}
		seenHashes[session.tokenHash] = session.id
		if !session.absoluteExpiresAt.Valid {
			session.absoluteExpiresAt = sql.NullTime{Time: session.legacyExpiresAt, Valid: true}
		}
		if !session.idleExpiresAt.Valid {
			session.idleExpiresAt = sql.NullTime{Time: session.absoluteExpiresAt.Time, Valid: true}
		}
		if session.revokedAt.Valid && strings.TrimSpace(session.revocationReason) == "" {
			rows.Close()
			return fmt.Errorf("session %q is revoked without a reason", session.id)
		}
		if !session.revokedAt.Valid && session.revocationReason != "" {
			rows.Close()
			return fmt.Errorf("session %q has a revocation reason without a timestamp", session.id)
		}
		if !session.revokedAt.Valid && session.revokedBy.Valid {
			rows.Close()
			return fmt.Errorf("session %q has a revocation actor without a timestamp", session.id)
		}
		if session.stepUpAt.Valid != (session.stepUpMethod != "") {
			rows.Close()
			return fmt.Errorf("session %q has inconsistent step-up metadata", session.id)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read v78 sessions: %w", err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, session := range sessions {
		if session.legacyToken == "" || session.tokenHash != sessionTokenHash(session.legacyToken) {
			return fmt.Errorf("session %q token hash does not match its legacy bearer token", session.id)
		}
	}

	for _, table := range []string{"sessions_auth_v78_old", "auth_challenges_v78_old", "auth_events_v78_old"} {
		if exists, err := tableExistsTx(tx, table); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("temporary session migration table %q already exists", table)
		}
	}
	if _, err := tx.Exec(`ALTER TABLE sessions RENAME TO sessions_auth_v78_old`); err != nil {
		return fmt.Errorf("rename v78 sessions table: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(sessionsV79Table, "sessions")); err != nil {
		return fmt.Errorf("create hash-only sessions table: %w", err)
	}
	for _, session := range sessions {
		var revokedBy any
		if session.revokedBy.Valid && strings.TrimSpace(session.revokedBy.String) != "" {
			revokedBy = session.revokedBy.String
		}
		if _, err := tx.Exec(`
			INSERT INTO sessions (
				id, user_id, token_hash, auth_version, authentication_method, assurance_level,
				user_agent, authenticated_at, last_used_at, idle_expires_at, absolute_expires_at,
				step_up_at, step_up_method, revoked_at, revoked_by, revocation_reason, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			session.id, session.userID, session.tokenHash, session.authVersion,
			session.authenticationMethod, session.assuranceLevel, session.userAgent,
			session.authenticatedAt, session.lastUsedAt, session.idleExpiresAt.Time,
			session.absoluteExpiresAt.Time, nullableTime(session.stepUpAt), session.stepUpMethod,
			nullableTime(session.revokedAt), revokedBy, session.revocationReason, session.createdAt,
		); err != nil {
			return fmt.Errorf("migrate session %q to hash-only storage: %w", session.id, err)
		}
	}
	if err := rebuildAuthChallengesForV79(tx, true); err != nil {
		return err
	}
	if err := rebuildAuthEventsForV79(tx, true); err != nil {
		return err
	}
	var secureDeleteSetting int
	if err := tx.QueryRow(`PRAGMA secure_delete`).Scan(&secureDeleteSetting); err != nil {
		return fmt.Errorf("read secure deletion setting: %w", err)
	}
	if _, err := tx.Exec(`PRAGMA secure_delete = ON`); err != nil {
		return fmt.Errorf("enable secure deletion for legacy session tokens: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE sessions_auth_v78_old`); err != nil {
		_, _ = tx.Exec(fmt.Sprintf(`PRAGMA secure_delete = %d`, secureDeleteSetting))
		return fmt.Errorf("drop v78 sessions table: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA secure_delete = %d`, secureDeleteSetting)); err != nil {
		return fmt.Errorf("restore secure deletion setting: %w", err)
	}
	return createSessionV79Indexes(tx)
}

func rebuildAuthChallengesForV79(tx *sql.Tx, sessionTableRenamed bool) error {
	exists, err := tableExistsTx(tx, "auth_challenges")
	if err != nil {
		return err
	}
	if !exists {
		if _, err := tx.Exec(fmt.Sprintf(authChallengesV79Table, "auth_challenges")); err != nil {
			return err
		}
		return createAuthChallengeV79Indexes(tx)
	}
	if !sessionTableRenamed {
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE auth_challenges RENAME TO auth_challenges_v78_old`); err != nil {
		return fmt.Errorf("rename v78 auth challenges table: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(authChallengesV79Table, "auth_challenges")); err != nil {
		return fmt.Errorf("create v79 auth challenges table: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO auth_challenges (
			id, user_id, session_id, challenge_hash, purpose, attempts, max_attempts,
			payload_ciphertext, created_at, expires_at, consumed_at
		)
		SELECT id, user_id, session_id, challenge_hash, purpose, attempts, max_attempts,
		       payload_ciphertext, created_at, expires_at, consumed_at
		FROM auth_challenges_v78_old`); err != nil {
		return fmt.Errorf("copy v78 auth challenges: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE auth_challenges_v78_old`); err != nil {
		return fmt.Errorf("drop v78 auth challenges table: %w", err)
	}
	return createAuthChallengeV79Indexes(tx)
}

func rebuildAuthEventsForV79(tx *sql.Tx, sessionTableRenamed bool) error {
	exists, err := tableExistsTx(tx, "auth_events")
	if err != nil {
		return err
	}
	if !exists {
		if _, err := tx.Exec(fmt.Sprintf(authEventsV79Table, "auth_events")); err != nil {
			return err
		}
		return createAuthEventV79Indexes(tx)
	}
	if !sessionTableRenamed {
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE auth_events RENAME TO auth_events_v78_old`); err != nil {
		return fmt.Errorf("rename v78 auth events table: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(authEventsV79Table, "auth_events")); err != nil {
		return fmt.Errorf("create v79 auth events table: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO auth_events (
			id, occurred_at, actor_user_id, subject_user_id, session_id, request_id,
			event_type, success, reason, user_agent, source_hash, metadata_json
		)
		SELECT id, occurred_at, actor_user_id, subject_user_id, session_id, request_id,
		       event_type, success, reason, user_agent, source_hash, metadata_json
		FROM auth_events_v78_old`); err != nil {
		return fmt.Errorf("copy v78 auth events: %w", err)
	}
	if _, err := tx.Exec(`DROP TABLE auth_events_v78_old`); err != nil {
		return fmt.Errorf("drop v78 auth events table: %w", err)
	}
	return createAuthEventV79Indexes(tx)
}

func createSessionV79Indexes(tx *sql.Tx) error {
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_active_user ON sessions(user_id, revoked_at, absolute_expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_cleanup ON sessions(revoked_at, idle_expires_at, absolute_expires_at)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func createAuthChallengeV79Indexes(tx *sql.Tx) error {
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS idx_auth_challenges_active ON auth_challenges(purpose, expires_at) WHERE consumed_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_auth_challenges_user ON auth_challenges(user_id, purpose)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func createAuthEventV79Indexes(tx *sql.Tx) error {
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS idx_auth_events_subject ON auth_events(subject_user_id, occurred_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_auth_events_actor ON auth_events(actor_user_id, occurred_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_auth_events_type ON auth_events(event_type, occurred_at DESC)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func nullableTime(value sql.NullTime) any {
	if !value.Valid {
		return nil
	}
	return value.Time
}

func sessionTokenHash(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}
