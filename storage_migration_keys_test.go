package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestMigrationKeyLoaderPreservesExistingSecretAndEnvironmentPrecedence(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	for _, mode := range []string{"valid-file", "missing-file", "invalid-file", "environment", "invalid-environment", "directory"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("GOFER_SECRET_KEY", "")
			root := t.TempDir()
			path := filepath.Join(root, "secret.key")
			content := key
			if mode == "invalid-file" || mode == "environment" || mode == "invalid-environment" {
				content = []byte("retain invalid original bytes")
			}
			if mode == "directory" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if mode != "missing-file" {
				if err := os.WriteFile(path, content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "environment" {
				t.Setenv("GOFER_SECRET_KEY", hex.EncodeToString(key))
			}
			if mode == "invalid-environment" {
				t.Setenv("GOFER_SECRET_KEY", "private-invalid-value")
			}
			loaded, err := loadExistingMigrationKey(filepath.Join(root, "shared.db"))
			valid := mode == "valid-file" || mode == "environment"
			if (err == nil) != valid || (valid && !bytes.Equal(loaded, key)) {
				t.Fatal("key resolution", err)
			}
			if err != nil && strings.Contains(err.Error(), "private-invalid-value") {
				t.Fatal("key value leaked")
			}
			if mode == "missing-file" {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatal("missing key generated", err)
				}
			} else if mode != "directory" {
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, content) {
					t.Fatal("existing key replaced", err)
				}
			}
		})
	}
}

func TestMigrationSourceKeyAdapterRejectsWrongKeyOnReadOnlySource(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	path := filepath.Join(t.TempDir(), "shared.db")
	db, err := storage.New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('alice','alice','alice')`); err != nil {
		t.Fatal(err)
	}
	accounts, err := config.NewAccountStore(db, key)
	if err != nil {
		t.Fatal(err)
	}
	_, err = accounts.CreateAccount(t.Context(), "alice", &models.CreateAccountRequest{
		Provider: "imap", EmailAddress: "alice@example.com", DisplayName: "Mailbox", Username: "alice@example.com", Password: "private-password",
		IMAPHost: "imap.fixture.invalid", IMAPPort: 993, IMAPTLSMode: "tls", SMTPHost: "smtp.fixture.invalid", SMTPPort: 465, SMTPTLSMode: "tls", AuthMethod: "plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source, err := storage.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if err := validateMigrationSourceKey(t.Context(), source, key); err != nil {
		t.Fatal(err)
	}
	if err := validateMigrationSourceKey(t.Context(), source, []byte(strings.Repeat("x", 32))); err == nil || strings.Contains(err.Error(), "private-password") {
		t.Fatal("wrong key accepted or password leaked", err)
	}
	options := storage.UserStorageMigrationOptions{
		SourcePath: path, DestinationPath: filepath.Join(filepath.Dir(path), "owned.db"),
		ValidateSource: func(ctx context.Context, db *storage.DB) error {
			return validateMigrationSourceKey(ctx, db, []byte(strings.Repeat("x", 32)))
		},
		ImportCredentials: func(ctx context.Context, tx *sql.Tx) error {
			return mailauth.ImportUserStorageMigrationCredentials(ctx, tx, key)
		},
	}
	if _, err := storage.StageUserStorageMigration(t.Context(), options); err == nil {
		t.Fatal("wrong key staged")
	}
	if _, err := os.Lstat(options.DestinationPath + ".staging"); !os.IsNotExist(err) {
		t.Fatal("key failure created private stage", err)
	}
	options.ValidateSource = func(ctx context.Context, db *storage.DB) error { return validateMigrationSourceKey(ctx, db, key) }
	stage, err := storage.StageUserStorageMigration(t.Context(), options)
	if err != nil || stage.Owners != 1 {
		t.Fatal("authenticated source did not stage", err)
	}
	if _, err := os.Lstat(options.DestinationPath + ".layout.json"); !os.IsNotExist(err) {
		t.Fatal("key adapter published migration", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("key validation changed source", err)
	}
}
