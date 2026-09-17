package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func createMainAuthCommandDatabase(t *testing.T) string {
	t.Helper()
	databasePath := filepath.Join(t.TempDir(), "data", "gofer.db")
	db, err := storage.New(databasePath)
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized, name, status, auth_version, is_admin, user_type)
		VALUES ('owner', 'owner', 'owner', 'Owner', 'active', 1, 1, 'management')`); err != nil {
		db.Close()
		t.Fatalf("insert auth command owner: %v", err)
	}
	if _, err := db.Write().Exec(`
		INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method,
			assurance_level, authenticated_at, last_used_at,
			idle_expires_at, absolute_expires_at, created_at
		) VALUES (
			'owner-session', 'owner', ?, 1, 'password', 'single_factor',
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, datetime('now', '+1 hour'),
			datetime('now', '+1 day'), CURRENT_TIMESTAMP
		)`, strings.Repeat("c", 64)); err != nil {
		db.Close()
		t.Fatalf("insert auth command session: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close auth command database: %v", err)
	}
	return databasePath
}

func TestRunApplicationDispatchesAuthBeforeServerStartup(t *testing.T) {
	databasePath := createMainAuthCommandDatabase(t)
	t.Setenv("GOFER_DB_PATH", databasePath)
	t.Setenv("GOFER_ADDR", "invalid-listen-address")
	t.Setenv("GOFER_BASE_URL", "://invalid-http-origin")
	t.Setenv("GOFER_SECRET_KEY", "invalid-secret-key")
	t.Setenv("GOFER_SETUP_TOKEN", "short")

	var stdout, stderr bytes.Buffer
	serverStarted := false
	exitCode := runApplication(t.Context(), []string{"auth", "status"}, &stdout, &stderr, func() {
		serverStarted = true
	})
	if exitCode != 0 || serverStarted {
		t.Fatalf("auth command dispatch = code:%d serverStarted:%t stderr:%q", exitCode, serverStarted, stderr.String())
	}
	if !strings.Contains(stdout.String(), "authentication_initialized: false") || !strings.Contains(stdout.String(), "active_administrators: 1") || stderr.Len() != 0 {
		t.Fatalf("auth status output = stdout:%q stderr:%q", stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	serverStarted = false
	exitCode = runApplication(t.Context(), []string{"auth", "setup-token", "rotate"}, &stdout, &stderr, func() {
		serverStarted = true
	})
	if exitCode != 0 || serverStarted || !strings.Contains(stdout.String(), "expires_at: ") || !strings.Contains(stdout.String(), "setup_token: ") || stderr.Len() != 0 {
		t.Fatalf("auth setup-token rotation dispatch = code:%d serverStarted:%t stdout:%q stderr:%q", exitCode, serverStarted, stdout.String(), stderr.String())
	}
	dataDirectory := filepath.Dir(databasePath)
	for _, runtimeSecret := range []string{"secret.key", "vapid_private.key", "vapid_public.key"} {
		if _, err := os.Stat(filepath.Join(dataDirectory, runtimeSecret)); !os.IsNotExist(err) {
			t.Fatalf("auth command created runtime secret %q: %v", runtimeSecret, err)
		}
	}

	stdout.Reset()
	stderr.Reset()
	serverStarted = false
	exitCode = runApplication(t.Context(), []string{"auth", "sessions", "revoke", "--user", "owner", "--confirm", "owner"}, &stdout, &stderr, func() {
		serverStarted = true
	})
	if exitCode != 0 || serverStarted || stdout.String() != "user_id: \"owner\"\nrevoked_sessions: 1\n" || stderr.Len() != 0 {
		t.Fatalf("auth session revocation dispatch = code:%d serverStarted:%t stdout:%q stderr:%q", exitCode, serverStarted, stdout.String(), stderr.String())
	}
}

func TestProvisionInitialSetupTokenPrintsGeneratedSecretOnlyOnce(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "gofer.db")
	db, err := storage.New(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager := auth.NewManager(&auth.Config{Enabled: true, BaseURL: "https://gofer.example"}, db)

	var console bytes.Buffer
	if err := provisionInitialSetupToken(t.Context(), manager, "", &console); err != nil {
		t.Fatal(err)
	}
	output := console.String()
	if !strings.Contains(output, "shown once") || !strings.Contains(output, "Expires: ") || !strings.Contains(output, "setup_token: ") || !strings.Contains(output, "Open: https://gofer.example/setup") || !strings.Contains(output, "server local time") {
		t.Fatalf("initial setup console output = %q", output)
	}
	var rawToken string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "setup_token: ") {
			rawToken = strings.TrimPrefix(strings.TrimSpace(line), "setup_token: ")
		}
	}
	if rawToken == "" || strings.Count(output, rawToken) != 1 {
		t.Fatalf("generated setup token was not printed exactly once: %q", output)
	}
	var storedHash string
	if err := db.Read().QueryRow(`SELECT setup_token_hash FROM auth_system_state WHERE id = 1`).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == rawToken || len(storedHash) != 64 {
		t.Fatalf("stored setup token is not hash-only: %q", storedHash)
	}

	console.Reset()
	if err := provisionInitialSetupToken(t.Context(), manager, "", &console); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(console.String(), rawToken) || strings.Contains(console.String(), "setup_token:") || !strings.Contains(console.String(), "auth setup-token rotate") {
		t.Fatalf("incorrect restart notice: %q", console.String())
	}
	if _, err := db.Write().Exec(`UPDATE auth_system_state SET initialized = 1 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	console.Reset()
	if err := provisionInitialSetupToken(t.Context(), manager, "", &console); err != nil {
		t.Fatal(err)
	}
	if console.Len() != 0 {
		t.Fatal("completed setup still displayed a setup notice")
	}
	manager.Config().Enabled = false
	if err := provisionInitialSetupToken(t.Context(), manager, "", &console); err != nil || console.Len() != 0 {
		t.Fatal("open mode displayed setup notice")
	}
}

func TestProvisionInitialSetupTokenDoesNotEchoOperatorSuppliedSecret(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "gofer.db")
	db, err := storage.New(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager := auth.NewManager(&auth.Config{Enabled: true, BaseURL: "https://gofer.example"}, db)
	configuredToken := strings.Repeat("operator-supplied-secret-", 2)

	var console bytes.Buffer
	if err := provisionInitialSetupToken(t.Context(), manager, configuredToken, &console); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(console.String(), configuredToken) || !strings.Contains(console.String(), "GOFER_SETUP_TOKEN") {
		t.Fatalf("operator-supplied setup token was echoed: %q", console.String())
	}
}

func TestRunApplicationUsesServerPathForNonAuthArguments(t *testing.T) {
	serverStarts := 0
	if code := runApplication(t.Context(), nil, &bytes.Buffer{}, &bytes.Buffer{}, func() { serverStarts++ }); code != 0 || serverStarts != 1 {
		t.Fatalf("server dispatch = code:%d starts:%d", code, serverStarts)
	}
	serverStarts = 0
	if code := runApplication(t.Context(), []string{"unexpected"}, &bytes.Buffer{}, &bytes.Buffer{}, func() { serverStarts++ }); code != 0 || serverStarts != 1 {
		t.Fatalf("non-auth dispatch = code:%d starts:%d", code, serverStarts)
	}
}

func TestAuthCommandSubprocessRemainsIsolatedFromServerRuntime(t *testing.T) {
	databasePath := createMainAuthCommandDatabase(t)
	command := exec.Command(os.Args[0], "-test.run=^TestAuthCommandHelperProcess$", "--", "auth", "users", "list")
	command.Dir = t.TempDir()
	command.Env = append(os.Environ(),
		"GOFER_AUTH_COMMAND_HELPER=1",
		"GOFER_DB_PATH="+databasePath,
		"GOFER_ADDR=invalid-listen-address",
		"GOFER_BASE_URL=://invalid-http-origin",
		"GOFER_SECRET_KEY=invalid-secret-key",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("auth command subprocess error = %v output=%q", err, output)
	}
	text := string(output)
	if !strings.Contains(text, `"owner"`) || !strings.Contains(text, "USERNAME") {
		t.Fatalf("auth command subprocess output = %q", text)
	}
	for _, forbidden := range []string{"server-started", "boot:", "Gofer running on", "listening on"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("auth command subprocess entered server runtime (%q): %q", forbidden, text)
		}
	}
}

func TestAuthCommandHelperProcess(t *testing.T) {
	if os.Getenv("GOFER_AUTH_COMMAND_HELPER") != "1" {
		return
	}
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 {
		fmt.Fprintln(os.Stderr, "missing helper argument separator")
		os.Exit(98)
	}
	exitCode := runApplication(context.Background(), os.Args[separator+1:], os.Stdout, os.Stderr, func() {
		fmt.Fprintln(os.Stderr, "server-started")
	})
	os.Exit(exitCode)
}
