package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

const localProfilePassword = "correct horse battery staple 42"

// localProfileFixture builds the shared database an open or personal
// installation leaves behind: the local profile owns every mailbox.
func localProfileFixture(t *testing.T, personal bool) string {
	t.Helper()
	t.Setenv("GOFER_SECRET_KEY", "")
	root := t.TempDir()
	path := filepath.Join(root, "gofer.db")
	key := []byte("0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(filepath.Join(root, "secret.key"), key, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := storage.New(path)
	if err != nil {
		t.Fatal(err)
	}
	username := "local"
	if personal {
		username = "myself"
	}
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized,name) VALUES('default',?,?,'Local User')`, username, username); err != nil {
		t.Fatal(err)
	}
	if personal {
		hash, err := auth.HashPassword(localProfilePassword)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Write().Exec(`INSERT INTO password_credentials(user_id,password_hash,must_change,created_at,changed_at) VALUES('default',?,0,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP);
 INSERT INTO auth_system_state(id,initialized,owner_user_id,initialized_at) VALUES(1,1,'default',CURRENT_TIMESTAMP)
  ON CONFLICT(id) DO UPDATE SET initialized=1,owner_user_id='default',initialized_at=CURRENT_TIMESTAMP`, hash); err != nil {
			t.Fatal(err)
		}
	}
	accounts, err := config.NewAccountStore(db, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.CreateAccount(t.Context(), "default", &models.CreateAccountRequest{Provider: "imap", EmailAddress: "me@example.com", Username: "me@example.com", Password: "private-password", IMAPHost: "imap.fixture.invalid", IMAPPort: 993, IMAPTLSMode: "tls", SMTPHost: "smtp.fixture.invalid", SMTPPort: 465, SMTPTLSMode: "tls", AuthMethod: "plain"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// startConvertedManaged converts in place and runs managed startup's auth steps.
func startConvertedManaged(t *testing.T, path string) (*managedStorage, *auth.Manager, string) {
	t.Helper()
	s, err := openManagedStorage(t.Context(), path, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	t.Setenv("GOFER_AUTH_MODE", "managed")
	manager := auth.NewManager(auth.LoadConfig("https://gofer.example"), s.central, auth.Dependencies{BucketHashKey: s.key})
	if _, err := manager.AdoptLocalProfileForManagedSetup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := manager.ValidateRuntimeMode(t.Context()); err != nil {
		t.Fatal("converted local profile refused in managed mode", err)
	}
	var notice strings.Builder
	if err := provisionInitialSetupToken(t.Context(), manager, "", &notice); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notice.String(), "setup_token:") {
		t.Fatal("setup notice has no setup token", notice.String())
	}
	lease, err := s.stores.AcquireExisting(t.Context(), auth.LocalProfileUserID)
	if err != nil {
		t.Fatal(err)
	}
	var mailboxes int
	err = lease.DB().Read().QueryRowContext(t.Context(), `SELECT count(*) FROM accounts`).Scan(&mailboxes)
	lease.Release()
	if err != nil || mailboxes != 1 {
		t.Fatal("local profile lost its mailboxes", mailboxes, err)
	}
	return s, manager, notice.String()
}

func signIn(t *testing.T, manager *auth.Manager, username, password string) error {
	t.Helper()
	_, err := manager.AuthenticatePassword(t.Context(), auth.PasswordLoginOptions{Identifier: username, Password: password, RequiredUserType: auth.UserTypeWebmail})
	return err
}

func TestManagedStorageConvertsPersonalProfileToRegularUser(t *testing.T) {
	path := localProfileFixture(t, true)
	s, manager, notice := startConvertedManaged(t, path)
	if !strings.Contains(notice, `"myself" keeps its mailboxes`) || !strings.Contains(notice, "same credentials") || strings.Contains(notice, "reset_token:") {
		t.Fatal("personal conversion notice", notice)
	}
	state, err := manager.SetupState(t.Context())
	if err != nil || state.Initialized || state.OwnerUserID != "" {
		t.Fatal("managed setup not reopened", state, err)
	}
	if again, err := manager.AdoptLocalProfileForManagedSetup(t.Context()); err != nil || again {
		t.Fatal("adoption repeated", again, err)
	}
	if err := signIn(t, manager, "myself", localProfilePassword); err != nil {
		t.Fatal("profile lost its password", err)
	}
	// The original personal database is kept unchanged for rollback.
	original, err := sql.Open("sqlite", s.layout.SourcePath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	var initialized int
	var owner string
	if err := original.QueryRow(`SELECT initialized,owner_user_id FROM auth_system_state WHERE id=1`).Scan(&initialized, &owner); err != nil || initialized != 1 || owner != "default" {
		t.Fatal("retired original changed", initialized, owner, err)
	}
}

func TestManagedStorageConvertsOpenProfileWithPasswordLink(t *testing.T) {
	path := localProfileFixture(t, false)
	_, manager, notice := startConvertedManaged(t, path)
	if !strings.Contains(notice, `"local" keeps its mailboxes`) || !strings.Contains(notice, "no password yet") {
		t.Fatal("open conversion notice", notice)
	}
	token := regexp.MustCompile(`reset_token: (\S+)`).FindStringSubmatch(notice)
	if token == nil {
		t.Fatal("no password token", notice)
	}
	// A restart before setup replaces the link; the earlier one stops working.
	var restarted strings.Builder
	if err := provisionInitialSetupToken(t.Context(), manager, "", &restarted); err != nil {
		t.Fatal(err)
	}
	replacement := regexp.MustCompile(`reset_token: (\S+)`).FindStringSubmatch(restarted.String())
	if replacement == nil || replacement[1] == token[1] {
		t.Fatal("restart did not issue a new password token", restarted.String())
	}
	redeem := func(value string) error {
		_, err := manager.RedeemEnrollmentToken(t.Context(), auth.RedeemEnrollmentTokenOptions{Token: value, NewPassword: localProfilePassword, Purpose: auth.EnrollmentTokenPurposeCredentialReset})
		return err
	}
	if err := redeem(token[1]); err == nil {
		t.Fatal("replaced password token still works")
	}
	if err := redeem(replacement[1]); err != nil {
		t.Fatal(err)
	}
	if err := signIn(t, manager, "local", localProfilePassword); err != nil {
		t.Fatal("profile cannot sign in with its new password", err)
	}
	var after strings.Builder
	if err := provisionInitialSetupToken(t.Context(), manager, "", &after); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(after.String(), "reset_token:") || !strings.Contains(after.String(), "same credentials") {
		t.Fatal("profile with a password still offered a link", after.String())
	}
}
