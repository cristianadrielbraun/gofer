package notifications

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserMessagePrefetchAndRefetchHTTP(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	f.useIMAPServer(t, server)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.imap.Sync(t.Context(), owner, f.accounts[owner].ID); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if r := f.request(owner, "POST", "/api/messages/1/prefetch-body", ""); r.Code != 204 {
				t.Fatal("prefetch", owner, r.Code, r.Body.String())
			}
		}
		if server.bodyRequests(owner) != 1 {
			t.Fatal("cached prefetch fetched again", owner, server.bodyRequests(owner))
		}
	}
	// A failed refresh must preserve the previous cache, recipients and attachments.
	for _, failure := range []string{"remote", "publication"} {
		var before storage.MessageStorageInfo
		var recipients, attachments int
		if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
			info, err := db.GetMessageStorageInfoForUser(t.Context(), 1, "alice")
			if err != nil {
				return err
			}
			before = *info
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id=1`).Scan(&recipients); err != nil {
				return err
			}
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id=1`).Scan(&attachments); err != nil {
				return err
			}
			if failure == "publication" {
				_, err = db.Write().Exec(`CREATE TRIGGER fail_refetch BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT,'injected refresh failure'); END`)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		server.mu.Lock()
		server.rejectBody = failure == "remote"
		server.mu.Unlock()
		if r := f.request("alice", "POST", "/api/messages/1/refetch", ""); r.Code != 503 {
			t.Fatal("failed refresh claimed success", failure, r.Code)
		}
		if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
			info, err := db.GetMessageStorageInfoForUser(t.Context(), 1, "alice")
			if err != nil {
				return err
			}
			var currentRecipients, currentAttachments int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id=1`).Scan(&currentRecipients); err != nil {
				return err
			}
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id=1`).Scan(&currentAttachments); err != nil {
				return err
			}
			if *info != before || currentRecipients != recipients || currentAttachments != attachments || !db.IsBodyFetchedInternal(t.Context(), 1) {
				return fmt.Errorf("failed refresh changed published cache")
			}
			if failure == "publication" {
				_, err = db.Write().Exec(`DROP TRIGGER fail_refetch`)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if r := f.request("alice", "GET", "/email/1/body", ""); r.Code != 200 || !strings.Contains(r.Body.String(), "alicebodyuniquetoken") {
			t.Fatal("failed refresh lost readable cache", failure, r.Code)
		}
	}
	server.mu.Lock()
	server.rejectBody = false
	server.bodyOverride = "From: alice@example.com\r\nTo: new@example.com\r\nSubject: Fresh Alice\r\nContent-Type: text/plain\r\n\r\nalicefreshbodytoken"
	server.mu.Unlock()
	if r := f.request("alice", "POST", "/api/messages/1/refetch", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"status":"refetched"`) {
		t.Fatal("fresh refetch", r.Code, r.Body.String())
	}
	if r := f.request("alice", "GET", "/email/1/body", ""); r.Code != 200 || !strings.Contains(r.Body.String(), "alicefreshbodytoken") || strings.Contains(r.Body.String(), "alicebodyuniquetoken") {
		t.Fatal("refetch reused saved MIME", r.Code, r.Body.String())
	}
	if r := f.request("alice", "GET", "/search?q=alicefreshbodytoken", ""); r.Code != 200 || !strings.Contains(r.Body.String(), "Fresh Alice") {
		t.Fatal("refresh not indexed", r.Code)
	}
	if r := f.request("bob", "GET", "/email/1/body", ""); r.Code != 200 || !strings.Contains(r.Body.String(), "bobbodyuniquetoken") || strings.Contains(r.Body.String(), "alicefreshbodytoken") {
		t.Fatal("refresh crossed owner stores", r.Code)
	}
	for _, path := range []string{"/api/messages/999/refetch", "/api/messages/999/prefetch-body", "/api/messages/no/refetch", "/api/messages/0/prefetch-body"} {
		if r := f.request("alice", "POST", path, ""); r.Code != 404 && r.Code != 400 {
			t.Fatal("invalid/missing message", path, r.Code)
		}
	}
}

func TestUserMessageRefetchReleasesStoreDuringNetworkAndBrowserWaits(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	f.useIMAPServer(t, server)
	if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	if err := f.imap.EnsureBody(t.Context(), "alice", 1); err != nil {
		t.Fatal(err)
	}
	blocked, release := server.setBlock("alice", "body")
	var releaseOnce sync.Once
	w := &blockedContactViewWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	req := httptest.NewRequest("POST", "/api/messages/1/refetch", nil)
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	done := make(chan struct{})
	go func() { defer close(done); f.http.ServeHTTP(w, req) }()
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }); close(w.release); <-done })
	awaitIMAP(t, blocked)
	check := func(stage string) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := f.routing.WithUser(ctx, "bob", func(*storage.DB) error { return nil }); err != nil {
			t.Fatal(stage, err)
		}
	}
	check("network pinned store")
	releaseOnce.Do(func() { close(release) })
	awaitIMAP(t, w.entered)
	check("browser pinned store")
}

func TestUserProviderPrefetchAndRefetchHTTP(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderActionFixture(t, provider)
			api.attachmentAPI = true
			for _, owner := range []string{"alice", "bob"} {
				if r := f.request(owner, "POST", "/api/messages/1/prefetch-body", ""); r.Code != 204 {
					t.Fatal("provider prefetch", owner, r.Code, r.Body.String())
				}
				if r := f.request(owner, "POST", "/api/messages/1/refetch", ""); r.Code != 200 {
					t.Fatal("provider refresh", owner, r.Code, r.Body.String())
				}
				if r := f.request(owner, "GET", "/email/1/body", ""); r.Code != 200 || !strings.Contains(r.Body.String(), "private body") {
					t.Fatal("provider refreshed body", owner, r.Code)
				}
				if r := f.request(owner, "GET", "/api/inline-content/1/shared-cid", ""); r.Code != 200 || r.Body.String() != owner+" image" {
					t.Fatal("provider refresh crossed owners", owner, r.Code, r.Body.String())
				}
				path := "/users/me/messages/m1"
				if provider == "outlook" {
					path = "/me/messages/m1/$value"
				}
				api.mu.Lock()
				calls := api.calls[owner+"|GET|"+path]
				api.mu.Unlock()
				if calls != 2 {
					t.Fatal("provider refresh reused cached MIME", owner, calls)
				}
			}
		})
	}
}
