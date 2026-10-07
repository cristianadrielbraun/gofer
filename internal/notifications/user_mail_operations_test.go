package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func seedUserMailOperation(t *testing.T, f *userStorageFixture, owner, id string) {
	t.Helper()
	messageID := 1
	if id == "bob-only" {
		messageID = 2
	}
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		_, err := db.Write().Exec(`INSERT OR IGNORE INTO messages(id,account_id,internet_message_id,subject) VALUES(?,?,?,'private message')`, messageID, f.accounts[owner].ID, "<"+id+"@test.invalid>")
		if err != nil {
			return err
		}
		_, err = db.Write().Exec(`INSERT INTO message_mutations(id,account_id,message_id,folder_id,provider_type,kind,target_value,status,attempt_count,last_error,next_attempt_at) VALUES(?,?,?,'','imap','read',1,'failed',2,'access_token=private-secret',?)`, id, f.accounts[owner].ID, messageID, time.Now().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserMailOperationsHTTPIsolationAndBrowserRelease(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	for _, owner := range []string{"alice", "bob"} {
		seedUserMailOperation(t, f, owner, "same-operation")
	}
	seedUserMailOperation(t, f, "bob", "bob-only")
	for _, owner := range []string{"alice", "bob"} {
		response := f.request(owner, "GET", "/api/mail-operations?user_id=other", "")
		var result struct {
			Operations []models.MailOperationSummary `json:"operations"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || len(result.Operations) == 0 {
			t.Fatal("operation list", owner, response.Code)
		}
		for _, op := range result.Operations {
			if op.AccountID != f.accounts[owner].ID || strings.Contains(op.LastError, "private-secret") {
				t.Fatal("foreign/private operation", op)
			}
		}
		response = f.request(owner, "GET", "/settings/operations/content", "")
		if response.Code != 200 || !strings.Contains(response.Body.String(), "same-operation") || strings.Contains(response.Body.String(), "private-secret") {
			t.Fatal("operation settings", response.Code)
		}
	}
	if response := f.request("alice", "POST", "/api/mail-operations/message_mutation:bob-only/retry", ""); response.Code != 404 {
		t.Fatal("foreign operation retried", response.Code)
	}
	w := &blockedContactViewWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	req := httptest.NewRequest("GET", "/settings/operations/content", nil)
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	done := make(chan struct{})
	go func() { defer close(done); f.http.ServeHTTP(w, req) }()
	t.Cleanup(func() { close(w.release); <-done })
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("browser write not reached")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.routing.WithUser(ctx, "bob", func(*storage.DB) error { return nil }); err != nil {
		t.Fatal("browser pinned user store", err)
	}
}

func TestUserMailOperationsHTTPWriterWaitRejectsDisabledOwner(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	seedUserMailOperation(t, f, "alice", "same-operation")
	ready, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	unlock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unlock)
	var db *storage.DB
	go func() {
		done <- f.routing.WithUser(t.Context(), "alice", func(local *storage.DB) error {
			db = local
			tx, err := db.Write().BeginTx(t.Context(), nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			close(ready)
			<-release
			return nil
		})
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("writer not acquired")
	}
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- f.request("alice", "POST", "/api/mail-operations/message_mutation:same-operation/retry", "")
	}()
	waitUserIMAP(t, func() bool { return db.Write().Stats().WaitCount > 0 })
	if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-response:
		if result.Code == 200 {
			t.Fatal("disabled owner retried")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not end")
	}
	if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='alice'`); err != nil {
		t.Fatal(err)
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		op, err := db.GetMailOperationForUser(t.Context(), "alice", "message_mutation:same-operation")
		if err == nil && op.State != "failed" {
			t.Fatal("rejected retry committed", op)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
