package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
	"golang.org/x/oauth2"
)

// A store operation writes an imported contact once to its one place, with
// Gofer Sync off and no membership there; a sync operation for the same
// contact still writes nothing.
func TestUserContactStoreWritesOnceWithoutGoferSync(t *testing.T) {
	var creates int
	var mu sync.Mutex
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/people:searchContacts":
			_, _ = w.Write([]byte(`{"results":[]}`))
		case "/people:createContact":
			creates++
			_, _ = w.Write([]byte(`{"resourceName":"people/stored","etag":"v1"}`))
		default:
			http.Error(w, "unexpected provider path", 400)
		}
	}))
	defer api.Close()
	previous := http.DefaultTransport
	target, _ := url.Parse(api.URL)
	http.DefaultTransport = userContactPushTransport{base: previous, target: target}
	defer func() { http.DefaultTransport = previous }()
	allowLocalDAVServers(t)
	system, err := storage.New(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer system.Close()
	if _, err := system.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('alice','alice','alice')`); err != nil {
		t.Fatal(err)
	}
	stores, err := storage.NewUserStores(system, storage.UserStoreOptions{MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer stores.Close(context.Background())
	routing, err := storage.NewAccountRouting(stores)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	accounts, err := config.NewUserAccountStore(routing, key)
	if err != nil {
		t.Fatal(err)
	}
	account, err := accounts.CreateAccount(t.Context(), "alice", providers.GmailAccountRequest("alice@example.com", "alice", "subject"))
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.WithAccountForUser(t.Context(), "alice", account.ID, func(_ *config.AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET email_sync_enabled=0 WHERE id=?`, account.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	imap, err := mail.NewUserIMAP(ctx, accounts, store.NewBlobStore(t.TempDir()), mail.NewSyncOrchestrator(system, nil, nil, nil).Events())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); imap.Wait() }()
	credentials, err := mailauth.NewUserCredentials(ctx, &mailauth.Config{GoogleClient: &oauth2.Config{ClientID: "client", Endpoint: oauth2.Endpoint{TokenURL: api.URL + "/token"}}}, routing, key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); credentials.Wait() }()
	expires := time.Now().Add(time.Hour)
	if err := credentials.UpsertForUser(ctx, "alice", account.ID, "google", "subject", "access", "refresh", "Bearer", &expires, "https://www.googleapis.com/auth/contacts"); err != nil {
		t.Fatal(err)
	}
	h := &Handler{userStorage: routing, userAccounts: accounts, userIMAP: imap, userCredentials: credentials}

	var profile string
	if err := accounts.WithUser(ctx, "alice", func(_ *config.AccountStore, db *storage.DB) error {
		contact, err := db.SaveContact(ctx, "alice", models.Contact{Email: "stored@example.com", Phone: "333", SaveTargets: []string{"local"}})
		profile = contact.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Sync off: an ordinary sync operation is dropped without a write.
	if err := accounts.WithUser(ctx, "alice", func(_ *config.AccountStore, db *storage.DB) error {
		_, err := db.EnqueueContactSyncOperation(ctx, "alice", models.Contact{ID: profile, Email: "stored@example.com"}, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.ProcessUserContactSyncOperations(ctx, "alice", 1); err != nil {
		t.Fatal("sync job", err)
	}
	mu.Lock()
	early := creates
	mu.Unlock()
	if early != 0 {
		t.Fatal("a sync job wrote a contact without Gofer Sync")
	}

	id, err := accounts.EnqueueContactStore(ctx, "alice", profile, "account:"+account.ID)
	if err != nil || id == "" {
		t.Fatal("enqueue store", id, err)
	}
	if count, err := h.ProcessUserContactSyncOperations(ctx, "alice", 1); err != nil || count != 1 {
		t.Fatal("store job", count, err)
	}
	mu.Lock()
	gotCreates := creates
	mu.Unlock()
	if gotCreates != 1 {
		t.Fatal("store job writes", gotCreates)
	}
	if err := accounts.WithUser(ctx, "alice", func(_ *config.AccountStore, db *storage.DB) error {
		source, err := db.GetContactSource(ctx, "alice", profile, "gmail", account.ID)
		if err != nil {
			return err
		}
		if source == nil || source.RemoteID != "people/stored" {
			t.Fatal("store job kept no copy", source)
		}
		stored, err := db.ContactStoredIn(ctx, "alice", "stored@example.com", "account:"+account.ID)
		if err != nil || !stored {
			t.Fatal("stored in the account", stored, err)
		}
		progress, err := db.ContactOperationProgress(ctx, "alice", []string{id})
		if err != nil || progress.Done != 1 {
			t.Fatal("store progress", progress, err)
		}
		contact, err := db.GetContact(ctx, "alice", profile)
		if err != nil || contact == nil || contact.GoferSyncEnabled {
			t.Fatal("store job turned Gofer Sync on", contact, err)
		}
		var events int
		err = db.Read().QueryRow(`SELECT COUNT(*) FROM contact_activity_events WHERE event_type IN ('contact_sync_started','contact_synced')`).Scan(&events)
		if events != 0 {
			t.Fatal("store job logged Gofer Sync activity", events)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// The shared worker stores an imported contact the same way: one write to
// its place, Gofer Sync left off.
func TestContactStoreOperationWritesOnceInSharedMode(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	contact, err := db.SaveContact(ctx, "default", models.Contact{Name: "Stored Shared", Email: "shared-store@example.com", SaveTargets: []string{"local"}})
	if err != nil {
		t.Fatal(err)
	}
	var creates int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/people:searchContacts":
			_, _ = w.Write([]byte(`{"results":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/people:createContact":
			creates++
			_, _ = w.Write([]byte(`{"resourceName":"people/shared-stored","etag":"etag-1"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", 400)
		}
	}))
	defer server.Close()
	previousBase := googlePeopleAPIBaseURL
	googlePeopleAPIBaseURL = server.URL
	t.Cleanup(func() { googlePeopleAPIBaseURL = previousBase })

	id, err := db.EnqueueContactStoreOperation(ctx, "default", contact, "account:acc")
	if err != nil || id == "" {
		t.Fatal("enqueue store", id, err)
	}
	ops, err := db.ClaimContactSyncOperations(ctx, 1, time.Minute)
	if err != nil || len(ops) != 1 {
		t.Fatal("claim", ops, err)
	}
	h.processContactSyncOperation(ctx, ops[0])
	if creates != 1 {
		t.Fatal("store writes", creates)
	}
	source, err := db.GetContactSource(ctx, "default", contact.ID, providers.ProviderGmail, "acc")
	if err != nil || source == nil || source.RemoteID != "people/shared-stored" {
		t.Fatal("store kept no copy", source, err)
	}
	progress, err := db.ContactOperationProgress(ctx, "default", []string{id})
	if err != nil || progress.Done != 1 {
		t.Fatal("store progress", progress, err)
	}
	saved, err := db.GetContact(ctx, "default", contact.ID)
	if err != nil || saved == nil || saved.GoferSyncEnabled {
		t.Fatal("store turned Gofer Sync on", saved, err)
	}
}

func sharedContactImportRequest(t *testing.T, fields map[string]string, wire string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for name, value := range fields {
		_ = form.WriteField(name, value)
	}
	file, err := form.CreateFormFile("vcard", "contacts.vcf")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(file, wire)
	_ = form.Close()
	r := httptest.NewRequest("POST", "/api/contacts/import", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.Header.Set("Accept", "application/x-ndjson, application/json")
	return r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "default"}))
}

// Importing into an account saves the contacts in Gofer as imported, then
// writes them to the account while the progress says so; contacts already
// there can be left as they are.
func TestSharedContactImportStoresInAccount(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	var creates int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/people:searchContacts":
			_, _ = w.Write([]byte(`{"results":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/people:createContact":
			creates++
			_, _ = fmt.Fprintf(w, `{"resourceName":"people/imported-%d","etag":"etag-1"}`, creates)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", 400)
		}
	}))
	defer server.Close()
	previousBase := googlePeopleAPIBaseURL
	googlePeopleAPIBaseURL = server.URL
	t.Cleanup(func() { googlePeopleAPIBaseURL = previousBase })
	wire := "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Ada Example\r\nEMAIL:ada@example.com\r\nEND:VCARD\r\n" +
		"BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Bo Example\r\nEMAIL:bo@example.com\r\nEND:VCARD\r\n"

	// Ada is already in Gofer, observed, but not in the account: for this
	// import she is new.
	if err := db.UpsertObservedContact(ctx, "default", "Ada Seen", "ada@example.com", time.Now()); err != nil {
		t.Fatal(err)
	}
	preview := httptest.NewRecorder()
	h.handleImportContacts(preview, sharedContactImportRequest(t, map[string]string{"preview": "1", "store_in": "account:acc"}, wire))
	if preview.Code != 200 || !strings.Contains(preview.Body.String(), `"existing":0`) {
		t.Fatal("preview into the account", preview.Code, preview.Body.String())
	}

	response := httptest.NewRecorder()
	h.handleImportContacts(response, sharedContactImportRequest(t, map[string]string{"store_in": "account:acc", "existing": "skip"}, wire))
	body := response.Body.String()
	if response.Code != 200 || !strings.Contains(body, `"phase":"writing","done":2,"total":2`) || !strings.Contains(body, `"added":2,"updated":0,"skipped":0,"written":2`) {
		t.Fatal("import into the account", response.Code, body)
	}
	if creates != 2 {
		t.Fatal("account writes", creates)
	}
	for _, email := range []string{"ada@example.com", "bo@example.com"} {
		id, err := db.ContactInPlace(ctx, "default", email, "account:acc")
		if err != nil || id == "" {
			t.Fatal("stored in the account", email, id, err)
		}
		profile, err := db.GetContactProfile(ctx, "default", id)
		if err != nil || profile == nil {
			t.Fatal("imported profile", email, err)
		}
		contact, err := db.GetContact(ctx, "default", id)
		if err != nil || contact == nil || contact.GoferSyncEnabled {
			t.Fatal("import turned Gofer Sync on", email, contact, err)
		}
		// Both are new contacts on the account, imported; the Ada observed
		// in Gofer Local is another contact, left as she was.
		if profile.Origin != models.ContactOriginImported || profile.Home != "account:acc" {
			t.Fatal("imported contact", email, profile.Origin, profile.Home)
		}
	}
	if observed, err := db.ContactInPlace(ctx, "default", "ada@example.com", "local"); err != nil || observed == "" {
		t.Fatal("the observed Ada is gone", observed, err)
	} else if profile, err := db.GetContactProfile(ctx, "default", observed); err != nil || profile.Origin != "observed" || profile.Home != "local" {
		t.Fatal("the observed Ada changed", profile, err)
	}

	// Again: both are there now, and left as they are.
	preview = httptest.NewRecorder()
	h.handleImportContacts(preview, sharedContactImportRequest(t, map[string]string{"preview": "1", "store_in": "account:acc"}, wire))
	if !strings.Contains(preview.Body.String(), `"existing":2`) {
		t.Fatal("second preview", preview.Body.String())
	}
	response = httptest.NewRecorder()
	h.handleImportContacts(response, sharedContactImportRequest(t, map[string]string{"store_in": "account:acc", "existing": "skip"}, wire))
	if !strings.Contains(response.Body.String(), `"added":0,"updated":0,"skipped":2,"written":0`) || creates != 2 {
		t.Fatal("second import", response.Body.String(), creates)
	}

	// With Gofer Sync on the contact syncs, with the store place and the
	// other places as its sync locations, and Gofer Sync writes it.
	cy := "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Cy Example\r\nEMAIL:cy@example.com\r\nEND:VCARD\r\n"
	response = httptest.NewRecorder()
	h.handleImportContacts(response, sharedContactImportRequest(t, map[string]string{"store_in": "account:acc", "gofer_sync": "1", "sync_with": "local"}, cy))
	if !strings.Contains(response.Body.String(), `"added":1,"updated":0,"skipped":0,"written":1`) || creates != 3 {
		t.Fatal("import with Gofer Sync", response.Body.String(), creates)
	}
	profile, err := db.FindContactProfileByIdentity(ctx, "default", "email", "cy@example.com")
	if err != nil || profile == nil {
		t.Fatal("synced import", err)
	}
	synced, err := db.GetContact(ctx, "default", profile.ID)
	if err != nil || synced == nil || !synced.GoferSyncEnabled || !slices.Contains(synced.SaveTargets, "local") || !slices.Contains(synced.SaveTargets, "account:acc") {
		t.Fatal("synced import places", synced, err)
	}

	// A place the account doesn't have is refused.
	response = httptest.NewRecorder()
	h.handleImportContacts(response, sharedContactImportRequest(t, map[string]string{"store_in": "account:missing"}, wire))
	if response.Code != 400 {
		t.Fatal("unknown place", response.Code, response.Body.String())
	}
}

// A new contact is created where the form says it lives: on an account it is
// written there, with Gofer Sync off; a place that isn't the user's leaves it
// in Gofer Local.
func TestSharedNewContactStoredInAccount(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	var creates int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/people:searchContacts":
			_, _ = w.Write([]byte(`{"results":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/people:createContact":
			creates++
			_, _ = w.Write([]byte(`{"resourceName":"people/new-home","etag":"etag-1"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", 400)
		}
	}))
	defer server.Close()
	previousBase := googlePeopleAPIBaseURL
	googlePeopleAPIBaseURL = server.URL
	t.Cleanup(func() { googlePeopleAPIBaseURL = previousBase })

	save := func(storedIn, email string) string {
		form := url.Values{"name": {"New Home"}, "email": {email}, "stored_in": {storedIn}}
		r := httptest.NewRequest("POST", "/api/contacts", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "default"}))
		w := httptest.NewRecorder()
		h.handleSaveContact(w, r)
		if w.Code != http.StatusSeeOther {
			t.Fatal("save", w.Code, w.Body.String())
		}
		profile, err := db.FindContactProfileByIdentity(ctx, "default", "email", email)
		if err != nil || profile == nil {
			t.Fatal("saved profile", err)
		}
		return profile.Home
	}
	if home := save("account:acc", "new-home@example.com"); home != "account:acc" {
		t.Fatal("home", home)
	}
	ops, err := db.ClaimContactSyncOperations(ctx, 5, time.Minute)
	if err != nil || len(ops) != 1 || ops[0].Payload.StoreTarget != "account:acc" {
		t.Fatal("store operation", ops, err)
	}
	h.processContactSyncOperation(ctx, ops[0])
	if stored, err := db.ContactStoredIn(ctx, "default", "new-home@example.com", "account:acc"); err != nil || !stored || creates != 1 {
		t.Fatal("written to the account", stored, creates, err)
	}
	if stored, err := db.ContactStoredIn(ctx, "default", "new-home@example.com", "local"); err != nil || stored {
		t.Fatal("also given a Local copy", stored, err)
	}
	if home := save("account:someone-else", "elsewhere@example.com"); home != "local" {
		t.Fatal("foreign place home", home)
	}
}
