package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestContactSyncSetupRequired(t *testing.T) {
	requested := models.Contact{GoferSyncEnabled: true, SaveTargets: []string{"local", "account:a"}}
	if !contactSyncSetupRequired(requested, nil) {
		t.Fatal("newly enabled contact did not require setup")
	}
	previous := &models.Contact{GoferSyncEnabled: true, SaveTargets: []string{"local", "account:a"}}
	if contactSyncSetupRequired(requested, previous) {
		t.Fatal("unchanged enabled locations unexpectedly required setup")
	}
	requested.SaveTargets = append(requested.SaveTargets, "account:b")
	if !contactSyncSetupRequired(requested, previous) {
		t.Fatal("new sync location did not require setup")
	}
	requested.GoferSyncEnabled = false
	if contactSyncSetupRequired(requested, previous) {
		t.Fatal("disabled sync unexpectedly required setup")
	}
}

func TestContactSyncCandidateMatchingAndScore(t *testing.T) {
	local := models.Contact{
		Name:             "  Jane   Doe ",
		Email:            "jane@example.com",
		AdditionalEmails: []string{"jane@work.example"},
		Phone:            "+420 123 456 789",
	}
	remote := models.Contact{
		Name:             "jane doe",
		Email:            "other@example.com",
		AdditionalEmails: []string{"JANE@WORK.EXAMPLE"},
		Phone:            "+420 (123) 456-789",
	}
	if !contactSyncEmailsMatch(local, remote) {
		t.Fatal("additional email did not match case-insensitively")
	}
	if !contactSyncPhonesMatch(local, remote) {
		t.Fatal("formatted phone numbers did not match")
	}
	if !contactSyncNamesMatch(local, remote) {
		t.Fatal("normalized names did not match")
	}
	all := models.ContactSyncSetupCandidate{MatchEmail: true, MatchPhone: true, MatchName: true}
	if score := contactSyncSetupCandidateScore(all); score != 7 {
		t.Fatalf("all-fields score = %d, want 7", score)
	}
	emailOnly := models.ContactSyncSetupCandidate{MatchEmail: true}
	phoneAndName := models.ContactSyncSetupCandidate{MatchPhone: true, MatchName: true}
	if contactSyncSetupCandidateScore(emailOnly) <= contactSyncSetupCandidateScore(phoneAndName) {
		t.Fatal("email match must outrank a combined phone and name match")
	}
}

func TestSavingNewlyEnabledContactStagesSetupBeforeSync(t *testing.T) {
	ctx := context.Background()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'gmail', 'account@example.com')`); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/contacts", h.handleSaveContact)
	mux.HandleFunc("POST /api/contacts/{id}/sync-setup/confirm", h.handleConfirmContactSyncSetup)

	form := url.Values{
		"name":         {"Jane"},
		"email":        {"jane@example.com"},
		"sync_enabled": {"on"},
		"save_targets": {"local,account:acc"},
	}
	req := httptest.NewRequest(http.MethodPost, "/api/contacts", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		ContactID string `json:"contact_id"`
		SetupURL  string `json:"contact_sync_setup_url"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.ContactID == "" || response.SetupURL == "" {
		t.Fatalf("staged response = %#v, want contact and setup URL", response)
	}
	contact, err := db.GetContact(ctx, "default", response.ContactID)
	if err != nil || contact == nil {
		t.Fatalf("GetContact() = %#v, %v", contact, err)
	}
	if contact.GoferSyncEnabled {
		t.Fatal("sync became active before setup confirmation")
	}

	confirm := httptest.NewRequest(http.MethodPost, "/api/contacts/"+response.ContactID+"/sync-setup/confirm", strings.NewReader(""))
	confirm.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	confirmRec := httptest.NewRecorder()
	mux.ServeHTTP(confirmRec, confirm)
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("confirm status = %d: %s", confirmRec.Code, confirmRec.Body.String())
	}
	contact, err = db.GetContact(ctx, "default", response.ContactID)
	if err != nil || contact == nil || !contact.GoferSyncEnabled {
		t.Fatalf("confirmed contact = %#v, %v; want enabled", contact, err)
	}
	var pending int
	if err := db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM contact_sync_operations WHERE contact_id = ? AND status = 'pending'`, response.ContactID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("pending sync operations = %d, want 1 after confirmation", pending)
	}
}
