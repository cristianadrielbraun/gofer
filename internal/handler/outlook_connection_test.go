package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
)

func TestHandleTestAccountUsesGraphForOutlook(t *testing.T) {
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
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address, display_name, imap_host, imap_port, imap_tls_mode, smtp_host, smtp_port, smtp_tls_mode, username, auth_method)
		VALUES ('acc', 'default', ?, 'subject-id', 'user@example.com', 'Outlook', 'outlook.office365.com', 993, 'tls', 'smtp-mail.outlook.com', 587, 'starttls', 'user@example.com', 'oauth2')`, providers.ProviderOutlook); err != nil {
		t.Fatalf("insert account: %v", err)
	}
	expires := time.Now().Add(-time.Hour)
	manager := auth.NewManager(&auth.Config{}, db)
	if err := manager.UpsertOAuthAccount(ctx, "default", providers.OAuthMicrosoft, "subject-id", "stale-token", "refresh-token", "Bearer", &expires, ""); err != nil {
		t.Fatalf("UpsertOAuthAccount() error = %v", err)
	}

	var tokenScope string
	var sawMailFolders bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parse token form: %v", err)
			}
			tokenScope = r.Form.Get("scope")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "graph-token",
				"token_type":   "Bearer",
				"expires_in":   3600,
				"scope":        tokenScope,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/me/mailFolders":
			sawMailFolders = true
			if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
				t.Fatalf("Prefer = %q, want immutable ID preference", r.Header.Get("Prefer"))
			}
			if top := r.URL.Query().Get("$top"); top != "1" {
				t.Fatalf("$top = %q, want 1", top)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]string{{"id": "inbox"}}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	manager = auth.NewManager(&auth.Config{MicrosoftClient: &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: server.URL + "/token"}}}, db)
	store, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewAccountStore() error = %v", err)
	}
	previousGraphBase := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previousGraphBase })
	h := New(db, store, nil, nil, manager, "")

	req := httptest.NewRequest(http.MethodPost, "/api/accounts/acc/test", nil)
	req.SetPathValue("id", "acc")
	rec := httptest.NewRecorder()
	h.handleTestAccount(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !sawMailFolders {
		t.Fatal("Graph mailFolders probe was not observed")
	}
	if strings.Contains(tokenScope, "outlook.office.com/IMAP") || strings.Contains(tokenScope, "outlook.office.com/SMTP") {
		t.Fatalf("scope = %q, must not request legacy Outlook IMAP/SMTP scopes", tokenScope)
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
	if len(payload.Results) != 1 || payload.Results[0].Service != "graph" || !payload.Results[0].Success {
		t.Fatalf("results = %#v, want one successful Graph result", payload.Results)
	}
	if _, err := url.Parse(outlookGraphMailFoldersProbeEndpoint()); err != nil {
		t.Fatalf("probe endpoint is not parseable: %v", err)
	}
}
