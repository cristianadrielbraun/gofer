package storage_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
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

func TestUserStorageMigrationStageCrashHelper(t *testing.T) {
	if os.Getenv("GOFER_MIGRATION_CRASH_HELPER") != "1" {
		return
	}
	options := storage.UserStorageMigrationOptions{SourcePath: os.Getenv("GOFER_MIGRATION_CRASH_SOURCE"), DestinationPath: os.Getenv("GOFER_MIGRATION_CRASH_DESTINATION")}
	options.ValidateSource = func(context.Context, *storage.DB) error { return nil }
	options.ImportCredentials = func(context.Context, *sql.Tx) error { os.Exit(23); return nil }
	_, err := storage.StageUserStorageMigration(t.Context(), options)
	t.Fatalf("crash checkpoint not reached: %v", err)
}

func TestUserStorageMigrationStageRetriesAfterActualProcessExit(t *testing.T) {
	options, before := newMigrationStageFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), executable, "-test.run=^TestUserStorageMigrationStageCrashHelper$")
	command.Env = append(os.Environ(), "GOFER_MIGRATION_CRASH_HELPER=1", "GOFER_MIGRATION_CRASH_SOURCE="+options.SourcePath, "GOFER_MIGRATION_CRASH_DESTINATION="+options.DestinationPath)
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("crash helper failed: %v %s", err, output)
	}
	old := options.DestinationPath + ".staging"
	centralBefore, err := os.ReadFile(filepath.Join(old, "central.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.StageUserStorageMigration(t.Context(), options); err == nil {
		t.Fatal("implicit retry accepted")
	}
	options.Retry = true
	stage, err := storage.StageUserStorageMigration(t.Context(), options)
	if err != nil || stage.Owners != 3 || stage.Directory == old || stage.AttemptID == "" {
		t.Fatal("fresh retry failed", stage, err)
	}
	centralAfter, err := os.ReadFile(filepath.Join(old, "central.db"))
	if err != nil || sha256.Sum256(centralBefore) != sha256.Sum256(centralAfter) {
		t.Fatal("failed stage overwritten or opened", err)
	}
	after, err := os.ReadFile(options.SourcePath)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("crash/retry changed source", err)
	}
	if _, err := os.Lstat(options.DestinationPath + ".layout.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retry published layout", err)
	}
	if _, err := os.Lstat(filepath.Join(old, "INCOMPLETE")); err != nil {
		t.Fatal("old crash stage discarded", err)
	}
	data, err := os.ReadFile(options.DestinationPath + ".migration.json")
	if err != nil {
		t.Fatal(err)
	}
	var journal struct {
		State  string
		Owners int64 `json:"owners_copied"`
		ID     string
	}
	if err := json.Unmarshal(data, &journal); err != nil || journal.State != "verified" || journal.Owners != 3 || journal.ID != stage.AttemptID {
		t.Fatal("retry journal not verified", journal, err)
	}
}

func TestUserStorageMigrationStageDetectsDatabaseChangesBeforeVerification(t *testing.T) {
	options, _ := newMigrationStageFixture(t)
	importCredentials := options.ImportCredentials
	options.ImportCredentials = func(ctx context.Context, tx *sql.Tx) error {
		if err := importCredentials(ctx, tx); err != nil {
			return err
		}
		// Deliberately bypass the cooperative runtime guard on disposable data.
		source, err := storage.OpenExisting(options.SourcePath)
		if err != nil {
			return err
		}
		_, err = source.Write().Exec(`UPDATE users SET name='changed after copying' WHERE id='alice'`)
		return errors.Join(err, source.Close())
	}
	stage, err := storage.StageUserStorageMigration(t.Context(), options)
	if err == nil || stage.Owners != 3 {
		t.Fatal("changed database certified", stage, err)
	}
	data, err := os.ReadFile(options.DestinationPath + ".migration.json")
	if err != nil {
		t.Fatal(err)
	}
	var journal struct{ State string }
	if err := json.Unmarshal(data, &journal); err != nil || journal.State != "preparing" {
		t.Fatal("changed source marked verified", journal, err)
	}
	if _, err := os.Lstat(options.DestinationPath + ".layout.json"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("changed source published", err)
	}
}

func TestUserStorageMigrationStageSourceWALHelper(t *testing.T) {
	if os.Getenv("GOFER_MIGRATION_WAL_HELPER") != "1" {
		return
	}
	source, err := storage.OpenExisting(os.Getenv("GOFER_MIGRATION_WAL_SOURCE"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Write().Exec(`UPDATE users SET name='Original retained WAL value' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	// Keep the newest committed pages exclusively in the source WAL.
	os.Exit(24)
}

func TestUserStorageMigrationStagePreservesUncheckpointedSourceWAL(t *testing.T) {
	options, _ := newMigrationStageFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), executable, "-test.run=^TestUserStorageMigrationStageSourceWALHelper$")
	command.Env = append(os.Environ(), "GOFER_MIGRATION_WAL_HELPER=1", "GOFER_MIGRATION_WAL_SOURCE="+options.SourcePath)
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 24 {
		t.Fatalf("source WAL helper failed: %v %s", err, output)
	}
	databaseBefore, err := os.ReadFile(options.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	walBefore, err := os.ReadFile(options.SourcePath + "-wal")
	if err != nil || len(walBefore) < 32 {
		t.Fatal("source WAL not retained", err)
	}
	stage, err := storage.StageUserStorageMigration(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	central, err := storage.OpenExisting(filepath.Join(stage.Directory, "central.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer central.Close()
	var name string
	if err := central.Read().QueryRow(`SELECT name FROM users WHERE id='alice'`).Scan(&name); err != nil || name != "Original retained WAL value" {
		t.Fatal("latest source WAL pages omitted", name, err)
	}
	databaseAfter, err := os.ReadFile(options.SourcePath)
	if err != nil || sha256.Sum256(databaseBefore) != sha256.Sum256(databaseAfter) {
		t.Fatal("source checkpointed or changed", err)
	}
	walAfter, err := os.ReadFile(options.SourcePath + "-wal")
	if err != nil || sha256.Sum256(walBefore) != sha256.Sum256(walAfter) {
		t.Fatal("source WAL modified or removed", err)
	}
}

func TestUserStorageMigrationStageRebuildsEligibleAvatarHintsWithoutChangingCache(t *testing.T) {
	options, _ := newMigrationStageFixture(t)
	source, err := storage.OpenExisting(options.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	hash := avatarresolver.GravatarHash("sender@example.com")
	if _, err := source.Write().Exec(`UPDATE messages SET from_email=' Sender@Example.COM ';
 INSERT INTO sender_avatars(email_hash,email,status,image_data,error) VALUES(?,'original@example.com','ready',x'0001FF','retained metadata');
 INSERT INTO accounts(id,user_id,email_address,is_deleting) VALUES('alice-deleting','alice','deleting@example.com',1),('charlie-mail','charlie','charlie@example.com',0)`, hash); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ account, email string }{{"alice-deleting", "hidden@example.com"}, {"charlie-mail", "sender@example.com"}} {
		folder := entry.account + "-inbox"
		if err := source.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{ID: folder, AccountID: entry.account, Name: "Inbox", Selectable: true}}); err != nil {
			t.Fatal(err)
		}
		if err := source.UpsertSyncMessages(t.Context(), []storage.SyncMessage{{AccountID: entry.account, FolderID: folder, RemoteUID: 1, MessageID: "<" + entry.account + "@fixture>", FromEmail: entry.email, DateSent: time.Now().UTC()}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.UpsertSyncMessages(t.Context(), []storage.SyncMessage{{AccountID: "alice-mail", FolderID: "alice-inbox", RemoteUID: 2, MessageID: "<uncached@fixture>", FromEmail: "uncached@example.com", DateSent: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(options.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := storage.StageUserStorageMigration(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	central, err := storage.OpenExisting(filepath.Join(stage.Directory, "central.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer central.Close()
	var interests, foreign, cacheRows int
	if err := central.Read().QueryRow(`SELECT (SELECT count(*) FROM gofer_avatar_interests),(SELECT count(*) FROM gofer_avatar_interests WHERE user_id NOT IN ('alice','charlie')),(SELECT count(*) FROM sender_avatars)`).Scan(&interests, &foreign, &cacheRows); err != nil || interests != 3 || foreign != 0 || cacheRows != 2 {
		t.Fatal("avatar hint ownership", interests, foreign, cacheRows, err)
	}
	var email, status, retained string
	var image []byte
	if err := central.Read().QueryRow(`SELECT email,status,image_data,error FROM sender_avatars WHERE email_hash=?`, hash).Scan(&email, &status, &image, &retained); err != nil || email != "original@example.com" || status != "ready" || retained != "retained metadata" || len(image) != 3 || image[2] != 255 {
		t.Fatal("original avatar cache replaced", err)
	}
	if err := central.Read().QueryRow(`SELECT status FROM sender_avatars WHERE email_hash=?`, avatarresolver.GravatarHash("uncached@example.com")).Scan(&status); err != nil || status != "pending" {
		t.Fatal("uncached sender was not initialized", status, err)
	}
	after, err := os.ReadFile(options.SourcePath)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("avatar hints changed source", err)
	}
}

func TestUserStorageMigrationStageFinalAuditRejectsLateChangesToEarlierStores(t *testing.T) {
	for _, damage := range []string{"content", "high-water", "missing-file"} {
		t.Run(damage, func(t *testing.T) {
			options, _ := newMigrationStageFixture(t)
			importCredentials := options.ImportCredentials
			options.ImportCredentials = func(ctx context.Context, tx *sql.Tx) error {
				if err := importCredentials(ctx, tx); err != nil {
					return err
				}
				hash := sha256.Sum256([]byte("alice"))
				path := filepath.Join(options.DestinationPath+".staging", "users", fmt.Sprintf("%x.db", hash))
				if damage == "missing-file" {
					return os.Remove(path)
				}
				local, err := storage.OpenExisting(path)
				if err != nil {
					return err
				}
				query := `UPDATE messages SET subject='changed after owner copy'`
				if damage == "high-water" {
					query = `UPDATE sqlite_sequence SET seq=seq+1 WHERE name='messages'`
				}
				_, err = local.Write().Exec(query)
				return errors.Join(err, local.Close())
			}
			stage, err := storage.StageUserStorageMigration(t.Context(), options)
			if err == nil || stage.Owners != 3 {
				t.Fatal("late owner-store change certified", stage, err)
			}
			data, err := os.ReadFile(options.DestinationPath + ".migration.json")
			if err != nil {
				t.Fatal(err)
			}
			var journal struct{ State string }
			if err := json.Unmarshal(data, &journal); err != nil || journal.State != "preparing" {
				t.Fatal("final audit failure marked verified", journal, err)
			}
			if _, err := os.Lstat(options.DestinationPath + ".layout.json"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("final audit failure published", err)
			}
		})
	}
}
