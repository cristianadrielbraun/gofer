package storage_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserStorageMigrationPublishesCompletedLayoutAndAllowsLiveWrites(t *testing.T) {
	options, before := newMigrationStageFixture(t)
	stage, err := storage.StageUserStorageMigration(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := storage.PublishUserStorageMigration(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if layout.ID != stage.AttemptID || layout.CentralPath != options.DestinationPath || layout.UserDirectory != options.DestinationPath+".users" || layout.SourcePath != options.SourcePath || layout.OwnersAtMigration != 3 {
		t.Fatal("wrong completed layout", layout)
	}
	for _, private := range []string{filepath.Join(stage.Directory, "central.db"), filepath.Join(stage.Directory, "users")} {
		if _, err := os.Lstat(private); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("published databases remain private", private, err)
		}
	}
	loaded, err := storage.LoadUserStorageLayout(t.Context(), options.DestinationPath)
	if err != nil || loaded != layout {
		t.Fatal("completed layout did not load", err, loaded)
	}
	central, err := storage.OpenExisting(layout.CentralPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := central.Write().Exec(`UPDATE users SET name='legitimate post-migration change' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	stores, err := storage.NewUserStores(central, storage.UserStoreOptions{Directory: layout.UserDirectory, MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := stores.AcquireExisting(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := lease.DB().Write().Exec(`UPDATE messages SET subject='legitimate post-migration subject'`)
	lease.Release()
	if err := errors.Join(writeErr, stores.Close(context.Background()), central.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.LoadUserStorageLayout(t.Context(), layout.CentralPath); err != nil {
		t.Fatal("initial fingerprints blocked live writes", err)
	}
	options.Retry = true
	resumed, err := storage.PublishUserStorageMigration(t.Context(), options)
	if err != nil || resumed != layout {
		t.Fatal("completed retry rewrote/rejected live changes", err, resumed)
	}
	after, err := os.ReadFile(options.SourcePath)
	if err != nil || sha256.Sum256(after) != sha256.Sum256(before) {
		t.Fatal("publication altered original database", err)
	}
	var journal struct{ State string }
	data, err := os.ReadFile(options.DestinationPath + ".migration.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &journal); err != nil || journal.State != "published" {
		t.Fatal("publication journal unfinished", err, journal.State)
	}
}

func TestUserStorageMigrationPublicationRevalidatesPreparationAndRefusesUnknownFiles(t *testing.T) {
	for _, damage := range []string{"source-change", "private-change", "private-owner-change", "unexpected-user-file", "malformed-sidecar", "destination", "destination-directory", "destination-sidecar", "source-lock", "destination-lock", "manager-lock"} {
		t.Run(damage, func(t *testing.T) {
			options, _ := newMigrationStageFixture(t)
			stage, err := storage.StageUserStorageMigration(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			var lock *runtimeguard.Lock
			switch damage {
			case "source-change", "private-change", "private-owner-change":
				path := options.SourcePath
				if damage == "private-change" {
					path = filepath.Join(stage.Directory, "central.db")
				}
				if damage == "private-owner-change" {
					hash := sha256.Sum256([]byte("alice"))
					path = filepath.Join(stage.Directory, "users", hex.EncodeToString(hash[:])+".db")
				}
				db, openErr := storage.OpenExisting(path)
				if openErr != nil {
					t.Fatal(openErr)
				}
				_, err = db.Write().Exec(`UPDATE users SET name='changed after staging' WHERE id='alice'`)
				err = errors.Join(err, db.Close())
			case "malformed-sidecar":
				hash := sha256.Sum256([]byte("alice"))
				err = os.WriteFile(filepath.Join(stage.Directory, "users", hex.EncodeToString(hash[:])+".db-shm-wal"), []byte("unclassified sidecar"), 0600)
			case "unexpected-user-file":
				err = os.WriteFile(filepath.Join(stage.Directory, "users", "unclassified"), []byte("preserve unknown file"), 0600)
			case "destination":
				err = os.WriteFile(options.DestinationPath, []byte("preserve existing destination"), 0600)
			case "destination-sidecar":
				err = os.WriteFile(options.DestinationPath+"-wal", []byte("unrecognized WAL"), 0600)
			case "destination-directory":
				err = os.Mkdir(options.DestinationPath+".users", 0700)
			case "source-lock":
				lock, err = runtimeguard.Acquire(options.SourcePath)
			case "destination-lock":
				lock, err = runtimeguard.Acquire(options.DestinationPath)
			case "manager-lock":
				lock, err = runtimeguard.Acquire(filepath.Join(stage.Directory, "users", "manager"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if lock != nil {
				defer lock.Close()
			}
			if _, err := storage.PublishUserStorageMigration(t.Context(), options); err == nil {
				t.Fatal("unverified publication accepted")
			}
			if _, err := os.Lstat(options.DestinationPath + ".layout.json"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed publication activated layout", err)
			}
			if damage == "destination" {
				data, err := os.ReadFile(options.DestinationPath)
				if err != nil || string(data) != "preserve existing destination" {
					t.Fatal("existing destination replaced", err)
				}
			}
		})
	}
}

func TestUserStorageMigrationLayoutRefusesMissingStoresAndWrongIdentity(t *testing.T) {
	for _, damage := range []string{"missing-manifest", "missing-owner", "wrong-identity", "trailing-manifest", "unknown-manifest-field", "manifest-working-directory", "manifest-source-path"} {
		t.Run(damage, func(t *testing.T) {
			options, _ := newMigrationStageFixture(t)
			if _, err := storage.StageUserStorageMigration(t.Context(), options); err != nil {
				t.Fatal(err)
			}
			layout, err := storage.PublishUserStorageMigration(t.Context(), options)
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "missing-manifest":
				err = os.Remove(layout.CentralPath + ".layout.json")
			case "missing-owner":
				hash := sha256.Sum256([]byte("charlie"))
				err = os.Remove(filepath.Join(layout.UserDirectory, hex.EncodeToString(hash[:])+".db"))
			case "wrong-identity":
				db, openErr := storage.OpenExisting(layout.CentralPath)
				if openErr != nil {
					t.Fatal(openErr)
				}
				_, err = db.Write().Exec(`UPDATE gofer_storage_layout SET layout_id='00000000000000000000000000000000'`)
				err = errors.Join(err, db.Close())
			case "trailing-manifest":
				file, openErr := os.OpenFile(layout.CentralPath+".layout.json", os.O_APPEND|os.O_WRONLY, 0)
				if openErr != nil {
					t.Fatal(openErr)
				}
				_, err = file.WriteString("{}")
				err = errors.Join(err, file.Close())
			case "manifest-working-directory", "manifest-source-path":
				edited := layout
				if damage == "manifest-working-directory" {
					edited.WorkingDirectory = t.TempDir()
				} else {
					edited.SourcePath = filepath.Join(filepath.Dir(layout.SourcePath), "different-source.db")
				}
				data, encodeErr := json.Marshal(edited)
				if encodeErr != nil {
					t.Fatal(encodeErr)
				}
				err = os.WriteFile(layout.CentralPath+".layout.json", data, 0600)
			case "unknown-manifest-field":
				data, _ := json.Marshal(layout)
				var object map[string]any
				if err := json.Unmarshal(data, &object); err != nil {
					t.Fatal(err)
				}
				object["unknown"] = true
				data, err = json.Marshal(object)
				if err == nil {
					err = os.WriteFile(layout.CentralPath+".layout.json", data, 0600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := storage.LoadUserStorageLayout(t.Context(), layout.CentralPath); err == nil {
				t.Fatal("invalid layout activated")
			}
		})
	}
}

func TestUserStorageMigrationPublicationFinalVerifiersHoldRuntimeLocks(t *testing.T) {
	options, _ := newMigrationStageFixture(t)
	if _, err := storage.StageUserStorageMigration(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	original := options.VerifyCredentials
	options.VerifyCredentials = func(ctx context.Context, tx *sql.Tx) error {
		for _, path := range []string{options.SourcePath, options.DestinationPath} {
			lock, err := runtimeguard.Acquire(path)
			if lock != nil {
				lock.Close()
			}
			if !errors.Is(err, runtimeguard.ErrAlreadyLocked) {
				return errors.New("publication verifier ran without runtime locks")
			}
		}
		return original(ctx, tx)
	}
	if _, err := storage.PublishUserStorageMigration(t.Context(), options); err != nil {
		t.Fatal(err)
	}
}

func TestUserStorageMigrationInspectionRejectsManagedLayoutMarkerEvenWithoutRows(t *testing.T) {
	options, _ := newMigrationStageFixture(t)
	source, err := storage.OpenExisting(options.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = source.Write().Exec(`CREATE TABLE gofer_storage_layout(singleton INTEGER,layout_version INTEGER,layout_id TEXT)`)
	if err := errors.Join(err, source.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.InspectUserStorageMigration(t.Context(), options.SourcePath); err == nil {
		t.Fatal("incomplete managed layout treated as shared source")
	}
}
