package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestManagedStorageFreshInitializationRestartAndExclusiveLocks(t *testing.T) {
	t.Setenv("GOFER_SECRET_KEY", "")
	path := filepath.Join(t.TempDir(), "new-data", "central.db")
	s, err := openManagedStorage(t.Context(), path, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	original := s.layout
	if original.OwnersAtMigration != 0 || original.CentralPath != path || original.SourcePath != path+".shared" || original.KeyFingerprint == "" {
		t.Fatal("fresh layout", original)
	}
	key, err := os.ReadFile(filepath.Join(filepath.Dir(path), "secret.key"))
	if err != nil || len(key) != 32 {
		t.Fatal("generated key", err)
	}
	for _, target := range []string{path, original.SourcePath, filepath.Join(original.UserDirectory, "manager")} {
		lock, err := runtimeguard.Acquire(target)
		if err == nil {
			lock.Close()
			t.Fatal("missing exclusive runtime lock", target)
		}
		if !errors.Is(err, runtimeguard.ErrAlreadyLocked) {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openManagedStorage(t.Context(), path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if s.layout != original || !bytes.Equal(s.key, key) {
		t.Fatal("restart changed layout/key")
	}
}

func TestManagedStorageRejectsSharedAndIncompleteDataWithoutAdoption(t *testing.T) {
	for _, suffix := range []string{"", ".migration.json", ".staging", ".users", ".shared"} {
		t.Run("existing"+suffix, func(t *testing.T) {
			t.Setenv("GOFER_SECRET_KEY", "")
			path := filepath.Join(t.TempDir(), "central.db")
			retained := []byte("retained data")
			if err := os.WriteFile(path+suffix, retained, 0600); err != nil {
				t.Fatal(err)
			}
			if s, err := openManagedStorage(t.Context(), path, 1); err == nil {
				s.Close()
				t.Fatal("adopted existing data")
			}
			after, err := os.ReadFile(path + suffix)
			if err != nil || !bytes.Equal(after, retained) {
				t.Fatal("changed retained file", err)
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(path), "secret.key")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("generated key for existing data", err)
			}
			if _, err := os.Lstat(path + ".layout.json"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("published invalid layout", err)
			}
		})
	}
}

func TestManagedStorageRejectsWrongKeyEvenForEmptyArchive(t *testing.T) {
	t.Setenv("GOFER_SECRET_KEY", "")
	path := filepath.Join(t.TempDir(), "central.db")
	s, err := openManagedStorage(t.Context(), path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFER_SECRET_KEY", strings.Repeat("ab", 32))
	if s, err := openManagedStorage(t.Context(), path, 1); err == nil {
		s.Close()
		t.Fatal("wrong key accepted for empty archive")
	}
	after, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("wrong-key activation changed central", err)
	}
}

func TestManagedStorageInvalidExistingKeyNeverReplaced(t *testing.T) {
	t.Setenv("GOFER_SECRET_KEY", "")
	path := filepath.Join(t.TempDir(), "central.db")
	keyPath := filepath.Join(filepath.Dir(path), "secret.key")
	if err := os.WriteFile(keyPath, []byte("invalid retained key"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openManagedStorage(t.Context(), path, 1); err == nil {
		t.Fatal("invalid key accepted")
	}
	after, err := os.ReadFile(keyPath)
	if err != nil || string(after) != "invalid retained key" {
		t.Fatal("key replaced", err)
	}
	if _, err := os.Lstat(path + ".shared"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created source despite invalid key", err)
	}
}

func TestManagedStorageMigratedOwnersRemainIsolatedAcrossCacheEviction(t *testing.T) {
	options, _, key := migrationCommandFixture(t)
	if _, err := storage.MigrateUserStorage(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFER_SECRET_KEY", hex.EncodeToString(key))
	s, err := openManagedStorage(t.Context(), options.DestinationPath, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, owner := range []string{"alice", "bob", "alice", "bob"} {
		lease, err := s.stores.AcquireExisting(t.Context(), owner)
		if err != nil {
			t.Fatal(err)
		}
		var contacts, accounts int
		err = lease.DB().Read().QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM contact_profiles),(SELECT count(*) FROM accounts)`).Scan(&contacts, &accounts)
		lease.Release()
		if err != nil {
			t.Fatal(err)
		}
		if owner == "bob" && (contacts != 1 || accounts != 0) || owner == "alice" && (contacts != 0 || accounts != 2) {
			t.Fatal("owner data mixed", owner, contacts, accounts)
		}
	}
	if err := storage.VerifyUserStorageRuntimeBoundary(context.Background(), options.DestinationPath); err != nil {
		t.Fatal(err)
	}
}

func TestPublishedManagedLayoutCannotOpenAsSharedRuntime(t *testing.T) {
	t.Setenv("GOFER_SECRET_KEY", "")
	path := filepath.Join(t.TempDir(), "central.db")
	s, err := openManagedStorage(t.Context(), path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := storage.VerifySharedStorageRuntime(t.Context(), path); err == nil {
		t.Fatal("shared runtime accepted managed manifest")
	}
	if err := os.Remove(path + ".layout.json"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".migration.json"); err != nil {
		t.Fatal(err)
	}
	if err := storage.VerifySharedStorageRuntime(t.Context(), path); err == nil {
		t.Fatal("shared runtime accepted bare managed central")
	}
	if _, err := openManagedStorage(t.Context(), path, 1); err == nil {
		t.Fatal("managed runtime adopted bare central")
	}
}

func TestManagedOperatorMutationsRequireCompletedLayoutAndAllRuntimeLocks(t *testing.T) {
	t.Setenv("GOFER_SECRET_KEY", "")
	path := filepath.Join(t.TempDir(), "central.db")
	s, err := openManagedStorage(t.Context(), path, 1)
	if err != nil {
		t.Fatal(err)
	}
	layout := s.layout
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFER_DB_PATH", path)
	for _, target := range []string{layout.SourcePath, path, filepath.Join(layout.UserDirectory, "manager")} {
		lock, err := runtimeguard.Acquire(target)
		if err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		code := runApplication(t.Context(), []string{"auth", "setup-token", "rotate"}, &stdout, &stderr, func() { t.Error("operator started server") })
		lock.Close()
		if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "already using this database") {
			t.Fatal("operator ignored runtime lock", target, code, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	if code := runApplication(t.Context(), []string{"auth", "setup-token", "rotate"}, &stdout, &stderr, func() { t.Error("operator started server") }); code != 0 || !strings.Contains(stdout.String(), "setup_token:") {
		t.Fatal("completed layout operator failed", code, stderr.String())
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path + ".layout.json"); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runApplication(t.Context(), []string{"auth", "setup-token", "rotate"}, &stdout, &stderr, func() { t.Error("operator started server") }); code != 1 || stdout.Len() != 0 {
		t.Fatal("operator mutated incomplete layout", code)
	}
	after, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("incomplete layout changed", err)
	}
}
