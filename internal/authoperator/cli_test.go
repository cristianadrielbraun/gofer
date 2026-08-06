package authoperator

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func createOperatorTestDatabase(t *testing.T) string {
	t.Helper()
	databasePath := filepath.Join(t.TempDir(), "gofer.db")
	db, err := storage.New(databasePath)
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	ownerID := "owner\x1b[31m"
	if _, err := db.Write().Exec(`
		INSERT INTO users (
			id, email, email_normalized, username, username_normalized, name,
			status, auth_version, is_admin
		) VALUES
			(?, 'Owner@Example.com', 'owner@example.com', 'Owner', 'owner', 'Owner', 'active', 3, 1),
			('disabled-user', 'disabled@example.com', 'disabled@example.com', NULL, NULL, 'Disabled', 'disabled', 2, 0)`,
		ownerID,
	); err != nil {
		db.Close()
		t.Fatalf("insert operator users: %v", err)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO auth_system_state (id, initialized, owner_user_id, initialized_at, cutover_version)
		VALUES (1, 1, ?, CURRENT_TIMESTAMP, 1)`, ownerID); err != nil {
		db.Close()
		t.Fatalf("insert auth system state: %v", err)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO password_credentials (user_id, password_hash)
		VALUES (?, 'do-not-print-password-hash')`, ownerID); err != nil {
		db.Close()
		t.Fatalf("insert password fixture: %v", err)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO user_enrollment_tokens (
			id, user_id, token_hash, purpose, expires_at
		) VALUES ('private-token-id', 'disabled-user', 'do-not-print-token-hash',
		          'credential_reset', datetime('now', '+1 hour'))`); err != nil {
		db.Close()
		t.Fatalf("insert token fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close operator test database: %v", err)
	}
	return databasePath
}

func TestStatusReportsInitializationAndActiveAdministratorsWithoutSecretsOrWrites(t *testing.T) {
	databasePath := createOperatorTestDatabase(t)
	before, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), []string{"status"}, databasePath, &stdout, &stderr); code != 0 {
		t.Fatalf("Run(status) code = %d stderr=%q", code, stderr.String())
	}
	expected := "schema_version: 80\n" +
		"authentication_initialized: true\n" +
		"owner_user_id: \"owner\\x1b[31m\"\n" +
		"active_administrators: 1\n"
	if stdout.String() != expected || stderr.Len() != 0 {
		t.Fatalf("status output = stdout:%q stderr:%q", stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "do-not-print") || strings.ContainsRune(stdout.String(), '\x1b') {
		t.Fatal("status output exposed a secret or unescaped terminal control")
	}
	after, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("read-only status command changed the database file")
	}
}

func TestUsersListIsDeterministicScopedAndTerminalSafe(t *testing.T) {
	databasePath := createOperatorTestDatabase(t)
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), []string{"users", "list"}, databasePath, &stdout, &stderr); code != 0 {
		t.Fatalf("Run(users list) code = %d stderr=%q", code, stderr.String())
	}
	output := stdout.String()
	for _, required := range []string{
		"ID", "USERNAME", "EMAIL", "STATUS", "ROLE",
		`"disabled-user"`, `"disabled@example.com"`, "disabled", "user",
		`"owner\x1b[31m"`, `"Owner"`, `"Owner@Example.com"`, "active", "administrator",
	} {
		if !strings.Contains(output, required) {
			t.Fatalf("users list missing %q: %q", required, output)
		}
	}
	if strings.Index(output, `"disabled-user"`) > strings.Index(output, `"owner\x1b[31m"`) {
		t.Fatalf("users list is not deterministically sorted: %q", output)
	}
	if strings.Contains(output, "do-not-print") || strings.Contains(output, "private-token-id") || strings.ContainsRune(output, '\x1b') || stderr.Len() != 0 {
		t.Fatalf("users list exposed private data or terminal controls: stdout:%q stderr:%q", output, stderr.String())
	}
}

func TestCLIHelpAndUsageDoNotOpenOrCreateDatabase(t *testing.T) {
	missingPath := filepath.Join(t.TempDir(), "missing", "gofer.db")
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout bool
	}{
		{name: "help", args: []string{"--help"}, wantCode: 0, wantStdout: true},
		{name: "missing command", wantCode: 2},
		{name: "unknown command", args: []string{"recover"}, wantCode: 2},
		{name: "extra argument", args: []string{"status", "extra"}, wantCode: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run(t.Context(), test.args, missingPath, &stdout, &stderr); code != test.wantCode {
				t.Fatalf("Run(%v) code = %d, want %d", test.args, code, test.wantCode)
			}
			if test.wantStdout != (stdout.Len() > 0) || !strings.Contains(stdout.String()+stderr.String(), "gofer auth status") {
				t.Fatalf("Run(%v) output = stdout:%q stderr:%q", test.args, stdout.String(), stderr.String())
			}
		})
	}
	if _, err := os.Stat(filepath.Dir(missingPath)); !os.IsNotExist(err) {
		t.Fatalf("help or usage created database directory: %v", err)
	}
}

func TestCLIReportsMissingDatabaseWithoutCreatingIt(t *testing.T) {
	missingDirectory := filepath.Join(t.TempDir(), "missing")
	missingPath := filepath.Join(missingDirectory, "gofer.db")
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), []string{"status"}, missingPath, &stdout, &stderr); code != 1 {
		t.Fatalf("Run(missing status) code = %d", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "auth status: open database") {
		t.Fatalf("missing status output = stdout:%q stderr:%q", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(missingDirectory); !os.IsNotExist(err) {
		t.Fatalf("status created missing database directory: %v", err)
	}
}
