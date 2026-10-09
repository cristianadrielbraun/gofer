package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
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

func TestManagedStorageMigratesSharedDatabaseInPlace(t *testing.T) {
	for _, interruption := range []string{"none", "retired", "preparing"} {
		t.Run(interruption, func(t *testing.T) {
			options, _, _ := migrationCommandFixture(t)
			path := options.SourcePath
			if _, err := os.Lstat(path + "-wal"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("fixture left a write-ahead log", err)
			}
			switch interruption {
			case "retired":
				if err := storage.RetireSharedDatabase(t.Context(), path, path+".shared"); err != nil {
					t.Fatal(err)
				}
			case "preparing":
				if err := storage.RetireSharedDatabase(t.Context(), path, path+".shared"); err != nil {
					t.Fatal(err)
				}
				options.SourcePath, options.DestinationPath = path+".shared", path
				options.ImportCredentials = func(context.Context, *sql.Tx) error { return errors.New("interrupted") }
				if _, err := storage.StageUserStorageMigration(t.Context(), options); err == nil {
					t.Fatal("interruption not injected")
				}
			}
			s, err := openManagedStorage(t.Context(), path, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if s.layout.CentralPath != path || s.layout.SourcePath != path+".shared" || s.layout.OwnersAtMigration != 2 {
				t.Fatal("in-place layout", s.layout)
			}
			lease, err := s.stores.AcquireExisting(t.Context(), "bob")
			if err != nil {
				t.Fatal(err)
			}
			var contacts int
			err = lease.DB().Read().QueryRowContext(t.Context(), `SELECT count(*) FROM contact_profiles`).Scan(&contacts)
			lease.Release()
			if err != nil || contacts != 1 {
				t.Fatal("migrated owner data", contacts, err)
			}
			for _, leftover := range []string{path + ".staging", path + ".shared-wal", path + "-journal"} {
				if _, err := os.Lstat(leftover); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("migration left private files behind", leftover, err)
				}
			}
		})
	}
}

func TestManagedStorageRejectsAmbiguousSharedDatabases(t *testing.T) {
	options, before, _ := migrationCommandFixture(t)
	path := options.SourcePath
	if err := os.WriteFile(path+".shared", []byte("retained data"), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := openManagedStorage(t.Context(), path, 1); err == nil {
		s.Close()
		t.Fatal("adopted ambiguous databases")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("changed original database", err)
	}
}

func TestManagedStorageUpgradesOlderLayoutSchemas(t *testing.T) {
	options, _, key := migrationCommandFixture(t)
	if _, err := storage.MigrateUserStorage(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFER_SECRET_KEY", hex.EncodeToString(key))
	path := options.DestinationPath
	userPath := func(owner string) string {
		hash := sha256.Sum256([]byte(owner))
		return filepath.Join(path+".users", hex.EncodeToString(hash[:])+".db")
	}
	exec := func(target string, statements ...string) {
		t.Helper()
		db, err := sql.Open("sqlite", target)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, statement := range statements {
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(statement, err)
			}
		}
	}
	// Return the central and one user database to the v106 schema, as an older
	// release left them, and drop a guard the way a rebuilt table loses it.
	downgrade := []string{`ALTER TABLE calendar_response_requests DROP COLUMN claim_id`, `DELETE FROM schema_version WHERE version>=106`, `INSERT INTO schema_version(version) VALUES(106)`}
	exec(path, downgrade...)
	exec(userPath("alice"), append(downgrade, `DROP TRIGGER gofer_store_owner_contact_profiles_insert`)...)

	s, err := openManagedStorage(t.Context(), path, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		lease, err := s.stores.AcquireExisting(t.Context(), owner)
		if err != nil {
			t.Fatal(err)
		}
		var version, claim, guard int
		err = lease.DB().Read().QueryRowContext(t.Context(), `SELECT (SELECT max(version) FROM schema_version),
 (SELECT count(*) FROM pragma_table_info('calendar_response_requests') WHERE name='claim_id'),
 (SELECT count(*) FROM sqlite_schema WHERE type='trigger' AND name='gofer_store_owner_contact_profiles_insert')`).Scan(&version, &claim, &guard)
		lease.Release()
		if err != nil || version != storage.CurrentSchemaVersion || claim != 1 || guard != 1 {
			t.Fatal("user database not upgraded", owner, version, claim, guard, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// A database written by a newer release is refused and left unchanged.
	exec(userPath("bob"), `INSERT INTO schema_version(version) VALUES(9999)`)
	if _, err := openManagedStorage(t.Context(), path, 1); err == nil || !strings.Contains(err.Error(), "newer Gofer release") {
		t.Fatal("newer schema accepted", err)
	}
	db, err := sql.Open("sqlite", userPath("bob"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`SELECT max(version) FROM schema_version`).Scan(&version); err != nil || version != 9999 {
		t.Fatal("newer schema changed", version, err)
	}
}

func TestManagedStorageFailedMigrationRestoresOriginal(t *testing.T) {
	for _, interruption := range []string{"none", "preparing"} {
		t.Run(interruption, func(t *testing.T) {
			options, _, _ := migrationCommandFixture(t)
			path := options.SourcePath
			stray := func(target string) {
				t.Helper()
				db, err := sql.Open("sqlite", target)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				// A table without a migration policy fails source inspection.
				if _, err := db.Exec(`CREATE TABLE stray_table(value TEXT); INSERT INTO stray_table VALUES('kept')`); err != nil {
					t.Fatal(err)
				}
			}
			if interruption == "preparing" {
				if err := storage.RetireSharedDatabase(t.Context(), path, path+".shared"); err != nil {
					t.Fatal(err)
				}
				options.SourcePath, options.DestinationPath = path+".shared", path
				options.ImportCredentials = func(context.Context, *sql.Tx) error { return errors.New("interrupted") }
				if _, err := storage.StageUserStorageMigration(t.Context(), options); err == nil {
					t.Fatal("interruption not injected")
				}
				stray(path + ".shared")
			} else {
				stray(path)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if s, err := openManagedStorage(t.Context(), path, 1); err == nil {
					s.Close()
					t.Fatal("migrated a database without a policy")
				} else if !strings.Contains(err.Error(), "stray_table") {
					t.Fatal("unexpected failure", err)
				}
				for _, leftover := range []string{path + ".shared", path + ".migration.json", path + ".staging", path + ".users", path + ".layout.json"} {
					if _, err := os.Lstat(leftover); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("failed migration left files behind", leftover, err)
					}
				}
				if err := storage.VerifySharedStorageRuntime(t.Context(), path); err != nil {
					t.Fatal("restored original cannot run in shared mode", err)
				}
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				var value string
				var contacts int
				err = db.QueryRow(`SELECT (SELECT value FROM stray_table),(SELECT count(*) FROM contact_profiles)`).Scan(&value, &contacts)
				db.Close()
				if err != nil || value != "kept" || contacts != 1 {
					t.Fatal("original contents changed", value, contacts, err)
				}
			}
		})
	}
}

func TestSharedRuntimeRefusesRetiredOriginal(t *testing.T) {
	options, _, _ := migrationCommandFixture(t)
	path := options.SourcePath
	if err := storage.RetireSharedDatabase(t.Context(), path, path+".shared"); err != nil {
		t.Fatal(err)
	}
	if err := storage.VerifySharedStorageRuntime(t.Context(), path); err == nil || !strings.Contains(err.Error(), ".shared") {
		t.Fatal("shared runtime would start over without the retired original", err)
	}
}

func TestManagedStorageStartsFromAnotherWorkingDirectory(t *testing.T) {
	options, _, key := migrationCommandFixture(t)
	root := filepath.Dir(options.SourcePath)
	relative := filepath.Join("accounts", "oauth-mail", "messages", "1", "raw.eml")
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(relative)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, relative), []byte("retained message"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := storage.New(options.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO messages(id,account_id,raw_path) VALUES(1,'oauth-mail',?)`, relative); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// The shared layout stored the path relative to the directory it ran from.
	options.WorkingDirectory = root
	if _, err := storage.MigrateUserStorage(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFER_SECRET_KEY", hex.EncodeToString(key))
	t.Chdir(t.TempDir())
	s, err := openManagedStorage(t.Context(), options.DestinationPath, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	lease, err := s.stores.AcquireExisting(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	err = lease.DB().Read().QueryRowContext(t.Context(), `SELECT raw_path FROM messages WHERE id=1`).Scan(&raw)
	lease.Release()
	if err != nil || raw != filepath.Join(s.layout.BlobDirectory, "oauth-mail", "messages", "1", "raw.eml") {
		t.Fatal("retained path not made absolute", raw, err)
	}
	if content, err := os.ReadFile(raw); err != nil || string(content) != "retained message" {
		t.Fatal("retained file unreachable", err)
	}
}

func TestManagedStorageRefusesForeignOwnedDatabaseWithoutChanges(t *testing.T) {
	options, _, _ := migrationCommandFixture(t)
	path := options.SourcePath
	db, err := storage.New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('default','myself','myself');
 INSERT INTO auth_system_state(id,initialized,owner_user_id) VALUES(1,1,'default') ON CONFLICT(id) DO UPDATE SET initialized=1,owner_user_id='default'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := openManagedStorage(t.Context(), path, 1); err == nil {
		s.Close()
		t.Fatal("converted an installation owned by a regular user")
	} else if !strings.Contains(err.Error(), "cannot be converted") {
		t.Fatal("unexpected failure", err)
	}
	var stdout, stderr bytes.Buffer
	if code := runApplication(t.Context(), []string{"storage", "migrate", "--db", path, "--to", options.DestinationPath}, &stdout, &stderr, func() {}); code == 0 || !strings.Contains(stderr.String(), "cannot be converted") {
		t.Fatal("migration command converted an installation owned by a regular user", code, stderr.String())
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused database changed", err)
	}
	for _, leftover := range []string{path + ".shared", path + ".migration.json", path + ".layout.json", path + ".users", options.DestinationPath} {
		if _, err := os.Lstat(leftover); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("refused conversion left files behind", leftover, err)
		}
	}
	if err := storage.VerifySharedStorageRuntime(t.Context(), path); err != nil {
		t.Fatal("refused database no longer opens as shared storage", err)
	}
}

func TestManagedStorageConversionFromAnotherDirectoryLeavesOriginal(t *testing.T) {
	options, _, _ := migrationCommandFixture(t)
	path := options.SourcePath
	relative := filepath.Join("accounts", "oauth-mail", "messages", "1", "raw.eml")
	db, err := storage.New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO messages(id,account_id,raw_path) VALUES(1,'oauth-mail',?)`, relative); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	if s, err := openManagedStorage(t.Context(), path, 1); err == nil {
		s.Close()
		t.Fatal("converted with paths resolved from the wrong directory")
	} else if !strings.Contains(err.Error(), "run Gofer from the directory the previous version was started from") {
		t.Fatal("unhelpful failure", err)
	}
	for _, leftover := range []string{path + ".shared", path + ".migration.json", path + ".layout.json", path + ".users", path + ".staging"} {
		if _, err := os.Lstat(leftover); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("refused conversion left files behind", leftover, err)
		}
	}
	if err := storage.VerifySharedStorageRuntime(t.Context(), path); err != nil {
		t.Fatal("original no longer opens as shared storage", err)
	}
}
