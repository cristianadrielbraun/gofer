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

	const targetSchemaVersion = 62

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

	if currentVersion >= 1 && currentVersion <= 24 {
		if err := migrateV24ToV25(tx); err != nil {
			return fmt.Errorf("migrate v24 to v25: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 25 {
		if err := migrateV25ToV26(tx); err != nil {
			return fmt.Errorf("migrate v25 to v26: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 26 {
		if err := migrateV26ToV27(tx); err != nil {
			return fmt.Errorf("migrate v26 to v27: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 27 {
		if err := migrateV27ToV28(tx); err != nil {
			return fmt.Errorf("migrate v27 to v28: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 28 {
		if err := migrateV28ToV29(tx); err != nil {
			return fmt.Errorf("migrate v28 to v29: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 29 {
		if err := migrateV29ToV30(tx); err != nil {
			return fmt.Errorf("migrate v29 to v30: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 30 {
		if err := migrateV30ToV31(tx); err != nil {
			return fmt.Errorf("migrate v30 to v31: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 31 {
		if err := migrateV31ToV32(tx); err != nil {
			return fmt.Errorf("migrate v31 to v32: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 32 {
		if err := migrateV32ToV33(tx); err != nil {
			return fmt.Errorf("migrate v32 to v33: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 33 {
		if err := migrateV33ToV34(tx); err != nil {
			return fmt.Errorf("migrate v33 to v34: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 34 {
		if err := migrateV34ToV35(tx); err != nil {
			return fmt.Errorf("migrate v34 to v35: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 35 {
		if err := migrateV35ToV36(tx); err != nil {
			return fmt.Errorf("migrate v35 to v36: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 36 {
		if err := migrateV36ToV37(tx); err != nil {
			return fmt.Errorf("migrate v36 to v37: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 37 {
		if err := migrateV37ToV38(tx); err != nil {
			return fmt.Errorf("migrate v37 to v38: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 38 {
		if err := migrateV38ToV39(tx); err != nil {
			return fmt.Errorf("migrate v38 to v39: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 39 {
		if err := migrateV39ToV40(tx); err != nil {
			return fmt.Errorf("migrate v39 to v40: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 40 {
		if err := migrateV40ToV41(tx); err != nil {
			return fmt.Errorf("migrate v40 to v41: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 41 {
		if err := migrateV41ToV42(tx); err != nil {
			return fmt.Errorf("migrate v41 to v42: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 42 {
		if err := migrateV42ToV43(tx); err != nil {
			return fmt.Errorf("migrate v42 to v43: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 43 {
		if err := migrateV43ToV44(tx); err != nil {
			return fmt.Errorf("migrate v43 to v44: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 44 {
		if err := migrateV44ToV45(tx); err != nil {
			return fmt.Errorf("migrate v44 to v45: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 45 {
		if err := migrateV45ToV46(tx); err != nil {
			return fmt.Errorf("migrate v45 to v46: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 46 {
		if err := migrateV46ToV47(tx); err != nil {
			return fmt.Errorf("migrate v46 to v47: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 47 {
		if err := migrateV47ToV48(tx); err != nil {
			return fmt.Errorf("migrate v47 to v48: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 48 {
		if err := migrateV48ToV49(tx); err != nil {
			return fmt.Errorf("migrate v48 to v49: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 49 {
		if err := migrateV49ToV50(tx); err != nil {
			return fmt.Errorf("migrate v49 to v50: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 50 {
		if err := migrateV50ToV51(tx); err != nil {
			return fmt.Errorf("migrate v50 to v51: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 51 {
		if err := migrateV51ToV52(tx); err != nil {
			return fmt.Errorf("migrate v51 to v52: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 52 {
		if err := migrateV52ToV53(tx); err != nil {
			return fmt.Errorf("migrate v52 to v53: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 53 {
		if err := migrateV53ToV54(tx); err != nil {
			return fmt.Errorf("migrate v53 to v54: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 54 {
		if err := migrateV54ToV55(tx); err != nil {
			return fmt.Errorf("migrate v54 to v55: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 55 {
		if err := migrateV55ToV56(tx); err != nil {
			return fmt.Errorf("migrate v55 to v56: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 56 {
		if err := migrateV56ToV57(tx); err != nil {
			return fmt.Errorf("migrate v56 to v57: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 57 {
		if err := migrateV57ToV58(tx); err != nil {
			return fmt.Errorf("migrate v57 to v58: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 58 {
		if err := migrateV58ToV59(tx); err != nil {
			return fmt.Errorf("migrate v58 to v59: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 59 {
		if err := migrateV59ToV60(tx); err != nil {
			return fmt.Errorf("migrate v59 to v60: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 60 {
		if err := migrateV60ToV61(tx); err != nil {
			return fmt.Errorf("migrate v60 to v61: %w", err)
		}
	}

	if currentVersion >= 1 && currentVersion <= 61 {
		if err := migrateV61ToV62(tx); err != nil {
			return fmt.Errorf("migrate v61 to v62: %w", err)
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

func migrateV24ToV25(tx *sql.Tx) error {
	if ok, err := columnExistsTx(tx, "accounts", "provider"); err != nil {
		return err
	} else if !ok {
		if _, err := tx.Exec(`ALTER TABLE accounts ADD COLUMN provider TEXT NOT NULL DEFAULT 'imap'`); err != nil {
			return err
		}
	}

	if ok, err := columnExistsTx(tx, "accounts", "provider_account_id"); err != nil {
		return err
	} else if !ok {
		if _, err := tx.Exec(`ALTER TABLE accounts ADD COLUMN provider_account_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}

	migrations := []string{
		`UPDATE accounts SET provider = 'gmail' WHERE provider = 'imap' AND imap_host = 'imap.gmail.com' AND auth_method = 'oauth2'`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_provider_identity ON accounts(provider, provider_account_id)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (25)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV25ToV26(tx *sql.Tx) error {
	migrations := []string{
		`UPDATE contacts
		 SET source = 'synced:' || (
		   SELECT a.id FROM accounts a
		   WHERE a.user_id = contacts.user_id AND a.provider = 'gmail'
		   LIMIT 1
		 )
		 WHERE source = 'provider:gmail'
		   AND 1 = (SELECT COUNT(*) FROM accounts a WHERE a.user_id = contacts.user_id AND a.provider = 'gmail')`,
		`INSERT OR IGNORE INTO contact_save_targets (contact_id, user_id, target, created_at, updated_at)
		 SELECT cst.contact_id, cst.user_id, 'account:' || a.id, cst.created_at, CURRENT_TIMESTAMP
		 FROM contact_save_targets cst
		 JOIN accounts a ON a.user_id = cst.user_id AND a.provider = 'gmail'
		 WHERE cst.target = 'gmail'
		   AND 1 = (SELECT COUNT(*) FROM accounts ga WHERE ga.user_id = cst.user_id AND ga.provider = 'gmail')`,
		`DELETE FROM contact_save_targets
		 WHERE target = 'gmail'
		   AND 1 = (SELECT COUNT(*) FROM accounts a WHERE a.user_id = contact_save_targets.user_id AND a.provider = 'gmail')`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (26)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV26ToV27(tx *sql.Tx) error {
	migrations := []string{
		`DELETE FROM contact_sources
		 WHERE rowid NOT IN (
		   SELECT MIN(rowid)
		   FROM contact_sources
		   GROUP BY user_id, contact_id, provider, account_id
		 )`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_contact_sources_contact_provider_account
		 ON contact_sources(user_id, contact_id, provider, account_id)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_sources_remote
		 ON contact_sources(user_id, provider, account_id, remote_id)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (27)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV27ToV28(tx *sql.Tx) error {
	migrations := []string{
		`CREATE INDEX IF NOT EXISTS idx_folders_role_account
		 ON folders(role, account_id, id)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_account_date_id
		 ON messages(account_id, date_received DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_folder_state_folder_deleted_msg
		 ON message_folder_state(folder_id, is_deleted, message_id)`,
		`CREATE INDEX IF NOT EXISTS idx_folder_state_starred_deleted_msg
		 ON message_folder_state(is_starred, is_deleted, message_id)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (28)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV28ToV29(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS folder_thread_state (
			folder_id TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
			thread_key TEXT NOT NULL,
			head_message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			last_message_at DATETIME,
			thread_count INTEGER NOT NULL DEFAULT 1,
			thread_is_read INTEGER NOT NULL DEFAULT 1,
			thread_is_starred INTEGER NOT NULL DEFAULT 0,
			thread_has_attachments INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (folder_id, thread_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_folder_thread_state_folder_last
		 ON folder_thread_state(folder_id, last_message_at DESC, head_message_id DESC)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (29)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV29ToV30(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS account_contact_sync_configs (
			account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			provider TEXT NOT NULL DEFAULT 'carddav',
			enabled INTEGER NOT NULL DEFAULT 0,
			base_url TEXT NOT NULL DEFAULT '',
			addressbook_url TEXT NOT NULL DEFAULT '',
			username TEXT NOT NULL DEFAULT '',
			encrypted_password BLOB,
			last_sync_token TEXT NOT NULL DEFAULT '',
			last_success_at DATETIME,
			last_error TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_account_contact_sync_configs_user
		 ON account_contact_sync_configs(user_id, enabled, provider)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (30)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV30ToV31(tx *sql.Tx) error {
	migrations := []string{
		`ALTER TABLE account_contact_sync_configs ADD COLUMN last_started_at DATETIME`,
		`ALTER TABLE account_contact_sync_configs ADD COLUMN last_import_count INTEGER NOT NULL DEFAULT 0`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (31)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV31ToV32(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS account_contact_address_books (
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			url TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			is_default INTEGER NOT NULL DEFAULT 0,
			last_sync_token TEXT NOT NULL DEFAULT '',
			last_success_at DATETIME,
			last_error TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (account_id, url)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_account_contact_address_books_user
		 ON account_contact_address_books(user_id, account_id)`,
		`INSERT OR IGNORE INTO account_contact_address_books (account_id, user_id, url, name, is_default, last_sync_token, last_success_at, last_error)
		 SELECT account_id, user_id, addressbook_url, '', 1, last_sync_token, last_success_at, last_error
		 FROM account_contact_sync_configs
		 WHERE TRIM(addressbook_url) != ''`,
		`DROP INDEX IF EXISTS idx_contact_sources_contact_provider_account`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_contact_sources_contact_provider_account_remote
		 ON contact_sources(user_id, contact_id, provider, account_id, remote_id)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (32)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV32ToV33(tx *sql.Tx) error {
	migrations := []string{
		`ALTER TABLE account_contact_address_books ADD COLUMN id TEXT NOT NULL DEFAULT ''`,
		`UPDATE account_contact_address_books SET id = lower(hex(randomblob(16))) WHERE id = ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_account_contact_address_books_id
		 ON account_contact_address_books(id)`,
		`ALTER TABLE contact_sources ADD COLUMN address_book_id TEXT NOT NULL DEFAULT ''`,
		`UPDATE contact_sources
		 SET address_book_id = COALESCE((
			SELECT ab.id
			FROM account_contact_address_books ab
			WHERE ab.account_id = contact_sources.account_id
			  AND contact_sources.remote_id LIKE ab.url || '%'
			ORDER BY length(ab.url) DESC
			LIMIT 1
		 ), '')
		 WHERE provider = 'carddav' AND remote_id != ''`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (33)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV33ToV34(tx *sql.Tx) error {
	migrations := []string{
		`ALTER TABLE accounts ADD COLUMN email_sync_enabled INTEGER NOT NULL DEFAULT 1`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (34)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV34ToV35(tx *sql.Tx) error {
	migrations := []string{
		`ALTER TABLE folders ADD COLUMN selectable INTEGER NOT NULL DEFAULT 1`,
		`UPDATE folders
		 SET selectable = 0
		 WHERE lower(COALESCE(remote_id, '')) IN ('[gmail]', '[google mail]')
		   AND account_id IN (
			SELECT id FROM accounts WHERE lower(COALESCE(imap_host, '')) IN ('imap.gmail.com', 'imap.googlemail.com')
		   )`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (35)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV35ToV36(tx *sql.Tx) error {
	migrations := []string{
		`DROP TRIGGER IF EXISTS trg_messages_after_insert`,
		`DROP TABLE IF EXISTS message_search_docs`,
		`DROP TABLE IF EXISTS message_fts`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS message_search USING fts5(
			account_id UNINDEXED,
			thread_key UNINDEXED,
			subject,
			sender,
			recipients,
			snippet,
			body,
			attachment_names,
			tokenize='unicode61 remove_diacritics 2'
		)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	if ok, err := tableExistsTx(tx, "messages"); err != nil {
		return err
	} else if ok {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO message_search(rowid, account_id, thread_key, subject, sender, recipients, snippet, body, attachment_names)
			SELECT m.id,
			       m.account_id,
			       COALESCE(NULLIF(m.thread_id, ''), printf('msg:%d', m.id)),
			       COALESCE(m.subject, ''),
			       trim(COALESCE(m.from_name, '') || ' ' || COALESCE(m.from_email, '')),
			       COALESCE((SELECT group_concat(trim(COALESCE(mr.name, '') || ' ' || COALESCE(mr.email, '')), ' ') FROM message_recipients mr WHERE mr.message_id = m.id), ''),
			       COALESCE(m.snippet, ''),
			       COALESCE(m.preview_text, m.snippet, ''),
			       COALESCE((SELECT group_concat(att.filename, ' ') FROM attachments att WHERE att.message_id = m.id), '')
			FROM messages m`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO schema_version (version) VALUES (36)`); err != nil {
		return err
	}
	return nil
}

func migrateV36ToV37(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS web_push_subscriptions (
			endpoint TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			p256dh TEXT NOT NULL,
			auth TEXT NOT NULL,
			user_agent TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_web_push_subscriptions_user
		 ON web_push_subscriptions(user_id)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (37)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV37ToV38(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS scheduled_sends (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			scheduled_for DATETIME NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			attempt_count INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			locked_at DATETIME,
			sent_message_id TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(message_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_scheduled_sends_due
		 ON scheduled_sends(status, scheduled_for)`,
		`CREATE INDEX IF NOT EXISTS idx_scheduled_sends_account
		 ON scheduled_sends(account_id, status, scheduled_for)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (38)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV38ToV39(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS contact_sync_operations (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			contact_id TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
			email TEXT NOT NULL DEFAULT '',
			payload_json TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending',
			attempt_count INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			locked_at DATETIME,
			next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_sync_operations_due
		 ON contact_sync_operations(status, next_attempt_at, locked_at)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_sync_operations_contact
		 ON contact_sync_operations(user_id, contact_id, created_at DESC)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (39)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV39ToV40(tx *sql.Tx) error {
	if _, err := tx.Exec(`DROP TABLE IF EXISTS contact_sync_operations`); err != nil {
		return err
	}
	for _, table := range []string{"contact_sources", "contact_save_targets", "contact_emails", "contacts"} {
		ok, err := tableExistsTx(tx, table)
		if err != nil {
			return err
		}
		if ok {
			if _, err := tx.Exec(`DELETE FROM ` + table); err != nil {
				return err
			}
		}
	}
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS contact_sync_operations (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			contact_id TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '',
			payload_json TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending',
			attempt_count INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			locked_at DATETIME,
			next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_sync_operations_due
		 ON contact_sync_operations(status, next_attempt_at, locked_at)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_sync_operations_contact
		 ON contact_sync_operations(user_id, contact_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS contact_profiles (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			display_name TEXT NOT NULL DEFAULT '',
			sort_name TEXT NOT NULL DEFAULT '',
			primary_email TEXT NOT NULL DEFAULT '',
			avatar_url TEXT NOT NULL DEFAULT '',
			notes TEXT NOT NULL DEFAULT '',
			is_deleted INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_profiles_user_name
		 ON contact_profiles(user_id, is_deleted, sort_name COLLATE NOCASE, display_name COLLATE NOCASE)`,
		`CREATE TABLE IF NOT EXISTS contact_cards (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			profile_id TEXT NOT NULL REFERENCES contact_profiles(id) ON DELETE CASCADE,
			kind TEXT NOT NULL DEFAULT 'local',
			provider TEXT NOT NULL DEFAULT '',
			account_id TEXT NOT NULL DEFAULT '',
			address_book_id TEXT NOT NULL DEFAULT '',
			remote_id TEXT NOT NULL DEFAULT '',
			etag TEXT NOT NULL DEFAULT '',
			raw_payload TEXT NOT NULL DEFAULT '',
			raw_payload_type TEXT NOT NULL DEFAULT '',
			sync_status TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '',
			is_deleted INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_cards_profile
		 ON contact_cards(user_id, profile_id, is_deleted)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_cards_remote
		 ON contact_cards(user_id, provider, account_id, address_book_id, remote_id)`,
		`CREATE TABLE IF NOT EXISTS contact_fields (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			profile_id TEXT NOT NULL REFERENCES contact_profiles(id) ON DELETE CASCADE,
			card_id TEXT REFERENCES contact_cards(id) ON DELETE CASCADE,
			kind TEXT NOT NULL,
			label TEXT NOT NULL DEFAULT '',
			value TEXT NOT NULL DEFAULT '',
			normalized_value TEXT NOT NULL DEFAULT '',
			is_primary INTEGER NOT NULL DEFAULT 0,
			ordinal INTEGER NOT NULL DEFAULT 0,
			source TEXT NOT NULL DEFAULT '',
			confidence REAL NOT NULL DEFAULT 1.0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_fields_profile
		 ON contact_fields(user_id, profile_id, kind, ordinal)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_fields_lookup
		 ON contact_fields(user_id, kind, normalized_value)`,
		`CREATE TABLE IF NOT EXISTS contact_identities (
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			profile_id TEXT NOT NULL REFERENCES contact_profiles(id) ON DELETE CASCADE,
			kind TEXT NOT NULL,
			normalized_value TEXT NOT NULL,
			confidence REAL NOT NULL DEFAULT 1.0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (user_id, kind, normalized_value)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_identities_profile
		 ON contact_identities(user_id, profile_id)`,
		`CREATE TABLE IF NOT EXISTS contact_observations (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			profile_id TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '',
			normalized_email TEXT NOT NULL,
			observed_name TEXT NOT NULL DEFAULT '',
			message_count INTEGER NOT NULL DEFAULT 0,
			last_seen_at DATETIME,
			is_suppressed INTEGER NOT NULL DEFAULT 0,
			suppress_auto_create INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(user_id, normalized_email)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_observations_profile
		 ON contact_observations(user_id, profile_id)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_observations_suppressed
		 ON contact_observations(user_id, is_suppressed, updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS contact_groups (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			provider TEXT NOT NULL DEFAULT '',
			account_id TEXT NOT NULL DEFAULT '',
			remote_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			color TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_groups_user_name
		 ON contact_groups(user_id, name COLLATE NOCASE)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_contact_groups_remote
		 ON contact_groups(user_id, provider, account_id, remote_id)`,
		`CREATE TABLE IF NOT EXISTS contact_card_groups (
			card_id TEXT NOT NULL REFERENCES contact_cards(id) ON DELETE CASCADE,
			group_id TEXT NOT NULL REFERENCES contact_groups(id) ON DELETE CASCADE,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (card_id, group_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_card_groups_user
		 ON contact_card_groups(user_id, group_id)`,
		`CREATE TABLE IF NOT EXISTS contact_conflicts (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			profile_id TEXT NOT NULL REFERENCES contact_profiles(id) ON DELETE CASCADE,
			field_kind TEXT NOT NULL DEFAULT '',
			local_value TEXT NOT NULL DEFAULT '',
			remote_value TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '',
			account_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'open',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_conflicts_profile
		 ON contact_conflicts(user_id, profile_id, status)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (40)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV40ToV41(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS contact_observations (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			profile_id TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '',
			normalized_email TEXT NOT NULL,
			observed_name TEXT NOT NULL DEFAULT '',
			message_count INTEGER NOT NULL DEFAULT 0,
			last_seen_at DATETIME,
			is_suppressed INTEGER NOT NULL DEFAULT 0,
			suppress_auto_create INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(user_id, normalized_email)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_observations_profile
		 ON contact_observations(user_id, profile_id)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_observations_suppressed
		 ON contact_observations(user_id, is_suppressed, updated_at DESC)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (41)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV41ToV42(tx *sql.Tx) error {
	if ok, err := columnExistsTx(tx, "accounts", "email_sync_error"); err != nil {
		return err
	} else if !ok {
		if _, err := tx.Exec(`ALTER TABLE accounts ADD COLUMN email_sync_error TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}

	if ok, err := columnExistsTx(tx, "accounts", "email_sync_error_at"); err != nil {
		return err
	} else if !ok {
		if _, err := tx.Exec(`ALTER TABLE accounts ADD COLUMN email_sync_error_at DATETIME`); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(`INSERT OR REPLACE INTO schema_version (version) VALUES (42)`); err != nil {
		return err
	}
	return nil
}

func migrateV42ToV43(tx *sql.Tx) error {
	createTables := []string{
		`CREATE TABLE IF NOT EXISTS labels (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			color TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS message_labels (
			message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			label_id TEXT NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
			PRIMARY KEY (message_id, label_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_message_labels_message
		 ON message_labels(message_id)`,
		`CREATE INDEX IF NOT EXISTS idx_message_labels_label
		 ON message_labels(label_id)`,
	}
	for _, m := range createTables {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}

	for _, column := range []struct {
		name string
		sql  string
	}{
		{name: "provider_id", sql: `ALTER TABLE labels ADD COLUMN provider_id TEXT NOT NULL DEFAULT ''`},
		{name: "provider_type", sql: `ALTER TABLE labels ADD COLUMN provider_type TEXT NOT NULL DEFAULT ''`},
		{name: "is_system", sql: `ALTER TABLE labels ADD COLUMN is_system INTEGER NOT NULL DEFAULT 0`},
		{name: "updated_at", sql: `ALTER TABLE labels ADD COLUMN updated_at DATETIME NOT NULL DEFAULT ''`},
	} {
		if ok, err := columnExistsTx(tx, "labels", column.name); err != nil {
			return err
		} else if !ok {
			if _, err := tx.Exec(column.sql); err != nil {
				return err
			}
		}
	}

	migrations := []string{
		`CREATE INDEX IF NOT EXISTS idx_labels_account_name
		 ON labels(account_id, name COLLATE NOCASE)`,
		`CREATE INDEX IF NOT EXISTS idx_labels_account_provider
		 ON labels(account_id, provider_type, provider_id)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (43)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV43ToV44(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS label_sync_state (
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			provider_type TEXT NOT NULL,
			scope TEXT NOT NULL DEFAULT '',
			cursor TEXT NOT NULL DEFAULT '',
			last_full_sync_at DATETIME,
			last_success_at DATETIME,
			last_error TEXT NOT NULL DEFAULT '',
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (account_id, provider_type, scope)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_label_sync_state_account
		 ON label_sync_state(account_id, provider_type)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (44)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV44ToV45(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS label_mutation_queue (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			folder_id TEXT NOT NULL DEFAULT '',
			provider_type TEXT NOT NULL,
			operation TEXT NOT NULL,
			label_name TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			last_error TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_label_mutation_queue_due
		 ON label_mutation_queue(account_id, provider_type, next_attempt_at)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_label_mutation_queue_unique
		 ON label_mutation_queue(message_id, provider_type, operation, label_name COLLATE NOCASE)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (45)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV45ToV46(tx *sql.Tx) error {
	if ok, err := columnExistsTx(tx, "label_mutation_queue", "folder_id"); err != nil {
		return err
	} else if !ok {
		if _, err := tx.Exec(`ALTER TABLE label_mutation_queue ADD COLUMN folder_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO schema_version (version) VALUES (46)`); err != nil {
		return err
	}
	return nil
}

func migrateV46ToV47(tx *sql.Tx) error {
	if ok, err := tableExistsTx(tx, "label_sync_state"); err != nil {
		return err
	} else if !ok {
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS label_sync_state (
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			provider_type TEXT NOT NULL,
			scope TEXT NOT NULL DEFAULT '',
			cursor TEXT NOT NULL DEFAULT '',
			last_full_sync_at DATETIME,
			last_success_at DATETIME,
			last_error TEXT NOT NULL DEFAULT '',
			last_run_started_at DATETIME,
			last_run_finished_at DATETIME,
			last_total_messages INTEGER NOT NULL DEFAULT 0,
			last_synced_messages INTEGER NOT NULL DEFAULT 0,
			last_with_labels INTEGER NOT NULL DEFAULT 0,
			last_without_labels INTEGER NOT NULL DEFAULT 0,
			last_missing_provider_messages INTEGER NOT NULL DEFAULT 0,
			last_skipped_messages INTEGER NOT NULL DEFAULT 0,
			last_failed_messages INTEGER NOT NULL DEFAULT 0,
			last_pending_mutations INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (account_id, provider_type, scope)
		)`); err != nil {
			return err
		}
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_label_sync_state_account
		 ON label_sync_state(account_id, provider_type)`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO schema_version (version) VALUES (47)`); err != nil {
			return err
		}
		return nil
	}
	columns := []struct {
		name string
		sql  string
	}{
		{name: "last_run_started_at", sql: `ALTER TABLE label_sync_state ADD COLUMN last_run_started_at DATETIME`},
		{name: "last_run_finished_at", sql: `ALTER TABLE label_sync_state ADD COLUMN last_run_finished_at DATETIME`},
		{name: "last_total_messages", sql: `ALTER TABLE label_sync_state ADD COLUMN last_total_messages INTEGER NOT NULL DEFAULT 0`},
		{name: "last_synced_messages", sql: `ALTER TABLE label_sync_state ADD COLUMN last_synced_messages INTEGER NOT NULL DEFAULT 0`},
		{name: "last_with_labels", sql: `ALTER TABLE label_sync_state ADD COLUMN last_with_labels INTEGER NOT NULL DEFAULT 0`},
		{name: "last_without_labels", sql: `ALTER TABLE label_sync_state ADD COLUMN last_without_labels INTEGER NOT NULL DEFAULT 0`},
		{name: "last_missing_provider_messages", sql: `ALTER TABLE label_sync_state ADD COLUMN last_missing_provider_messages INTEGER NOT NULL DEFAULT 0`},
		{name: "last_skipped_messages", sql: `ALTER TABLE label_sync_state ADD COLUMN last_skipped_messages INTEGER NOT NULL DEFAULT 0`},
		{name: "last_failed_messages", sql: `ALTER TABLE label_sync_state ADD COLUMN last_failed_messages INTEGER NOT NULL DEFAULT 0`},
		{name: "last_pending_mutations", sql: `ALTER TABLE label_sync_state ADD COLUMN last_pending_mutations INTEGER NOT NULL DEFAULT 0`},
	}
	for _, column := range columns {
		if ok, err := columnExistsTx(tx, "label_sync_state", column.name); err != nil {
			return err
		} else if !ok {
			if _, err := tx.Exec(column.sql); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO schema_version (version) VALUES (47)`); err != nil {
		return err
	}
	return nil
}

func migrateV47ToV48(tx *sql.Tx) error {
	hasFolders := false
	if ok, err := tableExistsTx(tx, "folders"); err != nil {
		return err
	} else if ok {
		hasFolders = true
		if exists, err := columnExistsTx(tx, "folders", "provider_remote_id"); err != nil {
			return err
		} else if !exists {
			if _, err := tx.Exec(`ALTER TABLE folders ADD COLUMN provider_remote_id TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
	}
	if hasFolders {
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_folders_account_provider_remote
		 ON folders(account_id, provider_remote_id)`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO schema_version (version) VALUES (48)`); err != nil {
		return err
	}
	return nil
}

func migrateV48ToV49(tx *sql.Tx) error {
	hasAttachments := false
	if ok, err := tableExistsTx(tx, "attachments"); err != nil {
		return err
	} else if ok {
		hasAttachments = true
		if exists, err := columnExistsTx(tx, "attachments", "provider_remote_id"); err != nil {
			return err
		} else if !exists {
			if _, err := tx.Exec(`ALTER TABLE attachments ADD COLUMN provider_remote_id TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
	}
	if hasAttachments {
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_attachments_message_provider_remote
		 ON attachments(message_id, provider_remote_id)`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO schema_version (version) VALUES (49)`); err != nil {
		return err
	}
	return nil
}

func migrateV49ToV50(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS gmail_poll_state (
			account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			profile_history_id TEXT NOT NULL DEFAULT '',
			last_checked_at DATETIME,
			last_changed_at DATETIME,
			last_error TEXT NOT NULL DEFAULT '',
			consecutive_errors INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_gmail_poll_state_checked
		 ON gmail_poll_state(last_checked_at)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (50)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV50ToV51(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS gmail_poll_state (
			account_id TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			profile_history_id TEXT NOT NULL DEFAULT '',
			last_checked_at DATETIME,
			last_changed_at DATETIME,
			last_error TEXT NOT NULL DEFAULT '',
			consecutive_errors INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_gmail_poll_state_checked
		 ON gmail_poll_state(last_checked_at)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	tableIndexes := []struct {
		table string
		sql   string
	}{
		{"messages", `CREATE INDEX IF NOT EXISTS idx_messages_thread_parent ON messages(thread_parent_id)`},
		{"threads", `CREATE INDEX IF NOT EXISTS idx_threads_root_message ON threads(root_message_id)`},
		{"folder_thread_state", `CREATE INDEX IF NOT EXISTS idx_folder_thread_state_account ON folder_thread_state(account_id)`},
		{"folder_thread_state", `CREATE INDEX IF NOT EXISTS idx_folder_thread_state_head ON folder_thread_state(head_message_id)`},
		{"unresolved_references", `CREATE INDEX IF NOT EXISTS idx_unresolved_references_child ON unresolved_references(child_message_id)`},
		{"contact_cards", `CREATE INDEX IF NOT EXISTS idx_contact_cards_account ON contact_cards(account_id)`},
		{"contact_groups", `CREATE INDEX IF NOT EXISTS idx_contact_groups_account ON contact_groups(account_id)`},
		{"contact_conflicts", `CREATE INDEX IF NOT EXISTS idx_contact_conflicts_account ON contact_conflicts(account_id)`},
	}
	for _, idx := range tableIndexes {
		exists, err := tableExistsTx(tx, idx.table)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := tx.Exec(idx.sql); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO schema_version (version) VALUES (51)`); err != nil {
		return err
	}
	return nil
}

func migrateV51ToV52(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS label_aliases (
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			provider_type TEXT NOT NULL,
			provider_id TEXT NOT NULL,
			display_name TEXT NOT NULL,
			color TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (account_id, provider_type, provider_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_label_aliases_display
		 ON label_aliases(account_id, provider_type, display_name COLLATE NOCASE)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (52)`,
	}
	for _, m := range migrations {
		if _, err := tx.Exec(m); err != nil {
			return err
		}
	}
	return nil
}

func migrateV52ToV53(tx *sql.Tx) error {
	if ok, err := tableExistsTx(tx, "contact_observations"); err != nil {
		return err
	} else if ok {
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_contact_observations_profile_active
		 ON contact_observations(user_id, profile_id, is_suppressed, last_seen_at, message_count)`); err != nil {
			return err
		}
	}
	if ok, err := tableExistsTx(tx, "contact_profiles"); err != nil {
		return err
	} else if ok {
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_contact_profiles_user_updated
		 ON contact_profiles(user_id, is_deleted, updated_at DESC, display_name COLLATE NOCASE)`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO schema_version (version) VALUES (53)`); err != nil {
		return err
	}
	return nil
}

func migrateV53ToV54(tx *sql.Tx) error {
	columns := []struct {
		name string
		sql  string
	}{
		{"provider_count_drift_first_seen_at", `ALTER TABLE folders ADD COLUMN provider_count_drift_first_seen_at DATETIME`},
		{"provider_count_drift_last_seen_at", `ALTER TABLE folders ADD COLUMN provider_count_drift_last_seen_at DATETIME`},
		{"provider_count_drift_local_count", `ALTER TABLE folders ADD COLUMN provider_count_drift_local_count INTEGER NOT NULL DEFAULT 0`},
		{"provider_count_drift_remote_count", `ALTER TABLE folders ADD COLUMN provider_count_drift_remote_count INTEGER NOT NULL DEFAULT 0`},
		{"provider_count_drift_cursor", `ALTER TABLE folders ADD COLUMN provider_count_drift_cursor TEXT NOT NULL DEFAULT ''`},
		{"provider_count_drift_confirmations", `ALTER TABLE folders ADD COLUMN provider_count_drift_confirmations INTEGER NOT NULL DEFAULT 0`},
	}
	if ok, err := tableExistsTx(tx, "folders"); err != nil {
		return err
	} else if ok {
		for _, column := range columns {
			exists, err := columnExistsTx(tx, "folders", column.name)
			if err != nil {
				return err
			}
			if exists {
				continue
			}
			if _, err := tx.Exec(column.sql); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO schema_version (version) VALUES (54)`); err != nil {
		return err
	}
	return nil
}

func migrateV54ToV55(tx *sql.Tx) error {
	if ok, err := tableExistsTx(tx, "message_folder_state"); err != nil {
		return err
	} else if ok {
		if _, err := tx.Exec(`UPDATE message_folder_state SET remote_uid = NULL WHERE remote_uid = 0`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO schema_version (version) VALUES (55)`); err != nil {
		return err
	}
	return nil
}

func migrateV55ToV56(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS mail_security_exceptions (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL CHECK (kind IN ('http_discovery', 'plaintext_transport')),
			protocol TEXT NOT NULL DEFAULT '',
			host TEXT NOT NULL CHECK (host <> ''),
			port INTEGER NOT NULL DEFAULT 0,
			created_by TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			CHECK (
				(kind = 'http_discovery' AND protocol = '' AND port = 0)
				OR
				(kind = 'plaintext_transport' AND protocol IN ('imap', 'smtp') AND port BETWEEN 1 AND 65535)
			),
			UNIQUE(kind, protocol, host, port)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_mail_security_exceptions_lookup
		 ON mail_security_exceptions(kind, protocol, host, port)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (56)`,
	}
	for _, migration := range migrations {
		if _, err := tx.Exec(migration); err != nil {
			return err
		}
	}
	return nil
}

func migrateV56ToV57(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS oauth_account_flows (
			state_hash TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			session_token_hash TEXT NOT NULL,
			provider TEXT NOT NULL,
			form_data TEXT NOT NULL,
			expires_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_oauth_account_flows_expires
		 ON oauth_account_flows(expires_at)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (57)`,
	}
	for _, migration := range migrations {
		if _, err := tx.Exec(migration); err != nil {
			return err
		}
	}
	return nil
}

func migrateV57ToV58(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS outgoing_sends (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
			draft_id TEXT NOT NULL DEFAULT '',
			transport TEXT NOT NULL CHECK (transport IN ('smtp', 'gmail', 'outlook')),
			envelope_from TEXT NOT NULL,
			envelope_recipients TEXT NOT NULL DEFAULT '[]',
			mime_data BLOB,
			message_json TEXT NOT NULL DEFAULT '',
			send_after DATETIME NOT NULL,
			is_scheduled INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'pending',
			attempt_count INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			locked_at DATETIME,
			sent_message_id TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(message_id)
		)`); err != nil {
		return err
	}
	hasScheduledSends, err := tableExistsTx(tx, "scheduled_sends")
	if err != nil {
		return err
	}
	hasMessages, err := tableExistsTx(tx, "messages")
	if err != nil {
		return err
	}
	hasProvider, err := columnExistsTx(tx, "accounts", "provider")
	if err != nil {
		return err
	}
	hasEmailAddress, err := columnExistsTx(tx, "accounts", "email_address")
	if err != nil {
		return err
	}
	if hasScheduledSends && hasMessages && hasProvider && hasEmailAddress {
		if _, err := tx.Exec(`INSERT INTO outgoing_sends (
			id, account_id, message_id, draft_id, transport, envelope_from,
			envelope_recipients, mime_data, message_json, send_after, is_scheduled,
			status, attempt_count, last_error, locked_at, sent_message_id, created_at, updated_at
		)
		SELECT ss.id, ss.account_id, ss.message_id, COALESCE(m.internet_message_id, ''),
			CASE lower(COALESCE(a.provider, ''))
				WHEN 'gmail' THEN 'gmail'
				WHEN 'outlook' THEN 'outlook'
				ELSE 'smtp'
			END,
			COALESCE(a.email_address, ''), '[]', NULL, '', ss.scheduled_for, 1,
			ss.status, ss.attempt_count, ss.last_error, ss.locked_at, ss.sent_message_id,
			ss.created_at, ss.updated_at
		FROM scheduled_sends ss
		JOIN accounts a ON a.id = ss.account_id
		LEFT JOIN messages m ON m.id = ss.message_id`); err != nil {
			return err
		}
	}
	if hasScheduledSends {
		if _, err := tx.Exec(`DROP TABLE scheduled_sends`); err != nil {
			return err
		}
	}
	for _, migration := range []string{
		`CREATE INDEX IF NOT EXISTS idx_outgoing_sends_due ON outgoing_sends(status, send_after)`,
		`CREATE INDEX IF NOT EXISTS idx_outgoing_sends_account ON outgoing_sends(account_id, status, send_after)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (58)`,
	} {
		if _, err := tx.Exec(migration); err != nil {
			return err
		}
	}
	return nil
}

func migrateV58ToV59(tx *sql.Tx) error {
	columns := []struct {
		name string
		sql  string
	}{
		{"sent_copy_status", `ALTER TABLE outgoing_sends ADD COLUMN sent_copy_status TEXT NOT NULL DEFAULT 'not_required' CHECK (sent_copy_status IN ('not_required', 'pending', 'copying', 'complete', 'failed', 'ambiguous'))`},
		{"sent_copy_attempt_count", `ALTER TABLE outgoing_sends ADD COLUMN sent_copy_attempt_count INTEGER NOT NULL DEFAULT 0`},
		{"sent_copy_last_error", `ALTER TABLE outgoing_sends ADD COLUMN sent_copy_last_error TEXT NOT NULL DEFAULT ''`},
		{"sent_copy_locked_at", `ALTER TABLE outgoing_sends ADD COLUMN sent_copy_locked_at DATETIME`},
		{"sent_copy_next_attempt_at", `ALTER TABLE outgoing_sends ADD COLUMN sent_copy_next_attempt_at DATETIME`},
		{"sent_copy_uid", `ALTER TABLE outgoing_sends ADD COLUMN sent_copy_uid INTEGER NOT NULL DEFAULT 0`},
		{"sent_copy_uid_validity", `ALTER TABLE outgoing_sends ADD COLUMN sent_copy_uid_validity INTEGER NOT NULL DEFAULT 0`},
	}
	for _, column := range columns {
		exists, err := columnExistsTx(tx, "outgoing_sends", column.name)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := tx.Exec(column.sql); err != nil {
				return err
			}
		}
	}
	for _, migration := range []string{
		`CREATE INDEX IF NOT EXISTS idx_outgoing_sends_sent_copy ON outgoing_sends(status, sent_copy_status, sent_copy_next_attempt_at)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (59)`,
	} {
		if _, err := tx.Exec(migration); err != nil {
			return err
		}
	}
	return nil
}

func migrateV59ToV60(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS imap_draft_states (
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			draft_key TEXT NOT NULL,
			local_message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
			folder_id TEXT NOT NULL DEFAULT '',
			folder_remote_name TEXT NOT NULL,
			remote_uid INTEGER NOT NULL DEFAULT 0,
			uid_validity INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (account_id, draft_key)
		)`,
		`CREATE TABLE IF NOT EXISTS imap_draft_operations (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			draft_key TEXT NOT NULL,
			kind TEXT NOT NULL CHECK (kind IN ('upsert', 'delete')),
			revision_token TEXT NOT NULL DEFAULT '',
			mime_data BLOB,
			message_date DATETIME,
			status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'syncing', 'failed', 'ambiguous')),
			attempt_count INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			locked_at DATETIME,
			next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (account_id, draft_key) REFERENCES imap_draft_states(account_id, draft_key) ON DELETE CASCADE
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_imap_draft_operations_coalesced
		 ON imap_draft_operations(account_id, draft_key)
		 WHERE status IN ('pending', 'failed')`,
		`CREATE INDEX IF NOT EXISTS idx_imap_draft_operations_due
		 ON imap_draft_operations(status, next_attempt_at, created_at)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (60)`,
	}
	for _, migration := range migrations {
		if _, err := tx.Exec(migration); err != nil {
			return err
		}
	}
	return nil
}

func migrateV60ToV61(tx *sql.Tx) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS message_mutations (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			folder_id TEXT NOT NULL DEFAULT '',
			provider_type TEXT NOT NULL CHECK (provider_type IN ('gmail', 'outlook', 'imap')),
			kind TEXT NOT NULL CHECK (kind IN ('read', 'starred')),
			target_value INTEGER NOT NULL CHECK (target_value IN (0, 1)),
			status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'failed', 'applied')),
			attempt_count INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			locked_at DATETIME,
			next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(message_id, kind, folder_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_message_mutations_due
		 ON message_mutations(status, next_attempt_at, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_message_mutations_account
		 ON message_mutations(account_id, status, next_attempt_at)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (61)`,
	}
	for _, migration := range migrations {
		if _, err := tx.Exec(migration); err != nil {
			return err
		}
	}
	return nil
}

func migrateV61ToV62(tx *sql.Tx) error {
	messagesExist, err := tableExistsTx(tx, "messages")
	if err != nil {
		return err
	}
	if !messagesExist {
		if _, err := tx.Exec(`ALTER TABLE message_mutations ADD COLUMN destination_folder_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT OR REPLACE INTO schema_version (version) VALUES (62)`)
		return err
	}
	migrations := []string{
		`CREATE TABLE message_mutations_v62 (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			folder_id TEXT NOT NULL DEFAULT '',
			provider_type TEXT NOT NULL CHECK (provider_type IN ('gmail', 'outlook', 'imap')),
			kind TEXT NOT NULL CHECK (kind IN ('read', 'starred', 'move')),
			target_value INTEGER NOT NULL CHECK (target_value IN (0, 1)),
			destination_folder_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'failed', 'applied')),
			attempt_count INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			locked_at DATETIME,
			next_attempt_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(message_id, kind, folder_id)
		)`,
		`INSERT INTO message_mutations_v62 (
			id, account_id, message_id, folder_id, provider_type, kind, target_value,
			status, attempt_count, last_error, locked_at, next_attempt_at, created_at, updated_at
		) SELECT
			id, account_id, message_id, folder_id, provider_type, kind, target_value,
			status, attempt_count, last_error, locked_at, next_attempt_at, created_at, updated_at
		  FROM message_mutations`,
		`DROP TABLE message_mutations`,
		`ALTER TABLE message_mutations_v62 RENAME TO message_mutations`,
		`CREATE INDEX idx_message_mutations_due
		 ON message_mutations(status, next_attempt_at, created_at)`,
		`CREATE INDEX idx_message_mutations_account
		 ON message_mutations(account_id, status, next_attempt_at)`,
		`INSERT OR REPLACE INTO schema_version (version) VALUES (62)`,
	}
	for _, migration := range migrations {
		if _, err := tx.Exec(migration); err != nil {
			return err
		}
	}
	return nil
}

func tableExistsTx(tx *sql.Tx, table string) (bool, error) {
	var name string
	err := tx.QueryRow(`SELECT name FROM sqlite_master WHERE type IN ('table', 'view') AND name = ?`, table).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func columnExistsTx(tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
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
