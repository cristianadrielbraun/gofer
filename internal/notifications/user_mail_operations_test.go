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

func TestUserMailOperationsHTTPUndoesOnlyOwnParkedWork(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	seedUserMailOperation(t, f, "alice", "still-retryable")
	seedUserMailOperation(t, f, "bob", "bob-only")
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`INSERT INTO message_mutations(id,account_id,message_id,folder_id,provider_type,kind,target_value,status,attempt_count,last_error,next_attempt_at) VALUES('alice-parked',?,1,'','imap','starred',1,'failed',1,'',CURRENT_TIMESTAMP)`, f.accounts["alice"].ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Park the way folder cleanup does when a folder is deleted on the server.
	for owner, id := range map[string]string{"alice": "alice-parked", "bob": "bob-only"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE message_mutations SET next_attempt_at=?, last_error='folder removed remotely (imap discovery)' WHERE id=?`, time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC), id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	page := f.request("alice", "GET", "/settings/operations/content", "")
	if page.Code != 200 || !strings.Contains(page.Body.String(), `data-mail-operation-undo`) || !strings.Contains(page.Body.String(), "Can&#39;t be completed") {
		t.Fatal("parked operation not offered for undo", page.Code)
	}
	if response := f.request("alice", "POST", "/api/mail-operations/message_mutation:bob-only/undo", ""); response.Code != 404 {
		t.Fatal("foreign operation undone", response.Code)
	}
	if response := f.request("alice", "POST", "/api/mail-operations/message_mutation:still-retryable/undo", ""); response.Code != 409 {
		t.Fatal("retryable operation undone", response.Code)
	}
	if response := f.request("alice", "POST", "/api/mail-operations/message_mutation:alice-parked/retry", ""); response.Code != 409 {
		t.Fatal("parked operation retried", response.Code)
	}
	if response := f.request("alice", "POST", "/api/mail-operations/message_mutation:alice-parked/undo", ""); response.Code != 200 {
		t.Fatal("own parked operation not undone", response.Code, response.Body.String())
	}
	for owner, want := range map[string]int{"alice": 1, "bob": 1} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			var n int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations`).Scan(&n); err != nil {
				return err
			}
			if n != want {
				t.Errorf("%s has %d queued changes, want %d", owner, n, want)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUserMailOperationsHTTPRetryAllGoesOnPastARefusingAccount(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	seedUserMailOperation(t, f, "alice", "alice-failed")
	seedUserMailOperation(t, f, "bob", "bob-only")
	leaving, err := f.accountStore.CreateAccount(t.Context(), "alice", &models.CreateAccountRequest{
		Provider: "imap", EmailAddress: "alex@example.com", DisplayName: "Alex",
		IMAPHost: "imap.example.com", SMTPHost: "smtp.example.com", Username: "alex", Password: "synthetic-only"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		if _, err := db.Write().Exec(`INSERT INTO messages(id,account_id,internet_message_id,subject) VALUES(5,?,'<leaving@test.invalid>','Lunch on Friday?')`, leaving.ID); err != nil {
			return err
		}
		if _, err := db.Write().Exec(`INSERT INTO message_mutations(id,account_id,message_id,folder_id,provider_type,kind,target_value,status,attempt_count,last_error,next_attempt_at) VALUES('leaving-failed',?,5,'','imap','read',1,'failed',1,'server busy',?)`, leaving.ID, time.Now().Add(time.Hour)); err != nil {
			return err
		}
		// Being removed: its guard refuses any change.
		_, err := db.Write().Exec(`UPDATE accounts SET is_deleting=1 WHERE id=?`, leaving.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	page := f.request("alice", "GET", "/settings/operations/content", "")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "data-mail-operations-retry-all") {
		t.Fatal("Retry all not offered", page.Code)
	}
	response := f.request("alice", "POST", "/api/mail-operations/retry-failed", "")
	var result struct{ Retried, Skipped int }
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil {
		t.Fatal("retry all", response.Code, response.Body.String())
	}
	if result.Retried != 1 || result.Skipped != 1 {
		t.Fatalf("retry all = %+v, want 1 retried and the leaving account's change skipped", result)
	}
	status := func(owner, id string) string {
		var s string
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			return db.Read().QueryRow(`SELECT status FROM message_mutations WHERE id=?`, id).Scan(&s)
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if got := status("alice", "alice-failed"); got == "failed" {
		t.Fatalf("alice's change not retried: %s", got)
	}
	if got := status("alice", "leaving-failed"); got != "failed" {
		t.Fatalf("change of an account being removed was retried: %s", got)
	}
	if got := status("bob", "bob-only"); got != "failed" {
		t.Fatalf("bob's change was retried by alice: %s", got)
	}
}

func TestUserMailOperationsHTTPGroupActionsStayWithTheirOwner(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	leaving, err := f.accountStore.CreateAccount(t.Context(), "alice", &models.CreateAccountRequest{
		Provider: "imap", EmailAddress: "alex@example.com", DisplayName: "Alex",
		IMAPHost: "imap.example.com", SMTPHost: "smtp.example.com", Username: "alex", Password: "synthetic-only"})
	if err != nil {
		t.Fatal(err)
	}
	parked := time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)
	seed := func(owner string, statements ...func(db *storage.DB) error) {
		t.Helper()
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			for _, statement := range statements {
				if err := statement(db); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(query string, args ...any) func(db *storage.DB) error {
		return func(db *storage.DB) error {
			_, err := db.Write().Exec(query, args...)
			return err
		}
	}
	// One action of alice's: a failed change, a parked one, and a failed one
	// in an account being removed. Bob happens to have an action with the
	// same ID.
	alice := f.accounts["alice"].ID
	seed("alice",
		exec(`INSERT INTO messages(id,account_id,internet_message_id,subject) VALUES(1,?,'<lunch@test.invalid>','Lunch on Friday?')`, alice),
		exec(`INSERT INTO messages(id,account_id,internet_message_id,subject) VALUES(5,?,'<plans@test.invalid>','Weekend plans')`, leaving.ID),
		exec(`INSERT INTO message_mutations(id,account_id,message_id,folder_id,provider_type,kind,target_value,status,attempt_count,last_error,next_attempt_at,action_id,action_kind,action_source) VALUES('failed-read',?,1,'','imap','read',1,'failed',1,'server busy',?,'act-1','mark_read','you')`, alice, time.Now().Add(time.Hour)),
		exec(`INSERT INTO message_mutations(id,account_id,message_id,folder_id,provider_type,kind,target_value,status,attempt_count,last_error,next_attempt_at,action_id,action_kind,action_source) VALUES('parked-star',?,1,'','imap','starred',1,'failed',1,'folder removed remotely (imap discovery)',?,'act-1','mark_read','you')`, alice, parked),
		exec(`INSERT INTO message_mutations(id,account_id,message_id,folder_id,provider_type,kind,target_value,status,attempt_count,last_error,next_attempt_at,action_id,action_kind,action_source) VALUES('leaving-read',?,5,'','imap','read',1,'failed',1,'server busy',?,'act-1','mark_read','you')`, leaving.ID, time.Now().Add(time.Hour)),
		exec(`UPDATE accounts SET is_deleting=1 WHERE id=?`, leaving.ID),
	)
	seed("bob",
		exec(`INSERT INTO messages(id,account_id,internet_message_id,subject) VALUES(1,?,'<bob@test.invalid>','Book club notes')`, f.accounts["bob"].ID),
		exec(`INSERT INTO message_mutations(id,account_id,message_id,folder_id,provider_type,kind,target_value,status,attempt_count,last_error,next_attempt_at,action_id,action_kind,action_source) VALUES('bob-read',?,1,'','imap','read',1,'failed',1,'server busy',?,'act-1','mark_read','you')`, f.accounts["bob"].ID, time.Now().Add(time.Hour)),
	)

	page := f.request("alice", "GET", "/settings/operations/content", "")
	if page.Code != 200 || !strings.Contains(page.Body.String(), `data-mail-operation-group-row="act-1"`) || !strings.Contains(page.Body.String(), "Mark 2 messages as read") {
		t.Fatal("group not listed", page.Code, page.Body.String())
	}
	if strings.Contains(page.Body.String(), "Lunch on Friday?") {
		t.Fatal("a closed group lists its messages")
	}
	if open := f.request("alice", "GET", "/settings/operations/content?open=act-1", ""); !strings.Contains(open.Body.String(), "Lunch on Friday?") {
		t.Fatal("an open group came back without its messages")
	}
	changes := f.request("alice", "GET", "/settings/operations/actions/act-1", "")
	if changes.Code != 200 || !strings.Contains(changes.Body.String(), "Lunch on Friday?") || !strings.Contains(changes.Body.String(), "Weekend plans") || strings.Contains(changes.Body.String(), "Book club notes") {
		t.Fatal("group messages", changes.Code, changes.Body.String())
	}

	var result struct{ Changed, Skipped int }
	response := f.request("alice", "POST", "/api/mail-operations/actions/act-1/retry", "")
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Changed != 1 || result.Skipped != 1 {
		t.Fatalf("group retry = %d %s, want 1 retried and the leaving account skipped", response.Code, response.Body.String())
	}
	response = f.request("alice", "POST", "/api/mail-operations/actions/act-1/undo", "")
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Changed != 1 {
		t.Fatalf("group undo = %d %s", response.Code, response.Body.String())
	}
	status := func(owner, id string) string {
		var s string
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			return db.Read().QueryRow(`SELECT COALESCE((SELECT status FROM message_mutations WHERE id=?), 'gone')`, id).Scan(&s)
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	// The retried change was woken, so it may already be done.
	if got := status("alice", "failed-read"); got == "failed" {
		t.Errorf("alice's failed change was not retried")
	}
	for _, check := range []struct{ owner, id, want string }{
		{"alice", "parked-star", "gone"},
		{"alice", "leaving-read", "failed"},
		{"bob", "bob-read", "failed"},
	} {
		if got := status(check.owner, check.id); got != check.want {
			t.Errorf("%s %s = %s, want %s", check.owner, check.id, got, check.want)
		}
	}
	// Nothing left to undo: refused, as for a single change.
	if response := f.request("alice", "POST", "/api/mail-operations/actions/act-1/undo", ""); response.Code != 409 {
		t.Fatal("second group undo", response.Code, response.Body.String())
	}
}
