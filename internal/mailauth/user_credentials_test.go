package mailauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
)

type userCredentialFixture struct {
	system   *storage.DB
	routing  *storage.AccountRouting
	accounts *config.UserAccountStore
	service  *UserCredentials
	ids      map[string]string
	cancel   context.CancelFunc
}

func newUserCredentialFixture(t *testing.T, endpoint http.Handler) *userCredentialFixture {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, owner := range []string{"alice", "bob"} {
		if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES(?,?,?)`, owner, owner, owner); err != nil {
			t.Fatal(err)
		}
	}
	stores, err := storage.NewUserStores(db, storage.UserStoreOptions{MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := stores.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	routing, err := storage.NewAccountRouting(stores)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := config.NewUserAccountStore(routing, testMailboxCredentialKey)
	if err != nil {
		t.Fatal(err)
	}
	f := &userCredentialFixture{system: db, routing: routing, accounts: accounts, ids: map[string]string{}}
	for _, owner := range []string{"alice", "bob"} {
		account, err := accounts.CreateAccount(t.Context(), owner, providers.GmailAccountRequest(owner+"@mail.test", owner, "same-google-subject"))
		if err != nil {
			t.Fatal(err)
		}
		f.ids[owner] = account.ID
	}
	account, err := accounts.CreateAccount(t.Context(), "alice", providers.OutlookAccountRequest("alice@outlook.test", "Alice", "microsoft-subject"))
	if err != nil {
		t.Fatal(err)
	}
	f.ids["outlook"] = account.ID
	cfg := &Config{}
	if endpoint != nil {
		server := httptest.NewServer(endpoint)
		t.Cleanup(server.Close)
		client := &oauth2.Config{ClientID: "client", ClientSecret: "secret", Endpoint: oauth2.Endpoint{TokenURL: server.URL, AuthStyle: oauth2.AuthStyleInParams}}
		cfg.GoogleClient, cfg.MicrosoftClient = client, client
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.service, err = NewUserCredentials(ctx, cfg, routing, testMailboxCredentialKey)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); f.service.Wait() })
	return f
}

func (f *userCredentialFixture) grant(t *testing.T, key, access, refresh string, expired bool, scopes string) {
	t.Helper()
	owner, provider, subject := key, providers.OAuthGoogle, "same-google-subject"
	if key == "outlook" {
		owner, provider, subject = "alice", providers.OAuthMicrosoft, "microsoft-subject"
	}
	expiry := time.Now().Add(time.Hour)
	if expired {
		expiry = time.Now().Add(-time.Hour)
	}
	if err := f.service.UpsertForUser(t.Context(), owner, f.ids[key], provider, subject, access, refresh, "Bearer", &expiry, scopes); err != nil {
		t.Fatal(err)
	}
}

type credentialResult struct {
	token string
	err   error
}

func asyncCredential(fn func() (string, error)) <-chan credentialResult {
	result := make(chan credentialResult, 1)
	go func() { token, err := fn(); result <- credentialResult{token, err} }()
	return result
}
func awaitCredential(t *testing.T, result <-chan credentialResult) credentialResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("credential operation did not finish")
		return credentialResult{}
	}
}
func awaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("token endpoint was not reached")
	}
}

func TestUserCredentialsOwnerIsolationAndEncryptedCentralStorage(t *testing.T) {
	f := newUserCredentialFixture(t, nil)
	for _, owner := range []string{"alice", "bob"} {
		f.grant(t, owner, owner+"-access", owner+"-refresh", false, "mail")
		got, err := f.service.Account(owner, f.ids[owner]).GetOAuthTokenForAccount(t.Context(), f.ids[owner])
		if err != nil || got != owner+"-access" {
			t.Fatalf("own grant: %q %v", got, err)
		}
		var ciphertext, refresh []byte
		if err := f.system.Read().QueryRow(`SELECT access_token_ciphertext,refresh_token_ciphertext FROM gofer_mailbox_credentials WHERE account_id=? AND user_id=?`, f.ids[owner], owner).Scan(&ciphertext, &refresh); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(ciphertext, []byte(owner+"-access")) || bytes.Contains(refresh, []byte(owner+"-refresh")) {
			t.Fatal("plaintext central credential")
		}
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			var count int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM oauth_accounts`).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return errors.New("user store contains OAuth credentials")
			}
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE name='gofer_mailbox_credentials'`).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return errors.New("central credential table exists in user store")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"accounts", "oauth_accounts"} {
		var count int
		if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("shared %s copies: %d %v", table, count, err)
		}
	}
	if got, err := f.service.GetOAuthTokenForUser(t.Context(), "bob", f.ids["alice"]); !errors.Is(err, storage.ErrAccountRoute) || got != "" {
		t.Fatalf("foreign read: %q %v", got, err)
	}
	if got, err := f.service.Account("alice", f.ids["alice"]).RefreshOAuthTokenForAccount(t.Context(), f.ids["bob"]); !errors.Is(err, storage.ErrAccountRoute) || got != "" {
		t.Fatalf("adapter escaped: %q %v", got, err)
	}
	if err := f.service.UpsertForUser(t.Context(), "bob", f.ids["alice"], "google", "same-google-subject", "bad", "bad", "", nil, ""); err == nil {
		t.Fatal("foreign grant accepted")
	}
	if err := f.service.UpsertForUser(t.Context(), "alice", f.ids["alice"], "microsoft", "wrong-subject", "bad", "bad", "", nil, ""); err == nil {
		t.Fatal("mismatched grant accepted")
	}
	if err := f.service.CleanupAccount(t.Context(), f.ids["alice"]); !errors.Is(err, storage.ErrAccountRoute) {
		t.Fatalf("active cleanup: %v", err)
	}
	if _, err := NewUserCredentials(t.Context(), nil, f.routing, testMailboxCredentialKey); err != nil {
		t.Fatalf("repeated initialization: %v", err)
	}
}

func TestUserCredentialsRefreshCoalescesWithoutPinningOtherUsers(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		if r.FormValue("refresh_token") != "alice-refresh" || r.FormValue("grant_type") != "refresh_token" {
			t.Error("wrong refresh request")
		}
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"refreshed","refresh_token":"rotated","expires_in":3600,"token_type":"Bearer"}`)
	}))
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	f.grant(t, "alice", "old", "alice-refresh", true, "mail")
	f.grant(t, "bob", "bob-cached", "bob-refresh", false, "mail")
	results := make([]<-chan credentialResult, 12)
	for i := range results {
		results[i] = asyncCredential(func() (string, error) { return f.service.GetOAuthTokenForUser(t.Context(), "alice", f.ids["alice"]) })
	}
	awaitSignal(t, entered)
	// The user cache has exactly one slot. Bob must evict Alice and write while
	// Alice's token request is blocked; no network work may pin that slot.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if token, err := f.service.GetOAuthTokenForUser(ctx, "bob", f.ids["bob"]); err != nil || token != "bob-cached" {
		t.Fatalf("Bob blocked: %q %v", token, err)
	}
	if err := f.routing.WithUser(ctx, "bob", func(db *storage.DB) error { return db.SetSetting(ctx, "bob", "network_probe", "written") }); err != nil {
		t.Fatal(err)
	}
	waitCtx, stop := context.WithCancel(t.Context())
	waiting := asyncCredential(func() (string, error) { return f.service.GetOAuthTokenForUser(waitCtx, "alice", f.ids["alice"]) })
	stop()
	if result := awaitCredential(t, waiting); result.token != "" || !errors.Is(result.err, context.Canceled) {
		t.Fatalf("canceled waiter: %+v", result)
	}
	unblock()
	for _, result := range results {
		if got := awaitCredential(t, result); got.err != nil || got.token != "refreshed" {
			t.Fatalf("refresh: %+v", got)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh requests: %d", calls.Load())
	}
	record, err := f.service.load(t.Context(), "alice", f.ids["alice"])
	if err != nil || record.RefreshToken != "rotated" || record.Scopes != "mail" {
		t.Fatalf("rotated grant: %+v %v", record, err)
	}
	f.service.mu.Lock()
	defer f.service.mu.Unlock()
	if len(f.service.gates) != 0 {
		t.Fatal("completed refresh gates retained")
	}
}

func TestUserCredentialsRejectStaleRefreshPublication(t *testing.T) {
	for _, change := range []string{"reconnect", "identity", "disable", "deletion"} {
		t.Run(change, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"access_token":"stale-access","refresh_token":"stale-refresh","expires_in":3600}`)
			}))
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			f.grant(t, "alice", "before", "refresh", true, "mail")
			result := asyncCredential(func() (string, error) { return f.service.GetOAuthTokenForUser(t.Context(), "alice", f.ids["alice"]) })
			awaitSignal(t, entered)
			switch change {
			case "reconnect":
				f.grant(t, "alice", "reconnected", "new-refresh", false, "mail")
			case "identity":
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE accounts SET provider_account_id='new-subject' WHERE id=?`, f.ids["alice"])
					return err
				}); err != nil {
					t.Fatal(err)
				}
			case "disable":
				if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			case "deletion":
				if err := f.routing.RequestAccountDeletion(t.Context(), "alice", f.ids["alice"]); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
				err := f.routing.BeginAccountDeletion(ctx, "alice", f.ids["alice"])
				cancel()
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("deletion did not drain blocked refresh: %v", err)
				}
			}
			unblock()
			got := awaitCredential(t, result)
			if got.err == nil || got.token != "" {
				t.Fatalf("stale token escaped: %+v", got)
			}
			if (change == "reconnect" || change == "identity") && !errors.Is(got.err, ErrMailboxAuthorizationChanged) {
				t.Fatalf("missing conflict: %v", got.err)
			}
			var revision int
			if err := f.system.Read().QueryRow(`SELECT revision FROM gofer_mailbox_credentials WHERE account_id=?`, f.ids["alice"]).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			want := 1
			if change == "reconnect" {
				want = 2
				record, err := f.service.load(t.Context(), "alice", f.ids["alice"])
				if err != nil || record.AccessToken != "reconnected" || record.RefreshToken != "new-refresh" {
					t.Fatalf("reconnect overwritten: %+v %v", record, err)
				}
			}
			if revision != want {
				t.Fatalf("stale refresh changed revision: %d", revision)
			}
			if change == "deletion" {
				if err := f.accounts.DeleteAccount(t.Context(), "alice", f.ids["alice"], f.service.CleanupAccount); err != nil {
					t.Fatal(err)
				}
				if err := f.service.CleanupAccount(t.Context(), f.ids["alice"]); err != nil {
					t.Fatalf("cleanup retry: %v", err)
				}
				var count int
				if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_mailbox_credentials WHERE account_id=?`, f.ids["alice"]).Scan(&count); err != nil || count != 0 {
					t.Fatalf("deleted credentials: %d %v", count, err)
				}
				if err := f.service.UpsertForUser(t.Context(), "alice", f.ids["alice"], "google", "same-google-subject", "resurrect", "refresh", "", nil, ""); err == nil {
					t.Fatal("deleted credential resurrected")
				}
			}
		})
	}
}

func TestUserCredentialsGraphScopesAndRefreshRetention(t *testing.T) {
	var calls atomic.Int32
	f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			return
		}
		if r.FormValue("refresh_token") != "graph-refresh" || r.FormValue("scope") != strings.Join(microsoftGraphMailScopes(), " ") {
			t.Errorf("incorrect Graph scope/refresh: %v", r.Form)
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"graph-mail","expires_in":3600,"scope":"`+strings.Join(microsoftGraphMailScopes(), " ")+`"}`)
	}))
	f.grant(t, "outlook", "smtp-resource", "graph-refresh", false, "https://outlook.office.com/SMTP.Send")
	for i := 0; i < 2; i++ {
		got, err := f.service.Account("alice", f.ids["outlook"]).GetMicrosoftGraphMailTokenForAccount(t.Context(), f.ids["outlook"])
		if err != nil || got != "graph-mail" {
			t.Fatalf("Graph token: %q %v", got, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("Graph cache missed: %d", calls.Load())
	}
	if got, err := f.service.RefreshOAuthTokenForUser(t.Context(), "alice", f.ids["outlook"]); err != nil || got != "graph-mail" || calls.Load() != 2 {
		t.Fatalf("forced Graph refresh: %q %v calls:%d", got, err, calls.Load())
	}
	record, err := f.service.load(t.Context(), "alice", f.ids["outlook"])
	if err != nil || record.RefreshToken != "graph-refresh" || !recordHasScopes(record.Scopes, microsoftGraphMailScopes()...) {
		t.Fatalf("Graph grant: %+v %v", record, err)
	}
	f.grant(t, "outlook", "reconnected-without-refresh", "", false, record.Scopes)
	record, err = f.service.load(t.Context(), "alice", f.ids["outlook"])
	if err != nil || record.RefreshToken != "graph-refresh" {
		t.Fatalf("reconnect lost refresh: %+v %v", record, err)
	}
	f.grant(t, "alice", "google-cached", "google-refresh", false, "mail")
	if got, err := f.service.GetMicrosoftGraphMailTokenForUser(t.Context(), "alice", f.ids["alice"]); err == nil || got != "" {
		t.Fatalf("Google used for Graph: %q %v", got, err)
	}
}

func TestUserCredentialsRejectTamperedAndReassignedGrants(t *testing.T) {
	f := newUserCredentialFixture(t, nil)
	f.grant(t, "alice", "alice-access", "alice-refresh", false, "mail")
	f.grant(t, "bob", "bob-access", "bob-refresh", false, "mail")
	wrong, err := NewUserCredentials(t.Context(), nil, f.routing, []byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := wrong.GetOAuthTokenForUser(t.Context(), "alice", f.ids["alice"]); err == nil || got != "" {
		t.Fatalf("wrong key accepted: %q %v", got, err)
	}
	if _, err := f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET refresh_token_ciphertext=(SELECT refresh_token_ciphertext FROM gofer_mailbox_credentials WHERE account_id=?) WHERE account_id=?`, f.ids["bob"], f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	if got, err := f.service.GetOAuthTokenForUser(t.Context(), "alice", f.ids["alice"]); err == nil || got != "" {
		t.Fatalf("swapped ciphertext accepted: %q %v", got, err)
	}
	if err := f.service.UpsertForUser(t.Context(), "alice", f.ids["alice"], "google", "same-google-subject", "replacement", "", "", nil, ""); err == nil {
		t.Fatal("tampered retained refresh accepted")
	}
	// An explicit replacement grant repairs corrupted bytes. An identity change
	// cannot silently reuse the previous identity's refresh authorization.
	f.grant(t, "alice", "repaired", "repaired-refresh", false, "mail")
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET provider_account_id='changed-subject' WHERE id=?`, f.ids["alice"])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.UpsertForUser(t.Context(), "alice", f.ids["alice"], "google", "changed-subject", "new-access", "", "", nil, ""); err == nil {
		t.Fatal("refresh from old identity reused")
	}
	if _, err := f.system.Write().Exec(`UPDATE gofer_mailbox_credentials SET user_id='bob' WHERE account_id=?`, f.ids["alice"]); err == nil {
		t.Fatal("credential ownership changed")
	}
	if err := f.service.UpsertForUser(t.Context(), "alice", f.ids["alice"], "google", "changed-subject", "new-access", "new-refresh", "", nil, "mail"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.load(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
}

func TestUserCredentialsProviderFailuresAndCancellation(t *testing.T) {
	for _, provider := range []string{"google", "microsoft"} {
		t.Run(provider, func(t *testing.T) {
			f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(http.StatusTooManyRequests)
				fmt.Fprint(w, `{"error":"temporarily_unavailable","error_description":"secret-provider-description"}`)
			}))
			key, owner := "alice", "alice"
			if provider == "microsoft" {
				key = "outlook"
			}
			f.grant(t, key, "old", "refresh", true, "mail")
			got, err := f.service.GetOAuthTokenForUser(t.Context(), owner, f.ids[key])
			var failure *OAuthTokenError
			if got != "" || !errors.As(err, &failure) || failure.Status != 429 || failure.RetryAt.Before(time.Now().Add(time.Minute)) || strings.Contains(err.Error(), "secret-provider-description") {
				t.Fatalf("unsanitized/nonretryable failure: %q %v", got, err)
			}
			record, loadErr := f.service.load(t.Context(), owner, f.ids[key])
			if loadErr != nil || record.AccessToken != "old" || record.revision != 1 {
				t.Fatalf("failed refresh changed grant: %+v %v", record, loadErr)
			}
		})
	}
	for _, rootCancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("lifecycle-cancel-%t", rootCancel), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer close(release)
			f.grant(t, "alice", "old", "refresh", true, "mail")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := asyncCredential(func() (string, error) { return f.service.GetOAuthTokenForUser(ctx, "alice", f.ids["alice"]) })
			awaitSignal(t, entered)
			if rootCancel {
				f.cancel()
			} else {
				cancel()
			}
			if got := awaitCredential(t, result); !errors.Is(got.err, context.Canceled) || got.token != "" {
				t.Fatalf("canceled refresh: %+v", got)
			}
			if rootCancel {
				f.service.Wait()
				if got, err := f.service.GetOAuthTokenForUser(t.Context(), "alice", f.ids["alice"]); err == nil || got != "" {
					t.Fatalf("shutdown accepted new work: %q %v", got, err)
				}
			}
		})
	}
}

func TestUserCredentialsRejectDuplicateIdentityWithinOwner(t *testing.T) {
	f := newUserCredentialFixture(t, nil)
	f.grant(t, "alice", "original", "refresh", false, "mail")
	other, err := f.accounts.CreateAccount(t.Context(), "alice", providers.GmailAccountRequest("other@mail.test", "Other", "other-subject"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET provider_account_id='same-google-subject' WHERE id=?`, other.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err = f.service.UpsertForUser(t.Context(), "alice", other.ID, "google", "same-google-subject", "duplicate", "refresh", "", nil, "mail")
	if err == nil {
		t.Fatal("duplicate provider identity within owner accepted")
	}
	if got, err := f.service.GetOAuthTokenForUser(t.Context(), "alice", f.ids["alice"]); err != nil || got != "original" {
		t.Fatalf("original grant changed: %q %v", got, err)
	}
	// A non-OAuth account must never accept a grant.
	plain, err := f.accounts.CreateAccount(t.Context(), "alice", &models.CreateAccountRequest{Provider: "imap", EmailAddress: "plain@mail.test", Username: "plain", Password: "password", AuthMethod: "plain", IMAPHost: "mail.test", IMAPPort: 993, IMAPTLSMode: "tls", SMTPHost: "mail.test", SMTPPort: 465, SMTPTLSMode: "tls"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.UpsertForUser(t.Context(), "alice", plain.ID, "google", "subject", "access", "refresh", "", nil, ""); err == nil {
		t.Fatal("plain IMAP grant accepted")
	}
}

func TestUserCredentialsRejectEmptyProviderTokens(t *testing.T) {
	for _, key := range []string{"alice", "outlook"} {
		t.Run(key, func(t *testing.T) {
			f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"access_token":"","refresh_token":"do-not-persist","expires_in":3600}`)
			}))
			f.grant(t, key, "original", "refresh", true, "mail")
			if token, err := f.service.GetOAuthTokenForUser(t.Context(), "alice", f.ids[key]); err == nil || token != "" {
				t.Fatalf("empty provider token accepted: %q %v", token, err)
			}
			record, err := f.service.load(t.Context(), "alice", f.ids[key])
			if err != nil || record.AccessToken != "original" || record.RefreshToken != "refresh" || record.revision != 1 {
				t.Fatalf("invalid response changed grant: %+v %v", record, err)
			}
		})
	}
}

func TestUserCredentialsReconnectRetainsConcurrentRefreshRotation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	f := newUserCredentialFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"refreshed","refresh_token":"rotated","expires_in":3600}`)
	}))
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	f.grant(t, "alice", "old", "original-refresh", true, "mail")
	refresh := asyncCredential(func() (string, error) { return f.service.GetOAuthTokenForUser(t.Context(), "alice", f.ids["alice"]) })
	awaitSignal(t, entered)
	expiry := time.Now().Add(time.Hour)
	reconnect := asyncCredential(func() (string, error) {
		return "", f.service.UpsertForUser(t.Context(), "alice", f.ids["alice"], "google", "same-google-subject", "reconnected", "", "Bearer", &expiry, "mail")
	})
	select {
	case result := <-reconnect:
		t.Fatalf("reconnect retained an in-flight refresh token: %+v", result)
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	if got := awaitCredential(t, refresh); got.err != nil || got.token != "refreshed" {
		t.Fatalf("refresh: %+v", got)
	}
	if got := awaitCredential(t, reconnect); got.err != nil {
		t.Fatalf("reconnect: %+v", got)
	}
	record, err := f.service.load(t.Context(), "alice", f.ids["alice"])
	if err != nil || record.AccessToken != "reconnected" || record.RefreshToken != "rotated" {
		t.Fatalf("reconnect lost token rotation: %+v %v", record, err)
	}
}
