package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
)

// RetireSharedDatabase moves an original shared database aside so its path can
// become the central database of an automatic in-place migration. The file is
// upgraded to a supported shared schema if needed, checkpointed into a single
// self-contained file and renamed atomically. Its contents are not otherwise
// changed. Anything that is not a recognizable shared database is rejected.
func RetireSharedDatabase(ctx context.Context, path, retired string) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if path == retired || filepath.Dir(path) != filepath.Dir(retired) {
		return errors.New("retired shared database must be a sibling path")
	}
	lock, err := runtimeguard.Acquire(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if err := requireExistingDatabase(path); err != nil {
		return err
	}
	for _, candidate := range []string{retired, retired + "-wal", retired + "-shm", retired + "-journal"} {
		if _, err := os.Lstat(candidate); err == nil {
			return fmt.Errorf("cannot retire shared database: %q already exists", candidate)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	version, err := sharedDatabaseVersion(ctx, path)
	if err != nil {
		return err
	}
	if !migrationSharedVersionSupported(version) {
		if version > CurrentSchemaVersion {
			return migrationSourceError("schema_version", "source version is newer than this Gofer release")
		}
		// Older shared schemas take the ordinary application upgrade first.
		db, err := New(path)
		if err != nil {
			return fmt.Errorf("upgrade shared database before migration: %w", err)
		}
		if err := db.Close(); err != nil {
			return err
		}
		if version, err = sharedDatabaseVersion(ctx, path); err != nil {
			return err
		}
		if !migrationSharedVersionSupported(version) {
			return migrationSourceError("schema_version", "source version is not a recognized shared schema")
		}
	}
	if err := checkpointSharedDatabase(ctx, path); err != nil {
		return err
	}
	if err := migrationSyncFile(path); err != nil {
		return err
	}
	if err := os.Rename(path, retired); err != nil {
		return err
	}
	return migrationSyncDirectory(filepath.Dir(path))
}

func sharedDatabaseVersion(ctx context.Context, path string) (version int, err error) {
	db, err := openReadOnlyDB(path)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	var owned bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name IN ('gofer_storage_layout','gofer_user_store','gofer_account_directory'))`).Scan(&owned); err != nil {
		return 0, err
	}
	if owned {
		return 0, migrationSourceError("sqlite_schema", "database already belongs to a per-user layout")
	}
	if err := db.QueryRowContext(ctx, `SELECT coalesce(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

// Fold the WAL into the main file and leave it in rollback-journal mode, so the
// retired database is one file that read-only migration connections can open.
func checkpointSharedDatabase(ctx context.Context, path string) (err error) {
	db, err := openDB(path)
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	defer func() { err = errors.Join(err, db.Close()) }()
	var busy, frames, checkpointed int
	if err := db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &frames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return errors.New("shared database is still in use; stop other Gofer processes and try again")
	}
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode=DELETE`).Scan(&mode); err != nil {
		return err
	}
	if mode != "delete" {
		return errors.New("could not checkpoint the shared database")
	}
	if err := db.Close(); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		info, err := os.Lstat(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if suffix == "-wal" && info.Size() != 0 {
			return errors.New("shared database write-ahead log was not checkpointed")
		}
		if err := os.Remove(path + suffix); err != nil {
			return err
		}
	}
	return nil
}

// RestoreSharedDatabase undoes RetireSharedDatabase after an automatic
// migration stopped before publication: the unpublished private copy and its
// journal are discarded and the original returns to path unchanged. Once
// publication has started, files may already be in place and the migration
// can only be resumed, so restoration is refused.
func RestoreSharedDatabase(path, retired string) (err error) {
	if path == retired || filepath.Dir(path) != filepath.Dir(retired) {
		return errors.New("retired shared database must be a sibling path")
	}
	retiredLock, err := runtimeguard.Acquire(retired)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, retiredLock.Close()) }()
	lock, err := runtimeguard.Acquire(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if err := migrationRequireRegular(retired); err != nil {
		return err
	}
	for _, candidate := range []string{retired + "-wal", retired + "-journal", path, path + "-wal", path + "-shm", path + "-journal", path + ".users", path + ".layout.json"} {
		if _, err := os.Lstat(candidate); err == nil {
			return fmt.Errorf("cannot restore shared database: %q exists", candidate)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	journalPath := path + ".migration.json"
	journal, err := readMigrationPreparationJournal(journalPath)
	if err == nil {
		if journal.SourcePath != retired || journal.DestinationPath != path {
			return errors.New("cannot restore shared database: migration journal belongs to other files")
		}
		if journal.State != "preparing" && journal.State != "verified" {
			return errors.New("cannot restore shared database: publication has started")
		}
		if err := migrationRemoveStage(journal); err != nil {
			return err
		}
		if err := os.Remove(journalPath); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(retired, path); err != nil {
		return err
	}
	return migrationSyncDirectory(filepath.Dir(path))
}
