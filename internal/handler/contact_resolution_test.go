package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestSyncContactNowQueuesOperationAndPublishesLiveStatus(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Write().Exec(`INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('account-1', 'default', 'gmail', 'account@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	profile, err := db.SaveContactProfile(ctx, "default", models.ContactProfile{
		DisplayName:  "Jane",
		PrimaryEmail: "jane@example.com",
		SyncEnabled:  true,
		Cards:        []models.ContactCard{{Kind: "local"}},
		Fields: []models.ContactField{{
			Kind:      "email",
			Value:     "jane@example.com",
			IsPrimary: true,
			Source:    "manual",
		}},
	})
	if err != nil {
		t.Fatalf("SaveContactProfile() error = %v", err)
	}
	if err := db.ReplaceContactSyncMemberships(ctx, "default", profile.ID, []string{"local", "account:account-1"}); err != nil {
		t.Fatalf("ReplaceContactSyncMemberships() error = %v", err)
	}

	syncer := mailpkg.NewSyncOrchestrator(db, nil, nil, nil)
	h := New(db, nil, syncer, nil, nil, "")
	events := syncer.Events().Subscribe()
	defer syncer.Events().Unsubscribe(events)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/contacts/{id}/sync-now", h.handleSyncContactNow)
	req := httptest.NewRequest(http.MethodPost, "/api/contacts/"+profile.ID+"/sync-now", nil)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["contact_id"] != profile.ID || response["status"] != "pending" || response["contact_sync_queued"] != true {
		t.Fatalf("response = %#v, want queued pending contact", response)
	}
	var pending int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_sync_operations WHERE user_id = 'default' AND contact_id = ? AND status = 'pending'`, profile.ID).Scan(&pending); err != nil {
		t.Fatalf("count sync operations: %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending sync operations = %d, want 1", pending)
	}
	select {
	case event := <-events:
		if event.Type != mailpkg.EventContactActivity {
			t.Fatalf("event type = %q, want %q", event.Type, mailpkg.EventContactActivity)
		}
		if event.Payload["contact_id"] != profile.ID || event.Payload["event_type"] != "contact_sync_queued" || event.Payload["status"] != "pending" {
			t.Fatalf("event payload = %#v, want contact-specific pending sync event", event.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for contact sync SSE event")
	}
}

func TestInboundProviderChangeQueuesMultiMasterFanout(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Write().Exec(`INSERT INTO accounts (id, user_id, provider, email_address) VALUES
		('acc-source', 'default', 'gmail', 'source@example.com'),
		('acc-target', 'default', 'outlook', 'target@example.com')`); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	profile, err := db.SaveContactProfile(ctx, "default", models.ContactProfile{
		DisplayName:  "Jane",
		PrimaryEmail: "jane@example.com",
		SyncEnabled:  true,
		Fields: []models.ContactField{
			{ID: "manual-email", Kind: "email", Value: "jane@example.com", IsPrimary: true, Source: "manual"},
			{ID: "manual-phone", Kind: "phone", Value: "+1 555 0100", IsPrimary: true, Source: "manual"},
		},
	})
	if err != nil {
		t.Fatalf("SaveContactProfile() error = %v", err)
	}
	if err := db.InitializeContactCanonicalFields(ctx, "default", profile.ID, map[string]string{"email": "manual-email", "phone": "manual-phone"}); err != nil {
		t.Fatalf("InitializeContactCanonicalFields() error = %v", err)
	}
	if err := db.ReplaceContactSyncMemberships(ctx, "default", profile.ID, []string{"account:acc-source", "account:acc-target"}); err != nil {
		t.Fatalf("ReplaceContactSyncMemberships() error = %v", err)
	}
	if err := db.ReplaceSyncedContactFieldsForProfile(ctx, "default", profile.ID, "acc-source", models.Contact{Name: "Jane", Email: "jane@example.com", Phone: "+1 555 0100"}); err != nil {
		t.Fatalf("store initial provider snapshot: %v", err)
	}
	if err := db.UpsertContactSource(ctx, storage.ContactSource{ContactID: profile.ID, UserID: "default", Provider: "gmail", AccountID: "acc-source", RemoteID: "people/c1"}); err != nil {
		t.Fatalf("store provider source: %v", err)
	}
	h := &Handler{db: db}
	contactID, canonicalChanged, err := h.upsertInboundSyncedContact(ctx, "default", "gmail", "acc-source", "people/c1", models.Contact{Name: "Jane Provider", Email: "jane.new@example.com", Phone: "+1 555 0200"})
	if err != nil {
		t.Fatalf("provider change: %v", err)
	}
	if !canonicalChanged {
		t.Fatal("provider change did not become canonical")
	}
	if contactID != profile.ID {
		t.Fatalf("provider email edit changed profile ID to %q, want %q", contactID, profile.ID)
	}
	if err := h.scheduleInboundContactFanout(ctx, "default", profile.ID, "acc-source"); err != nil {
		t.Fatalf("scheduleInboundContactFanout() error = %v", err)
	}
	var pending int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_sync_operations WHERE user_id = 'default' AND contact_id = ? AND status = 'pending'`, profile.ID).Scan(&pending); err != nil {
		t.Fatalf("count sync operations: %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending sync operations = %d, want 1", pending)
	}
	var payloadJSON string
	if err := db.Read().QueryRowContext(ctx, `SELECT payload_json FROM contact_sync_operations WHERE user_id = 'default' AND contact_id = ? AND status = 'pending'`, profile.ID).Scan(&payloadJSON); err != nil {
		t.Fatalf("load sync operation payload: %v", err)
	}
	var payload storage.ContactSyncOperationPayload
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		t.Fatalf("decode sync operation payload: %v", err)
	}
	if payload.ExcludedAccountID != "acc-source" {
		t.Fatalf("excluded account = %q, want inbound source account", payload.ExcludedAccountID)
	}
	updated, err := db.GetContact(ctx, "default", profile.ID)
	if err != nil {
		t.Fatalf("GetContact() error = %v", err)
	}
	if updated == nil || updated.Email != "jane.new@example.com" || updated.Phone != "+1 555 0200" || updated.Name != "Jane Provider" {
		t.Fatalf("updated contact = %#v, want inbound provider values", updated)
	}
}

func TestUnifyContactCreatesGoferManagedFields(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	profile, err := db.SaveContactProfile(ctx, "default", models.ContactProfile{
		DisplayName:  "Observed Person",
		PrimaryEmail: "seen@example.com",
		Cards:        []models.ContactCard{{Kind: "observed"}},
		Fields: []models.ContactField{
			{ID: "observed-email", Kind: "email", Value: "seen@example.com", IsPrimary: true, Source: "observed"},
		},
	})
	if err != nil {
		t.Fatalf("SaveContactProfile() error = %v", err)
	}

	h := &Handler{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/contacts/{id}/unify", h.handleUnifyContact)
	req := httptest.NewRequest(http.MethodPost, "/api/contacts/"+profile.ID+"/unify", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got := rec.Header().Get("Location"); got != "/contacts?contact="+profile.ID {
		t.Fatalf("Location = %q, want unified contact redirect", got)
	}
	var manualFields int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_fields WHERE user_id = 'default' AND profile_id = ? AND source = 'manual'`, profile.ID).Scan(&manualFields); err != nil {
		t.Fatalf("count manual fields: %v", err)
	}
	if manualFields == 0 {
		t.Fatalf("manual fields = 0, want Gofer-managed fields")
	}
	updated, err := db.GetContact(ctx, "default", profile.ID)
	if err != nil {
		t.Fatalf("GetContact() error = %v", err)
	}
	if updated == nil || !updated.IsManual {
		t.Fatalf("updated contact = %#v, want manual Gofer contact", updated)
	}
}

func TestUnifyContactJSONRequestsDetailRefresh(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	profile, err := db.SaveContactProfile(ctx, "default", models.ContactProfile{
		DisplayName:  "Observed Person",
		PrimaryEmail: "seen@example.com",
		Cards:        []models.ContactCard{{Kind: "observed"}},
		Fields: []models.ContactField{
			{ID: "observed-email", Kind: "email", Value: "seen@example.com", IsPrimary: true, Source: "observed"},
		},
	})
	if err != nil {
		t.Fatalf("SaveContactProfile() error = %v", err)
	}

	h := &Handler{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/contacts/{id}/unify", h.handleUnifyContact)
	req := httptest.NewRequest(http.MethodPost, "/api/contacts/"+profile.ID+"/unify", nil)
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode JSON response: %v", err)
	}
	if body["contact_id"] != profile.ID {
		t.Fatalf("contact_id = %v, want %q", body["contact_id"], profile.ID)
	}
	if body["refresh_detail"] != true {
		t.Fatalf("refresh_detail = %v, want true", body["refresh_detail"])
	}
	if body["location"] != "/contacts?contact="+profile.ID {
		t.Fatalf("location = %v, want contact URL", body["location"])
	}
}
