package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
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
	s.layout, err = storage.LoadUserStorageLayout(ctx, path)
	if err != nil {
		return nil, err
	}
	if s.layout != metadata {
		return nil, errors.New("storage layout changed while acquiring runtime locks")
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	workingDirectory, err = filepath.EvalSymlinks(workingDirectory)
	if err != nil {
		return nil, err
	}
	retainedDirectory, err := filepath.EvalSymlinks(s.layout.WorkingDirectory)
	if err != nil {
		return nil, err
	}
	if workingDirectory != retainedDirectory {
		return nil, fmt.Errorf("start Gofer from the retained working directory %q", s.layout.WorkingDirectory)
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
// Existing or interrupted data is never adopted as an empty installation.
func initializeManagedStorage(ctx context.Context, path string) (err error) {
	lock, err := runtimeguard.Acquire(path + ".initialization")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if _, err := os.Lstat(path + ".layout.json"); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	source := path + ".shared"
	for _, candidate := range []string{path, path + ".migration.json", path + ".staging", path + ".users", source} {
		if _, err := os.Lstat(candidate); err == nil {
			return fmt.Errorf("managed storage requires a completed layout; retained data exists at %q: use gofer storage migrate --db ORIGINAL --to NEW, with --retry for interrupted work, then set GOFER_DB_PATH to NEW", candidate)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
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
	_, err = storage.MigrateUserStorage(ctx, migrationOptions(source, path, key))
	return err
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
