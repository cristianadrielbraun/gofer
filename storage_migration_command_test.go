package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
