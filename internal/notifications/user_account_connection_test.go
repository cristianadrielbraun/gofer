package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserAccountConnectionHTTPPlainIMAPSMTPAndOwnership(t *testing.T) {
	f, _, _ := newUserComposeFixture(t)
	for _, owner := range []string{"alice", "bob"} {
		response := f.request(owner, "POST", "/api/accounts/"+f.accounts[owner].ID+"/test", "")
		var result struct {
			Results []models.ConnectionTestResult `json:"results"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Results) != 2 {
			t.Fatal("connection result", owner, response.Code, response.Body.String())
		}
		for _, service := range result.Results {
			if !service.Success {
				t.Fatal("real local protocol connection failed", owner, service)
			}
		}
	}
	if response := f.request("alice", "POST", "/api/accounts/"+f.accounts["bob"].ID+"/test", ""); response.Code != 404 {
		t.Fatal("foreign connection test", response.Code)
	}
	if response := f.request("alice", "POST", "/api/mail/sync/accounts/"+f.accounts["bob"].ID+"/repair", ""); response.Code != 404 {
		t.Fatal("foreign repair", response.Code)
	}
	if response := f.request("alice", "POST", "/api/mail/sync/accounts/"+f.accounts["alice"].ID+"/repair", ""); response.Code != 200 || response.Header().Get("X-Gofer-Status") != "error" {
		t.Fatal("non-Gmail repair", response.Code)
	}
	w := &blockedContactViewWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	req := httptest.NewRequest("POST", "/api/accounts/"+f.accounts["alice"].ID+"/test?wizard=add", nil)
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	done := make(chan struct{})
	go func() { defer close(done); f.http.ServeHTTP(w, req) }()
	t.Cleanup(func() { close(w.release); <-done })
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("wizard response not rendered")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.routing.WithUser(ctx, "bob", func(*storage.DB) error { return nil }); err != nil {
		t.Fatal("browser pinned connection-test store", err)
	}
}

func TestUserAccountDiscoveryRequiresNoUserStoreLease(t *testing.T) {
	f := newUserStorageFixture(t)
	if err := f.routing.WithUser(t.Context(), "alice", func(*storage.DB) error {
		for _, body := range []string{"email=", "email=not-an-address", strings.Repeat("x", 65<<10)} {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			req := httptest.NewRequest("POST", "/api/accounts/discover", strings.NewReader(body)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["bob"].Token})
			response := httptest.NewRecorder()
			f.http.ServeHTTP(response, req)
			cancel()
			if response.Code != 400 {
				t.Fatal("invalid/oversize discovery", response.Code)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserAccountConnectionHTTPProviderAndRepairAdmission(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderActionFixture(t, provider)
			path := "/api/accounts/" + f.accounts["alice"].ID + "/test"
			response := f.request("alice", "POST", path, "")
			var result struct {
				Results []models.ConnectionTestResult `json:"results"`
			}
			if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Results) != 1 || !result.Results[0].Success {
				t.Fatal("owned provider connection", response.Code, response.Body.String())
			}
			service := "gmail"
			if provider == "outlook" {
				service = "graph"
			}
			if result.Results[0].Service != service {
				t.Fatal("wrong provider probe", result)
			}
			if provider != "gmail" {
				return
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE accounts SET email_sync_enabled=1 WHERE id=?`, f.accounts["alice"].ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			api.blockOwner = "alice"
			api.blockPath = "/users/me/labels"
			api.mu.Unlock()
			response = f.request("alice", "POST", "/api/mail/sync/accounts/"+f.accounts["alice"].ID+"/repair", "")
			if response.Code != 200 || response.Header().Get("X-Gofer-Mail-Sync-Mode") != "repair" || response.Header().Get("X-Gofer-Mail-Sync-Run-ID") == "" || response.Header().Get("X-Gofer-Status") != "ok" {
				t.Fatal("HTTP repair admission", response.Code, response.Header())
			}
			awaitIMAP(t, api.entered)
			snapshot, err := f.imap.ManualSyncSnapshot(t.Context(), "alice")
			if err != nil || len(snapshot) != 2 || snapshot[0].Payload["mode"] != "repair" {
				t.Fatal("HTTP repair snapshot", snapshot, err)
			}
			if response := f.request("alice", "POST", "/api/mail/sync/cancel", ""); response.Code != 200 {
				t.Fatal("HTTP repair cancellation", response.Code)
			}
		})
	}
}
