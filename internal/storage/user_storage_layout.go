package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// UserStorageLayout certifies completed publication. Initial copy digests remain
// historical evidence in the migration journal; live databases are allowed to
// change. Startup must retain SourcePath's runtime lock alongside CentralPath's.
// SchemaVersion records the schema at publication; startup upgrades later.
type UserStorageLayout struct {
	Version             int    `json:"version"`
	KeyFingerprint      string `json:"key_fingerprint,omitempty"`
	ID                  string `json:"layout_id"`
	SourcePath          string `json:"source_path"`
	CentralPath         string `json:"central_path"`
	UserDirectory       string `json:"user_directory"`
	BlobDirectory       string `json:"blob_directory"`
	WorkingDirectory    string `json:"working_directory"`
	SourceSchemaVersion int    `json:"source_schema_version"`
	SchemaVersion       int    `json:"schema_version"`
	OwnersAtMigration   int64  `json:"owners_at_migration"`
}

const migrationLayoutSchema = `CREATE TABLE gofer_storage_layout (
 singleton INTEGER PRIMARY KEY CHECK(singleton=1),
 layout_version INTEGER NOT NULL CHECK(layout_version=1),
 layout_id TEXT NOT NULL CHECK(length(layout_id)=32),
 manifest_digest TEXT NOT NULL CHECK(length(manifest_digest)=64)
 );`

// LoadUserStorageLayout never creates databases or upgrades schemas. The caller
// holds central/source and user-manager locks through activation. Missing files,
// incomplete manifests and mismatched identities are errors, never fresh stores.
func LoadUserStorageLayout(ctx context.Context, path string) (layout UserStorageLayout, err error) {
	if ctx == nil {
		return layout, errors.New("layout verification requires a context")
	}
	layout, err = ReadUserStorageLayoutMetadata(path)
	if err != nil {
		return layout, err
	}
	central := layout.CentralPath
	if err := migrationRequireRegular(central); err != nil {
		return layout, err
	}
	if err := migrationRequireDirectory(layout.UserDirectory); err != nil {
		return layout, err
	}
	db, err := OpenReadOnly(central)
	if err != nil {
		return layout, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return layout, err
	}
	defer tx.Rollback()
	if err := migrationCheckLayoutIdentity(ctx, tx, layout); err != nil {
		return layout, err
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT max(version) FROM schema_version`).Scan(&version); err != nil {
		return layout, err
	}
	if version != CurrentSchemaVersion {
		return layout, fmt.Errorf("completed layout database has schema version %d, expected %d; starting the Gofer server upgrades it", version, CurrentSchemaVersion)
	}
	// Follow current durable store history, not the initial source user count:
	// users may have been added or removed since publication.
	cursor := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT user_id FROM gofer_user_store_directory WHERE state='present' AND user_id>? ORDER BY user_id LIMIT 64`, cursor)
		if err != nil {
			return layout, err
		}
		var owners []string
		for rows.Next() {
			var owner string
			if err := rows.Scan(&owner); err != nil {
				rows.Close()
				return layout, err
			}
			owners = append(owners, owner)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return layout, err
		}
		if len(owners) == 0 {
			break
		}
		for _, owner := range owners {
			hash := sha256.Sum256([]byte(owner))
			filename := filepath.Join(layout.UserDirectory, hex.EncodeToString(hash[:])+".db")
			if err := migrationRequireRegular(filename); err != nil {
				return layout, err
			}
			if err := checkUserStoreIdentity(filename, owner); err != nil {
				return layout, err
			}
		}
		cursor = owners[len(owners)-1]
	}
	return layout, nil
}

func migrationValidateLayoutPaths(layout UserStorageLayout, central string) error {
	if layout.KeyFingerprint != "" && !migrationValidKeyFingerprint(layout.KeyFingerprint) {
		return errors.New("completed layout has an invalid application key fingerprint")
	}
	if layout.Version != 1 || !validMigrationAttemptID(layout.ID) || layout.CentralPath != central || layout.SourcePath == central || !filepath.IsAbs(layout.SourcePath) || filepath.Clean(layout.SourcePath) != layout.SourcePath || filepath.Dir(layout.SourcePath) != filepath.Dir(central) || layout.UserDirectory != central+".users" || layout.BlobDirectory != filepath.Join(filepath.Dir(layout.SourcePath), "accounts") || !filepath.IsAbs(layout.WorkingDirectory) || filepath.Clean(layout.WorkingDirectory) != layout.WorkingDirectory || layout.SourceSchemaVersion < 105 || layout.SourceSchemaVersion > CurrentSchemaVersion || layout.SchemaVersion < 1 || layout.SchemaVersion > CurrentSchemaVersion || layout.OwnersAtMigration < 0 {
		return errors.New("completed storage layout has an invalid identity or path binding")
	}
	return nil
}

func migrationCheckLayoutIdentity(ctx context.Context, tx *sql.Tx, layout UserStorageLayout) error {
	digest, err := migrationLayoutDigest(layout)
	if err != nil {
		return err
	}
	var matches, count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(singleton=1 AND layout_version=1 AND layout_id=? AND manifest_digest=?),0) FROM main.gofer_storage_layout`, layout.ID, digest).Scan(&count, &matches); err != nil {
		return err
	}
	if count != 1 || matches != 1 {
		return errors.New("central database does not match the completed layout identity")
	}
	var definition string
	if err := tx.QueryRowContext(ctx, `SELECT sql FROM main.sqlite_schema WHERE name='gofer_storage_layout' AND type='table'`).Scan(&definition); err != nil {
		return err
	}
	normalize := func(value string) string {
		return strings.Join(strings.Fields(strings.TrimSuffix(strings.TrimSpace(value), ";")), " ")
	}
	if normalize(definition) != normalize(migrationLayoutSchema) {
		return errors.New("central layout identity schema changed")
	}
	return nil
}

func migrationRequireRegular(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("migration requires an existing regular file without symlinks")
	}
	return nil
}

func migrationRequireDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("migration requires an existing real directory")
	}
	return nil
}

func migrationReadJSON(path string, destination any) error {
	if err := migrationRequireRegular(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Size() > 64*1024 {
		return errors.New("migration metadata exceeds its size limit")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64*1024+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("migration metadata is invalid")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("migration metadata has trailing data")
	}
	return nil
}

// Flush a complete private JSON file, then publish it without replacing an
// existing destination. A failed unlink leaves only an unused private temporary.
func migrationPublishJSON(path string, value any) (err error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".migration-layout-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	encodeErr := json.NewEncoder(file).Encode(value)
	if err := errors.Join(encodeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Link(temporary, path); err != nil {
		return fmt.Errorf("publish completed layout without replacement: %w", err)
	}
	return migrationSyncDirectory(filepath.Dir(path))
}

func migrationLayoutDigest(layout UserStorageLayout) (string, error) {
	data, err := json.Marshal(layout)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func migrationValidKeyFingerprint(value string) bool {
	bytes, err := hex.DecodeString(value)
	return err == nil && len(bytes) == 32 && value == strings.ToLower(value)
}

// VerifySharedStorageRuntime prevents the legacy server from opening an owned
// layout. It allows older shared schemas so their normal upgrade still works.
// The caller holds the database runtime lock; this check performs no writes.
func VerifySharedStorageRuntime(ctx context.Context, path string) (err error) {
	for _, metadata := range []string{path + ".layout.json", path + ".migration.json"} {
		if _, err := os.Lstat(metadata); err == nil {
			return errors.New("per-user storage requires managed mode and a completed layout")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		// An interrupted automatic migration moved the original aside. Never
		// start over with an empty database in its place.
		if _, err := os.Lstat(path + ".shared"); err == nil {
			return fmt.Errorf("the original database was moved to %q by an interrupted per-user migration; start Gofer in managed mode to finish it, or rename it back", path+".shared")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	} else if err != nil {
		return err
	}
	db, err := openReadOnlyDB(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	var owned bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name IN ('gofer_storage_layout','gofer_user_store','gofer_account_directory'))`).Scan(&owned); err != nil {
		return err
	}
	if owned {
		return errors.New("per-user storage requires managed mode and a completed layout")
	}
	return nil
}

// ReadUserStorageLayoutMetadata resolves only metadata needed to acquire locks.
// It does not certify the SQLite identity or authorize runtime activation.
func ReadUserStorageLayoutMetadata(path string) (layout UserStorageLayout, err error) {
	central, err := canonicalMigrationPath(path)
	if err != nil {
		return layout, err
	}
	if err := migrationReadJSON(central+".layout.json", &layout); err != nil {
		return layout, err
	}
	return layout, migrationValidateLayoutPaths(layout, central)
}

// VerifyUserStorageRuntimeBoundary rejects private rows in a central layout,
// including accidental shared-layout writes made after publication.
func VerifyUserStorageRuntimeBoundary(ctx context.Context, path string) (err error) {
	db, err := OpenReadOnly(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.Read().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var invalid bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='gofer_user_store') OR EXISTS(SELECT 1 FROM app_settings p JOIN users u ON u.id=p.user_id WHERE u.user_type<>'management')`).Scan(&invalid); err != nil {
		return err
	}
	if invalid {
		return errors.New("central layout contains local identity or preferences")
	}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type='table'`)
	if err != nil {
		return err
	}
	present := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		present[name] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if err := requireSharedMigrationTables(ctx, present, CurrentSchemaVersion); err != nil {
		return err
	}
	for name, rule := range userStorageMigrationTables {
		if rule.destination != migrationLocal && rule.destination != migrationOAuth && rule.destination != migrationSearch {
			continue
		}
		if !present[name] {
			continue
		} // optional owned extensions need not exist centrally
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM main.`+quoteStoreIdentifier(name)+`)`).Scan(&invalid); err != nil {
			return err
		}
		if invalid {
			return errors.New("central layout contains private mailbox data")
		}
	}
	return nil
}
