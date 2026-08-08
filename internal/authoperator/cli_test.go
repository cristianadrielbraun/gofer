package authoperator

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/runtimeguard"
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
		"setup_token_configured: false\n" +
		"setup_token_expires_at: -\n" +
		"setup_token_attempts: 0\n" +
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

func TestRecoverRequiresExactConfirmationBeforeOpeningDatabase(t *testing.T) {
	missingDirectory := filepath.Join(t.TempDir(), "missing")
	databasePath := filepath.Join(missingDirectory, "gofer.db")
	tests := [][]string{
		{"recover", "--user", "target", "--confirm", "other"},
		{"recover", "--user", "target", "--user", "target"},
		{"recover", "--user", "target", "--unknown", "target"},
		{"recover", "--user", "target"},
	}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		if code := Run(t.Context(), args, databasePath, &stdout, &stderr); code != 2 {
			t.Fatalf("Run(%v) code = %d, stderr=%q", args, code, stderr.String())
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "gofer auth recover") {
			t.Fatalf("Run(%v) output = stdout:%q stderr:%q", args, stdout.String(), stderr.String())
		}
	}
	if _, err := os.Stat(missingDirectory); !os.IsNotExist(err) {
		t.Fatalf("invalid recovery created database directory: %v", err)
	}
}

func TestRecoverCommitsResetAndPrintsRawTokenExactlyOnce(t *testing.T) {
	databasePath := createOperatorTestDatabase(t)
	db, err := storage.OpenExisting(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method,
			assurance_level, authenticated_at, last_used_at,
			idle_expires_at, absolute_expires_at, created_at
		) VALUES (
			'disabled-session', 'disabled-user', ?, 2, 'password',
			'single_factor', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP,
			datetime('now', '+1 hour'), datetime('now', '+1 day'), CURRENT_TIMESTAMP
		)`, strings.Repeat("a", 64)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), []string{"recover", "--user", "disabled-user", "--confirm", "disabled-user"}, databasePath, &stdout, &stderr); code != 0 {
		t.Fatalf("Run(recover) code = %d stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("Run(recover) stderr = %q", stderr.String())
	}
	output := stdout.String()
	for _, required := range []string{
		`user_id: "disabled-user"`, "token_purpose: credential_reset", "revoked_sessions: 1", "replaced_reset_tokens: 1", "reset_token: ",
	} {
		if !strings.Contains(output, required) {
			t.Fatalf("recovery output missing %q: %q", required, output)
		}
	}
	var rawToken string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "reset_token: ") {
			rawToken = strings.TrimPrefix(line, "reset_token: ")
		}
	}
	if rawToken == "" || strings.Count(output, rawToken) != 1 {
		t.Fatalf("raw reset token was not printed exactly once: %q", output)
	}
	if strings.Contains(output, "do-not-print-token-hash") || strings.Contains(output, "do-not-print-password-hash") {
		t.Fatalf("recovery output exposed stored secret material: %q", output)
	}

	reopened, err := storage.OpenReadOnly(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var status string
	var authVersion int64
	if err := reopened.Read().QueryRow(`SELECT status, auth_version FROM users WHERE id = 'disabled-user'`).Scan(&status, &authVersion); err != nil {
		t.Fatal(err)
	}
	if status != "disabled" || authVersion != 3 {
		t.Fatalf("recovered user state = status:%q version:%d", status, authVersion)
	}
	var storedHash string
	if err := reopened.Read().QueryRow(`
		SELECT token_hash FROM user_enrollment_tokens
		WHERE user_id = 'disabled-user' AND purpose = 'credential_reset' AND revoked_at IS NULL`,
	).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	expectedHash := sha256.Sum256([]byte(rawToken))
	if storedHash != fmt.Sprintf("%x", expectedHash) || storedHash == rawToken {
		t.Fatalf("persisted recovery token hash = %q", storedHash)
	}
	var activeSessions int
	if err := reopened.Read().QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = 'disabled-user' AND revoked_at IS NULL`).Scan(&activeSessions); err != nil || activeSessions != 0 {
		t.Fatalf("active sessions after recovery = %d, %v", activeSessions, err)
	}
}

func TestRecoverRefusesToRunWhileRuntimeLockIsHeld(t *testing.T) {
	databasePath := createOperatorTestDatabase(t)
	lock, err := runtimeguard.Acquire(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), []string{"recover", "--user", "disabled-user", "--confirm", "disabled-user"}, databasePath, &stdout, &stderr); code != 1 {
		t.Fatalf("Run(recover while locked) code = %d", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "stop the server") {
		t.Fatalf("locked recovery output = stdout:%q stderr:%q", stdout.String(), stderr.String())
	}
	db, err := storage.OpenReadOnly(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var authVersion int64
	if err := db.Read().QueryRow(`SELECT auth_version FROM users WHERE id = 'disabled-user'`).Scan(&authVersion); err != nil || authVersion != 2 {
		t.Fatalf("locked recovery changed auth version = %d, %v", authVersion, err)
	}
}

func TestSessionRevocationRequiresExactConfirmationBeforeOpeningDatabase(t *testing.T) {
	missingDirectory := filepath.Join(t.TempDir(), "missing")
	databasePath := filepath.Join(missingDirectory, "gofer.db")
	tests := [][]string{
		{"sessions", "revoke", "--user", "target", "--confirm", "other"},
		{"sessions", "revoke", "--user", "target", "--user", "target"},
		{"sessions", "revoke", "--unknown", "target", "--confirm", "target"},
		{"sessions", "revoke", "--user", "target"},
	}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		if code := Run(t.Context(), args, databasePath, &stdout, &stderr); code != 2 {
			t.Fatalf("Run(%v) code = %d, stderr=%q", args, code, stderr.String())
		}
		if stdout.Len() != 0 || !strings.Contains(stderr.String(), "gofer auth sessions revoke") {
			t.Fatalf("Run(%v) output = stdout:%q stderr:%q", args, stdout.String(), stderr.String())
		}
	}
	if _, err := os.Stat(missingDirectory); !os.IsNotExist(err) {
		t.Fatalf("invalid session revocation created database directory: %v", err)
	}
}

func TestSessionRevocationCommitsExactTargetAndPrintsSecretFreeCount(t *testing.T) {
	databasePath := createOperatorTestDatabase(t)
	db, err := storage.OpenExisting(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method,
			assurance_level, authenticated_at, last_used_at,
			idle_expires_at, absolute_expires_at, created_at
		) VALUES (
			'private-session-id', 'disabled-user', ?, 2, 'password',
			'single_factor', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP,
			datetime('now', '+1 hour'), datetime('now', '+1 day'), CURRENT_TIMESTAMP
		)`, strings.Repeat("b", 64)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	args := []string{"sessions", "revoke", "--user", "disabled-user", "--confirm", "disabled-user"}
	if code := Run(t.Context(), args, databasePath, &stdout, &stderr); code != 0 {
		t.Fatalf("Run(sessions revoke) code = %d stderr=%q", code, stderr.String())
	}
	expected := "user_id: \"disabled-user\"\nrevoked_sessions: 1\n"
	if stdout.String() != expected || stderr.Len() != 0 {
		t.Fatalf("session revocation output = stdout:%q stderr:%q", stdout.String(), stderr.String())
	}
	for _, secret := range []string{"private-session-id", strings.Repeat("b", 64), "private-token-id", "do-not-print-token-hash", "do-not-print-password-hash", "reset_token"} {
		if strings.Contains(stdout.String(), secret) {
			t.Fatalf("session revocation output exposed %q: %q", secret, stdout.String())
		}
	}

	reopened, err := storage.OpenReadOnly(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var status string
	var authVersion int64
	var activeSessions int
	var activeResetTokens int
	if err := reopened.Read().QueryRow(`SELECT status, auth_version FROM users WHERE id = 'disabled-user'`).Scan(&status, &authVersion); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Read().QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = 'disabled-user' AND revoked_at IS NULL`).Scan(&activeSessions); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Read().QueryRow(`
		SELECT COUNT(*) FROM user_enrollment_tokens
		WHERE user_id = 'disabled-user' AND revoked_at IS NULL AND used_at IS NULL`,
	).Scan(&activeResetTokens); err != nil {
		t.Fatal(err)
	}
	if status != "disabled" || authVersion != 2 || activeSessions != 0 || activeResetTokens != 1 {
		t.Fatalf("persisted session revocation = status:%q version:%d sessions:%d resetTokens:%d", status, authVersion, activeSessions, activeResetTokens)
	}
}

func TestSessionRevocationRefusesToRunWhileRuntimeLockIsHeld(t *testing.T) {
	databasePath := createOperatorTestDatabase(t)
	lock, err := runtimeguard.Acquire(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	var stdout, stderr bytes.Buffer
	args := []string{"sessions", "revoke", "--user", "disabled-user", "--confirm", "disabled-user"}
	if code := Run(t.Context(), args, databasePath, &stdout, &stderr); code != 1 {
		t.Fatalf("Run(sessions revoke while locked) code = %d", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "stop the server") {
		t.Fatalf("locked session revocation output = stdout:%q stderr:%q", stdout.String(), stderr.String())
	}
}

func createUninitializedOperatorTestDatabase(t *testing.T) string {
	t.Helper()
	databasePath := filepath.Join(t.TempDir(), "gofer.db")
	db, err := storage.New(databasePath)
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO auth_system_state (
			id, initialized, setup_token_hash, setup_expires_at, setup_attempts, cutover_version
		) VALUES (1, 0, ?, datetime('now', '-1 minute'), 6, 0)`, strings.Repeat("d", 64)); err != nil {
		db.Close()
		t.Fatalf("insert uninitialized setup state: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close uninitialized operator database: %v", err)
	}
	return databasePath
}

func TestSetupTokenRotationPrintsOneReplacementAndPersistsOnlyItsHash(t *testing.T) {
	databasePath := createUninitializedOperatorTestDatabase(t)
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), []string{"setup-token", "rotate"}, databasePath, &stdout, &stderr); code != 0 {
		t.Fatalf("Run(setup-token rotate) code = %d stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 || !strings.Contains(stdout.String(), "expires_at: ") || !strings.Contains(stdout.String(), "setup_token: ") {
		t.Fatalf("setup token rotation output = stdout:%q stderr:%q", stdout.String(), stderr.String())
	}
	var rawToken string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(line, "setup_token: ") {
			rawToken = strings.TrimPrefix(line, "setup_token: ")
		}
	}
	if rawToken == "" || strings.Count(stdout.String(), rawToken) != 1 {
		t.Fatalf("replacement setup token was not printed exactly once: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), strings.Repeat("d", 64)) {
		t.Fatalf("setup token rotation exposed the replaced hash: %q", stdout.String())
	}

	reopened, err := storage.OpenReadOnly(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var storedHash string
	var attempts int64
	var rotatedAt sql.NullTime
	if err := reopened.Read().QueryRow(`
		SELECT setup_token_hash, setup_attempts, setup_rotated_at
		FROM auth_system_state WHERE id = 1`,
	).Scan(&storedHash, &attempts, &rotatedAt); err != nil {
		t.Fatal(err)
	}
	expectedHash := sha256.Sum256([]byte(rawToken))
	if storedHash != fmt.Sprintf("%x", expectedHash) || storedHash == rawToken || attempts != 0 || !rotatedAt.Valid {
		t.Fatalf("persisted replacement = hash:%q attempts:%d rotated:%#v", storedHash, attempts, rotatedAt)
	}
	var eventType, reason, metadata string
	if err := reopened.Read().QueryRow(`
		SELECT event_type, reason, metadata_json FROM auth_events
		WHERE event_type = 'setup_token_rotated'`,
	).Scan(&eventType, &reason, &metadata); err != nil {
		t.Fatal(err)
	}
	if reason != "local_operator" || strings.Contains(metadata, rawToken) || strings.Contains(metadata, storedHash) {
		t.Fatalf("setup rotation event = type:%q reason:%q metadata:%q", eventType, reason, metadata)
	}

	var statusOut, statusErr bytes.Buffer
	if code := Run(t.Context(), []string{"status"}, databasePath, &statusOut, &statusErr); code != 0 {
		t.Fatalf("Run(status after setup rotation) code = %d stderr=%q", code, statusErr.String())
	}
	if !strings.Contains(statusOut.String(), "authentication_initialized: false\n") ||
		!strings.Contains(statusOut.String(), "setup_token_configured: true\n") ||
		!strings.Contains(statusOut.String(), "setup_token_expires_at: ") ||
		!strings.Contains(statusOut.String(), "setup_token_attempts: 0\n") ||
		strings.Contains(statusOut.String(), rawToken) || strings.Contains(statusOut.String(), storedHash) {
		t.Fatalf("status after setup rotation exposed incorrect metadata: %q", statusOut.String())
	}
}

func TestSetupTokenRotationRejectsInitializedInstanceWithoutOutput(t *testing.T) {
	databasePath := createOperatorTestDatabase(t)
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), []string{"setup-token", "rotate"}, databasePath, &stdout, &stderr); code != 1 {
		t.Fatalf("Run(setup-token rotate initialized) code = %d", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "authentication setup is already complete") || strings.Contains(stderr.String(), "setup_token: ") {
		t.Fatalf("initialized setup rotation output = stdout:%q stderr:%q", stdout.String(), stderr.String())
	}
}

func TestSetupTokenRotationRefusesToRunWhileRuntimeLockIsHeld(t *testing.T) {
	databasePath := createUninitializedOperatorTestDatabase(t)
	lock, err := runtimeguard.Acquire(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), []string{"setup-token", "rotate"}, databasePath, &stdout, &stderr); code != 1 {
		t.Fatalf("Run(setup-token rotate while locked) code = %d", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "stop the server") {
		t.Fatalf("locked setup rotation output = stdout:%q stderr:%q", stdout.String(), stderr.String())
	}
}
