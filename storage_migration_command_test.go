package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestStorageInspectionCommandUsesActualEntryPointAndPreservesMainSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared-main.db")
	db, err := storage.New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('alice','private-username','private-username'); INSERT INTO accounts(id,user_id,email_address,encrypted_password) VALUES('account','alice','private-mailbox@example.com',X'010203'); ALTER TABLE web_push_subscriptions DROP COLUMN revision; ALTER TABLE calendar_response_requests DROP COLUMN claim_id; DELETE FROM schema_version; INSERT INTO schema_version(version) VALUES(105)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFER_DB_PATH", filepath.Join(t.TempDir(), "unused-default.db"))
	var stdout, stderr bytes.Buffer
	started := false
	code := runApplication(t.Context(), []string{"storage", "inspect", "--db", path}, &stdout, &stderr, func() { started = true })
	if code != 0 || started || stderr.Len() != 0 {
		t.Fatal("entry-point inspection", code, started, stderr.String())
	}
	var report storage.UserStorageMigrationPreflight
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.SchemaVersion != 105 || report.TargetSchemaVersion != storage.CurrentSchemaVersion || report.Accounts != 1 {
		t.Fatal("inspection report", report, err)
	}
	for _, private := range []string{"private-username", "private-mailbox@example.com", "010203"} {
		if strings.Contains(stdout.String(), private) {
			t.Fatal("inspection exposed private row content")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("source changed", err)
	}
	if _, err := os.Lstat(os.Getenv("GOFER_DB_PATH")); !os.IsNotExist(err) {
		t.Fatal("default database opened", err)
	}
	for _, name := range []string{"secret.key", "vapid_private.key", "vapid_public.key"} {
		if _, err := os.Lstat(filepath.Join(filepath.Dir(path), name)); !os.IsNotExist(err) {
			t.Fatal("server startup created keys", name, err)
		}
	}
}

func TestStorageInspectionCommandRejectsInvalidArgumentsAndRunningRuntime(t *testing.T) {
	for _, args := range [][]string{{"storage"}, {"storage", "unknown"}, {"storage", "inspect", "--unknown"}, {"storage", "inspect", "unexpected"}, {"storage", "inspect", "--db", ""}} {
		var stdout, stderr bytes.Buffer
		started := false
		if code := runApplication(t.Context(), args, &stdout, &stderr, func() { started = true }); code != 2 || started || stderr.Len() == 0 || stdout.Len() != 0 {
			t.Fatal("invalid command started runtime", args, code, started)
		}
	}
	path := filepath.Join(t.TempDir(), "system.db")
	db, err := storage.New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err := runtimeguard.Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	var stdout, stderr bytes.Buffer
	if code := runApplication(t.Context(), []string{"storage", "inspect", "--db", path}, &stdout, &stderr, func() { t.Error("started runtime") }); code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "already using this database") {
		t.Fatal("runtime lock ignored", code, stderr.String())
	}
}

func migrationCommandFixture(t *testing.T) (storage.UserStorageMigrationOptions, []byte, []byte) {
	t.Helper()
	t.Setenv("GOFER_SECRET_KEY", "")
	root := t.TempDir()
	path := filepath.Join(root, "shared.db")
	key := []byte("0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(filepath.Join(root, "secret.key"), key, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := storage.New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('alice','alice','alice'),('bob','bob','bob'); INSERT INTO accounts(id,user_id,provider,provider_account_id,auth_method,email_address) VALUES('oauth-mail','alice','gmail','original-subject','oauth2','private-mail@example.com'); INSERT INTO contact_profiles(id,user_id,display_name) VALUES('bob-profile','bob','Retained contact')`); err != nil {
		t.Fatal(err)
	}
	accounts, err := config.NewAccountStore(db, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.CreateAccount(t.Context(), "alice", &models.CreateAccountRequest{Provider: "imap", EmailAddress: "alice@example.com", Username: "alice@example.com", Password: "private-password", IMAPHost: "imap.fixture.invalid", IMAPPort: 993, IMAPTLSMode: "tls", SMTPHost: "smtp.fixture.invalid", SMTPPort: 465, SMTPTLSMode: "tls", AuthMethod: "plain"}); err != nil {
		t.Fatal(err)
	}
	if err := mailauth.New(nil, db, key).UpsertOAuthAccount(t.Context(), "oauth-mail", "google", "original-subject", "private-access", "private-refresh", "Bearer", nil, "mail"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	options := storage.UserStorageMigrationOptions{SourcePath: path, DestinationPath: filepath.Join(root, "owned.db"),
		ValidateSource: func(ctx context.Context, db *storage.DB) error { return validateMigrationSourceKey(ctx, db, key) },
		ImportCredentials: func(ctx context.Context, tx *sql.Tx) error {
			return mailauth.ImportUserStorageMigrationCredentials(ctx, tx, key)
		},
		VerifyCredentials: func(ctx context.Context, tx *sql.Tx) error {
			return mailauth.VerifyUserStorageMigrationCredentials(ctx, tx, key)
		}}
	return options, before, key
}

func TestStorageMigrationCommandPublishesThroughActualEntryPointWithoutStartingServer(t *testing.T) {
	options, before, key := migrationCommandFixture(t)
	unused := filepath.Join(t.TempDir(), "unused.db")
	t.Setenv("GOFER_DB_PATH", unused)
	var stdout, stderr bytes.Buffer
	code := runApplication(t.Context(), []string{"storage", "migrate", "--db", options.SourcePath, "--to", options.DestinationPath}, &stdout, &stderr, func() { t.Error("migration started the server") })
	if code != 0 || stderr.Len() != 0 {
		t.Fatal("migration command failed", code, stderr.String())
	}
	var layout storage.UserStorageLayout
	if err := json.Unmarshal(stdout.Bytes(), &layout); err != nil || layout.OwnersAtMigration != 2 || layout.CentralPath != options.DestinationPath {
		t.Fatal("wrong migration result", err, layout)
	}
	if _, err := storage.LoadUserStorageLayout(t.Context(), layout.CentralPath); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-password", "private-access", "private-refresh", "private-mail@example.com", hex.EncodeToString(key)} {
		if strings.Contains(stdout.String(), secret) || strings.Contains(stderr.String(), secret) {
			t.Fatal("command exposed private contents")
		}
	}
	after, err := os.ReadFile(options.SourcePath)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("command changed original source", err)
	}
	savedKey, err := os.ReadFile(filepath.Join(filepath.Dir(options.SourcePath), "secret.key"))
	if err != nil || !bytes.Equal(savedKey, key) {
		t.Fatal("command replaced application key", err)
	}
	for _, path := range []string{unused, filepath.Join(filepath.Dir(options.SourcePath), "vapid_private.key"), filepath.Join(filepath.Dir(options.SourcePath), "vapid_public.key")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("command initialized runtime resources", path, err)
		}
	}
}

func TestStorageMigrationCommandRequiresExplicitRetryAndRetainsFailedCopies(t *testing.T) {
	for _, checkpoint := range []string{"preparing", "verified", "published"} {
		t.Run(checkpoint, func(t *testing.T) {
			options, before, _ := migrationCommandFixture(t)
			original := options.ImportCredentials
			if checkpoint == "preparing" {
				options.ImportCredentials = func(ctx context.Context, tx *sql.Tx) error {
					if err := original(ctx, tx); err != nil {
						return err
					}
					return errors.New("disposable interruption")
				}
			}
			stage, err := storage.StageUserStorageMigration(t.Context(), options)
			if (err != nil) != (checkpoint == "preparing") {
				t.Fatal("fixture checkpoint", err)
			}
			if checkpoint == "published" {
				if _, err := storage.PublishUserStorageMigration(t.Context(), options); err != nil {
					t.Fatal(err)
				}
			}
			oldPath := filepath.Join(stage.Directory, "central.db")
			old, oldErr := os.ReadFile(oldPath)
			args := []string{"storage", "migrate", "--db", options.SourcePath, "--to", options.DestinationPath}
			var stdout, stderr bytes.Buffer
			serve := func() { t.Error("retry started runtime") }
			if code := runApplication(t.Context(), args, &stdout, &stderr, serve); code != 1 || stdout.Len() != 0 {
				t.Fatal("recorded work reused without retry", code, stderr.String())
			}
			stdout.Reset()
			stderr.Reset()
			if code := runApplication(t.Context(), append(args, "--retry"), &stdout, &stderr, serve); code != 0 || stderr.Len() != 0 {
				t.Fatal("retry failed", code, stderr.String())
			}
			if _, err := storage.LoadUserStorageLayout(t.Context(), options.DestinationPath); err != nil {
				t.Fatal(err)
			}
			if checkpoint == "preparing" {
				if oldErr != nil {
					t.Fatal(oldErr)
				}
				retained, err := os.ReadFile(oldPath)
				if err != nil || !bytes.Equal(old, retained) {
					t.Fatal("failed preparation overwritten", err)
				}
			}
			after, err := os.ReadFile(options.SourcePath)
			if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatal("retry changed source", err)
			}
		})
	}
}

func TestStorageMigrationCommandRefusesInvalidArgumentsKeysAndRunningSource(t *testing.T) {
	for _, args := range [][]string{{"storage", "migrate"}, {"storage", "migrate", "--unknown"}, {"storage", "migrate", "--to", "x", "unexpected"}, {"storage", "migrate", "--db", "", "--to", "x"}} {
		var stdout, stderr bytes.Buffer
		if code := runApplication(t.Context(), args, &stdout, &stderr, func() { t.Error("invalid command started runtime") }); code != 2 || stdout.Len() != 0 {
			t.Fatal("invalid migration arguments", args, code)
		}
	}
	for _, failure := range []string{"missing-key", "wrong-key", "running-source", "running-destination"} {
		t.Run(failure, func(t *testing.T) {
			options, _, _ := migrationCommandFixture(t)
			var lock *runtimeguard.Lock
			switch failure {
			case "missing-key":
				if err := os.Remove(filepath.Join(filepath.Dir(options.SourcePath), "secret.key")); err != nil {
					t.Fatal(err)
				}
			case "wrong-key":
				t.Setenv("GOFER_SECRET_KEY", strings.Repeat("78", 32))
			case "running-source", "running-destination":
				path := options.SourcePath
				if failure == "running-destination" {
					path = options.DestinationPath
				}
				var err error
				lock, err = runtimeguard.Acquire(path)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			}
			var stdout, stderr bytes.Buffer
			if code := runApplication(t.Context(), []string{"storage", "migrate", "--db", options.SourcePath, "--to", options.DestinationPath}, &stdout, &stderr, func() { t.Error("failed command started runtime") }); code != 1 || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatal("migration preflight accepted", failure, code)
			}
			if _, err := os.Lstat(options.DestinationPath + ".staging"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("preflight created private copies", err)
			}
			if failure == "missing-key" {
				if _, err := os.Lstat(filepath.Join(filepath.Dir(options.SourcePath), "secret.key")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("missing key generated", err)
				}
			}
		})
	}
}
