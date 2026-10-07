package notifications

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/handler"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

type userStorageFixture struct {
	system       *storage.DB
	routing      *storage.AccountRouting
	service      *Service
	http         http.Handler
	sessions     map[string]*auth.Session
	accounts     map[string]*models.Account
	accountStore *config.UserAccountStore
	base         *handler.Handler
	events       *mail.EventBus
	imap         *mail.UserIMAP
	blobs        *store.BlobStore
	stopIMAP     context.CancelFunc
	credentials  *mailauth.UserCredentials
}

func newUserStorageFixture(t *testing.T, hooks ...handler.UserAccountHooks) *userStorageFixture {
	return newUserStorageFixtureMode(t, false, hooks...)
}
func newUserStorageFixtureMode(t *testing.T, enableIMAP bool, hooks ...handler.UserAccountHooks) *userStorageFixture {
	return newUserStorageFixtureConfigured(t, enableIMAP, nil, hooks...)
}
func newUserStorageFixtureConfigured(t *testing.T, enableIMAP bool, oauth *mailauth.Config, hooks ...handler.UserAccountHooks) *userStorageFixture {
	return newUserStorageFixtureContacts(t, enableIMAP, oauth, nil, hooks...)
}

func newUserStorageFixtureContacts(t *testing.T, enableIMAP bool, oauth *mailauth.Config, contacts *handler.UserContactSyncOptions, hooks ...handler.UserAccountHooks) *userStorageFixture {
	return newUserStorageFixtureServices(t, enableIMAP, oauth, contacts, nil, hooks...)
}
func newUserStorageFixtureServices(t *testing.T, enableIMAP bool, oauth *mailauth.Config, contacts *handler.UserContactSyncOptions, calendar *handler.UserCalendarSyncOptions, hooks ...handler.UserAccountHooks) *userStorageFixture {
	t.Helper()
	system, err := storage.New(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := system.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, owner := range []string{"alice", "bob"} {
		if _, err := system.Write().Exec(`INSERT INTO users(id, username, username_normalized) VALUES (?, ?, ?)`, owner, owner, owner); err != nil {
			t.Fatal(err)
		}
	}
	stores, err := storage.NewUserStores(system, storage.UserStoreOptions{MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := stores.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	routing, err := storage.NewAccountRouting(stores)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	manager := auth.NewManager(&auth.Config{Enabled: true, Mode: auth.ModeManaged, BaseURL: "http://localhost:8090"}, system, auth.Dependencies{BucketHashKey: key})
	accountStore, err := config.NewUserAccountStore(routing, key)
	if err != nil {
		t.Fatal(err)
	}
	f := &userStorageFixture{system: system, routing: routing, sessions: make(map[string]*auth.Session), accounts: make(map[string]*models.Account)}
	f.accountStore = accountStore
	for _, owner := range []string{"alice", "bob"} {
		f.sessions[owner], err = manager.CreateSession(t.Context(), owner, "integration test")
		if err != nil {
			t.Fatal(err)
		}
		f.accounts[owner], err = accountStore.CreateAccount(t.Context(), owner, &models.CreateAccountRequest{
			Provider: "imap", EmailAddress: owner + "@example.com", DisplayName: owner,
			IMAPHost: "imap.example.com", SMTPHost: "smtp.example.com", Username: owner, Password: "synthetic-only"})
		if err != nil {
			t.Fatal(err)
		}
		if err := routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			if err := db.SetUISettings(t.Context(), owner, map[string]string{"theme": owner, "contacts_auto_create_observed": "false", "desktop_notifications": "true", "notification_mode": "web_push"}); err != nil {
				return err
			}
			if _, err := db.SaveContact(t.Context(), owner, models.Contact{ID: "same-contact-id", Name: owner + " private contact", Email: owner + "-contact@example.com"}); err != nil {
				return err
			}
			return db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{ID: owner + "-inbox", AccountID: f.accounts[owner].ID, Name: "Inbox", Role: "inbox", Selectable: true}})
		}); err != nil {
			t.Fatal(err)
		}
	}
	syncer := mail.NewSyncOrchestrator(system, nil, nil, nil)
	f.events = syncer.Events()
	base := handler.New(system, nil, syncer, nil, manager, "test-vapid-public")
	f.base = base
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	mux := http.NewServeMux()
	var options []handler.UserStorageOptions
	if len(hooks) > 0 {
		options = append(options, handler.UserStorageOptions{Accounts: accountStore, Hooks: hooks[0]})
	}
	if enableIMAP {
		f.blobs = store.NewBlobStore(t.TempDir())
		f.stopIMAP = cancel
		f.imap, err = mail.NewUserIMAP(ctx, accountStore, f.blobs, f.events)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cancel(); f.imap.Wait() })
		options = []handler.UserStorageOptions{{Accounts: accountStore, IMAP: f.imap, ContactSync: contacts, CalendarSync: calendar}}
		if oauth != nil {
			f.credentials, err = mailauth.NewUserCredentials(ctx, oauth, routing, key)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cancel(); f.imap.Wait(); f.credentials.Wait() })
			options[0].Credentials = f.credentials
		}
	}
	if err := base.RegisterUserStorageRoutes(ctx, mux, routing, options...); err != nil {
		t.Fatal(err)
	}
	f.http = manager.Middleware(mux)
	f.service, err = NewWithUserStorage(routing, f.events, "test-vapid-public", "test-vapid-private", "mailto:test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *userStorageFixture) request(owner, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if body != "" && !strings.HasPrefix(body, "{") {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if session := f.sessions[owner]; session != nil {
		req.AddCookie(&http.Cookie{Name: "gofer_session", Value: session.Token})
	}
	rec := httptest.NewRecorder()
	f.http.ServeHTTP(rec, req)
	return rec
}

func (f *userStorageFixture) subscribe(t *testing.T, owner, endpoint string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"endpoint": endpoint, "keys": map[string]string{"p256dh": "synthetic-public", "auth": "synthetic-auth"}})
	if rec := f.request(owner, http.MethodPost, "/api/push/subscription", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("subscribe %s: %d %s", owner, rec.Code, rec.Body.String())
	}
}

func (f *userStorageFixture) event(owner string) mail.Event {
	return mail.Event{Type: mail.EventNewMail, AccountID: f.accounts[owner].ID, UserID: owner, FolderID: owner + "-inbox", FolderRole: "inbox", Payload: map[string]any{"unread_count": 1}}
}

func pushResponse(status int) *http.Response {
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(""))}
}

func TestUserStorageHTTPPreferencesContactsAndGlobalEndpointOwnership(t *testing.T) {
	f := newUserStorageFixture(t)
	if err := f.system.SetUISettings(t.Context(), "alice", map[string]string{"theme": "central-sentinel"}); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		rec := f.request(owner, http.MethodGet, "/api/settings/ui?user_id=bob", "")
		var settings map[string]string
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &settings) != nil || settings["theme"] != owner {
			t.Fatalf("settings %s: %d %s", owner, rec.Code, rec.Body.String())
		}
		for _, path := range []string{"/api/contacts/search?q=private", "/api/contacts/same-contact-id/export", "/api/contacts/export"} {
			rec := f.request(owner, http.MethodGet, path, "")
			other := "alice"
			if owner == other {
				other = "bob"
			}
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), owner+"-contact@example.com") || strings.Contains(rec.Body.String(), other+"-contact@example.com") {
				t.Fatalf("contacts %s %s: %d %s", owner, path, rec.Code, rec.Body.String())
			}
		}
	}
	if rec := f.request("alice", http.MethodPatch, "/api/settings/ui", `{"theme":"changed"}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if got := f.system.GetUISettings(t.Context(), "alice")["theme"]; got != "central-sentinel" {
		t.Fatalf("local patch wrote central preferences: %s", got)
	}
	f.subscribe(t, "alice", "https://push.example/shared")
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		if err := db.SaveWebPushSubscription(t.Context(), storage.WebPushSubscription{
			Endpoint: "https://push.example/local", UserID: "alice", P256DH: "public", Auth: "private",
		}); err == nil {
			t.Error("user database accepted a central push registration")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rec := f.request("bob", http.MethodPost, "/api/push/subscription", `{"endpoint":"https://push.example/shared","keys":{"p256dh":"other","auth":"other"}}`); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign endpoint claimed: %d", rec.Code)
	}
	if rec := f.request("bob", http.MethodDelete, "/api/push/subscription", `{"endpoint":"https://push.example/shared"}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	subs, err := f.system.ListWebPushSubscriptions(t.Context(), "alice")
	if err != nil || len(subs) != 1 {
		t.Fatalf("foreign delete removed endpoint: %v %v", subs, err)
	}
	if rec := f.request("", http.MethodGet, "/api/settings/ui", ""); rec.Code == http.StatusOK {
		t.Fatal("unauthenticated user reached local store")
	}
	if _, err := f.system.Write().Exec(`UPDATE users SET status = 'disabled' WHERE id = 'alice'`); err != nil {
		t.Fatal(err)
	}
	if rec := f.request("alice", http.MethodGet, "/api/settings/ui", ""); rec.Code == http.StatusOK {
		t.Fatal("disabled session reached local store")
	}
}

func TestUserStorageHTTPConcurrentPreferencePatchesPreserveEachUpdate(t *testing.T) {
	f := newUserStorageFixture(t)
	var wg sync.WaitGroup
	for _, key := range []string{"timezone", "theme", "notification_mode", "contacts_observed_sources", "date_format"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]string{key: "updated-" + key})
			if rec := f.request("alice", http.MethodPatch, "/api/settings/ui", string(body)); rec.Code != http.StatusOK {
				t.Errorf("patch %s: %d", key, rec.Code)
			}
		}(key)
	}
	wg.Wait()
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		settings := db.GetUISettings(t.Context(), "alice")
		for _, key := range []string{"timezone", "theme", "notification_mode", "contacts_observed_sources", "date_format"} {
			if settings[key] != "updated-"+key {
				t.Errorf("lost concurrent patch %s", key)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserStorageNotificationWorkerUsesDirectoryAndLocalPreferences(t *testing.T) {
	f := newUserStorageFixture(t)
	f.subscribe(t, "alice", "https://push.example/alice")
	f.subscribe(t, "bob", "https://push.example/bob")
	if rec := f.request("bob", http.MethodPatch, "/api/settings/ui", `{"desktop_notifications":"false"}`); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	delivered := make(chan string, 4)
	f.service.sendNotification = func(_ context.Context, _ []byte, sub *webpush.Subscription, _ *webpush.Options) (*http.Response, error) {
		delivered <- sub.Endpoint
		return pushResponse(http.StatusCreated), nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.service.Start(ctx)
	f.events.Publish(f.event("alice"))
	select {
	case endpoint := <-delivered:
		if endpoint != "https://push.example/alice" {
			t.Fatal(endpoint)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker never delivered notification")
	}
	// Direct calls make negative checks deterministic; the positive call above
	// exercises the event bus and actual background worker entry point.
	f.service.handleNewMail(t.Context(), f.event("bob"))
	forged := f.event("alice")
	forged.UserID = "bob"
	f.service.handleNewMail(t.Context(), forged)
	forged = f.event("alice")
	forged.FolderID = "bob-inbox"
	f.service.handleNewMail(t.Context(), forged)
	select {
	case endpoint := <-delivered:
		t.Fatalf("unexpected foreign/disabled notification: %s", endpoint)
	default:
	}
}

func TestUserStoragePushDeliveryReleasesLeaseAndRejectsStaleAcknowledgement(t *testing.T) {
	f := newUserStorageFixture(t)
	f.subscribe(t, "alice", "https://push.example/shared")
	before, err := f.system.ListWebPushSubscriptions(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	var slotAcquired bool
	f.service.sendNotification = func(ctx context.Context, _ []byte, _ *webpush.Subscription, _ *webpush.Options) (*http.Response, error) {
		work, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := f.routing.WithUser(work, "bob", func(db *storage.DB) error {
			slotAcquired = true
			return db.SetSetting(work, "bob", "while_alice_pushes", "ok")
		}); err != nil {
			t.Errorf("push pinned sole database slot: %v", err)
		}
		// Renew the same endpoint and keys while its older send is outstanding.
		f.subscribe(t, "alice", "https://push.example/shared")
		return pushResponse(http.StatusGone), nil
	}
	f.service.handleNewMail(t.Context(), f.event("alice"))
	after, err := f.system.ListWebPushSubscriptions(t.Context(), "alice")
	if err != nil || !slotAcquired || len(after) != 1 || after[0].Revision == before[0].Revision {
		t.Fatalf("renewed registration lost: slot=%v subscriptions=%v err=%v", slotAcquired, after, err)
	}
}

func TestUserStoragePushAcknowledgementCannotDeleteReassignedEndpoint(t *testing.T) {
	f := newUserStorageFixture(t)
	f.subscribe(t, "alice", "https://push.example/shared")
	f.service.sendNotification = func(_ context.Context, _ []byte, _ *webpush.Subscription, _ *webpush.Options) (*http.Response, error) {
		if rec := f.request("alice", http.MethodDelete, "/api/push/subscription", `{"endpoint":"https://push.example/shared"}`); rec.Code != http.StatusOK {
			t.Error(rec.Body.String())
		}
		f.subscribe(t, "bob", "https://push.example/shared")
		return pushResponse(http.StatusGone), nil
	}
	f.service.handleNewMail(t.Context(), f.event("alice"))
	subs, err := f.system.ListWebPushSubscriptions(t.Context(), "bob")
	if err != nil || len(subs) != 1 {
		t.Fatalf("late result deleted another owner's registration: %v %v", subs, err)
	}
}

func TestUserStorageContactBackfillWorkerKeepsOwnersSeparate(t *testing.T) {
	f := newUserStorageFixture(t)
	updates := f.events.Subscribe()
	defer f.events.Unsubscribe(updates)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			if err := db.MergeUISettings(t.Context(), owner, map[string]string{"contacts_auto_create_observed": "true", "contacts_observed_sources": "senders"}); err != nil {
				return err
			}
			return db.UpsertSyncMessages(t.Context(), []storage.SyncMessage{{AccountID: f.accounts[owner].ID, FolderID: owner + "-inbox", RemoteUID: 1, MessageID: "<" + owner + "@example.com>", FromName: owner + " observed", FromEmail: owner + "-observed@example.com", DateSent: time.Now().UTC()}})
		}); err != nil {
			t.Fatal(err)
		}
		if rec := f.request(owner, http.MethodGet, "/api/contacts/search?q=observed", ""); rec.Code != http.StatusOK {
			t.Fatal(rec.Body.String())
		}
	}
	completed := make(map[string]bool)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for len(completed) < 2 {
		select {
		case event := <-updates:
			if event.Type == mail.EventContactBackfill {
				if state, ok := event.Payload["backfill"].(models.ContactBackfillState); ok && !state.InProgress {
					if state.LastError != "" {
						t.Fatal(state.LastError)
					}
					completed[event.UserID] = true
				}
			}
		case <-deadline.C:
			t.Fatal("owner backfills did not finish")
		}
	}
	for _, owner := range []string{"alice", "bob"} {
		rec := f.request(owner, http.MethodGet, "/api/contacts/search?q=observed", "")
		other := "alice"
		if owner == other {
			other = "bob"
		}
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), owner+"-observed@example.com") || strings.Contains(rec.Body.String(), other+"-observed@example.com") {
			t.Fatalf("observed contacts %s: %d %s", owner, rec.Code, rec.Body.String())
		}
	}
	var centralContacts int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM contact_profiles`).Scan(&centralContacts); err != nil || centralContacts != 0 {
		t.Fatalf("backfill wrote central contacts: rows=%d err=%v", centralContacts, err)
	}
}
