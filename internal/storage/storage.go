package storage

import (
	"database/sql"
	"embed"
	"fmt"
	"log"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

type DB struct {
	write *sql.DB
	read  *sql.DB
	path  string
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

	return db, nil
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=temp_store(MEMORY)")
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

	if currentVersion >= 2 {
		log.Printf("schema at version %d, no migration needed", currentVersion)
		return nil
	}

	if currentVersion == 0 {
		if _, err := tx.Exec(string(schema)); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if _, err := tx.Exec("INSERT OR REPLACE INTO schema_version (version) VALUES (2)"); err != nil {
			return fmt.Errorf("update schema version: %w", err)
		}
		log.Println("schema migrated to version 2")
	}

	if currentVersion == 1 {
		if err := migrateV1ToV2(tx); err != nil {
			return fmt.Errorf("migrate v1 to v2: %w", err)
		}
		log.Println("schema migrated to version 2")
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
