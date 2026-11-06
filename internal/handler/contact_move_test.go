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
)

func placeContactRequest(t *testing.T, h *Handler, id, action, place string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/contacts/"+id+"/"+action, strings.NewReader(url.Values{"place": {place}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Accept", "application/json")
	r.SetPathValue("id", id)
	r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "default"}))
	w := httptest.NewRecorder()
	if action == "move" {
		h.handleMoveContact(w, r)
	} else {
		h.handleDuplicateContact(w, r)
	}
	return w
}

// Move writes the contact to its new place before deleting it where it
// was, and keeps its origin and the mail seen for it; a failed write leaves
// it where it was. Duplicate makes an independent contact in another place.
func TestSharedMoveAndDuplicateContact(t *testing.T) {
	ctx := context.Background()
	h, db := newGmailAPITestHandler(t, ctx)
	var creates, fail int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/people:searchContacts":
			_, _ = w.Write([]byte(`{"results":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/people:createContact":
			if fail > 0 {
				http.Error(w, `{"error":{"code":500,"message":"backend error"}}`, 500)
				return
			}
			creates++
			_, _ = w.Write([]byte(`{"resourceName":"people/moved","etag":"etag-1"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", 400)
		}
	}))
	defer server.Close()
	previousBase := googlePeopleAPIBaseURL
	googlePeopleAPIBaseURL = server.URL
	t.Cleanup(func() { googlePeopleAPIBaseURL = previousBase })

	if err := db.UpsertObservedContact(ctx, "default", "Ada Seen", "ada@example.com", time.Now()); err != nil {
		t.Fatal(err)
	}
	original, err := db.ContactInPlace(ctx, "default", "ada@example.com", "local")
	if err != nil || original == "" {
		t.Fatal("observed contact", err)
	}

	// The write fails: Ada stays in Gofer Local and no copy is left.
	fail = 1
	if w := placeContactRequest(t, h, original, "move", "account:acc"); w.Code != http.StatusBadGateway {
		t.Fatal("failed move", w.Code, w.Body.String())
	}
	if id, _ := db.ContactInPlace(ctx, "default", "ada@example.com", "local"); id != original {
		t.Fatal("failed move lost the contact", id)
	}
	var count int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM contact_profiles WHERE is_deleted = 0 AND primary_email = 'ada@example.com'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("failed move left a copy", count, err)
	}

	fail = 0
	w := placeContactRequest(t, h, original, "move", "account:acc")
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 {
		t.Fatal("move", w.Code, w.Body.String(), err)
	}
	moved, _ := result["contact_id"].(string)
	profile, err := db.GetContactProfile(ctx, "default", moved)
	if err != nil || profile == nil || profile.Home != "account:acc" || profile.Origin != "observed" || creates != 1 {
		t.Fatal("moved contact", profile, creates, err)
	}
	if gone, err := db.GetContact(ctx, "default", original); err != nil || gone != nil {
		t.Fatal("the old contact stayed", gone, err)
	}
	var observedFor string
	if err := db.Read().QueryRow(`SELECT profile_id FROM contact_observations WHERE normalized_email = 'ada@example.com'`).Scan(&observedFor); err != nil || observedFor != moved {
		t.Fatal("the mail seen for Ada stayed with the old contact", observedFor, err)
	}
	var suppressed int
	if err := db.Read().QueryRow(`SELECT is_suppressed FROM contact_observations WHERE normalized_email = 'ada@example.com'`).Scan(&suppressed); err != nil || suppressed != 0 {
		t.Fatal("moving stopped Gofer collecting the address", suppressed, err)
	}
	if w := placeContactRequest(t, h, moved, "move", "account:acc"); w.Code != http.StatusBadRequest {
		t.Fatal("move to where it already is", w.Code)
	}

	// Duplicate back into Gofer Local: two independent contacts.
	w = placeContactRequest(t, h, moved, "duplicate", "local")
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result["writing"] != false {
		t.Fatal("duplicate", w.Code, w.Body.String(), err)
	}
	copy, _ := result["contact_id"].(string)
	if copy == moved {
		t.Fatal("duplicate reused the contact")
	}
	copyProfile, err := db.GetContactProfile(ctx, "default", copy)
	if err != nil || copyProfile == nil || copyProfile.Home != "local" || copyProfile.Origin != "manual" {
		t.Fatal("duplicate", copyProfile, err)
	}
	if still, err := db.GetContact(ctx, "default", moved); err != nil || still == nil {
		t.Fatal("duplicate removed the contact", still, err)
	}

	// A contact with Gofer Sync on isn't moved.
	synced, err := db.SaveContact(ctx, "default", models.Contact{Name: "Synced", Email: "synced@example.com", GoferSyncEnabled: true, SaveTargets: []string{"account:acc"}})
	if err != nil {
		t.Fatal(err)
	}
	if w := placeContactRequest(t, h, synced.ID, "move", "account:acc"); w.Code != http.StatusConflict {
		t.Fatal("synced move", w.Code, w.Body.String())
	}
}
