package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestPreferContactFieldQueuesProviderSync(t *testing.T) {
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
		DisplayName:  "Jane",
		PrimaryEmail: "jane@example.com",
		Fields: []models.ContactField{
			{ID: "manual-email", Kind: "email", Value: "jane@example.com", IsPrimary: true, Source: "manual"},
			{ID: "provider-email", Kind: "email", Value: "jane.alt@example.com", Source: "synced:acc"},
		},
	})
	if err != nil {
		t.Fatalf("SaveContactProfile() error = %v", err)
	}
	if err := db.UpsertContactSource(ctx, storage.ContactSource{
		ContactID: profile.ID,
		UserID:    "default",
		Provider:  "gmail",
		AccountID: "acc",
		RemoteID:  "people/c1",
	}); err != nil {
		t.Fatalf("UpsertContactSource() error = %v", err)
	}

	h := &Handler{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/contacts/{id}/fields/{fieldID}/prefer", h.handlePreferContactField)
	req := httptest.NewRequest(http.MethodPost, "/api/contacts/"+profile.ID+"/fields/provider-email/prefer", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if got := rec.Header().Get("Location"); got != "/contacts?contact="+profile.ID+"&sync=queued" {
		t.Fatalf("Location = %q, want sync queued redirect", got)
	}
	var pending int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_sync_operations WHERE user_id = 'default' AND contact_id = ? AND status = 'pending'`, profile.ID).Scan(&pending); err != nil {
		t.Fatalf("count sync operations: %v", err)
	}
	if pending != 1 {
		t.Fatalf("pending sync operations = %d, want 1", pending)
	}
	updated, err := db.GetContact(ctx, "default", profile.ID)
	if err != nil {
		t.Fatalf("GetContact() error = %v", err)
	}
	if updated == nil || updated.Email != "jane.alt@example.com" {
		t.Fatalf("updated contact = %#v, want preferred email", updated)
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
