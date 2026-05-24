package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestDiscoverCardDAVAddressBooks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			t.Fatalf("method = %s, want PROPFIND", r.Method)
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		switch r.URL.Path {
		case "/.well-known/carddav":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/.well-known/carddav</d:href>
    <d:propstat><d:prop><d:current-user-principal><d:href>/principals/user/</d:href></d:current-user-principal></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>
  </d:response>
</d:multistatus>`))
		case "/principals/user/":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">
  <d:response>
    <d:href>/principals/user/</d:href>
    <d:propstat><d:prop><card:addressbook-home-set><d:href>/addressbooks/user/</d:href></card:addressbook-home-set></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>
  </d:response>
</d:multistatus>`))
		case "/addressbooks/user/":
			if r.Header.Get("Depth") != "1" {
				t.Fatalf("Depth = %q, want 1", r.Header.Get("Depth"))
			}
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">
  <d:response>
    <d:href>/addressbooks/user/</d:href>
    <d:propstat><d:prop><d:displayname>Home</d:displayname><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>
  </d:response>
  <d:response>
    <d:href>/addressbooks/user/default/</d:href>
    <d:propstat><d:prop><d:displayname>Personal</d:displayname><d:resourcetype><d:collection/><card:addressbook/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>
  </d:response>
</d:multistatus>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	books, err := discoverCardDAVAddressBooks(context.Background(), server.URL, "user", "pass")
	if err != nil {
		t.Fatalf("discoverCardDAVAddressBooks() error = %v", err)
	}
	if len(books) != 1 {
		t.Fatalf("len(books) = %d, want 1", len(books))
	}
	if books[0].Name != "Personal" {
		t.Fatalf("book name = %q, want Personal", books[0].Name)
	}
	if books[0].URL != server.URL+"/addressbooks/user/default/" {
		t.Fatalf("book URL = %q, want %q", books[0].URL, server.URL+"/addressbooks/user/default/")
	}
}

func TestNormalizeCardDAVBaseURL(t *testing.T) {
	tests := map[string]string{
		"example.com":        "https://example.com",
		"person@example.com": "https://example.com",
		"http://example.com": "http://example.com",
	}
	for input, want := range tests {
		got, err := normalizeCardDAVBaseURL(input)
		if err != nil {
			t.Fatalf("normalizeCardDAVBaseURL(%q) error = %v", input, err)
		}
		if got != want {
			t.Fatalf("normalizeCardDAVBaseURL(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCardDAVSyncCollectionFetchesChangedAndDeleted(t *testing.T) {
	var sawSync, sawMultiget bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "REPORT" {
			t.Fatalf("method = %s, want REPORT", r.Method)
		}
		data, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		body := string(data)
		switch {
		case strings.Contains(body, "sync-collection"):
			sawSync = true
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:">
  <d:response>
    <d:href>/addressbooks/user/default/alice.vcf</d:href>
    <d:propstat><d:prop><d:getetag>"a2"</d:getetag></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>
  </d:response>
  <d:response>
    <d:href>/addressbooks/user/default/bob.vcf</d:href>
    <d:status>HTTP/1.1 404 Not Found</d:status>
  </d:response>
  <d:sync-token>token-2</d:sync-token>
</d:multistatus>`))
		case strings.Contains(body, "addressbook-multiget"):
			sawMultiget = true
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">
  <d:response>
    <d:href>/addressbooks/user/default/alice.vcf</d:href>
    <d:propstat><d:prop><d:getetag>"a2"</d:getetag><card:address-data>BEGIN:VCARD&#x0A;VERSION:4.0&#x0A;FN:Alice&#x0A;EMAIL:alice@example.com&#x0A;END:VCARD&#x0A;</card:address-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>
  </d:response>
</d:multistatus>`))
		default:
			t.Fatalf("unexpected REPORT body: %s", body)
		}
	}))
	defer server.Close()

	result, err := cardDAVSync(context.Background(), models.ContactSyncConfig{
		AddressBookURL: server.URL + "/addressbooks/user/default/",
		Username:       "user",
		LastSyncToken:  "token-1",
	}, "pass")
	if err != nil {
		t.Fatalf("cardDAVSync() error = %v", err)
	}
	if !sawSync || !sawMultiget {
		t.Fatalf("sawSync=%t sawMultiget=%t, want both true", sawSync, sawMultiget)
	}
	if result.SyncToken != "token-2" {
		t.Fatalf("SyncToken = %q, want token-2", result.SyncToken)
	}
	if len(result.Responses) != 2 {
		t.Fatalf("len(Responses) = %d, want 2", len(result.Responses))
	}
	if result.Responses[0].addressData() == "" {
		t.Fatalf("changed response missing address data")
	}
	if !result.Responses[1].deleted() {
		t.Fatalf("deleted response not marked deleted")
	}
}

func TestCardDAVSyncFallsBackToFullQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		body := string(data)
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		if strings.Contains(body, "sync-collection") {
			http.Error(w, "sync not supported", http.StatusForbidden)
			return
		}
		if !strings.Contains(body, "addressbook-query") {
			t.Fatalf("unexpected fallback body: %s", body)
		}
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">
  <d:response>
    <d:href>/addressbooks/user/default/alice.vcf</d:href>
    <d:propstat><d:prop><d:getetag>"a1"</d:getetag><card:address-data>BEGIN:VCARD&#x0A;VERSION:4.0&#x0A;FN:Alice&#x0A;EMAIL:alice@example.com&#x0A;END:VCARD&#x0A;</card:address-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>
  </d:response>
</d:multistatus>`))
	}))
	defer server.Close()

	result, err := cardDAVSync(context.Background(), models.ContactSyncConfig{
		AddressBookURL: server.URL + "/addressbooks/user/default/",
		Username:       "user",
		LastSyncToken:  "bad-token",
	}, "pass")
	if err != nil {
		t.Fatalf("cardDAVSync() error = %v", err)
	}
	if !result.Fallback {
		t.Fatalf("Fallback = false, want true")
	}
	if len(result.Responses) != 1 {
		t.Fatalf("len(Responses) = %d, want 1", len(result.Responses))
	}
}

func TestPruneMissingCardDAVSourcesRemovesTargetOnly(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	contactID, _, err := db.UpsertSyncedContact(ctx, "default", "acc", "Old Remote", "old@example.com")
	if err != nil {
		t.Fatalf("UpsertSyncedContact() error = %v", err)
	}
	oldHref := "https://dav.example.test/addressbooks/default/old.vcf"
	if err := db.UpsertContactSource(ctx, storage.ContactSource{ContactID: contactID, UserID: "default", Provider: providers.ProviderCardDAV, AccountID: "acc", RemoteID: oldHref}); err != nil {
		t.Fatalf("UpsertContactSource() error = %v", err)
	}

	h := &Handler{db: db}
	if err := h.pruneMissingCardDAVSources(ctx, "default", "acc", map[string]bool{"https://dav.example.test/addressbooks/default/new.vcf": true}); err != nil {
		t.Fatalf("pruneMissingCardDAVSources() error = %v", err)
	}
	source, err := db.GetContactSourceByRemoteID(ctx, "default", providers.ProviderCardDAV, "acc", oldHref)
	if err != nil {
		t.Fatalf("GetContactSourceByRemoteID() error = %v", err)
	}
	if source != nil {
		t.Fatalf("stale source still exists: %#v", source)
	}
	contact, err := db.GetContact(ctx, "default", contactID)
	if err != nil || contact == nil {
		t.Fatalf("GetContact() = %#v, %v; want contact kept", contact, err)
	}
	for _, target := range contact.SaveTargets {
		if target == "account:acc" {
			t.Fatalf("stale account target still present: %#v", contact.SaveTargets)
		}
	}
}

func TestCardDAVPutUsesPreconditionHeaders(t *testing.T) {
	var createChecked, updateChecked bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("method = %s, want PUT", r.Method)
		}
		switch r.URL.Path {
		case "/new.vcf":
			if r.Header.Get("If-None-Match") != "*" {
				t.Fatalf("If-None-Match = %q, want *", r.Header.Get("If-None-Match"))
			}
			createChecked = true
			w.Header().Set("ETag", `"new"`)
			w.WriteHeader(http.StatusCreated)
		case "/existing.vcf":
			if r.Header.Get("If-Match") != `"old"` {
				t.Fatalf("If-Match = %q, want old etag", r.Header.Get("If-Match"))
			}
			updateChecked = true
			w.Header().Set("ETag", `"updated"`)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cfg := models.ContactSyncConfig{AddressBookURL: server.URL + "/", Username: "user"}
	etag, err := cardDAVPut(context.Background(), cfg, "pass", server.URL+"/new.vcf", "", []byte("BEGIN:VCARD\nEND:VCARD\n"))
	if err != nil {
		t.Fatalf("create cardDAVPut() error = %v", err)
	}
	if etag != `"new"` {
		t.Fatalf("create etag = %q, want new", etag)
	}
	etag, err = cardDAVPut(context.Background(), cfg, "pass", server.URL+"/existing.vcf", `"old"`, []byte("BEGIN:VCARD\nEND:VCARD\n"))
	if err != nil {
		t.Fatalf("update cardDAVPut() error = %v", err)
	}
	if etag != `"updated"` {
		t.Fatalf("update etag = %q, want updated", etag)
	}
	if !createChecked || !updateChecked {
		t.Fatalf("createChecked=%t updateChecked=%t, want both true", createChecked, updateChecked)
	}
}

func TestCardDAVPutConflictReturnsTypedError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "etag mismatch", http.StatusPreconditionFailed)
	}))
	defer server.Close()

	_, err := cardDAVPut(context.Background(), models.ContactSyncConfig{AddressBookURL: server.URL + "/"}, "", server.URL+"/contact.vcf", `"stale"`, []byte("BEGIN:VCARD\nEND:VCARD\n"))
	if !isCardDAVStatus(err, http.StatusPreconditionFailed) {
		t.Fatalf("cardDAVPut() error = %T %v, want precondition status", err, err)
	}
}

func TestCardDAVDeleteNotFoundIsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method = %s, want DELETE", r.Method)
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	if err := cardDAVDelete(context.Background(), models.ContactSyncConfig{}, "", server.URL+"/missing.vcf", `"old"`); err != nil {
		t.Fatalf("cardDAVDelete() error = %v, want nil", err)
	}
}

func TestDeleteCardDAVContactsByEmailDeletesTrackedSource(t *testing.T) {
	ctx := context.Background()
	var deletedPath, ifMatch string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method = %s, want DELETE", r.Method)
		}
		deletedPath = r.URL.Path
		ifMatch = r.Header.Get("If-Match")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	db, store := newCardDAVDeleteTestStore(t)
	if _, err := db.Write().Exec(`INSERT INTO accounts (id, user_id, email_address, display_name) VALUES ('acc', 'default', 'me@example.com', 'Me')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := store.SaveContactSyncConfig(ctx, "default", "acc", models.ContactSyncConfig{
		AccountID:      "acc",
		UserID:         "default",
		Provider:       providers.ProviderCardDAV,
		Enabled:        true,
		AddressBookURL: server.URL + "/book/",
		AddressBooks:   []models.ContactAddressBook{{Name: "Personal", URL: server.URL + "/book/", Default: true}},
		Username:       "user",
	}, "pass"); err != nil {
		t.Fatalf("SaveContactSyncConfig() error = %v", err)
	}
	cfg, err := store.GetContactSyncConfig(ctx, "default", "acc")
	if err != nil {
		t.Fatalf("GetContactSyncConfig() error = %v", err)
	}
	contactID, _, err := db.UpsertSyncedContact(ctx, "default", "acc", "Alice", "alice@example.com")
	if err != nil {
		t.Fatalf("UpsertSyncedContact() error = %v", err)
	}
	if err := db.UpsertContactSource(ctx, storage.ContactSource{
		ContactID:     contactID,
		UserID:        "default",
		Provider:      providers.ProviderCardDAV,
		AccountID:     "acc",
		AddressBookID: cfg.AddressBooks[0].ID,
		RemoteID:      server.URL + "/book/alice.vcf",
		Etag:          `"old"`,
	}); err != nil {
		t.Fatalf("UpsertContactSource() error = %v", err)
	}

	h := &Handler{db: db, accountStore: store}
	if err := h.deleteCardDAVContactsByEmail(ctx, "default", "acc", "alice@example.com"); err != nil {
		t.Fatalf("deleteCardDAVContactsByEmail() error = %v", err)
	}
	if deletedPath != "/book/alice.vcf" || ifMatch != `"old"` {
		t.Fatalf("DELETE path=%q If-Match=%q, want alice href and etag", deletedPath, ifMatch)
	}
	source, err := db.GetContactSourceByRemoteID(ctx, "default", providers.ProviderCardDAV, "acc", server.URL+"/book/alice.vcf")
	if err != nil {
		t.Fatalf("GetContactSourceByRemoteID() error = %v", err)
	}
	if source != nil {
		t.Fatalf("tracked source still exists: %#v", source)
	}
}

func TestDeleteCardDAVContactsByEmailSearchFallbackExactMatch(t *testing.T) {
	ctx := context.Background()
	deleted := make(map[string]bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "REPORT":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "alice@example.com") {
				t.Fatalf("REPORT body missing email filter: %s", string(body))
			}
			w.Header().Set("Content-Type", "application/xml; charset=utf-8")
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="utf-8"?>
<d:multistatus xmlns:d="DAV:" xmlns:card="urn:ietf:params:xml:ns:carddav">
  <d:response>
    <d:href>/book/alice.vcf</d:href>
    <d:propstat><d:prop><d:getetag>"a1"</d:getetag><card:address-data>BEGIN:VCARD&#x0A;VERSION:4.0&#x0A;FN:Alice&#x0A;EMAIL:alice@example.com&#x0A;END:VCARD&#x0A;</card:address-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>
  </d:response>
  <d:response>
    <d:href>/book/bob.vcf</d:href>
    <d:propstat><d:prop><d:getetag>"b1"</d:getetag><card:address-data>BEGIN:VCARD&#x0A;VERSION:4.0&#x0A;FN:Bob&#x0A;EMAIL:bob@example.com&#x0A;END:VCARD&#x0A;</card:address-data></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat>
  </d:response>
</d:multistatus>`))
		case http.MethodDelete:
			deleted[r.URL.Path] = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	db, store := newCardDAVDeleteTestStore(t)
	if _, err := db.Write().Exec(`INSERT INTO accounts (id, user_id, email_address, display_name) VALUES ('acc', 'default', 'me@example.com', 'Me')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if err := store.SaveContactSyncConfig(ctx, "default", "acc", models.ContactSyncConfig{
		AccountID:      "acc",
		UserID:         "default",
		Provider:       providers.ProviderCardDAV,
		Enabled:        true,
		AddressBookURL: server.URL + "/book/",
		AddressBooks:   []models.ContactAddressBook{{Name: "Personal", URL: server.URL + "/book/", Default: true}},
		Username:       "user",
	}, "pass"); err != nil {
		t.Fatalf("SaveContactSyncConfig() error = %v", err)
	}

	h := &Handler{db: db, accountStore: store}
	if err := h.deleteCardDAVContactsByEmail(ctx, "default", "acc", "alice@example.com"); err != nil {
		t.Fatalf("deleteCardDAVContactsByEmail() error = %v", err)
	}
	if !deleted["/book/alice.vcf"] {
		t.Fatalf("alice.vcf was not deleted")
	}
	if deleted["/book/bob.vcf"] {
		t.Fatalf("bob.vcf was deleted despite non-matching email")
	}
}

func newCardDAVDeleteTestStore(t *testing.T) (*storage.DB, *config.AccountStore) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	store, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewAccountStore() error = %v", err)
	}
	return db, store
}
