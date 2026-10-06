package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
	"golang.org/x/oauth2"
)

type userOAuthFixture struct {
	h        *Handler
	client   *http.Client
	sessions map[string]*auth.Session
	starts   atomic.Int32
	calls    atomic.Int32
	cancel   context.CancelFunc
}

type userOAuthTransport struct{ target *url.URL }

func (t userOAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	address := *r.URL
	address.Scheme, address.Host = t.target.Scheme, t.target.Host
	copy.URL = &address
	return http.DefaultTransport.RoundTrip(copy)
}

func newUserOAuthFixture(t *testing.T, block func(http.ResponseWriter, *http.Request) bool) *userOAuthFixture {
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
	ctx, cancel := context.WithCancel(t.Context())
	f := &userOAuthFixture{sessions: map[string]*auth.Session{}, cancel: cancel}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if block != nil && block(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			f.calls.Add(1)
			_ = r.ParseForm()
			code := r.FormValue("code")
			subject := "same-subject"
			if strings.HasSuffix(code, "-changed") {
				subject = "different-subject"
			}
			owner := strings.Split(code, "-")[0]
			claims, _ := json.Marshal(map[string]any{"sub": subject, "email": "shared@mail.test", "name": owner, "aud": "client", "exp": time.Now().Add(time.Hour).Unix()})
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": code, "refresh_token": owner + "-refresh", "token_type": "Bearer", "expires_in": 3600, "scope": "https://graph.microsoft.com/Mail.ReadWrite https://graph.microsoft.com/Mail.Send https://graph.microsoft.com/MailboxSettings.ReadWrite", "id_token": "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"})
		case "/v1/userinfo":
			code := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			subject := "same-subject"
			if strings.HasSuffix(code, "-changed") {
				subject = "different-subject"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sub": subject, "email": "shared@mail.test", "name": strings.Split(code, "-")[0], "email_verified": true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	address, _ := url.Parse(server.URL)
	f.client = &http.Client{Transport: userOAuthTransport{address}}
	cfg := &mailauth.Config{BaseURL: "https://gofer.test", GoogleClient: &oauth2.Config{ClientID: "client", ClientSecret: "secret", Endpoint: oauth2.Endpoint{AuthURL: "https://authorize.test/google", TokenURL: server.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}}, MicrosoftClient: &oauth2.Config{ClientID: "client", Endpoint: oauth2.Endpoint{AuthURL: "https://authorize.test/microsoft", TokenURL: server.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}}}
	credentials, err := mailauth.NewUserCredentials(ctx, cfg, routing, testMailboxCredentialKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); credentials.Wait() })
	manager := auth.NewManager(&auth.Config{Enabled: true, Mode: auth.ModeManaged, BaseURL: "https://gofer.test"}, db, auth.Dependencies{BucketHashKey: testMailboxCredentialKey})
	for _, owner := range []string{"alice", "bob"} {
		f.sessions[owner], err = manager.CreateSession(t.Context(), owner, "oauth test")
		if err != nil {
			t.Fatal(err)
		}
	}
	f.h = &Handler{db: db, auth: manager, userStorage: routing, userAccounts: accounts, userCredentials: credentials, userStorageContext: ctx, userAccountHooks: UserAccountHooks{Created: func(context.Context, string) error { f.starts.Add(1); return nil }, Updated: func(context.Context, string) error { f.starts.Add(1); return nil }}}
	return f
}

func (f *userOAuthFixture) request(owner, method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions[owner].Token})
	return req.WithContext(context.WithValue(req.Context(), oauth2.HTTPClient, f.client))
}
func (f *userOAuthFixture) start(t *testing.T, owner, provider, action string) string {
	t.Helper()
	path := "/api/accounts/oauth2/authorize"
	form := url.Values{"provider": {provider}, "email_address": {"shared@mail.test"}, "display_name": {owner + " mailbox"}, "flow_action": {action}, auth.CSRFFormFieldName: {csrfProofForSession(t, f.h.auth, f.sessions[owner].Token, path)}}
	rec := httptest.NewRecorder()
	f.h.auth.Middleware(http.HandlerFunc(f.h.handleUserAccountOAuthAuthorize)).ServeHTTP(rec, f.request(owner, "POST", path, form.Encode()))
	if rec.Code != 303 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return location.Query().Get("state")
}
func (f *userOAuthFixture) callback(owner, provider, state, code string) *httptest.ResponseRecorder {
	path := "/auth/google/mailbox/callback"
	if provider == "outlook" {
		path = "/auth/microsoft/mailbox/callback"
	}
	rec := httptest.NewRecorder()
	f.h.auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.h.handleUserAccountOAuthCallback(w, r, provider) })).ServeHTTP(rec, f.request(owner, "GET", path+"?state="+url.QueryEscape(state)+"&code="+url.QueryEscape(code), ""))
	return rec
}
func (f *userOAuthFixture) accounts(t *testing.T, owner string) []models.Account {
	t.Helper()
	var result []models.Account
	if err := f.h.userStorage.WithUser(t.Context(), owner, func(db *storage.DB) error {
		var err error
		result, err = db.GetAccounts(t.Context(), owner)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return result
}
func assertOAuthSuccess(t *testing.T, r *httptest.ResponseRecorder) {
	t.Helper()
	if r.Code != 303 || strings.Contains(r.Header().Get("Location"), "error=") {
		t.Fatalf("callback: %d %s", r.Code, r.Header().Get("Location"))
	}
}

func TestUserOAuthOnboardingReconnectAndEditing(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f := newUserOAuthFixture(t, nil)
			for _, owner := range []string{"alice", "bob"} {
				assertOAuthSuccess(t, f.callback(owner, provider, f.start(t, owner, provider, "add"), owner+"-grant"))
				accounts := f.accounts(t, owner)
				if len(accounts) != 1 {
					t.Fatalf("accounts: %+v", accounts)
				}
				id := accounts[0].ID
				if err := f.h.userAccounts.WithAccountForUser(t.Context(), owner, id, func(local *config.AccountStore, _ *storage.DB) error {
					return local.SetEmailSyncEnabled(t.Context(), owner, id, false)
				}); err != nil {
					t.Fatal(err)
				}
				state := f.start(t, owner, provider, "reconnect")
				assertOAuthSuccess(t, f.callback(owner, provider, state, owner+"-new"))
				token, err := f.h.userCredentials.GetOAuthTokenForUser(t.Context(), owner, id)
				if err != nil || token != owner+"-new" {
					t.Fatalf("owned token: %q %v", token, err)
				}
				accounts = f.accounts(t, owner)
				if len(accounts) != 1 || accounts[0].ID != id {
					t.Fatal("reconnect replaced account")
				}
				data, err := f.h.userEditData(t.Context(), owner, id)
				if err != nil || data.EmailSyncEnabled {
					t.Fatalf("reconnect changed preference: %+v %v", data, err)
				}
				path := "/api/accounts/" + id + "/edit"
				form := url.Values{"provider": {provider}, "auth_method": {"oauth2"}, "email_address": {"shared@mail.test"}, "display_name": {"Renamed"}, "password": {"should never replace credentials"}, "imap_host": {"evil.test"}, auth.CSRFFormFieldName: {csrfProofForSession(t, f.h.auth, f.sessions[owner].Token, path)}}
				req := f.request(owner, "POST", path, form.Encode())
				req.SetPathValue("id", id)
				rec := httptest.NewRecorder()
				f.h.auth.Middleware(http.HandlerFunc(f.h.handleUserUpdateAccount)).ServeHTTP(rec, req)
				if rec.Code != 200 {
					t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
				}
				updated, _ := f.h.userEditData(t.Context(), owner, id)
				if updated.DisplayName != "Renamed" || updated.ProviderAccountID != data.ProviderAccountID || updated.IMAPHost != data.IMAPHost || updated.EmailSyncEnabled {
					t.Fatalf("edit changed identity/connection: %+v", updated)
				}
			}
			alice, bob := f.accounts(t, "alice"), f.accounts(t, "bob")
			if alice[0].ID == bob[0].ID {
				t.Fatal("owners share ID")
			}
			for _, table := range []string{"accounts", "oauth_accounts"} {
				var count int
				if err := f.h.db.Read().QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("legacy %s populated: %d %v", table, count, err)
				}
			}
			var plaintext int
			if err := f.h.db.Read().QueryRow(`SELECT count(*) FROM gofer_mailbox_credentials WHERE CAST(access_token_ciphertext AS TEXT) LIKE '%grant%' OR CAST(refresh_token_ciphertext AS TEXT) LIKE '%refresh%'`).Scan(&plaintext); err != nil || plaintext != 0 {
				t.Fatalf("plaintext credential: %d %v", plaintext, err)
			}
		})
	}
}

func TestUserOAuthRejectsStateAndIdentityConfusion(t *testing.T) {
	f := newUserOAuthFixture(t, nil)
	state := f.start(t, "alice", "gmail", "add")
	if rec := f.callback("bob", "gmail", state, "bob-grant"); !strings.Contains(rec.Header().Get("Location"), "oauth_session_mismatch") {
		t.Fatal(rec.Header())
	}
	if rec := f.callback("alice", "outlook", state, "alice-grant"); !strings.Contains(rec.Header().Get("Location"), "oauth_session_mismatch") {
		t.Fatal(rec.Header())
	}
	if f.calls.Load() != 0 {
		t.Fatal("invalid state reached token HTTP")
	}
	assertOAuthSuccess(t, f.callback("alice", "gmail", state, "alice-grant"))
	if rec := f.callback("alice", "gmail", state, "alice-grant"); !strings.Contains(rec.Header().Get("Location"), "oauth_invalid_state") {
		t.Fatal("replay accepted")
	}
	if f.calls.Load() != 1 {
		t.Fatal("replay reached HTTP")
	}
	id := f.accounts(t, "alice")[0].ID
	rec := f.callback("alice", "gmail", f.start(t, "alice", "gmail", "reconnect"), "alice-changed")
	if !strings.Contains(rec.Header().Get("Location"), "oauth_identity_mismatch") {
		t.Fatal(rec.Header())
	}
	token, err := f.h.userCredentials.GetOAuthTokenForUser(t.Context(), "alice", id)
	if err != nil || token != "alice-grant" {
		t.Fatalf("identity mismatch overwrote token: %q %v", token, err)
	}
	rec = f.callback("alice", "gmail", f.start(t, "alice", "gmail", "add"), "alice-changed")
	if !strings.Contains(rec.Header().Get("Location"), "create_failed") || len(f.accounts(t, "alice")) != 1 {
		t.Fatal("email collision replaced subject")
	}
	if f.starts.Load() != 1 {
		t.Fatal("failed authorization started workers")
	}
	state = f.start(t, "alice", "gmail", "reconnect")
	if err := f.h.userStorage.BeginAccountDeletion(t.Context(), "alice", id); err != nil {
		t.Fatal(err)
	}
	calls := f.calls.Load()
	_ = f.callback("alice", "gmail", state, "alice-new")
	if f.calls.Load() != calls {
		t.Fatal("deleted reconnect reached provider")
	}
}

func TestUserOAuthCredentialSaveFailureCanRecover(t *testing.T) {
	f := newUserOAuthFixture(t, nil)
	if _, err := f.h.db.Write().Exec(`CREATE TRIGGER reject_test_grant BEFORE INSERT ON gofer_mailbox_credentials BEGIN SELECT RAISE(ABORT,'injected grant failure'); END`); err != nil {
		t.Fatal(err)
	}
	rec := f.callback("alice", "gmail", f.start(t, "alice", "gmail", "add"), "alice-grant")
	if !strings.Contains(rec.Header().Get("Location"), "oauth_store_failed") || f.starts.Load() != 0 {
		t.Fatalf("false success: %s", rec.Header())
	}
	saved := f.accounts(t, "alice")
	if len(saved) != 1 {
		t.Fatalf("recoverable account absent: %+v", saved)
	}
	if _, err := f.h.userCredentials.GetOAuthTokenForUser(t.Context(), "alice", saved[0].ID); err == nil {
		t.Fatal("missing grant did not fail closed")
	}
	if _, err := f.h.db.Write().Exec(`DROP TRIGGER reject_test_grant`); err != nil {
		t.Fatal(err)
	}
	assertOAuthSuccess(t, f.callback("alice", "gmail", f.start(t, "alice", "gmail", "add"), "alice-retry"))
	recovered := f.accounts(t, "alice")
	if len(recovered) != 1 || recovered[0].ID != saved[0].ID || f.starts.Load() != 1 {
		t.Fatal("retry duplicated account or failed worker start")
	}
}

func TestUserOAuthBlockedExchangeDoesNotPinDatabaseAndCancels(t *testing.T) {
	entered := make(chan struct{}, 1)
	f := newUserOAuthFixture(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/token" {
			return false
		}
		_ = r.ParseForm()
		if r.FormValue("code") != "alice-blocked" {
			return false
		}
		entered <- struct{}{}
		<-r.Context().Done()
		return true
	})
	state := f.start(t, "alice", "gmail", "add")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- f.callback("alice", "gmail", state, "alice-blocked") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP did not begin")
	}
	assertOAuthSuccess(t, f.callback("bob", "gmail", f.start(t, "bob", "gmail", "add"), "bob-grant"))
	f.cancel()
	select {
	case rec := <-done:
		if !strings.Contains(rec.Header().Get("Location"), "error=") {
			t.Fatal("cancel reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel HTTP")
	}
	if len(f.accounts(t, "alice")) != 0 {
		t.Fatal("canceled exchange created mailbox")
	}
}

func TestUserOAuthConcurrentCallbacksReuseAccount(t *testing.T) {
	f := newUserOAuthFixture(t, nil)
	a, b := f.start(t, "alice", "gmail", "add"), f.start(t, "alice", "gmail", "add")
	results := make(chan *httptest.ResponseRecorder, 2)
	go func() { results <- f.callback("alice", "gmail", a, "alice-first") }()
	go func() { results <- f.callback("alice", "gmail", b, "alice-second") }()
	for range 2 {
		select {
		case r := <-results:
			assertOAuthSuccess(t, r)
		case <-time.After(5 * time.Second):
			t.Fatal("callbacks stalled")
		}
	}
	if len(f.accounts(t, "alice")) != 1 {
		t.Fatal("duplicate mailbox")
	}
}

func TestUserOAuthRoutesUseScopedCredentials(t *testing.T) {
	f := newUserOAuthFixture(t, nil)
	syncer := mail.NewSyncOrchestrator(f.h.db, nil, nil, nil)
	worker, err := mail.NewUserIMAP(f.h.userStorageContext, f.h.userAccounts, store.NewBlobStore(t.TempDir()), syncer.Events())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.cancel(); worker.Wait() })
	base := &Handler{db: f.h.db, auth: f.h.auth, syncer: syncer}
	mux := http.NewServeMux()
	if err := base.RegisterUserStorageRoutes(f.h.userStorageContext, mux, f.h.userStorage, UserStorageOptions{Accounts: f.h.userAccounts, IMAP: worker, Credentials: f.h.userCredentials}); err != nil {
		t.Fatal(err)
	}
	path := "/api/accounts/oauth2/authorize"
	form := url.Values{"provider": {"gmail"}, "email_address": {"shared@mail.test"}, auth.CSRFFormFieldName: {csrfProofForSession(t, f.h.auth, f.sessions["alice"].Token, path)}}
	rec := httptest.NewRecorder()
	base.auth.Middleware(mux).ServeHTTP(rec, f.request("alice", "POST", path, form.Encode()))
	if rec.Code != 303 {
		t.Fatalf("registered authorization: %d %s", rec.Code, rec.Body.String())
	}
	for _, path := range []string{"/auth/google/mailbox/callback", "/auth/microsoft/mailbox/callback"} {
		rec = httptest.NewRecorder()
		base.auth.Middleware(mux).ServeHTTP(rec, f.request("alice", "GET", path+"?state=invalid", ""))
		if rec.Code != 303 || !strings.Contains(rec.Header().Get("Location"), "oauth_invalid_state") {
			t.Fatalf("registered callback %s: %d", path, rec.Code)
		}
	}
	var count int
	if err := f.h.db.Read().QueryRow(`SELECT count(*) FROM oauth_account_flows`).Scan(&count); err != nil || count != 1 {
		t.Fatal(fmt.Sprint(count, err))
	}
}

func TestUserOAuthSessionExpiryAndEmailMismatch(t *testing.T) {
	f := newUserOAuthFixture(t, nil)
	state := f.start(t, "alice", "gmail", "add")
	other, err := f.h.auth.CreateSession(t.Context(), "alice", "second browser")
	if err != nil {
		t.Fatal(err)
	}
	original := f.sessions["alice"]
	f.sessions["alice"] = other
	if rec := f.callback("alice", "gmail", state, "alice-grant"); !strings.Contains(rec.Header().Get("Location"), "oauth_session_mismatch") {
		t.Fatal("another browser consumed flow")
	}
	f.sessions["alice"] = original
	if _, err := f.h.db.Write().Exec(`UPDATE oauth_account_flows SET expires_at=datetime('now','-1 minute')`); err != nil {
		t.Fatal(err)
	}
	if rec := f.callback("alice", "gmail", state, "alice-grant"); !strings.Contains(rec.Header().Get("Location"), "oauth_expired_state") {
		t.Fatal("expired flow accepted")
	}
	state, err = f.h.userCredentials.CreateAccountOAuthFlow(t.Context(), "alice", original.Token, "gmail", map[string]string{"email_address": "different@mail.test"})
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.callback("alice", "gmail", state, "alice-grant"); !strings.Contains(rec.Header().Get("Location"), "oauth_email_mismatch") {
		t.Fatal("wrong mailbox email accepted")
	}
	if len(f.accounts(t, "alice")) != 0 || f.starts.Load() != 0 || f.calls.Load() != 1 {
		t.Fatal("rejected flow changed mailbox")
	}
	if _, err = f.h.userCredentials.CreateAccountOAuthFlow(t.Context(), "alice", "", "gmail", nil); err == nil {
		t.Fatal("empty session accepted when config Enabled was false")
	}
}

func TestUserOAuthReconnectDeletionDrainsAndRejectsPublication(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	f := newUserOAuthFixture(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/token" {
			return false
		}
		_ = r.ParseForm()
		if r.FormValue("code") != "alice-blocked" {
			return false
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"alice-blocked","refresh_token":"new-refresh","token_type":"Bearer","expires_in":3600}`)
		return true
	})
	assertOAuthSuccess(t, f.callback("alice", "gmail", f.start(t, "alice", "gmail", "add"), "alice-grant"))
	id := f.accounts(t, "alice")[0].ID
	state := f.start(t, "alice", "gmail", "reconnect")
	callback := make(chan *httptest.ResponseRecorder, 1)
	go func() { callback <- f.callback("alice", "gmail", state, "alice-blocked") }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reconnect not in flight")
	}
	deleting := make(chan error, 1)
	go func() { deleting <- f.h.userStorage.BeginAccountDeletion(t.Context(), "alice", id) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err := f.h.userStorage.AccountStateForUser(t.Context(), "alice", id)
		if err != nil {
			t.Fatal(err)
		}
		if status == storage.AccountDeleting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deletion intent missing")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-deleting:
		t.Fatalf("deletion did not drain HTTP: %v", err)
	default:
	}
	// Another owner can still open and authorize using the single cache slot.
	assertOAuthSuccess(t, f.callback("bob", "gmail", f.start(t, "bob", "gmail", "add"), "bob-grant"))
	close(release)
	select {
	case rec := <-callback:
		if !strings.Contains(rec.Header().Get("Location"), "error=") {
			t.Fatal("deletion allowed reconnect success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("callback stalled")
	}
	select {
	case err := <-deleting:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deletion did not drain")
	}
	var revision int
	if err := f.h.db.Read().QueryRow(`SELECT revision FROM gofer_mailbox_credentials WHERE account_id=?`, id).Scan(&revision); err != nil || revision != 1 {
		t.Fatalf("deleting mailbox published token: %d %v", revision, err)
	}
}

func TestUserOAuthEditingRejectsForeignOwnerAndConnectionChange(t *testing.T) {
	f := newUserOAuthFixture(t, nil)
	assertOAuthSuccess(t, f.callback("alice", "gmail", f.start(t, "alice", "gmail", "add"), "alice-grant"))
	id := f.accounts(t, "alice")[0].ID
	for _, test := range []struct {
		owner, email, provider string
		status                 int
	}{{"bob", "shared@mail.test", "gmail", 404}, {"alice", "changed@mail.test", "gmail", 400}, {"alice", "shared@mail.test", "imap", 400}} {
		path := "/api/accounts/" + id + "/edit"
		form := url.Values{"provider": {test.provider}, "email_address": {test.email}, "display_name": {"intruder"}, auth.CSRFFormFieldName: {csrfProofForSession(t, f.h.auth, f.sessions[test.owner].Token, path)}}
		req := f.request(test.owner, "POST", path, form.Encode())
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		f.h.auth.Middleware(http.HandlerFunc(f.h.handleUserUpdateAccount)).ServeHTTP(rec, req)
		if rec.Code != test.status {
			t.Fatalf("edit %s/%s/%s: %d", test.owner, test.email, test.provider, rec.Code)
		}
	}
	data, err := f.h.userEditData(t.Context(), "alice", id)
	if err != nil || data.DisplayName != "alice mailbox" || data.ProviderAccountID != "same-subject" {
		t.Fatalf("rejected edit changed mailbox: %+v %v", data, err)
	}
}

func TestUserOAuthPartialSetupReportsFailures(t *testing.T) {
	for _, stage := range []string{"creation", "metadata", "worker"} {
		t.Run(stage, func(t *testing.T) {
			f := newUserOAuthFixture(t, nil)
			if stage == "worker" {
				f.h.userAccountHooks.Created = func(context.Context, string) error { return fmt.Errorf("injected start failure") }
			}
			if stage != "worker" {
				if err := f.h.userStorage.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					trigger := `CREATE TRIGGER reject_test_account BEFORE INSERT ON accounts BEGIN SELECT RAISE(ABORT,'injected local creation failure'); END`
					if stage == "metadata" {
						trigger = `CREATE TRIGGER reject_test_account BEFORE UPDATE OF display_name ON accounts BEGIN SELECT RAISE(ABORT,'injected metadata failure'); END`
					}
					_, err := db.Write().Exec(trigger)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			rec := f.callback("alice", "gmail", f.start(t, "alice", "gmail", "add"), "alice-grant")
			expected := map[string]string{"creation": "create_failed", "metadata": "oauth_metadata_failed", "worker": "oauth_sync_failed"}[stage]
			if !strings.Contains(rec.Header().Get("Location"), expected) {
				t.Fatalf("partial %s: %s", stage, rec.Header())
			}
			id := rec.Header().Get("X-Gofer-Account-ID")
			if id == "" {
				t.Fatal("durable account ID was lost")
			}
			if stage == "creation" {
				state, err := f.h.userStorage.AccountStateForUser(t.Context(), "alice", id)
				if err != nil || state != storage.AccountCreating {
					t.Fatalf("failed creation not durable: %s %v", state, err)
				}
				if len(f.accounts(t, "alice")) != 0 {
					t.Fatal("failed insert leaked account")
				}
			} else {
				token, err := f.h.userCredentials.GetOAuthTokenForUser(t.Context(), "alice", id)
				if err != nil || token != "alice-grant" {
					t.Fatalf("saved grant not recoverable: %q %v", token, err)
				}
			}
			if f.starts.Load() != 0 {
				t.Fatal("failed setup reported worker startup")
			}
		})
	}
}

func TestUserOAuthLookupRejectsDifferentEmailForSameSubject(t *testing.T) {
	f := newUserOAuthFixture(t, nil)
	assertOAuthSuccess(t, f.callback("alice", "gmail", f.start(t, "alice", "gmail", "add"), "alice-grant"))
	if _, err := f.h.userAccounts.FindOAuthAccount(t.Context(), "alice", "gmail", "same-subject", "other@mail.test"); err == nil {
		t.Fatal("same subject bypassed email identity")
	}
	id, err := f.h.userAccounts.FindOAuthAccount(t.Context(), "alice", "gmail", "same-subject", "SHARED@MAIL.TEST")
	if err != nil || id != f.accounts(t, "alice")[0].ID {
		t.Fatalf("email case broke lookup: %q %v", id, err)
	}
}
