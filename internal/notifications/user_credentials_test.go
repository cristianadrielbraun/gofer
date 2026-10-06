package notifications

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/handler"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

func TestUserMailboxCredentialLifecycleCleanup(t *testing.T) {
	f := newUserStorageFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	blobs := store.NewBlobStore(t.TempDir())
	worker, err := mail.NewUserIMAP(ctx, f.accountStore, blobs, f.events)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	credentials, err := mailauth.NewUserCredentials(ctx, nil, f.routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		cancel()
		worker.Wait()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); worker.Wait(); credentials.Wait() })
	if err := f.base.RegisterUserStorageRoutes(ctx, http.NewServeMux(), f.routing, handler.UserStorageOptions{Accounts: f.accountStore, IMAP: worker, Credentials: credentials}); err != nil {
		t.Fatal(err)
	}
	if err := worker.SetCredentials(credentials); err == nil {
		t.Fatal("registrar did not install credential lifecycle hook")
	}
	ids := make(map[string]string)
	paths := make(map[string]string)
	for _, owner := range []string{"alice", "bob"} {
		account, err := f.accountStore.CreateAccount(t.Context(), owner, providers.GmailAccountRequest(owner+"@gmail.test", owner, "same-subject"))
		if err != nil {
			t.Fatal(err)
		}
		ids[owner] = account.ID
		expires := time.Now().Add(time.Hour)
		if err := credentials.UpsertForUser(t.Context(), owner, account.ID, "google", "same-subject", owner+"-access", owner+"-refresh", "Bearer", &expires, "mail"); err != nil {
			t.Fatal(err)
		}
		paths[owner], err = blobs.StoreRaw(t.Context(), account.ID, 1, []byte("synthetic message"))
		if err != nil {
			t.Fatal(err)
		}
	}
	// Administrative deletion must still clean credentials after disabling the
	// owner; normal token calls must already be denied by that status change.
	if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	if token, err := credentials.GetOAuthTokenForUser(t.Context(), "alice", ids["alice"]); err == nil || token != "" {
		t.Fatalf("disabled owner's token exposed: %q %v", token, err)
	}
	if err := f.accountStore.DeleteAccount(t.Context(), "alice", ids["alice"], worker.Cleanup); err != nil {
		t.Fatal(err)
	}
	if err := f.accountStore.DeleteAccount(t.Context(), "alice", ids["alice"], worker.Cleanup); err != nil {
		t.Fatalf("deletion retry: %v", err)
	}
	if _, err := os.Stat(paths["alice"]); !os.IsNotExist(err) {
		t.Fatalf("deleted blob remains: %v", err)
	}
	if _, err := os.Stat(paths["bob"]); err != nil {
		t.Fatalf("foreign blob lost: %v", err)
	}
	var count int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_mailbox_credentials WHERE account_id=?`, ids["alice"]).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted grant remains: %d %v", count, err)
	}
	if token, err := credentials.GetOAuthTokenForUser(t.Context(), "bob", ids["bob"]); err != nil || token != "bob-access" {
		t.Fatalf("foreign grant damaged: %q %v", token, err)
	}
}

func TestUserMailboxCredentialsRequireMatchingLifecycleService(t *testing.T) {
	f := newUserStorageFixture(t)
	credentials, err := mailauth.NewUserCredentials(t.Context(), nil, f.routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.base.RegisterUserStorageRoutes(t.Context(), http.NewServeMux(), f.routing, handler.UserStorageOptions{Credentials: credentials}); err == nil {
		t.Fatal("credentials accepted without lifecycle cleanup")
	}
	ctx, cancel := context.WithCancel(t.Context())
	worker, err := mail.NewUserIMAP(ctx, f.accountStore, store.NewBlobStore(t.TempDir()), f.events)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); worker.Wait() })
	if err := worker.SetCredentials(nil); err == nil {
		t.Fatal("nil credentials accepted")
	}
	foreign := newUserStorageFixture(t)
	wrong, err := mailauth.NewUserCredentials(t.Context(), nil, foreign.routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.SetCredentials(wrong); err == nil {
		t.Fatal("foreign credentials installed")
	}
	if err := worker.Start(mail.UserIMAPBackgroundOptions{ScanInterval: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := worker.SetCredentials(credentials); err == nil {
		t.Fatal("credentials changed after background start")
	}
}
