package config

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func davAccountTestRequest(username string) *models.CreateAccountRequest {
	return &models.CreateAccountRequest{Provider: providers.ProviderDAV, DisplayName: "Home server",
		Username: username, Password: username + "-dav-password",
		// Whatever mail settings come along are dropped.
		EmailAddress: "ignored@example.com", IMAPHost: "imap.example.com", SMTPHost: "smtp.example.com"}
}

// Accounts with no mail have no address, so several fit side by side, and
// they sign in to their server with the account's own login.
func TestDAVAccountsHaveNoMailboxAndUseTheirLogin(t *testing.T) {
	_, _, accounts := newUserAccountTestStore(t)
	first, err := accounts.CreateAccount(t.Context(), "alice", davAccountTestRequest("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := accounts.CreateAccount(t.Context(), "alice", davAccountTestRequest("alice-work")); err != nil {
		t.Fatal("second account with no mail refused:", err)
	}
	snapshot, err := accounts.SnapshotServices(t.Context(), "alice", first.ID)
	if err != nil {
		t.Fatal(err)
	}
	identity := snapshot.Identity()
	if identity.Provider != providers.ProviderDAV || identity.EmailAddress != "" || identity.IMAPHost != "" || identity.SMTPHost != "" || identity.Username != "alice" {
		t.Fatalf("identity = %+v", identity)
	}
	if snapshot.data.EmailSyncEnabled || snapshot.SendsEmail() {
		t.Fatal("an account with no mail has mail on")
	}
	if password, err := snapshot.CalDAVPassword("", true); err != nil || password != "alice-dav-password" {
		t.Fatalf("CalDAV login = %q, %v", password, err)
	}
	if password, err := snapshot.ContactSyncPassword(""); err != nil || password != "alice-dav-password" {
		t.Fatalf("CardDAV login = %q, %v", password, err)
	}
}

// Adding mail makes the account a mail account, while its calendars and
// contacts keep the login they signed in with.
func TestAddMailKeepsTheDAVLogin(t *testing.T) {
	_, _, accounts := newUserAccountTestStore(t)
	account, err := accounts.CreateAccount(t.Context(), "alice", davAccountTestRequest("alice"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := accounts.SnapshotServices(t.Context(), "alice", account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveCalDAVServices(t.Context(), snapshot, "https://dav.example.com/", "", "", true); err != nil {
		t.Fatal(err)
	}
	snapshot, err = accounts.SnapshotServices(t.Context(), "alice", account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.SaveContactServices(t.Context(), snapshot, models.ContactSyncConfig{Provider: providers.ProviderCardDAV, Enabled: true,
		BaseURL: "https://dav.example.com/", AddressBooks: []models.ContactAddressBook{{URL: "https://dav.example.com/alice/contacts/"}}}, ""); err != nil {
		t.Fatal(err)
	}

	mail := secureAccountStoreTestRequest("alice@example.com")
	mail.Password = "mail-password"
	if err := accounts.AddMail(t.Context(), "alice", account.ID, mail); err != nil {
		t.Fatal(err)
	}
	snapshot, err = accounts.SnapshotServices(t.Context(), "alice", account.ID)
	if err != nil {
		t.Fatal(err)
	}
	identity := snapshot.Identity()
	if identity.Provider != providers.ProviderIMAP || identity.EmailAddress != "alice@example.com" || identity.IMAPHost != "imap.example.com" || !snapshot.data.EmailSyncEnabled {
		t.Fatalf("after adding mail, identity = %+v", identity)
	}
	if password, err := snapshot.MailboxPassword(); err != nil || password != "mail-password" {
		t.Fatalf("mail login = %q, %v", password, err)
	}
	base, username, useAccount := snapshot.CalDAVSettings()
	if base != "https://dav.example.com/" || username != "alice" || useAccount {
		t.Fatalf("CalDAV settings = %q %q %v", base, username, useAccount)
	}
	if password, err := snapshot.CalDAVPassword("", useAccount); err != nil || password != "alice-dav-password" {
		t.Fatalf("CalDAV login after adding mail = %q, %v", password, err)
	}
	if cfg := snapshot.ContactConfig(); cfg.Username != "alice" {
		t.Fatalf("CardDAV username after adding mail = %q", cfg.Username)
	}
	if password, err := snapshot.ContactSyncPassword(""); err != nil || password != "alice-dav-password" {
		t.Fatalf("CardDAV login after adding mail = %q, %v", password, err)
	}

	if err := accounts.AddMail(t.Context(), "alice", account.ID, secureAccountStoreTestRequest("other@example.com")); !errors.Is(err, ErrAccountHasMail) {
		t.Fatal("added mail twice:", err)
	}
}

func TestAddMailRefusesAMailboxAlreadyConnected(t *testing.T) {
	_, _, accounts := newUserAccountTestStore(t)
	if _, err := accounts.CreateAccount(t.Context(), "alice", secureAccountStoreTestRequest("alice@example.com")); err != nil {
		t.Fatal(err)
	}
	account, err := accounts.CreateAccount(t.Context(), "alice", davAccountTestRequest("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.AddMail(t.Context(), "alice", account.ID, secureAccountStoreTestRequest("ALICE@example.com")); !errors.Is(err, ErrMailboxExists) {
		t.Fatal("same mailbox connected twice:", err)
	}
	if err := accounts.WithAccountForUser(t.Context(), "alice", account.ID, func(_ *AccountStore, db *storage.DB) error {
		var provider string
		if err := db.Read().QueryRow(`SELECT provider FROM accounts WHERE id = ?`, account.ID).Scan(&provider); err != nil {
			return err
		}
		if provider != providers.ProviderDAV {
			t.Fatalf("refused account became %q", provider)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// In open and personal mode an account with no mail gets its own ID, and
// adding a mailbox to it keeps the login its calendars sign in with.
func TestSharedStoreAddMail(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('alice','alice','alice')`); err != nil {
		t.Fatal(err)
	}
	accounts, err := NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := accounts.CreateAccount(t.Context(), "alice", davAccountTestRequest("alice"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := accounts.CreateAccount(t.Context(), "alice", davAccountTestRequest("alice-work"))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("both accounts are %q", first.ID)
	}
	if err := accounts.SaveCalDAVConfig(t.Context(), "alice", first.ID, "https://dav.example.com/", "", "", true); err != nil {
		t.Fatal(err)
	}
	mailbox := &models.CreateAccountRequest{Provider: providers.ProviderIMAP, EmailAddress: "alice@example.com", AuthMethod: "plain",
		IMAPHost: "imap.example.com", IMAPPort: 993, SMTPHost: "smtp.example.com", SMTPPort: 465, Username: "alice@example.com", Password: "mail-pass"}
	if err := accounts.AddMail(t.Context(), "alice", first.ID, mailbox); err != nil {
		t.Fatal(err)
	}
	if err := accounts.AddMail(t.Context(), "alice", first.ID, mailbox); !errors.Is(err, ErrAccountHasMail) {
		t.Fatalf("adding mail twice: %v", err)
	}
	data, err := accounts.GetEditData(t.Context(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if data.Provider != providers.ProviderIMAP || data.EmailAddress != "alice@example.com" || !data.EmailSyncEnabled {
		t.Fatalf("account = %+v", data)
	}
	if cal := data.CalendarSync; cal.CalDAVUseAccountCredentials || cal.CalDAVUsername != "alice" {
		t.Fatalf("calendars = %+v", cal)
	}
	if password, err := accounts.CalDAVPassword(t.Context(), "alice", first.ID); err != nil || password != "alice-dav-password" {
		t.Fatalf("calendars sign in with %q, %v", password, err)
	}
}
