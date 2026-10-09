package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// This fixture exercises the file-publication protocol without a mailauth import
// cycle. It has no encrypted secrets or OAuth rows; the external publication
// fixture separately uses the production key and credential adapters.
func publicationRecoveryOptions(source, destination string) UserStorageMigrationOptions {
	return UserStorageMigrationOptions{SourcePath: source, DestinationPath: destination,
		ValidateSource:    func(context.Context, *DB) error { return nil },
		ImportCredentials: func(context.Context, *sql.Tx) error { return nil },
		VerifyCredentials: func(context.Context, *sql.Tx) error { return nil }}
}

func TestUserStorageMigrationPublicationCrashHelper(t *testing.T) {
	if os.Getenv("GOFER_PUBLICATION_CRASH_HELPER") != "1" {
		return
	}
	options := publicationRecoveryOptions(os.Getenv("GOFER_PUBLICATION_CRASH_SOURCE"), os.Getenv("GOFER_PUBLICATION_CRASH_DESTINATION"))
	step := os.Getenv("GOFER_PUBLICATION_CRASH_STEP")
	_, err := publishUserStorageMigration(t.Context(), options, func(current string) error {
		if current == step {
			os.Exit(25)
		}
		return nil
	})
	t.Fatalf("publication crash checkpoint not reached: %v", err)
}

func TestUserStorageMigrationPublicationResumesAfterActualProcessExit(t *testing.T) {
	for _, step := range []string{"journal", "central-link", "central-moved", "users-moved", "before-manifest", "manifest"} {
		t.Run(step, func(t *testing.T) {
			root := t.TempDir()
			sourcePath := filepath.Join(root, "shared.db")
			destination := filepath.Join(root, "owned.db")
			source, err := New(sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := source.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('alice','alice','alice'),('bob','bob','bob'); INSERT INTO accounts(id,user_id,email_address) VALUES('alice-mail','alice','alice@example.com'),('bob-mail','bob','bob@example.com')`); err != nil {
				t.Fatal(err)
			}
			for _, owner := range []string{"alice", "bob"} {
				if err := source.UpsertFolders(t.Context(), []UpsertFolderInput{{ID: owner + "-inbox", AccountID: owner + "-mail", Name: "Inbox", Selectable: true}}); err != nil {
					t.Fatal(err)
				}
				if err := source.UpsertSyncMessages(t.Context(), []SyncMessage{{AccountID: owner + "-mail", FolderID: owner + "-inbox", RemoteUID: 1, MessageID: "<" + owner + "@fixture>", Subject: owner + " retained message", DateSent: time.Now().UTC()}}); err != nil {
					t.Fatal(err)
				}
			}
			if err := source.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			options := publicationRecoveryOptions(sourcePath, destination)
			if _, err := StageUserStorageMigration(t.Context(), options); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(t.Context(), executable, "-test.run=^TestUserStorageMigrationPublicationCrashHelper$")
			command.Env = append(os.Environ(), "GOFER_PUBLICATION_CRASH_HELPER=1", "GOFER_PUBLICATION_CRASH_SOURCE="+sourcePath, "GOFER_PUBLICATION_CRASH_DESTINATION="+destination, "GOFER_PUBLICATION_CRASH_STEP="+step)
			output, err := command.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 25 {
				t.Fatalf("crash helper failed: %v %s", err, output)
			}
			_, loadErr := LoadUserStorageLayout(t.Context(), destination)
			if step == "manifest" {
				if loadErr != nil {
					t.Fatal("completed manifest not valid after process exit", loadErr)
				}
			} else if loadErr == nil {
				t.Fatal("interrupted publication enabled a layout")
			}
			if _, err := PublishUserStorageMigration(t.Context(), options); err == nil {
				t.Fatal("publication resumed without explicit retry")
			}
			options.Retry = true
			layout, err := PublishUserStorageMigration(t.Context(), options)
			if err != nil {
				t.Fatal("publication did not resume", err)
			}
			if _, err := LoadUserStorageLayout(t.Context(), destination); err != nil {
				t.Fatal(err)
			}
			central, err := OpenExisting(layout.CentralPath)
			if err != nil {
				t.Fatal(err)
			}
			stores, err := NewUserStores(central, UserStoreOptions{Directory: layout.UserDirectory, MaxOpen: 1})
			if err != nil {
				t.Fatal(err)
			}
			for _, owner := range []string{"alice", "bob"} {
				lease, err := stores.AcquireExisting(t.Context(), owner)
				if err != nil {
					t.Fatal(err)
				}
				var subject string
				err = lease.DB().Read().QueryRow(`SELECT subject FROM messages`).Scan(&subject)
				lease.Release()
				if err != nil || subject != owner+" retained message" {
					t.Fatal("publication recovery changed owner data", owner, subject, err)
				}
			}
			if err := errors.Join(stores.Close(context.Background()), central.Close()); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(sourcePath)
			if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatal("publication recovery changed source", err)
			}
			if _, err := os.Lstat(destination + ".migration.json"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("recovered publication left its journal", err)
			}
		})
	}
}
