package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestHandleUpdateAccountServiceDoesNotEnableCalendarWithoutSources(t *testing.T) {
	db, err := storage.New(t.TempDir() + "/gofer.db")
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
	accountStore, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewAccountStore() error = %v", err)
	}
	h := &Handler{db: db, accountStore: accountStore}

	form := url.Values{"service": {"calendar"}, "enabled": {"true"}}
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/outlook-account/services", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("id", "outlook-account")
	req = req.WithContext(auth.ContextWithUser(req.Context(), &auth.User{ID: "default", Username: "default"}))
	rec := httptest.NewRecorder()
	h.handleUpdateAccountService(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d body = %q, want conflict", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Discover at least one calendar") {
		t.Fatalf("body = %q, want configuration guidance", rec.Body.String())
	}
	cfg, err := accountStore.GetCalendarSyncConfig(t.Context(), "default", "outlook-account")
	if err != nil {
		t.Fatalf("GetCalendarSyncConfig() error = %v", err)
	}
	if cfg.Enabled || cfg.SourceCount != 0 {
		t.Fatalf("calendar config = %#v, want disabled with no sources", cfg)
	}
}
