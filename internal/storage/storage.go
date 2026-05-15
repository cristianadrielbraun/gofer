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
	write          *sql.DB
	read           *sql.DB
	path           string
	threadingState ThreadingState
	threadingMu    sync.RWMutex
	contactHookMu  sync.RWMutex
	contactHook    func(ContactActivityNotification)
}

type ContactActivityNotification struct {
	UserID    string
	EventType string
	Email     string
	Message   string
	Count     int
	CreatedAt string
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

func (db *DB) SetContactActivityHook(hook func(ContactActivityNotification)) {
	db.contactHookMu.Lock()
	db.contactHook = hook
	db.contactHookMu.Unlock()
}

func (db *DB) notifyContactActivity(event ContactActivityNotification) {
	db.contactHookMu.RLock()
	hook := db.contactHook
	db.contactHookMu.RUnlock()
	if hook != nil {
		hook(event)
	}
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

	const targetSchemaVersion = 24

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

	if currentVersion >= 1 && currentVersion <= 13 {
		if err := migrateV13ToV14(tx); err != nil {
			return fmt.Errorf("migrate v13 to v14: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 14 {
		if err := migrateV14ToV15(tx); err != nil {
			return fmt.Errorf("migrate v14 to v15: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 15 {
		if err := migrateV15ToV16(tx); err != nil {
			return fmt.Errorf("migrate v15 to v16: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 16 {
		if err := migrateV16ToV17(tx); err != nil {
			return fmt.Errorf("migrate v16 to v17: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 17 {
		if err := migrateV17ToV18(tx); err != nil {
			return fmt.Errorf("migrate v17 to v18: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 18 {
		if err := migrateV18ToV19(tx); err != nil {
			return fmt.Errorf("migrate v18 to v19: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 19 {
		if err := migrateV19ToV20(tx); err != nil {
			return fmt.Errorf("migrate v19 to v20: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 20 {
		if err := migrateV20ToV21(tx); err != nil {
			return fmt.Errorf("migrate v20 to v21: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 21 {
		if err := migrateV21ToV22(tx); err != nil {
			return fmt.Errorf("migrate v21 to v22: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 22 {
		if err := migrateV22ToV23(tx); err != nil {
			return fmt.Errorf("migrate v22 to v23: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 23 {
		if err := migrateV23ToV24(tx); err != nil {
			return fmt.Errorf("migrate v23 to v24: %w", err)
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

func migrateV13ToV14(tx *sql.Tx) error {
	migrations := []string{
		`ALTER TABLE messages ADD COLUMN body_html_original_path TEXT`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (14)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV14ToV15(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS signatures (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			html_body TEXT NOT NULL DEFAULT '',
			text_body TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_signatures_user ON signatures(user_id, name)`,
		`CREATE TABLE IF NOT EXISTS account_signature_settings (
			account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			new_signature_id TEXT REFERENCES signatures(id) ON DELETE SET NULL,
			reply_signature_id TEXT REFERENCES signatures(id) ON DELETE SET NULL,
			forward_signature_id TEXT REFERENCES signatures(id) ON DELETE SET NULL,
			new_enabled INTEGER NOT NULL DEFAULT 0,
			reply_enabled INTEGER NOT NULL DEFAULT 0,
			forward_enabled INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (15)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV15ToV16(tx *sql.Tx) error {
	migrations := []string{
		`ALTER TABLE account_signature_settings ADD COLUMN reply_placement TEXT NOT NULL DEFAULT 'before'`,
		`ALTER TABLE account_signature_settings ADD COLUMN forward_placement TEXT NOT NULL DEFAULT 'before'`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (16)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV16ToV17(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS sender_avatars (
			email_hash TEXT PRIMARY KEY,
			email TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT 'gravatar',
			status TEXT NOT NULL DEFAULT 'pending',
			content_type TEXT NOT NULL DEFAULT '',
			image_data BLOB,
			fetched_at DATETIME,
			expires_at DATETIME,
			next_retry_at DATETIME,
			error TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sender_avatars_status_retry ON sender_avatars(status, next_retry_at)`,
		`CREATE INDEX IF NOT EXISTS idx_sender_avatars_expires ON sender_avatars(expires_at)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (17)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV17ToV18(tx *sql.Tx) error {
	migrations := []string{
		`ALTER TABLE sender_avatars ADD COLUMN gravatar_status TEXT NOT NULL DEFAULT 'unchecked'`,
		`ALTER TABLE sender_avatars ADD COLUMN gravatar_checked_at DATETIME`,
		`ALTER TABLE sender_avatars ADD COLUMN bimi_status TEXT NOT NULL DEFAULT 'unchecked'`,
		`ALTER TABLE sender_avatars ADD COLUMN bimi_checked_at DATETIME`,
		`UPDATE sender_avatars
		 SET gravatar_status = CASE
		 	WHEN source = 'gravatar' AND status = 'found' THEN 'found'
		 	WHEN source = 'gravatar' AND status = 'error' THEN 'error'
		 	WHEN status IN ('found', 'missing') THEN 'missing'
		 	ELSE gravatar_status
		 END`,
		`UPDATE sender_avatars
		 SET bimi_status = CASE
		 	WHEN source = 'bimi' AND status = 'found' THEN 'found'
		 	WHEN source = 'bimi' AND status = 'error' THEN 'error'
		 	WHEN source = 'none' AND status = 'missing' THEN 'missing'
		 	ELSE bimi_status
		 END`,
		`UPDATE sender_avatars SET gravatar_checked_at = fetched_at WHERE gravatar_status != 'unchecked' AND fetched_at IS NOT NULL`,
		`UPDATE sender_avatars SET bimi_checked_at = fetched_at WHERE bimi_status NOT IN ('unchecked', 'skipped') AND fetched_at IS NOT NULL`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (18)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV18ToV19(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS avatar_attempt_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			email_hash TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL,
			status TEXT NOT NULL,
			message TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_avatar_attempt_logs_created ON avatar_attempt_logs(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_avatar_attempt_logs_provider_status ON avatar_attempt_logs(provider, status, created_at DESC)`,
		`INSERT INTO avatar_attempt_logs (email_hash, email, provider, status, message, created_at)
		 SELECT email_hash, email, 'gravatar', gravatar_status,
		 	CASE WHEN gravatar_status = 'error' THEN error ELSE '' END,
		 	COALESCE(gravatar_checked_at, fetched_at, updated_at, CURRENT_TIMESTAMP)
		 FROM sender_avatars
		 WHERE gravatar_status IN ('found', 'missing', 'error')`,
		`INSERT INTO avatar_attempt_logs (email_hash, email, provider, status, message, created_at)
		 SELECT email_hash, email, 'bimi', bimi_status,
		 	CASE WHEN bimi_status = 'error' THEN error ELSE '' END,
		 	COALESCE(bimi_checked_at, fetched_at, updated_at, CURRENT_TIMESTAMP)
		 FROM sender_avatars
		 WHERE bimi_status IN ('found', 'missing', 'skipped', 'error')`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (19)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV19ToV20(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS avatar_provider_states (
			email_hash TEXT NOT NULL,
			email TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'unchecked',
			message TEXT NOT NULL DEFAULT '',
			checked_at DATETIME,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (email_hash, provider)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_avatar_provider_states_provider_status ON avatar_provider_states(provider, status)`,
		`INSERT OR REPLACE INTO avatar_provider_states (email_hash, email, provider, status, message, checked_at)
		 SELECT email_hash, email, 'gravatar', gravatar_status,
		 	CASE WHEN gravatar_status = 'error' THEN error ELSE '' END,
		 	gravatar_checked_at
		 FROM sender_avatars
		 WHERE gravatar_status != 'unchecked'`,
		`INSERT OR REPLACE INTO avatar_provider_states (email_hash, email, provider, status, message, checked_at)
		 SELECT email_hash, email, 'bimi', bimi_status,
		 	CASE WHEN bimi_status = 'error' THEN error ELSE '' END,
		 	bimi_checked_at
		 FROM sender_avatars
		 WHERE bimi_status != 'unchecked'`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (20)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV20ToV21(tx *sql.Tx) error {
	migrations := []string{
		`ALTER TABLE sender_avatars ADD COLUMN storage_path TEXT NOT NULL DEFAULT ''`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (21)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV21ToV22(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS contacts (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			display_name TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT 'observed',
			is_manual INTEGER NOT NULL DEFAULT 0,
			is_deleted INTEGER NOT NULL DEFAULT 0,
			suppress_auto_create INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS contact_emails (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			contact_id TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
			email TEXT NOT NULL,
			normalized_email TEXT NOT NULL,
			label TEXT NOT NULL DEFAULT '',
			is_primary INTEGER NOT NULL DEFAULT 0,
			observed_name TEXT NOT NULL DEFAULT '',
			message_count INTEGER NOT NULL DEFAULT 0,
			last_seen_at DATETIME,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(user_id, normalized_email),
			UNIQUE(contact_id, normalized_email)
		)`,
		`CREATE TABLE IF NOT EXISTS contact_sources (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			contact_id TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
			provider TEXT NOT NULL,
			account_id TEXT NOT NULL DEFAULT '',
			remote_id TEXT NOT NULL DEFAULT '',
			etag TEXT NOT NULL DEFAULT '',
			sync_token TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contacts_user_name ON contacts(user_id, is_deleted, display_name COLLATE NOCASE)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_emails_contact ON contact_emails(contact_id)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_emails_search ON contact_emails(user_id, normalized_email)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_sources_contact ON contact_sources(contact_id)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (22)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV22ToV23(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS contact_save_targets (
			contact_id TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			target TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (contact_id, target)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_save_targets_user ON contact_save_targets(user_id, target)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (23)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV23ToV24(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS contact_activity_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			event_type TEXT NOT NULL,
			email TEXT NOT NULL DEFAULT '',
			message TEXT NOT NULL DEFAULT '',
			event_count INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_activity_events_user_created ON contact_activity_events(user_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_activity_events_type_created ON contact_activity_events(event_type, created_at DESC)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (24)`,
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
