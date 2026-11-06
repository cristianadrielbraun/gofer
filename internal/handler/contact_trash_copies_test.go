package handler

import (
	"context"
	"encoding/json"
	"io"
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

// A synced contact deleted in one of its places shows in the sidebar's Por
// resolver count until it is kept without that place.
func TestSharedUnresolvedContactCountAndKeep(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	saved, err := db.SaveContact(ctx, "default", models.Contact{Name: "Ines Varga", Email: "ines@example.com", StoredIn: "local", GoferSyncEnabled: true, SaveTargets: []string{"account:acc"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertContactSource(ctx, storage.ContactSource{UserID: "default", ContactID: saved.ID, Provider: "gmail", AccountID: "acc", RemoteID: "people/ines"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveDeletedContactSource(ctx, "default", "gmail", "acc", "people/ines"); err != nil {
		t.Fatal(err)
	}
	count := func() string {
		r := httptest.NewRequest("GET", "/contacts/unresolved-count", nil)
		r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "default"}))
		w := httptest.NewRecorder()
		h.handleUnresolvedContactCount(w, r)
		return strings.TrimSpace(w.Body.String())
	}
	if got := count(); !strings.Contains(got, ">1<") {
		t.Fatal("count", got)
	}
	// It can't be edited while it waits.
	edit := httptest.NewRequest("POST", "/api/contacts?id="+url.QueryEscape(saved.ID), strings.NewReader(url.Values{"name": {"Ines V"}, "email": {"ines@example.com"}, "sync_enabled": {"on"}}.Encode()))
	edit.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	edit = edit.WithContext(auth.ContextWithUser(edit.Context(), &auth.User{ID: "default"}))
	edited := httptest.NewRecorder()
	h.handleSaveContact(edited, edit)
	if edited.Code != http.StatusConflict {
		t.Fatal("edited while to resolve", edited.Code, edited.Body.String())
	}
	resolver := httptest.NewRequest("GET", "/contacts/resolver", nil)
	resolver = resolver.WithContext(auth.ContextWithUser(resolver.Context(), &auth.User{ID: "default"}))
	dialog := httptest.NewRecorder()
	h.handleContactResolver(dialog, resolver)
	if dialog.Code != 200 || !strings.Contains(dialog.Body.String(), "Ines Varga") || !strings.Contains(dialog.Body.String(), `data-contacts-resolver-case="`+saved.ID+`"`) {
		t.Fatal("resolver", dialog.Code, dialog.Body.String())
	}
	keep := func() string {
		w := httptest.NewRecorder()
		h.handleKeepContactsWithoutDeletedPlaces(w, contactIDsRequest(t, "/api/contacts/unresolved/keep", saved.ID))
		if w.Code != 200 {
			t.Fatal("keep", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	if got := keep(); !strings.Contains(got, `"kept":1`) {
		t.Fatal("keep", got)
	}
	if got := count(); got != "" {
		t.Fatal("count after keep", got)
	}
	if got := keep(); !strings.Contains(got, `"kept":0`) {
		t.Fatal("keep again", got)
	}
}

// Deleted by its home, a contact to resolve is kept in the main location
// chosen in the resolver; a choice that no longer fits leaves it to resolve.
func TestSharedKeepContactWithoutItsHomeInTheChosenPlace(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	saved, err := db.SaveContact(ctx, "default", models.Contact{Name: "Teo Brand", Email: "teo@example.com", StoredIn: "account:acc", GoferSyncEnabled: true, SaveTargets: []string{"local"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertContactSource(ctx, storage.ContactSource{UserID: "default", ContactID: saved.ID, Provider: "gmail", AccountID: "acc", RemoteID: "people/teo"}); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveDeletedContactSource(ctx, "default", "gmail", "acc", "people/teo"); err != nil {
		t.Fatal(err)
	}
	resolver := httptest.NewRequest("GET", "/contacts/resolver", nil)
	resolver = resolver.WithContext(auth.ContextWithUser(resolver.Context(), &auth.User{ID: "default"}))
	dialog := httptest.NewRecorder()
	h.handleContactResolver(dialog, resolver)
	if body := dialog.Body.String(); dialog.Code != 200 || !strings.Contains(body, `name="home:`+saved.ID+`"`) || !strings.Contains(body, "data-contacts-resolver-home") {
		t.Fatal("resolver offers no new main location", dialog.Code, body)
	}
	keep := func(home string) string {
		form := url.Values{"id": {saved.ID}, "home:" + saved.ID: {home}}
		r := httptest.NewRequest("POST", "/api/contacts/unresolved/keep", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "default"}))
		w := httptest.NewRecorder()
		h.handleKeepContactsWithoutDeletedPlaces(w, r)
		if w.Code != 200 {
			t.Fatal("keep", w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	if got := keep("account:acc"); !strings.Contains(got, `"kept":0`) || !strings.Contains(got, `"stale":1`) {
		t.Fatal("kept in the place that deleted it", got)
	}
	if got := keep("local"); !strings.Contains(got, `"kept":1`) || !strings.Contains(got, `"stale":0`) {
		t.Fatal("keep", got)
	}
	if contact, err := db.GetContact(ctx, "default", saved.ID); err != nil || contact == nil || contact.StoredIn != "local" {
		t.Fatal("home", contact, err)
	}
}

// Restored to another place, a contact is written there, and lives there.
func TestSharedRestoreToAnotherPlaceWritesItThere(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	saved, err := db.SaveContact(ctx, "default", models.Contact{Name: "Lena Vos", Email: "lena@example.com", StoredIn: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.TrashContacts(ctx, "default", []string{saved.ID}); err != nil {
		t.Fatal(err)
	}
	restore := func(place string) *httptest.ResponseRecorder {
		r := contactIDsRequest(t, "/api/contacts/trash/restore", saved.ID)
		form := url.Values{"id": {saved.ID}, "place": {place}}
		r.Body = io.NopCloser(strings.NewReader(form.Encode()))
		r.ContentLength = int64(len(form.Encode()))
		w := httptest.NewRecorder()
		h.handleRestoreContacts(w, r)
		return w
	}
	if w := restore("account:someone-else"); w.Code != http.StatusBadRequest {
		t.Fatal("unknown place accepted", w.Code, w.Body.String())
	}
	if w := restore("account:acc"); w.Code != 200 || !strings.Contains(w.Body.String(), `"restored":1`) {
		t.Fatal("restore", w.Code, w.Body.String())
	}
	contact, err := db.GetContact(ctx, "default", saved.ID)
	if err != nil || contact == nil || contact.StoredIn != "account:acc" {
		t.Fatal("home", contact, err)
	}
	ops, err := db.ClaimContactSyncOperations(ctx, 5, time.Minute)
	if err != nil || len(ops) != 1 || ops[0].Payload.StoreTarget != "account:acc" {
		t.Fatal("written there", ops, err)
	}
}
