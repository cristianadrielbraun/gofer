package storage_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

var stageCredentialKey = []byte("0123456789abcdef0123456789abcdef")

func newMigrationStageFixture(t *testing.T) (storage.UserStorageMigrationOptions, []byte) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "shared.db")
	source, err := storage.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	if _, err := source.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('alice','alice','alice'),('bob','bob','bob'),('charlie','charlie','charlie');
 INSERT INTO users(id,username,username_normalized,user_type,is_admin) VALUES('admin','admin','admin','management',1);
 INSERT INTO accounts(id,user_id,provider,provider_account_id,auth_method,email_address) VALUES('alice-mail','alice','gmail','alice-original-subject','oauth2','alice@example.com'),('bob-mail','bob','imap','','plain','bob@example.com');
 INSERT INTO contact_profiles(id,user_id,display_name) VALUES('charlie-profile','charlie','Contacts without a mailbox');
 UPDATE users SET status='disabled',deletion_pending=1,deletion_started_by='admin' WHERE id='bob'; UPDATE accounts SET is_deleting=1 WHERE id='bob-mail'`); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := source.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{ID: owner + "-inbox", AccountID: owner + "-mail", Name: "Inbox", Selectable: true}}); err != nil {
			t.Fatal(err)
		}
		if err := source.UpsertSyncMessages(t.Context(), []storage.SyncMessage{{AccountID: owner + "-mail", FolderID: owner + "-inbox", RemoteUID: 1, MessageID: "<" + owner + "@test>", Subject: owner + " searchable", DateSent: time.Now().UTC()}}); err != nil {
			t.Fatal(err)
		}
	}
	expiry := time.Now().Add(time.Hour)
	if err := mailauth.New(nil, source, stageCredentialKey).UpsertOAuthAccount(t.Context(), "alice-mail", "google", "alice-original-subject", "alice-access", "alice-refresh", "Bearer", &expiry, "mail"); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	options := storage.UserStorageMigrationOptions{SourcePath: path, DestinationPath: filepath.Join(root, "owned.db")}
	options.ValidateSource = func(ctx context.Context, db *storage.DB) error {
		if _, err := db.Write().ExecContext(ctx, `UPDATE users SET name='must-not-write'`); err == nil {
			return errors.New("source verifier received a writable database")
		}
		for _, path := range []string{options.SourcePath, options.DestinationPath} {
			if lock, err := runtimeguard.Acquire(path); !errors.Is(err, runtimeguard.ErrAlreadyLocked) {
				if lock != nil {
					lock.Close()
				}
				return errors.New("migration verifier ran without both runtime locks")
			}
		}
		return nil
	}
	options.ImportCredentials = func(ctx context.Context, tx *sql.Tx) error {
		return mailauth.ImportUserStorageMigrationCredentials(ctx, tx, stageCredentialKey)
	}
	return options, before
}

func TestUserStorageMigrationStageCopiesAndReopensWithOneCachedStore(t *testing.T) {
	options, before := newMigrationStageFixture(t)
	stage, err := storage.StageUserStorageMigration(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if stage.Owners != 3 || stage.Source.Accounts != 2 {
		t.Fatal("stage counts", stage.Owners, stage.Source.Accounts)
	}
	for _, path := range []string{options.DestinationPath, options.DestinationPath + ".users", options.DestinationPath + ".layout.json"} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("stage published a destination/layout", path, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(stage.Directory, "INCOMPLETE")); err != nil {
		t.Fatal("stage mislabeled as complete", err)
	}
	if info, err := os.Stat(filepath.Join(stage.Directory, "central.db")); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatal("central stage is not private", err)
	}
	central, err := storage.OpenExisting(filepath.Join(stage.Directory, "central.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer central.Close()
	stores, err := storage.NewUserStores(central, storage.UserStoreOptions{Directory: filepath.Join(stage.Directory, "users"), MaxOpen: 1})
	if err != nil {
		t.Fatal("stage did not join SQLite/cache closure", err)
	}
	defer stores.Close(context.Background())
	routing, err := storage.NewAccountRouting(stores)
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob", "charlie", "alice"} {
		lease, err := stores.AcquireExisting(t.Context(), owner)
		if err != nil {
			t.Fatal("staged owner unavailable", owner, err)
		}
		var messages, foreign int
		err = lease.DB().Read().QueryRow(`SELECT (SELECT count(*) FROM messages),(SELECT count(*) FROM accounts WHERE user_id<>?)`, owner).Scan(&messages, &foreign)
		lease.Release()
		want := 1
		if owner == "charlie" {
			want = 0
		}
		if err != nil || messages != want || foreign != 0 {
			t.Fatal("stage partition changed on eviction/reopen", owner, messages, foreign, err)
		}
	}
	var deleting, hints, private, knownStores int
	if err := central.Read().QueryRow(`SELECT (SELECT count(*) FROM gofer_account_directory WHERE account_id='bob-mail' AND state='deleting'),(SELECT count(*) FROM gofer_account_service_schedule),(SELECT count(*) FROM accounts),(SELECT count(*) FROM gofer_user_store_directory WHERE state='present')`).Scan(&deleting, &hints, &private, &knownStores); err != nil || deleting != 1 || hints != 2 || private != 0 || knownStores != 3 {
		t.Fatal("central directory/scheduling boundary", deleting, hints, private, knownStores, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	credentials, err := mailauth.NewUserCredentials(ctx, nil, routing, stageCredentialKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); credentials.Wait() }()
	if access, err := credentials.GetOAuthTokenForUser(t.Context(), "alice", "alice-mail"); err != nil || access != "alice-access" {
		t.Fatal("staged credentials unusable through owned runtime", err)
	}
	after, err := os.ReadFile(options.SourcePath)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("stage changed source", err)
	}
	if _, err := storage.StageUserStorageMigration(t.Context(), options); err == nil {
		t.Fatal("existing private stage overwritten")
	}
}

func TestUserStorageMigrationStageRetainsFailureWithoutPublishing(t *testing.T) {
	options, before := newMigrationStageFixture(t)
	options.ImportCredentials = func(context.Context, *sql.Tx) error { return errors.New("injected credential failure") }
	stage, err := storage.StageUserStorageMigration(t.Context(), options)
	if err == nil || stage.Owners != 3 {
		t.Fatal("late stage failure not retained", stage.Owners, err)
	}
	if _, err := os.Lstat(filepath.Join(stage.Directory, "INCOMPLETE")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(options.DestinationPath + ".layout.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed stage published a layout", err)
	}
	after, err := os.ReadFile(options.SourcePath)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("failed stage changed source", err)
	}
	for _, path := range []string{options.SourcePath, options.DestinationPath, filepath.Join(stage.Directory, "users", "manager")} {
		lock, err := runtimeguard.Acquire(path)
		if err != nil {
			t.Fatal("failed stage leaked a runtime/cache lock", err)
		}
		lock.Close()
	}
}

func TestUserStorageMigrationStageVerifiesPreservedFilesAndRefusesChanges(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		t.Run(fmt.Sprint("change-files-", mutate), func(t *testing.T) {
			options, _ := newMigrationStageFixture(t)
			options.WorkingDirectory = filepath.Dir(options.SourcePath)
			blobs := store.NewBlobStore(filepath.Join(options.WorkingDirectory, "accounts"))
			path, err := blobs.StoreBodyText(t.Context(), "alice-mail", 1, []byte("original body"))
			if err != nil {
				t.Fatal(err)
			}
			relative, err := filepath.Rel(options.WorkingDirectory, path)
			if err != nil {
				t.Fatal(err)
			}
			source, err := storage.OpenExisting(options.SourcePath)
			if err != nil {
				t.Fatal(err)
			}
			_, updateErr := source.Write().Exec(`UPDATE messages SET body_text_path=? WHERE account_id='alice-mail'`, relative)
			if err := errors.Join(updateErr, source.Close()); err != nil {
				t.Fatal(err)
			}
			if mutate {
				importCredentials := options.ImportCredentials
				options.ImportCredentials = func(ctx context.Context, tx *sql.Tx) error {
					if err := importCredentials(ctx, tx); err != nil {
						return err
					}
					return os.WriteFile(path, []byte("injected change"), 0644)
				}
			}
			stage, err := storage.StageUserStorageMigration(t.Context(), options)
			if (err != nil) != mutate || stage.Owners != 3 || stage.Files.Files != 1 || stage.Files.References != 1 || stage.WorkingDirectory != options.WorkingDirectory {
				t.Fatal("staging file proof", stage, err)
			}
			if _, err := os.Lstat(filepath.Join(stage.Directory, "INCOMPLETE")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(options.DestinationPath + ".layout.json"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("file verification published a layout", err)
			}
			if !mutate {
				central, err := storage.OpenExisting(filepath.Join(stage.Directory, "central.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer central.Close()
				stores, err := storage.NewUserStores(central, storage.UserStoreOptions{Directory: filepath.Join(stage.Directory, "users"), MaxOpen: 1})
				if err != nil {
					t.Fatal(err)
				}
				defer stores.Close(context.Background())
				lease, err := stores.AcquireExisting(t.Context(), "alice")
				if err != nil {
					t.Fatal(err)
				}
				var preserved string
				err = lease.DB().Read().QueryRow(`SELECT body_text_path FROM messages WHERE account_id='alice-mail'`).Scan(&preserved)
				lease.Release()
				if err != nil || preserved != relative {
					t.Fatal("original relative path changed", err)
				}
				content, err := os.ReadFile(filepath.Join(stage.WorkingDirectory, preserved))
				if err != nil || string(content) != "original body" {
					t.Fatal("staged message body unavailable", err)
				}
			}
		})
	}
}

func TestUserStorageMigrationStagePagesOwnersAndPreservesUsersWithoutMailboxes(t *testing.T) {
	options, _ := newMigrationStageFixture(t)
	source, err := storage.OpenExisting(options.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 65; n++ {
		owner := fmt.Sprintf("owner-%03d", n)
		if _, err := source.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES(?,?,?)`, owner, owner, owner); err != nil {
			t.Fatal(err)
		}
	}
	source.Close()
	stage, err := storage.StageUserStorageMigration(t.Context(), options)
	if err != nil || stage.Owners != 68 {
		t.Fatal("bounded ownership traversal lost a page", stage.Owners, err)
	}
}

func TestUserStorageMigrationStageAcceptsSchema105WithoutUpgradingSource(t *testing.T) {
	options, _ := newMigrationStageFixture(t)
	source, err := storage.OpenExisting(options.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Write().Exec(`ALTER TABLE calendar_response_requests DROP COLUMN claim_id; ALTER TABLE web_push_subscriptions DROP COLUMN revision; UPDATE schema_version SET version=105`); err != nil {
		t.Fatal(err)
	}
	source.Close()
	before, err := os.ReadFile(options.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := storage.StageUserStorageMigration(t.Context(), options)
	if err != nil || stage.Source.SchemaVersion != 105 || stage.Source.TargetSchemaVersion != storage.CurrentSchemaVersion || stage.Owners != 3 {
		t.Fatal("legacy stage did not preserve source version", err)
	}
	after, err := os.ReadFile(options.SourcePath)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("legacy source upgraded during staging", err)
	}
}

func TestUserStorageMigrationStageRefusesBusyOrAmbiguousDestinations(t *testing.T) {
	for _, failure := range []string{"source-busy", "destination-busy", "existing-destination", "different-data-directory", "source-alias", "missing-source", "missing-verifier", "symlink-stage", "empty-owner", "cancel-before-copy"} {
		t.Run(failure, func(t *testing.T) {
			options, _ := newMigrationStageFixture(t)
			var lock *runtimeguard.Lock
			var err error
			switch failure {
			case "source-busy":
				lock, err = runtimeguard.Acquire(options.SourcePath)
			case "destination-busy":
				lock, err = runtimeguard.Acquire(options.DestinationPath)
			case "existing-destination":
				err = os.WriteFile(options.DestinationPath, []byte("keep existing destination"), 0600)
			case "different-data-directory":
				options.DestinationPath = filepath.Join(t.TempDir(), "owned.db")
			case "source-alias":
				options.DestinationPath = options.SourcePath
			case "missing-source":
				options.SourcePath += ".missing"
			case "missing-verifier":
				options.ValidateSource = nil
			case "symlink-stage":
				if runtime.GOOS == "windows" {
					t.Skip("symlink creation requires platform privileges")
				}
				err = os.Symlink(t.TempDir(), options.DestinationPath+".staging")
			case "empty-owner":
				source, openErr := storage.OpenExisting(options.SourcePath)
				if openErr != nil {
					t.Fatal(openErr)
				}
				_, err = source.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('','empty-owner','empty-owner')`)
				source.Close()
			}
			if err != nil {
				t.Fatal(err)
			}
			if lock != nil {
				defer lock.Close()
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if failure == "cancel-before-copy" {
				options.ValidateSource = func(context.Context, *storage.DB) error { cancel(); return nil }
			}
			if _, err := storage.StageUserStorageMigration(ctx, options); err == nil {
				t.Fatal("unsafe stage accepted")
			}
			if failure != "symlink-stage" {
				if _, err := os.Lstat(options.DestinationPath + ".staging"); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("refused stage still created data", err)
				}
			}
			if failure == "existing-destination" {
				data, err := os.ReadFile(options.DestinationPath)
				if err != nil || string(data) != "keep existing destination" {
					t.Fatal("existing destination overwritten", err)
				}
			}
		})
	}
}
