package server

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

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
