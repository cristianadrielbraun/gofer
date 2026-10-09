package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type managedStorage struct {
	layout                  storage.UserStorageLayout
	central                 *storage.DB
	stores                  *storage.UserStores
	routing                 *storage.AccountRouting
	key                     []byte
	sourceLock, centralLock *runtimeguard.Lock
}

func migrationOptions(source, destination string, key []byte) storage.UserStorageMigrationOptions {
	fingerprint := sha256.Sum256(key)
	return storage.UserStorageMigrationOptions{
		SourcePath: source, DestinationPath: destination, KeyFingerprint: hex.EncodeToString(fingerprint[:]),
		ValidateSource: func(ctx context.Context, db *storage.DB) error { return validateMigrationSourceKey(ctx, db, key) },
		ImportCredentials: func(ctx context.Context, tx *sql.Tx) error {
			return mailauth.ImportUserStorageMigrationCredentials(ctx, tx, key)
		},
		VerifyCredentials: func(ctx context.Context, tx *sql.Tx) error {
			return mailauth.VerifyUserStorageMigrationCredentials(ctx, tx, key)
		},
	}
}

func openManagedStorage(ctx context.Context, path string, maxOpen int) (result *managedStorage, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(parent, filepath.Base(path))
	if _, err := os.Lstat(path + ".layout.json"); errors.Is(err, os.ErrNotExist) {
		if err := initializeManagedStorage(ctx, path); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	metadata, err := storage.ReadUserStorageLayoutMetadata(path)
	if err != nil {
		return nil, err
	}
	s := &managedStorage{}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.Close())
		}
	}()
	s.sourceLock, err = runtimeguard.Acquire(metadata.SourcePath)
	if err != nil {
		return nil, err
	}
	s.centralLock, err = runtimeguard.Acquire(path)
	if err != nil {
		return nil, err
	}
	// Upgrades schemas and makes retained file paths absolute, so later
	// starts do not depend on the original working directory.
	if err := storage.UpgradeUserStorageLayout(ctx, path); err != nil {
		return nil, fmt.Errorf("upgrade per-user storage: %w", err)
	}
	s.layout, err = storage.LoadUserStorageLayout(ctx, path)
	if err != nil {
		return nil, err
	}
	if s.layout != metadata {
		return nil, errors.New("storage layout changed while acquiring runtime locks")
	}
	s.key, err = loadExistingMigrationKey(s.layout.SourcePath)
	if err != nil {
		return nil, err
	}
	fingerprint := sha256.Sum256(s.key)
	if s.layout.KeyFingerprint == "" || s.layout.KeyFingerprint != hex.EncodeToString(fingerprint[:]) {
		return nil, errors.New("application key does not match the completed storage layout")
	}
	if err := storage.VerifyUserStorageRuntimeBoundary(ctx, path); err != nil {
		return nil, err
	}
	s.central, err = storage.OpenExisting(path)
	if err != nil {
		return nil, err
	}
	s.stores, err = storage.NewUserStores(s.central, storage.UserStoreOptions{Directory: s.layout.UserDirectory, MaxOpen: maxOpen})
	if err != nil {
		return nil, err
	}
	s.routing, err = storage.NewAccountRouting(s.stores)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// A fresh installation uses the same verified publication path as an upgrade.
// An original shared database at path is retired to path.shared and migrated
// in place; interrupted automatic migrations resume on the next start.
func initializeManagedStorage(ctx context.Context, path string) (err error) {
	lock, err := runtimeguard.Acquire(path + ".initialization")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	exists := func(candidate string) (bool, error) {
		if _, err := os.Lstat(candidate); err == nil {
			return true, nil
		} else if errors.Is(err, os.ErrNotExist) {
			return false, nil
		} else {
			return false, err
		}
	}
	if published, err := exists(path + ".layout.json"); err != nil || published {
		return err
	}
	source := path + ".shared"
	journal, err := exists(path + ".migration.json")
	if err != nil {
		return err
	}
	if journal {
		log.Printf("storage: resuming interrupted per-user storage migration of %s", path)
		return migrateManagedStorage(ctx, source, path, true, true)
	}
	for _, candidate := range []string{path + ".staging", path + ".users"} {
		if found, err := exists(candidate); err != nil {
			return err
		} else if found {
			return fmt.Errorf("managed storage cannot start: unrecognized retained data at %q", candidate)
		}
	}
	original, err := exists(path)
	if err != nil {
		return err
	}
	retired, err := exists(source)
	if err != nil {
		return err
	}
	switch {
	case original && retired:
		return fmt.Errorf("managed storage cannot start: both %q and %q exist", path, source)
	case original:
		// Never move the original aside without the application key it needs,
		// or convert an installation managed mode would then refuse to open.
		if _, err := loadExistingMigrationKey(source); err != nil {
			return err
		}
		if err := requireManagedSharedDatabase(ctx, path); err != nil {
			return err
		}
		log.Printf("storage: migrating shared database %s to per-user storage; the original is kept at %s", path, source)
		if err := storage.RetireSharedDatabase(ctx, path, source); err != nil {
			return fmt.Errorf("prepare shared database for migration: %w", err)
		}
		return migrateManagedStorage(ctx, source, path, false, true)
	case retired:
		// Interrupted after retiring the original, or during a fresh install.
		// Nothing proves this file was moved here by Gofer, so it stays put.
		if err := requireManagedSharedDatabase(ctx, source); err != nil {
			return err
		}
		return migrateManagedStorage(ctx, source, path, false, false)
	}
	key, err := initializeManagedKey(source)
	if err != nil {
		return err
	}
	sourceLock, err := runtimeguard.Acquire(source)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, sourceLock.Close()) }()
	file, err := os.OpenFile(source, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	db, err := storage.New(source)
	if err != nil {
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	if err := sourceLock.Close(); err != nil {
		return err
	}
	if _, err := storage.MigrateUserStorage(ctx, migrationOptions(source, path, key)); err != nil {
		return err
	}
	// The empty database only seeded the layout; nothing needs it to roll back.
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(source + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func migrateManagedStorage(ctx context.Context, source, path string, retry, restore bool) (err error) {
	defer func() {
		if err == nil {
			return
		}
		err = fmt.Errorf("per-user storage migration: %w", err)
		if !restore {
			return
		}
		// Unless publication started, leave the original where it was found.
		if restoreErr := storage.RestoreSharedDatabase(path, source); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("the original database remains at %s: %w", source, restoreErr))
			return
		}
		log.Printf("storage: per-user storage migration failed; the original database was restored to %s", path)
	}()
	key, err := loadExistingMigrationKey(source)
	if err != nil {
		return err
	}
	options := migrationOptions(source, path, key)
	options.Retry = retry
	started := time.Now()
	if _, err := storage.MigrateUserStorage(ctx, options); err != nil {
		return err
	}
	log.Printf("storage: per-user storage migration completed in %s", time.Since(started).Round(time.Second))
	if info, err := os.Stat(source); err == nil {
		log.Printf("storage: the original database is kept at %s (%d MiB) to roll back the conversion; delete it once you no longer need to", source, info.Size()>>20)
	}
	return nil
}

// Open and personal installations convert with their local profile becoming a
// regular user. Anything else that managed mode would refuse to open stays
// shared; the original is checked read-only and left in place.
func requireManagedSharedDatabase(ctx context.Context, path string) error {
	convertible, err := storage.SharedDatabaseConvertible(ctx, path)
	if err != nil {
		return err
	}
	if !convertible {
		return fmt.Errorf("%s is owned by a user who is not an administrator and cannot be converted for managed mode; start Gofer in its previous mode (nothing was changed)", path)
	}
	return nil
}

func initializeManagedKey(source string) ([]byte, error) {
	if os.Getenv("GOFER_SECRET_KEY") != "" {
		return loadExistingMigrationKey(source)
	}
	path := filepath.Join(filepath.Dir(source), "secret.key")
	if _, err := os.Lstat(path); err == nil {
		return loadExistingMigrationKey(source)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, os.ErrExist) {
		return loadExistingMigrationKey(source)
	}
	if err != nil {
		return nil, err
	}
	_, writeErr := file.Write(key)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return nil, err
	}
	return key, nil
}

// All request and service workers must be joined before calling Close.
func (s *managedStorage) Close() error {
	if s == nil {
		return nil
	}
	var result error
	if s.stores != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		result = s.stores.Close(ctx)
		cancel()
		if result != nil {
			return result
		} // retain central and runtime locks while leases remain
		s.stores = nil
	}
	if s.central != nil {
		result = errors.Join(result, s.central.Close())
		s.central = nil
	}
	result = errors.Join(result, s.centralLock.Close(), s.sourceLock.Close())
	return result
}
