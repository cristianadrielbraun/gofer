package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestHandleTestAccountUsesGmailAPIForGmail(t *testing.T) {
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := t.Context()
	if _, err := db.Write().ExecContext(ctx, `INSERT OR IGNORE INTO users (id, email, name) VALUES ('default', 'default@example.com', 'Default')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address, display_name, auth_method)
		VALUES ('acc', 'default', ?, 'google-subject', 'user@gmail.com', 'Gmail', 'oauth2')`, providers.ProviderGmail); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	expires := time.Now().Add(time.Hour)
	manager := auth.NewManager(&auth.Config{}, db)
	if err := manager.UpsertOAuthAccount(ctx, "default", providers.OAuthGoogle, "google-subject", "gmail-token", "refresh-token", "Bearer", &expires, "https://mail.google.com/"); err != nil {
		t.Fatalf("UpsertOAuthAccount() error = %v", err)
	}

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/users/me/profile" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer gmail-token" {
			t.Fatalf("Authorization = %q, want Gmail bearer token", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"emailAddress": "user@gmail.com"})
	}))
	defer server.Close()
	previousBase := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	t.Cleanup(func() { gmailAPIBaseURL = previousBase })

	store, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewAccountStore() error = %v", err)
	}
	h := New(db, store, nil, nil, manager, "")
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/acc/test", nil)
	req.SetPathValue("id", "acc")
	rec := httptest.NewRecorder()
	h.handleTestAccount(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if requests != 1 {
		t.Fatalf("Gmail profile requests = %d, want 1", requests)
	}
	var payload struct {
		Results []struct {
			Success bool   `json:"success"`
			Service string `json:"service"`
			Message string `json:"message"`
			Error   string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Results) != 1 || payload.Results[0].Service != "gmail" || !payload.Results[0].Success {
		t.Fatalf("results = %#v, want one successful Gmail API result", payload.Results)
	}
}
