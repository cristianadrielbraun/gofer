package storage

import (
	"database/sql"
	"embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

type DB struct {
	write *sql.DB
	read  *sql.DB
	path  string
	threadingState ThreadingState
	threadingMu    sync.RWMutex
}

type ThreadingState struct {
	InProgress bool `json:"in_progress"`
	Processed  int  `json:"processed"`
	Total      int  `json:"total"`
}

func New(dbPath string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("create db directory: %w", err)
	}

	write, err := openDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open write connection: %w", err)
	}
	write.SetMaxOpenConns(1)

	read, err := openDB(dbPath)
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("open read connection: %w", err)
	}
	read.SetMaxOpenConns(4)

	db := &DB{
		write: write,
		read:  read,
		path:  dbPath,
	}

	if err := db.migrate(); err != nil {
		write.Close()
		read.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	log.Printf("storage: schema migration check complete")
	log.Printf("storage: threading backfill deferred to background startup worker")

	return db, nil
}

func (db *DB) SetThreadingState(state ThreadingState) {
	db.threadingMu.Lock()
	db.threadingState = state
	db.threadingMu.Unlock()
}

func (db *DB) GetThreadingState() ThreadingState {
	db.threadingMu.RLock()
	defer db.threadingMu.RUnlock()
	return db.threadingState
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=temp_store(MEMORY)&_texttotime=true")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) migrate() error {
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read embedded schema: %w", err)
	}

	tx, err := db.write.Begin()
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	defer tx.Rollback()

	var currentVersion int
	row := tx.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version")
	if err := row.Scan(&currentVersion); err != nil {
		if _, err := tx.Exec("CREATE TABLE IF NOT EXISTS schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP)"); err != nil {
			return fmt.Errorf("create schema_version table: %w", err)
		}
		currentVersion = 0
	}

	const targetSchemaVersion = 13

	if currentVersion >= targetSchemaVersion {
		log.Printf("schema at version %d, no migration needed", currentVersion)
		return nil
	}

	if currentVersion == 0 {
		if _, err := tx.Exec(string(schema)); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		log.Printf("schema initialized at version %d", targetSchemaVersion)
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration: %w", err)
		}
		return nil
	}

	if currentVersion >= 1 && currentVersion <= 1 {
		if err := migrateV1ToV2(tx); err != nil {
			return fmt.Errorf("migrate v1 to v2: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 2 {
		if err := migrateV2ToV3(tx); err != nil {
			return fmt.Errorf("migrate v2 to v3: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 3 {
		if err := migrateV3ToV4(tx); err != nil {
			return fmt.Errorf("migrate v3 to v4: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 4 {
		if err := migrateV4ToV5(tx); err != nil {
			return fmt.Errorf("migrate v4 to v5: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 5 {
		if err := migrateV5ToV6(tx); err != nil {
			return fmt.Errorf("migrate v5 to v6: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 6 {
		if err := migrateV6ToV7(tx); err != nil {
			return fmt.Errorf("migrate v6 to v7: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 7 {
		if err := migrateV7ToV8(tx); err != nil {
			return fmt.Errorf("migrate v7 to v8: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 8 {
		if err := migrateV8ToV9(tx); err != nil {
			return fmt.Errorf("migrate v8 to v9: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 9 {
		if err := migrateV9ToV10(tx); err != nil {
			return fmt.Errorf("migrate v9 to v10: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 10 {
		if err := migrateV10ToV11(tx); err != nil {
			return fmt.Errorf("migrate v10 to v11: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 11 {
		if err := migrateV11ToV12(tx); err != nil {
			return fmt.Errorf("migrate v11 to v12: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 12 {
		if err := migrateV12ToV13(tx); err != nil {
			return fmt.Errorf("migrate v12 to v13: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}

	if _, err := db.write.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		log.Printf("wal checkpoint: %v", err)
	}
	return nil
}

func migrateV1ToV2(tx *sql.Tx) error {
	migrations := []string{
		`DROP TABLE IF EXISTS message_fts`,
		`CREATE VIRTUAL TABLE message_fts USING fts5(subject, sender, recipients, body)`,
		`CREATE TRIGGER IF NOT EXISTS trg_messages_after_insert
		 AFTER INSERT ON messages
		 BEGIN
		     INSERT INTO message_fts(rowid, subject, sender, recipients, body)
		     VALUES (NEW.id, NEW.subject, NEW.from_name || ' <' || NEW.from_email || '>', '', COALESCE(NEW.preview_text, ''));
		 END`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (2)`,
	}

	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV2ToV3(tx *sql.Tx) error {
	migrations := []string{
		`ALTER TABLE accounts ADD COLUMN imap_host TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN imap_port INTEGER NOT NULL DEFAULT 993`,
		`ALTER TABLE accounts ADD COLUMN imap_tls_mode TEXT NOT NULL DEFAULT 'tls'`,
		`ALTER TABLE accounts ADD COLUMN smtp_host TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN smtp_port INTEGER NOT NULL DEFAULT 465`,
		`ALTER TABLE accounts ADD COLUMN smtp_tls_mode TEXT NOT NULL DEFAULT 'tls'`,
		`ALTER TABLE accounts ADD COLUMN username TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN encrypted_password BLOB`,
		`ALTER TABLE accounts ADD COLUMN auth_method TEXT NOT NULL DEFAULT 'plain'`,
		`ALTER TABLE folders ADD COLUMN highest_seen_uid INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE folders ADD COLUMN highest_modseq INTEGER`,
		`ALTER TABLE folders ADD COLUMN last_full_sync_at DATETIME`,
		`ALTER TABLE folders ADD COLUMN last_incremental_sync_at DATETIME`,
		`ALTER TABLE folders ADD COLUMN sync_error TEXT`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (3)`,
	}

	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV3ToV4(tx *sql.Tx) error {
	migrations := []string{
		`ALTER TABLE accounts ADD COLUMN smtp_username TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE accounts ADD COLUMN encrypted_smtp_password BLOB`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (4)`,
	}

	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV4ToV5(tx *sql.Tx) error {
	migrations := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_account_internet_msg_id
		 ON messages(account_id, internet_message_id)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (5)`,
	}

	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV5ToV6(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS app_settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (6)`,
	}

	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV6ToV7(tx *sql.Tx) error {
	migrations := []string{
		`ALTER TABLE messages ADD COLUMN in_reply_to TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE messages ADD COLUMN "references" TEXT NOT NULL DEFAULT ''`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (7)`,
	}

	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV7ToV8(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS threads (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			subject TEXT NOT NULL DEFAULT '',
			normalized_subject TEXT NOT NULL DEFAULT '',
			root_message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
			last_message_at DATETIME,
			message_count INTEGER NOT NULL DEFAULT 0,
			unread_count INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`ALTER TABLE messages ADD COLUMN message_id_normalized TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE messages ADD COLUMN normalized_subject TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE messages ADD COLUMN thread_parent_id INTEGER REFERENCES messages(id) ON DELETE SET NULL`,
		`ALTER TABLE messages ADD COLUMN provider_thread_id TEXT`,
		`CREATE TABLE IF NOT EXISTS message_references (
			message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			referenced_message_id TEXT NOT NULL,
			ordinal INTEGER NOT NULL,
			PRIMARY KEY (message_id, ordinal)
		)`,
		`CREATE TABLE IF NOT EXISTS unresolved_references (
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			referenced_message_id TEXT NOT NULL,
			child_message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			ordinal INTEGER NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (account_id, referenced_message_id, child_message_id, ordinal)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_threads_account_last ON threads(account_id, last_message_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_threads_subject ON threads(account_id, normalized_subject, last_message_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_msgid_norm ON messages(account_id, message_id_normalized)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_thread_date ON messages(account_id, thread_id, date_received)`,
		`CREATE INDEX IF NOT EXISTS idx_message_references_ref ON message_references(referenced_message_id)`,
		`CREATE INDEX IF NOT EXISTS idx_unresolved_references_ref ON unresolved_references(account_id, referenced_message_id)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (8)`,
	}

	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV8ToV9(tx *sql.Tx) error {
	migrations := []string{
		`DELETE FROM message_references`,
		`DELETE FROM unresolved_references`,
		`DELETE FROM threads`,
		`UPDATE messages SET thread_id = NULL, thread_parent_id = NULL, message_id_normalized = '', normalized_subject = ''`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (9)`,
	}

	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV9ToV10(tx *sql.Tx) error {
	migrations := []string{
		`DELETE FROM message_references`,
		`DELETE FROM unresolved_references`,
		`DELETE FROM threads`,
		`UPDATE messages SET thread_id = NULL, thread_parent_id = NULL, message_id_normalized = '', normalized_subject = ''`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (10)`,
	}

	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV10ToV11(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS remote_content_senders (
			sender_email TEXT PRIMARY KEY,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS remote_content_messages (
			message_id INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE
		)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (11)`,
	}

	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV11ToV12(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			email TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			avatar_url TEXT NOT NULL DEFAULT '',
			is_admin INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS oauth_accounts (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			provider TEXT NOT NULL,
			provider_account_id TEXT NOT NULL,
			access_token TEXT NOT NULL DEFAULT '',
			refresh_token TEXT NOT NULL DEFAULT '',
			token_type TEXT NOT NULL DEFAULT 'Bearer',
			expires_at DATETIME,
			scopes TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_oauth_provider_account
			ON oauth_accounts(provider, provider_account_id)`,
		`CREATE INDEX IF NOT EXISTS idx_oauth_accounts_user
			ON oauth_accounts(user_id)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			token TEXT NOT NULL UNIQUE,
			user_agent TEXT NOT NULL DEFAULT '',
			expires_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_token ON sessions(token)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at)`,
		`ALTER TABLE accounts ADD COLUMN user_id TEXT REFERENCES users(id) ON DELETE CASCADE`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_user ON accounts(user_id)`,
		`ALTER TABLE app_settings ADD COLUMN user_id TEXT REFERENCES users(id) ON DELETE CASCADE`,
		`CREATE INDEX IF NOT EXISTS idx_app_settings_user ON app_settings(user_id)`,
		`UPDATE accounts SET user_id = 'default' WHERE user_id IS NULL`,
		`UPDATE app_settings SET user_id = 'default' WHERE user_id IS NULL`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (12)`,
	}

	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV12ToV13(tx *sql.Tx) error {
	migrations := []string{
		`ALTER TABLE accounts ADD COLUMN is_deleting INTEGER NOT NULL DEFAULT 0`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (13)`,
	}

	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) Read() *sql.DB {
	return db.read
}

func (db *DB) Write() *sql.DB {
	return db.write
}

func (db *DB) Close() error {
	err1 := db.write.Close()
	err2 := db.read.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

func (db *DB) Path() string {
	return db.path
}
