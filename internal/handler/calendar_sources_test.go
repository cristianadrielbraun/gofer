package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestHandleSaveAccountCalendarSourcesUpdatesSelectionAndRendersPanel(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized, name)
		VALUES ('default', 'default', 'default', 'Default');
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address, display_name, auth_method)
		VALUES ('outlook-account', 'default', 'outlook', 'subject-id', 'person@outlook.com', 'Person', 'oauth2');`); err != nil {
		t.Fatalf("insert test account: %v", err)
	}
	if err := db.ReplaceCalendarSources(t.Context(), "default", "outlook-account", "outlook", []storage.CalendarSource{
		{ID: "primary-source", RemoteID: "primary", Name: "Primary", TimeZone: "Europe/Prague", AccessRole: "owner", IsPrimary: true, IsSelected: true},
		{ID: "team-source", RemoteID: "team", Name: "Team", AccessRole: "reader", IsSelected: false},
	}); err != nil {
		t.Fatalf("ReplaceCalendarSources() error = %v", err)
	}
	accountStore, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewAccountStore() error = %v", err)
	}
	h := &Handler{db: db, accountStore: accountStore}

	form := url.Values{"source_id": {"team-source"}}
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/outlook-account/calendar/sources", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", "outlook-account")
	req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "default", Username: "default"}))
	rec := httptest.NewRecorder()
	h.handleSaveAccountCalendarSources(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %q, want success", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"Calendar sources saved.", "Primary", "Team", "1 of 2 selected", "/api/accounts/outlook-account/calendar/sources"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("response body missing %q: %s", want, rec.Body.String())
		}
	}
	sources, err := db.ListCalendarSourcesForAccount(t.Context(), "default", "outlook-account")
	if err != nil {
		t.Fatalf("ListCalendarSourcesForAccount() error = %v", err)
	}
	if len(sources) != 2 || sources[0].IsSelected || !sources[1].IsSelected {
		t.Fatalf("stored Calendar source selection = %#v, want only team selected", sources)
	}

	emptyReq := httptest.NewRequest(http.MethodPost, "/api/accounts/outlook-account/calendar/sources", strings.NewReader(url.Values{}.Encode()))
	emptyReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	emptyReq.SetPathValue("id", "outlook-account")
	emptyReq = emptyReq.WithContext(auth.ContextWithUser(emptyReq.Context(), &auth.User{ID: "default", Username: "default"}))
	emptyRec := httptest.NewRecorder()
	h.handleSaveAccountCalendarSources(emptyRec, emptyReq)
	if emptyRec.Code != http.StatusConflict || !strings.Contains(emptyRec.Body.String(), "Select at least one calendar") {
		t.Fatalf("empty selection response = %d %q, want conflict guidance", emptyRec.Code, emptyRec.Body.String())
	}
}
