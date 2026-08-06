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
		INSERT INTO users (id, email, email_normalized, name, status, auth_version, is_admin)
		VALUES ('owner', 'owner@example.com', 'owner@example.com', 'Owner', 'active', 1, 1)`); err != nil {
		db.Close()
		t.Fatalf("insert auth command owner: %v", err)
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
	dataDirectory := filepath.Dir(databasePath)
	for _, runtimeSecret := range []string{"secret.key", "vapid_private.key", "vapid_public.key"} {
		if _, err := os.Stat(filepath.Join(dataDirectory, runtimeSecret)); !os.IsNotExist(err) {
			t.Fatalf("auth command created runtime secret %q: %v", runtimeSecret, err)
		}
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
	if !strings.Contains(text, `"owner"`) || !strings.Contains(text, `"owner@example.com"`) {
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
