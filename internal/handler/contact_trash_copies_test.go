package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func contactIDsRequest(t *testing.T, path string, ids ...string) *http.Request {
	t.Helper()
	form := url.Values{"id": ids}
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Accept", "application/json")
	return r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "default"}))
}

// Moving a contact to the Trash deletes it on its account first, and
// restoring it writes it back there.
func TestSharedTrashDeletesCopiesAndRestoreWritesBack(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	var creates, deletes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/people:searchContacts":
			_, _ = w.Write([]byte(`{"results":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/people:createContact":
			creates++
			_, _ = w.Write([]byte(`{"resourceName":"people/back","etag":"etag-2"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/people/first:deleteContact":
			deletes++
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", 400)
		}
	}))
	defer server.Close()
	previousBase := googlePeopleAPIBaseURL
	googlePeopleAPIBaseURL = server.URL
	t.Cleanup(func() { googlePeopleAPIBaseURL = previousBase })

	id, _, err := db.UpsertSyncedContact(ctx, "default", "acc", "Ada Gmail", "ada@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertContactSource(ctx, storage.ContactSource{UserID: "default", ContactID: id, Provider: "gmail", AccountID: "acc", RemoteID: "people/first", Etag: "etag-1"}); err != nil {
		t.Fatal(err)
	}

	places := httptest.NewRecorder()
	h.handleContactTrashPlaces(places, contactIDsRequest(t, "/api/contacts/trash/places", id))
	var copies struct {
		Places   []string `json:"places"`
		Contacts int      `json:"contacts"`
	}
	if err := json.Unmarshal(places.Body.Bytes(), &copies); err != nil || len(copies.Places) != 1 || copies.Contacts != 1 {
		t.Fatal("places", places.Body.String(), err)
	}

	trash := httptest.NewRecorder()
	h.handleTrashContacts(trash, contactIDsRequest(t, "/api/contacts/bulk-delete", id))
	if trash.Code != 200 || !strings.Contains(trash.Body.String(), `"trashed":1`) || deletes != 1 {
		t.Fatal("trash", trash.Code, trash.Body.String(), deletes)
	}
	if inTrash, err := db.ContactInTrash(ctx, "default", id); err != nil || !inTrash {
		t.Fatal("not in the Trash", inTrash, err)
	}
	if stored, err := db.ContactStoredIn(ctx, "default", "ada@example.com", "account:acc"); err != nil || stored {
		t.Fatal("the Gmail copy stayed", stored, err)
	}

	restore := httptest.NewRecorder()
	h.handleRestoreContacts(restore, contactIDsRequest(t, "/api/contacts/trash/restore", id))
	if restore.Code != 200 {
		t.Fatal("restore", restore.Code, restore.Body.String())
	}
	ops, err := db.ClaimContactSyncOperations(ctx, 5, time.Minute)
	if err != nil || len(ops) != 1 || ops[0].Payload.StoreTarget != "account:acc" {
		t.Fatal("write back", ops, err)
	}
	h.processContactSyncOperation(ctx, ops[0])
	if stored, err := db.ContactStoredIn(ctx, "default", "ada@example.com", "account:acc"); err != nil || !stored || creates != 1 {
		t.Fatal("written back", stored, creates, err)
	}
}

// Taking a place out of a contact's Gofer Sync deletes its copy there; its
// home keeps it.
func TestSharedDroppedSyncPlaceDeletesItsCopy(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	deletes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete && r.URL.Path == "/people/copy:deleteContact" {
			deletes++
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	previousBase := googlePeopleAPIBaseURL
	googlePeopleAPIBaseURL = server.URL
	t.Cleanup(func() { googlePeopleAPIBaseURL = previousBase })

	saved, err := db.SaveContact(ctx, "default", models.Contact{Name: "Ada Local", Email: "ada@example.com", StoredIn: "local", GoferSyncEnabled: true, SaveTargets: []string{"account:acc"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertContactSource(ctx, storage.ContactSource{UserID: "default", ContactID: saved.ID, Provider: "gmail", AccountID: "acc", RemoteID: "people/copy", Etag: "etag-1"}); err != nil {
		t.Fatal(err)
	}

	form := url.Values{"name": {"Ada Local"}, "email": {"ada@example.com"}, "sync_enabled": {"on"}, "save_targets": {""}}
	r := httptest.NewRequest("POST", "/api/contacts?id="+url.QueryEscape(saved.ID), strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Accept", "application/json")
	r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "default"}))
	w := httptest.NewRecorder()
	h.handleSaveContact(w, r)
	if w.Code != 200 || strings.Contains(w.Body.String(), "contact_place_remove_failed") {
		t.Fatal("save", w.Code, w.Body.String())
	}
	if deletes != 1 {
		t.Fatal("the dropped place kept its copy", deletes)
	}
	if stored, err := db.ContactStoredIn(ctx, "default", "ada@example.com", "account:acc"); err != nil || stored {
		t.Fatal("the Gmail copy stayed", stored, err)
	}
	if stored, err := db.ContactStoredIn(ctx, "default", "ada@example.com", "local"); err != nil || !stored {
		t.Fatal("the home lost its copy", stored, err)
	}
}
