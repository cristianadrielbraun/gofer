package mail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
	"golang.org/x/oauth2"
)

type routedGmailState struct {
	phase                       int
	profileStatus               int
	emptyProfile                bool
	missing, expired, rejectOld bool
	rawMode                     string
}
type routedGmailBlock struct {
	owner, path, format string
	entered, release    chan struct{}
	once, releaseOnce   sync.Once
}

func (b *routedGmailBlock) unblock() { b.releaseOnce.Do(func() { close(b.release) }) }

type routedGmailAPI struct {
	mu     sync.Mutex
	states map[string]routedGmailState
	calls  map[string]int
	block  *routedGmailBlock
}

func (a *routedGmailAPI) change(owner string, fn func(*routedGmailState)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.states[owner]
	fn(&state)
	a.states[owner] = state
}
func (a *routedGmailAPI) count(owner, path, format string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[owner+"|"+path+"|"+format]
}
func (a *routedGmailAPI) stopAt(owner, path, format string) *routedGmailBlock {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := &routedGmailBlock{owner: owner, path: path, format: format, entered: make(chan struct{}), release: make(chan struct{})}
	a.block = b
	return b
}
func routedGmailMIME(owner string) string {
	return "Message-ID: <" + owner + "-m1@mail.test>\r\nSubject: " + owner + " private message\r\nFrom: Sender <sender@mail.test>\r\nTo: " + owner + "@mail.test\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=gofer\r\n\r\n--gofer\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + owner + " private body\r\n--gofer\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=report.txt\r\n\r\n" + owner + " attachment\r\n--gofer--\r\n"
}
func (a *routedGmailAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
	if r.URL.Path == "/token" {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", 400)
			return
		}
		owner = strings.Split(r.FormValue("refresh_token"), "-")[0]
	}
	a.mu.Lock()
	state, ok := a.states[owner]
	if !ok {
		a.mu.Unlock()
		http.Error(w, "unknown credential owner", 401)
		return
	}
	format := r.URL.Query().Get("format")
	a.calls[owner+"|"+r.URL.Path+"|"+format]++
	block := a.block
	a.mu.Unlock()
	if block != nil && block.owner == owner && block.path == r.URL.Path && block.format == format {
		block.once.Do(func() { close(block.entered) })
		select {
		case <-block.release:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/token" {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": owner + "-fresh", "refresh_token": owner + "-rotated", "token_type": "Bearer", "expires_in": 3600})
		return
	}
	if state.rejectOld && r.Header.Get("Authorization") == "Bearer "+owner+"-access" {
		http.Error(w, "expired", 401)
		return
	}
	cursor := strconv.Itoa(100 + state.phase*100)
	id := "m" + strconv.Itoa(state.phase+1)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/users/me/labels":
		_ = json.NewEncoder(w).Encode(map[string]any{"labels": []map[string]string{{"id": "INBOX", "name": "INBOX", "type": "system"}, {"id": "Label_Work", "name": "Work", "type": "user"}}})
	case r.URL.Path == "/users/me/profile":
		if state.profileStatus != 0 {
			w.Header().Set("Retry-After", "120")
			http.Error(w, "synthetic profile rejection", state.profileStatus)
			return
		}
		if state.emptyProfile {
			cursor = ""
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"historyId": cursor})
	case r.Method == http.MethodGet && r.URL.Path == "/users/me/messages":
		messages := []map[string]string{}
		if r.URL.Query().Get("labelIds") != "" {
			messages = append(messages, map[string]string{"id": id, "threadId": "thread"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": messages, "resultSizeEstimate": len(messages)})
	case r.URL.Path == "/users/me/history":
		if state.expired {
			http.Error(w, "history expired", 404)
			return
		}
		history := []map[string]any{}
		if r.URL.Query().Get("startHistoryId") != cursor {
			history = append(history, map[string]any{"id": cursor, "messagesAdded": []map[string]any{{"message": map[string]string{"id": id}}}, "messagesDeleted": []map[string]any{{"message": map[string]string{"id": "m1"}}}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"history": history, "historyId": cursor})
	case strings.HasPrefix(r.URL.Path, "/users/me/messages/"):
		messageID := strings.TrimPrefix(r.URL.Path, "/users/me/messages/")
		if strings.HasSuffix(messageID, "/modify") {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": strings.TrimSuffix(messageID, "/modify")})
			return
		}
		if state.missing {
			http.Error(w, "message temporarily missing", 404)
			return
		}
		if format == "raw" {
			raw := base64.RawURLEncoding.EncodeToString([]byte(routedGmailMIME(owner)))
			if state.rawMode == "invalid" {
				raw = "not-base64!"
			}
			if state.rawMode == "wrong-id" {
				messageID = "wrong-message"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": messageID, "raw": raw})
			return
		}
		labels := []string{"INBOX", "Label_Work", "UNREAD"}
		if state.phase > 0 {
			labels = []string{"INBOX", "Label_Work", "STARRED"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": messageID, "threadId": "thread", "labelIds": labels, "historyId": cursor, "internalDate": "1791288000000", "snippet": owner + " snippet", "payload": map[string]any{"mimeType": "multipart/mixed", "headers": []map[string]string{{"name": "Message-ID", "value": "<" + owner + "-" + messageID + "@mail.test>"}, {"name": "Subject", "value": owner + " private message"}, {"name": "From", "value": "Sender <sender@mail.test>"}, {"name": "To", "value": owner + "@mail.test"}}, "parts": []map[string]any{{"mimeType": "text/plain", "filename": "report.txt", "body": map[string]string{"attachmentId": "attachment-1"}}}}})
	default:
		http.Error(w, "unexpected Gmail endpoint", 404)
	}
}

type userGmailFixture struct {
	cancel      context.CancelFunc
	system      *storage.DB
	routing     *storage.AccountRouting
	accounts    *config.UserAccountStore
	credentials *mailauth.UserCredentials
	worker      *UserIMAP
	api         *routedGmailAPI
	ids         map[string]string
	blobs       string
}

func newUserGmailFixture(t *testing.T) *userGmailFixture {
	t.Helper()
	f := &userGmailFixture{api: &routedGmailAPI{states: map[string]routedGmailState{"alice": {}, "bob": {}}, calls: map[string]int{}}, ids: map[string]string{}, blobs: t.TempDir()}
	server := httptest.NewServer(f.api)
	t.Cleanup(server.Close)
	previous := gmailAPIBaseURL
	gmailAPIBaseURL = server.URL
	previousWait := gmailAPIMessageMetadataWaitBeforeRetry
	gmailAPIMessageMetadataWaitBeforeRetry = func(ctx context.Context, _ int) error { return ctx.Err() }
	t.Cleanup(func() { gmailAPIBaseURL = previous; gmailAPIMessageMetadataWaitBeforeRetry = previousWait })
	var err error
	f.system, err = storage.New(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.system.Close() })
	for _, owner := range []string{"alice", "bob"} {
		if _, err := f.system.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES(?,?,?)`, owner, owner, owner); err != nil {
			t.Fatal(err)
		}
	}
	stores, err := storage.NewUserStores(f.system, storage.UserStoreOptions{MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := stores.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	f.routing, err = storage.NewAccountRouting(stores)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("0123456789abcdef0123456789abcdef")
	f.accounts, err = config.NewUserAccountStore(f.routing, key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	f.cancel = cancel
	f.credentials, err = mailauth.NewUserCredentials(ctx, &mailauth.Config{GoogleClient: &oauth2.Config{ClientID: "client", ClientSecret: "secret", Endpoint: oauth2.Endpoint{TokenURL: server.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}}}, f.routing, key)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	f.worker, err = NewUserIMAP(ctx, f.accounts, store.NewBlobStore(f.blobs), NewEventBus())
	if err != nil {
		cancel()
		f.credentials.Wait()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); f.worker.Wait(); f.credentials.Wait() })
	if err := f.worker.SetCredentials(f.credentials); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		account, err := f.accounts.CreateAccount(t.Context(), owner, providers.GmailAccountRequest(owner+"@mail.test", owner, "same-subject"))
		if err != nil {
			t.Fatal(err)
		}
		f.ids[owner] = account.ID
		expiry := time.Now().Add(time.Hour)
		if err := f.credentials.UpsertForUser(t.Context(), owner, account.ID, "google", "same-subject", owner+"-access", owner+"-refresh", "Bearer", &expiry, "https://mail.google.com/"); err != nil {
			t.Fatal(err)
		}
	}
	return f
}
func (f *userGmailFixture) local(t *testing.T, owner string, fn func(*storage.DB) error) {
	t.Helper()
	if err := f.routing.WithUser(t.Context(), owner, fn); err != nil {
		t.Fatal(err)
	}
}
func (f *userGmailFixture) messageID(t *testing.T, owner, providerID string) int64 {
	t.Helper()
	var id int64
	f.local(t, owner, func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT id FROM messages WHERE account_id=? AND remote_message_id=?`, f.ids[owner], providerID).Scan(&id)
	})
	return id
}
func gmailAwait(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("Gmail operation did not finish")
		return nil
	}
}
func gmailStarted(t *testing.T, b *routedGmailBlock) {
	t.Helper()
	select {
	case <-b.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("Gmail endpoint was not reached")
	}
}
func gmailAsync(fn func() error) <-chan error {
	result := make(chan error, 1)
	go func() { result <- fn() }()
	return result
}

func TestUserGmailImportsAndFetchesIsolatedBodies(t *testing.T) {
	f := newUserGmailFixture(t)
	events := f.worker.Events().Subscribe()
	defer f.worker.Events().Unsubscribe(events)
	ids := map[string]int64{}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.worker.Sync(t.Context(), owner, f.ids[owner]); err != nil {
			t.Fatal(err)
		}
		ids[owner] = f.messageID(t, owner, "m1")
		f.local(t, owner, func(db *storage.DB) error {
			state, err := db.GetLabelSyncState(t.Context(), f.ids[owner], storage.LabelProviderGmail, "messages")
			if err != nil {
				return err
			}
			if state.Cursor != "100" || !state.LastFullSyncAt.Valid {
				return fmt.Errorf("cursor/full import: %+v", state)
			}
			if db.IsBodyFetchedInternal(t.Context(), ids[owner]) {
				return errors.New("metadata sync fetched a body")
			}
			var labels int
			if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=? AND l.account_id=? AND l.provider_id='Label_Work'`, ids[owner], f.ids[owner]).Scan(&labels); err != nil {
				return err
			}
			if labels != 1 {
				return errors.New("Gmail labels were not imported")
			}
			return nil
		})
	}
	if ids["alice"] != ids["bob"] {
		t.Fatal("fixture did not exercise overlapping local message IDs")
	}
	for len(events) > 0 {
		event := <-events
		if event.AccountID != "" {
			want := "alice"
			if event.AccountID == f.ids["bob"] {
				want = "bob"
			}
			if event.UserID != want {
				t.Fatalf("unowned provider event: %+v", event)
			}
		}
	}
	requests := make([]<-chan error, 8)
	for i := range requests {
		requests[i] = gmailAsync(func() error { return f.worker.EnsureBody(t.Context(), "alice", ids["alice"]) })
	}
	for _, result := range requests {
		if err := gmailAwait(t, result); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.worker.EnsureBody(t.Context(), "bob", ids["bob"]); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		if f.api.count(owner, "/users/me/messages/m1", "raw") != 1 {
			t.Fatalf("body fetch not coalesced for %s", owner)
		}
		f.local(t, owner, func(db *storage.DB) error {
			var text, attachment string
			if err := db.Read().QueryRow(`SELECT body_text_path FROM messages WHERE id=?`, ids[owner]).Scan(&text); err != nil {
				return err
			}
			body, err := os.ReadFile(text)
			if err != nil {
				return err
			}
			if !strings.Contains(string(body), owner+" private body") {
				return errors.New("foreign body")
			}
			if err := db.Read().QueryRow(`SELECT storage_path FROM attachments WHERE message_id=?`, ids[owner]).Scan(&attachment); err != nil {
				return err
			}
			data, err := os.ReadFile(attachment)
			if err != nil {
				return err
			}
			if !strings.Contains(string(data), owner+" attachment") {
				return errors.New("foreign attachment")
			}
			return nil
		})
	}
	for _, table := range []string{"messages", "folders", "labels", "label_sync_state", "gmail_message_fetch_queue"} {
		var count int
		if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("central mailbox copies in %s: %d %v", table, count, err)
		}
	}
}

func TestUserGmailHistoryRecoveryAndMetadataRetry(t *testing.T) {
	f := newUserGmailFixture(t)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.worker.Sync(t.Context(), owner, f.ids[owner]); err != nil {
			t.Fatal(err)
		}
	}
	f.api.change("alice", func(state *routedGmailState) { state.phase = 1; state.missing = true })
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	f.local(t, "alice", func(db *storage.DB) error {
		var count int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM gmail_message_fetch_queue WHERE account_id=? AND provider_message_id='m2'`, f.ids["alice"]).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return errors.New("unavailable metadata not queued")
		}
		return nil
	})
	var next time.Time
	var due int64
	if err := f.system.Read().QueryRow(`SELECT next_due_ms FROM gofer_account_poll_schedule WHERE account_id=?`, f.ids["alice"]).Scan(&due); err != nil {
		t.Fatal(err)
	}
	f.local(t, "alice", func(db *storage.DB) error {
		var err error
		next, err = db.NextGmailQueueAttempt(t.Context(), f.ids["alice"])
		return err
	})
	if next.IsZero() || next.After(time.Now().Add(2*time.Minute)) {
		t.Fatalf("metadata retry ignored: %v", next)
	}
	if delta := time.UnixMilli(due).Sub(next); delta > time.Second || delta < -time.Second {
		t.Fatalf("central poll did not preserve metadata retry deadline: next=%v central=%v", next, time.UnixMilli(due))
	}
	f.api.change("alice", func(state *routedGmailState) { state.missing = false })
	f.local(t, "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE gmail_message_fetch_queue SET next_attempt_at=datetime('now','-1 minute') WHERE account_id=?`, f.ids["alice"])
		return err
	})
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	_ = f.messageID(t, "alice", "m2")
	f.local(t, "alice", func(db *storage.DB) error {
		var count int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM gmail_message_fetch_queue`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errors.New("recovered metadata remained queued")
		}
		return nil
	})
	f.api.change("alice", func(state *routedGmailState) { state.phase = 2; state.expired = true })
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	_ = f.messageID(t, "alice", "m3")
	for _, owner := range []string{"alice", "bob"} {
		f.local(t, owner, func(db *storage.DB) error {
			state, err := db.GetLabelSyncState(t.Context(), f.ids[owner], storage.LabelProviderGmail, "messages")
			want := "300"
			if owner == "bob" {
				want = "100"
			}
			if err == nil && state.Cursor != want {
				return fmt.Errorf("%s cursor: %q", owner, state.Cursor)
			}
			return err
		})
	}
}

func TestUserGmailBlockedRequestsReleaseCacheAndCancel(t *testing.T) {
	f := newUserGmailFixture(t)
	block := f.api.stopAt("alice", "/users/me/labels", "")
	defer block.unblock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	alice := gmailAsync(func() error { return f.worker.Sync(ctx, "alice", f.ids["alice"]) })
	gmailStarted(t, block)
	work, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	if err := f.worker.Sync(work, "bob", f.ids["bob"]); err != nil {
		t.Fatalf("Bob blocked by Alice provider wait: %v", err)
	}
	if err := f.routing.WithUser(work, "bob", func(db *storage.DB) error { return db.SetSetting(work, "bob", "cache_probe", "written") }); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := gmailAwait(t, alice); !errors.Is(err, context.Canceled) {
		t.Fatalf("provider request not canceled: %v", err)
	}
	block.unblock()
	f.local(t, "alice", func(db *storage.DB) error {
		var count int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errors.New("canceled session published messages")
		}
		return nil
	})
}

func TestUserGmailRefreshesUnauthorizedTokensAndReplaysLabels(t *testing.T) {
	f := newUserGmailFixture(t)
	f.api.change("alice", func(state *routedGmailState) { state.rejectOld = true })
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	if f.api.count("alice", "/token", "") != 1 {
		t.Fatal("unauthorized request did not refresh once")
	}
	id := f.messageID(t, "alice", "m1")
	f.local(t, "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`INSERT INTO label_mutation_queue(account_id,message_id,provider_type,operation,label_name) VALUES(?,?,'gmail','add','Work')`, f.ids["alice"], id)
		return err
	})
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	if f.api.count("alice", "/users/me/messages/m1/modify", "") != 1 || f.api.count("bob", "/users/me/messages/m1/modify", "") != 0 {
		t.Fatal("label replay escaped its owner")
	}
	f.local(t, "alice", func(db *storage.DB) error {
		count, err := db.CountLabelMutations(t.Context(), f.ids["alice"], storage.LabelProviderGmail)
		if err == nil && count != 0 {
			return errors.New("label replay did not finish")
		}
		return err
	})
	if err := f.worker.EnsureBody(t.Context(), "alice", id); err != nil {
		t.Fatal(err)
	}
	if f.api.count("alice", "/token", "") != 1 {
		t.Fatal("refreshed token was not retained for body fetching")
	}
}

func TestUserGmailColdBodyRefreshesUnauthorizedToken(t *testing.T) {
	f := newUserGmailFixture(t)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	f.api.change("alice", func(state *routedGmailState) { state.rejectOld = true })
	id := f.messageID(t, "alice", "m1")
	if err := f.worker.EnsureBody(t.Context(), "alice", id); err != nil {
		t.Fatal(err)
	}
	if f.api.count("alice", "/token", "") != 1 || f.api.count("alice", "/users/me/messages/m1", "raw") != 2 {
		t.Fatal("cold body did not refresh and retry exactly once")
	}
	if err := f.worker.EnsureBody(t.Context(), "alice", id); err != nil {
		t.Fatal(err)
	}
	if f.api.count("alice", "/users/me/messages/m1", "raw") != 2 || f.api.count("bob", "/token", "") != 0 {
		t.Fatal("cached body retried or refreshed another owner's token")
	}
}

func TestUserGmailMetadataRejectsChangedAccountIdentity(t *testing.T) {
	f := newUserGmailFixture(t)
	block := f.api.stopAt("alice", "/users/me/messages/m1", "metadata")
	defer block.unblock()
	result := gmailAsync(func() error { return f.worker.Sync(t.Context(), "alice", f.ids["alice"]) })
	gmailStarted(t, block)
	f.local(t, "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET provider_account_id='new-subject' WHERE id=?`, f.ids["alice"])
		return err
	})
	block.unblock()
	if err := gmailAwait(t, result); err == nil {
		t.Fatal("metadata session accepted a changed mailbox identity")
	}
	f.local(t, "alice", func(db *storage.DB) error {
		var count int
		if err := db.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM messages)+(SELECT COUNT(*) FROM label_sync_state WHERE cursor<>'')`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errors.New("stale metadata or cursor was published")
		}
		return nil
	})
}

func TestUserGmailRejectsChangedOrInvalidBodies(t *testing.T) {
	for _, change := range []string{"message-id", "account-subject", "deleted", "publication-failure", "invalid", "wrong-id"} {
		t.Run(change, func(t *testing.T) {
			f := newUserGmailFixture(t)
			if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
				t.Fatal(err)
			}
			id := f.messageID(t, "alice", "m1")
			if change == "invalid" || change == "wrong-id" {
				f.api.change("alice", func(state *routedGmailState) { state.rawMode = change })
			}
			block := f.api.stopAt("alice", "/users/me/messages/m1", "raw")
			defer block.unblock()
			result := gmailAsync(func() error { return f.worker.EnsureBody(t.Context(), "alice", id) })
			gmailStarted(t, block)
			f.local(t, "alice", func(db *storage.DB) error {
				var query string
				switch change {
				case "message-id":
					query = `UPDATE messages SET remote_message_id='replacement' WHERE id=` + strconv.FormatInt(id, 10)
				case "account-subject":
					query = `UPDATE accounts SET provider_account_id='changed-subject'`
				case "deleted":
					query = `UPDATE message_folder_state SET is_deleted=1 WHERE message_id=` + strconv.FormatInt(id, 10)
				case "publication-failure":
					query = `CREATE TRIGGER reject_gmail_body AFTER UPDATE OF body_text_path ON messages BEGIN SELECT RAISE(ABORT,'injected publication failure'); END`
				default:
					return nil
				}
				_, err := db.Write().Exec(query)
				return err
			})
			block.unblock()
			if err := gmailAwait(t, result); err == nil {
				t.Fatal("invalid/stale body published")
			}
			f.local(t, "alice", func(db *storage.DB) error {
				if db.IsBodyFetchedInternal(t.Context(), id) {
					return errors.New("failed body marked fetched")
				}
				var stored int
				if err := db.Read().QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id=? AND storage_path IS NOT NULL AND storage_path<>''`, id).Scan(&stored); err != nil {
					return err
				}
				if stored != 0 {
					return errors.New("partial attachment publication")
				}
				return nil
			})
			files := 0
			if err := filepath.WalkDir(f.blobs, func(_ string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.Type().IsRegular() {
					files++
				}
				return nil
			}); err != nil || files != 0 {
				t.Fatalf("failed candidate remains: %d %v", files, err)
			}
			if change == "publication-failure" {
				f.local(t, "alice", func(db *storage.DB) error { _, err := db.Write().Exec(`DROP TRIGGER reject_gmail_body`); return err })
				if err := f.worker.EnsureBody(t.Context(), "alice", id); err != nil {
					t.Fatalf("body retry failed: %v", err)
				}
			}
		})
	}
}

func TestUserGmailDeletionCancelsBodyAndCleansCredentials(t *testing.T) {
	f := newUserGmailFixture(t)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.worker.Sync(t.Context(), owner, f.ids[owner]); err != nil {
			t.Fatal(err)
		}
	}
	id := f.messageID(t, "alice", "m1")
	block := f.api.stopAt("alice", "/users/me/messages/m1", "raw")
	defer block.unblock()
	result := gmailAsync(func() error { return f.worker.EnsureBody(t.Context(), "alice", id) })
	gmailStarted(t, block)
	if err := f.worker.EnsureBody(t.Context(), "bob", f.messageID(t, "bob", "m1")); err != nil {
		t.Fatal(err)
	}
	if err := f.routing.RequestAccountDeletion(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	if err := f.accounts.DeleteAccount(t.Context(), "alice", f.ids["alice"], f.worker.Cleanup); err != nil {
		t.Fatal(err)
	}
	if err := gmailAwait(t, result); err == nil {
		t.Fatal("deleted body fetch succeeded")
	}
	var count int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_mailbox_credentials WHERE account_id=?`, f.ids["alice"]).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted grant remains: %d %v", count, err)
	}
	if _, err := os.Stat(filepath.Join(f.blobs, f.ids["alice"])); !os.IsNotExist(err) {
		t.Fatalf("deleted body files remain: %v", err)
	}
	if err := f.worker.EnsureBody(t.Context(), "bob", f.messageID(t, "bob", "m1")); err != nil {
		t.Fatalf("foreign cache damaged: %v", err)
	}
}

func TestUserGmailManualAndPeriodicReceiveAvoidIMAPIdle(t *testing.T) {
	f := newUserGmailFixture(t)
	if _, _, err := f.worker.StartManualSync(t.Context(), "bob", []string{f.ids["alice"]}); !errors.Is(err, storage.ErrAccountRoute) {
		t.Fatalf("foreign manual run: %v", err)
	}
	events := f.worker.Events().Subscribe()
	defer f.worker.Events().Unsubscribe(events)
	if _, started, err := f.worker.StartManualSync(t.Context(), "alice", []string{f.ids["alice"]}); err != nil || !started {
		t.Fatalf("Gmail manual run: %t %v", started, err)
	}
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
waitManual:
	for {
		select {
		case event := <-events:
			if event.Type == EventManualSyncComplete {
				if event.UserID != "alice" || event.Payload["failures"] != 0 {
					t.Fatalf("manual result: %+v", event)
				}
				break waitManual
			}
		case <-timer.C:
			t.Fatal("manual Gmail receive did not complete")
		}
	}
	if err := f.worker.Start(UserIMAPBackgroundOptions{PollInterval: time.Hour, ScanInterval: 25 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for f.api.count("bob", "/users/me/messages/m1", "metadata") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.api.count("bob", "/users/me/messages/m1", "metadata") == 0 {
		t.Fatal("directory discovery skipped Gmail")
	}
	f.worker.mu.Lock()
	watchers := f.worker.idleCount
	f.worker.mu.Unlock()
	if watchers != 0 {
		t.Fatal("Gmail receive started IMAP IDLE watchers")
	}
}
