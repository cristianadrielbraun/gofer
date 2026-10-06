package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

const userOutlookScopes = "https://graph.microsoft.com/Mail.ReadWrite https://graph.microsoft.com/Mail.Send https://graph.microsoft.com/MailboxSettings.ReadWrite"

type routedOutlookState struct {
	tokenThrottled                                                                      bool
	throttled                                                                           bool
	phase                                                                               int
	expired, paged, failSecond, rejectOld, incomplete, wrongLink, sparse, missingDetail bool
	mimeStatus                                                                          int
	rename                                                                              string
}
type routedOutlookAPI struct {
	mu     sync.Mutex
	states map[string]routedOutlookState
	calls  map[string]int
	block  *routedGmailBlock
	base   string
}

func (a *routedOutlookAPI) change(owner string, fn func(*routedOutlookState)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.states[owner]
	fn(&state)
	a.states[owner] = state
}
func (a *routedOutlookAPI) count(owner, path string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[owner+"|"+path]
}
func (a *routedOutlookAPI) stopAt(owner, path string) *routedGmailBlock {
	a.mu.Lock()
	defer a.mu.Unlock()
	b := &routedGmailBlock{owner: owner, path: path, entered: make(chan struct{}), release: make(chan struct{})}
	a.block = b
	return b
}
func routedOutlookMessage(owner, id string, phase int) map[string]any {
	return map[string]any{"id": id, "internetMessageId": "<" + owner + "-" + id + "@mail.test>", "conversationId": "same-thread", "parentFolderId": "inbox-id", "subject": owner + " private message", "bodyPreview": owner + " preview", "from": map[string]any{"emailAddress": map[string]string{"address": "sender@mail.test"}}, "toRecipients": []map[string]any{{"emailAddress": map[string]string{"address": owner + "@mail.test"}}}, "receivedDateTime": "2026-10-06T10:00:00Z", "categories": []string{"Work"}, "hasAttachments": true, "isRead": phase > 0, "flag": map[string]string{"flagStatus": "flagged"}}
}
func (a *routedOutlookAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
	if r.URL.Path == "/token" {
		_ = r.ParseForm()
		owner = strings.Split(r.FormValue("refresh_token"), "-")[0]
	}
	a.mu.Lock()
	state, ok := a.states[owner]
	a.calls[owner+"|"+r.URL.Path]++
	block := a.block
	a.mu.Unlock()
	if !ok {
		http.Error(w, "unknown owner", 401)
		return
	}
	if block != nil && block.owner == owner && block.path == r.URL.Path {
		block.once.Do(func() { close(block.entered) })
		select {
		case <-block.release:
		case <-r.Context().Done():
			return
		}
	}
	if r.URL.Path == "/token" {
		if state.tokenThrottled {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "300")
			w.WriteHeader(429)
			_, _ = fmt.Fprint(w, `{"error":"temporarily_unavailable"}`)
			return
		}
		if r.FormValue("scope") != userOutlookScopes {
			http.Error(w, "wrong Graph scopes", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": owner + "-fresh", "refresh_token": owner + "-rotated", "expires_in": 3600, "scope": userOutlookScopes})
		return
	}
	if !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
		http.Error(w, "missing immutable ID preference", 400)
		return
	}
	if state.rejectOld && r.Header.Get("Authorization") == "Bearer "+owner+"-access" {
		http.Error(w, "expired", 401)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/me/mailFolders":
		name := "Inbox"
		if state.rename != "" {
			name = state.rename
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{"id": "inbox-id", "displayName": name, "totalItemCount": 1, "unreadItemCount": 1, "childFolderCount": 1}, {"id": "trash-id", "displayName": "Deleted Items"}}})
	case r.URL.Path == "/me/mailFolders/inbox-id/childFolders":
		name := "Projects"
		if state.rename != "" {
			name = state.rename
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]any{{"id": "child-id", "displayName": name, "parentFolderId": "inbox-id"}}})
	case r.URL.Path == "/me/mailFolders/inbox":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "inbox-id", "displayName": "Inbox"})
	case r.URL.Path == "/me/mailFolders/deleteditems":
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "trash-id", "displayName": "Deleted Items"})
	case r.URL.Path == "/me/outlook/masterCategories":
		_ = json.NewEncoder(w).Encode(map[string]any{"value": []map[string]string{{"id": "work-id", "displayName": "Work", "color": "preset0"}}})
	case strings.HasSuffix(r.URL.Path, "/messages/delta"):
		folder := strings.Split(r.URL.Path, "/")[3]
		if state.throttled && folder == "inbox-id" {
			w.Header().Set("Retry-After", "300")
			http.Error(w, "throttled", 429)
			return
		}
		q := r.URL.Query()
		token := q.Get("$deltatoken")
		if state.expired && token != "" {
			http.Error(w, "expired delta", 410)
			return
		}
		if state.failSecond && q.Get("$skiptoken") != "" {
			http.Error(w, "try again", 503)
			return
		}
		value := []map[string]any{}
		if folder == "inbox-id" {
			id := fmt.Sprintf("m%d", state.phase+1)
			if q.Get("$skiptoken") != "" {
				id = "m2"
			}
			if state.sparse {
				value = append(value, map[string]any{"id": id})
			} else {
				value = append(value, routedOutlookMessage(owner, id, state.phase))
			}
			if state.phase > 0 && token != "" {
				value = append(value, map[string]any{"id": "m1", "@removed": map[string]string{"reason": "deleted"}})
			}
		}
		response := map[string]any{"value": value}
		if !state.incomplete {
			if state.paged && folder == "inbox-id" && token == "" && q.Get("$skiptoken") == "" {
				response["@odata.nextLink"] = a.base + r.URL.Path + "?%24skiptoken=page2"
			} else {
				response["@odata.deltaLink"] = a.base + r.URL.Path + fmt.Sprintf("?%%24deltatoken=cursor%d", state.phase)
			}
			if state.wrongLink {
				response["@odata.nextLink"] = "https://untrusted.invalid/steal"
				delete(response, "@odata.deltaLink")
			}
		}
		_ = json.NewEncoder(w).Encode(response)
	case r.URL.Path == "/me/messages/m1/$value" || r.URL.Path == "/me/messages/m2/$value":
		if state.mimeStatus != 0 {
			http.Error(w, "MIME unavailable", state.mimeStatus)
			return
		}
		w.Header().Set("Content-Type", "message/rfc822")
		_, _ = fmt.Fprint(w, routedGmailMIME(owner))
	case r.Method == http.MethodGet && (r.URL.Path == "/me/messages/m1" || r.URL.Path == "/me/messages/m2"):
		if state.missingDetail {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(routedOutlookMessage(owner, strings.TrimPrefix(r.URL.Path, "/me/messages/"), state.phase))
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/me/messages/"):
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

type userOutlookFixture struct {
	system      *storage.DB
	routing     *storage.AccountRouting
	accounts    *config.UserAccountStore
	credentials *mailauth.UserCredentials
	worker      *UserIMAP
	api         *routedOutlookAPI
	ids         map[string]string
	blobs       string
}

func newUserOutlookFixture(t *testing.T) *userOutlookFixture {
	t.Helper()
	f := &userOutlookFixture{api: &routedOutlookAPI{states: map[string]routedOutlookState{"alice": {}, "bob": {}}, calls: map[string]int{}}, ids: map[string]string{}, blobs: t.TempDir()}
	server := httptest.NewServer(f.api)
	f.api.base = server.URL
	t.Cleanup(server.Close)
	previous := outlookGraphBaseURL
	outlookGraphBaseURL = server.URL
	t.Cleanup(func() { outlookGraphBaseURL = previous })
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
	f.credentials, err = mailauth.NewUserCredentials(ctx, &mailauth.Config{MicrosoftClient: &oauth2.Config{ClientID: "client", ClientSecret: "secret", Endpoint: oauth2.Endpoint{TokenURL: server.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}}}, f.routing, key)
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
		account, err := f.accounts.CreateAccount(t.Context(), owner, providers.OutlookAccountRequest(owner+"@mail.test", owner, "same-subject"))
		if err != nil {
			t.Fatal(err)
		}
		f.ids[owner] = account.ID
		expiry := time.Now().Add(time.Hour)
		if err := f.credentials.UpsertForUser(t.Context(), owner, account.ID, "microsoft", "same-subject", owner+"-access", owner+"-refresh", "Bearer", &expiry, userOutlookScopes); err != nil {
			t.Fatal(err)
		}
	}
	return f
}
func (f *userOutlookFixture) local(t *testing.T, owner string, fn func(*storage.DB) error) {
	t.Helper()
	if err := f.routing.WithUser(t.Context(), owner, fn); err != nil {
		t.Fatal(err)
	}
}
func (f *userOutlookFixture) messageID(t *testing.T, owner, providerID string) int64 {
	t.Helper()
	var id int64
	f.local(t, owner, func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT id FROM messages WHERE account_id=? AND remote_message_id=?`, f.ids[owner], providerID).Scan(&id)
	})
	return id
}
func TestUserOutlookImportHistoryAndBodiesAreIsolated(t *testing.T) {
	f := newUserOutlookFixture(t)
	events := f.worker.Events().Subscribe()
	defer f.worker.Events().Unsubscribe(events)
	ids := map[string]int64{}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.worker.Sync(t.Context(), owner, f.ids[owner]); err != nil {
			t.Fatal(err)
		}
		ids[owner] = f.messageID(t, owner, "m1")
		f.local(t, owner, func(db *storage.DB) error {
			var folders, cursors, labels int
			if err := db.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM folders),(SELECT COUNT(*) FROM folders WHERE sync_cursor LIKE '%cursor0%' AND last_full_sync_at IS NOT NULL),(SELECT COUNT(*) FROM message_labels WHERE message_id=?)`, ids[owner]).Scan(&folders, &cursors, &labels); err != nil {
				return err
			}
			if folders != 3 || cursors != 3 || labels != 1 || db.IsBodyFetchedInternal(t.Context(), ids[owner]) {
				return fmt.Errorf("import: folders=%d cursors=%d labels=%d", folders, cursors, labels)
			}
			return nil
		})
	}
	if ids["alice"] != ids["bob"] {
		t.Fatal("message IDs did not overlap")
	}
	for len(events) > 0 {
		e := <-events
		if e.AccountID != "" {
			want := "alice"
			if e.AccountID == f.ids["bob"] {
				want = "bob"
			}
			if e.UserID != want {
				t.Fatalf("unowned event: %+v", e)
			}
		}
	}
	reads := make([]<-chan error, 8)
	for i := range reads {
		reads[i] = gmailAsync(func() error { return f.worker.EnsureBody(t.Context(), "alice", ids["alice"]) })
	}
	for _, ch := range reads {
		if err := gmailAwait(t, ch); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.worker.EnsureBody(t.Context(), "bob", ids["bob"]); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		if f.api.count(owner, "/me/messages/m1/$value") != 1 {
			t.Fatal("body request not coalesced")
		}
		f.local(t, owner, func(db *storage.DB) error {
			var text, attachment string
			if err := db.Read().QueryRow(`SELECT body_text_path FROM messages WHERE id=?`, ids[owner]).Scan(&text); err != nil {
				return err
			}
			if err := db.Read().QueryRow(`SELECT storage_path FROM attachments WHERE message_id=?`, ids[owner]).Scan(&attachment); err != nil {
				return err
			}
			for _, path := range []string{text, attachment} {
				raw, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if !strings.Contains(string(raw), owner+" ") {
					return errors.New("foreign MIME publication")
				}
			}
			return nil
		})
	}
	f.api.change("alice", func(s *routedOutlookState) { s.phase = 1; s.rename = "Renamed projects" })
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	_ = f.messageID(t, "alice", "m2")
	f.local(t, "alice", func(db *storage.DB) error {
		var gone int
		var name, cursor string
		if err := db.Read().QueryRow(`SELECT is_deleted FROM message_folder_state WHERE message_id=?`, ids["alice"]).Scan(&gone); err != nil {
			return err
		}
		if err := db.Read().QueryRow(`SELECT (SELECT name FROM folders WHERE provider_remote_id='child-id'),sync_cursor FROM folders WHERE provider_remote_id='inbox-id'`).Scan(&name, &cursor); err != nil {
			return err
		}
		if gone != 1 || !strings.Contains(name, "Renamed projects") || !strings.Contains(cursor, "cursor1") {
			return fmt.Errorf("delta/rename: %d %s %s", gone, name, cursor)
		}
		return nil
	})
	f.local(t, "bob", func(db *storage.DB) error {
		var count int
		err := db.Read().QueryRow(`SELECT COUNT(*) FROM folders WHERE sync_cursor LIKE '%cursor0%'`).Scan(&count)
		if err == nil && count != 3 {
			return errors.New("Bob cursor changed")
		}
		return err
	})
	for _, table := range []string{"messages", "folders", "labels", "attachments"} {
		var count int
		if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("central mail table %s: %d %v", table, count, err)
		}
	}
}

func TestUserOutlookResumesPagesAndRecoversExpiredDelta(t *testing.T) {
	f := newUserOutlookFixture(t)
	f.api.change("alice", func(s *routedOutlookState) { s.paged = true; s.failSecond = true })
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err == nil {
		t.Fatal("failed second page accepted")
	}
	f.local(t, "alice", func(db *storage.DB) error {
		var cursor string
		var current int
		if err := db.Read().QueryRow(`SELECT sync_cursor,sync_progress_current FROM folders WHERE provider_remote_id='inbox-id'`).Scan(&cursor, &current); err != nil {
			return err
		}
		if !outlookGraphDeltaNextLink(cursor) || current != 1 {
			return fmt.Errorf("resume checkpoint: %s %d", cursor, current)
		}
		return nil
	})
	f.api.change("alice", func(s *routedOutlookState) { s.failSecond = false })
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"m1", "m2"} {
		_ = f.messageID(t, "alice", id)
	}
	f.api.change("alice", func(s *routedOutlookState) { s.paged = false; s.expired = true; s.phase = 1 })
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	f.local(t, "alice", func(db *storage.DB) error {
		var live int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_folder_state WHERE is_deleted=0`).Scan(&live); err != nil {
			return err
		}
		if live != 1 {
			return fmt.Errorf("expired baseline live rows: %d", live)
		}
		return nil
	})
}

func TestUserOutlookBlockedSyncReleasesCacheAndHonorsCancellation(t *testing.T) {
	f := newUserOutlookFixture(t)
	block := f.api.stopAt("alice", "/me/mailFolders")
	defer block.unblock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := gmailAsync(func() error { return f.worker.Sync(ctx, "alice", f.ids["alice"]) })
	gmailStarted(t, block)
	work, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	if err := f.worker.Sync(work, "bob", f.ids["bob"]); err != nil {
		t.Fatal(err)
	}
	if err := f.routing.WithUser(work, "bob", func(db *storage.DB) error { return db.SetSetting(work, "bob", "cache_probe", "ok") }); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := gmailAwait(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	f.local(t, "alice", func(db *storage.DB) error {
		var n int
		err := db.Read().QueryRow(`SELECT COUNT(*) FROM folders`).Scan(&n)
		if err == nil && n != 0 {
			return errors.New("canceled sync published folders")
		}
		return err
	})
}

func TestUserOutlookRefreshAndCategoryReplay(t *testing.T) {
	f := newUserOutlookFixture(t)
	f.api.change("alice", func(s *routedOutlookState) { s.rejectOld = true })
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	if f.api.count("alice", "/token") != 1 {
		t.Fatal("sync did not retain the refreshed Graph token")
	}
	id := f.messageID(t, "alice", "m1")
	f.local(t, "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`INSERT INTO label_mutation_queue(account_id,message_id,provider_type,operation,label_name) VALUES(?,?,'outlook','add','Work')`, f.ids["alice"], id)
		return err
	})
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	f.local(t, "alice", func(db *storage.DB) error {
		n, err := db.CountLabelMutations(t.Context(), f.ids["alice"], storage.LabelProviderOutlook)
		if err == nil && n != 0 {
			return errors.New("category replay unfinished")
		}
		return err
	})
	// Exactly one GET state plus one PATCH after receiving; Bob is untouched.
	if f.api.count("alice", "/me/messages/m1") != 2 || f.api.count("bob", "/me/messages/m1") != 0 {
		t.Fatal("category replay escaped owner or omitted patch")
	}
	if err := f.worker.EnsureBody(t.Context(), "alice", id); err != nil {
		t.Fatal(err)
	}
	if f.api.count("alice", "/token") != 1 {
		t.Fatal("body failed to reuse refreshed token")
	}
}

func TestUserOutlookColdBodyRefreshesUnauthorizedToken(t *testing.T) {
	f := newUserOutlookFixture(t)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	f.api.change("alice", func(s *routedOutlookState) { s.rejectOld = true })
	if err := f.worker.EnsureBody(t.Context(), "alice", f.messageID(t, "alice", "m1")); err != nil {
		t.Fatal(err)
	}
	if f.api.count("alice", "/token") != 1 || f.api.count("alice", "/me/messages/m1/$value") != 2 {
		t.Fatal("MIME did not refresh and retry once")
	}
}

func TestUserOutlookMetadataFailureRetainsCheckpointForRetry(t *testing.T) {
	f := newUserOutlookFixture(t)
	f.api.change("alice", func(s *routedOutlookState) { s.sparse = true; s.missingDetail = true })
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err == nil {
		t.Fatal("missing detail advanced checkpoint")
	}
	f.local(t, "alice", func(db *storage.DB) error {
		var n int
		err := db.Read().QueryRow(`SELECT COUNT(*) FROM folders WHERE provider_remote_id='inbox-id' AND sync_cursor<>''`).Scan(&n)
		if err == nil && n != 0 {
			return errors.New("missing detail cursor advanced")
		}
		return err
	})
	var next int64
	if err := f.system.Read().QueryRow(`SELECT next_due_ms FROM gofer_account_poll_schedule WHERE account_id=?`, f.ids["alice"]).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if time.UnixMilli(next).After(time.Now().Add(time.Minute)) {
		t.Fatal("failed Graph receiving hidden behind long poll interval")
	}
	f.api.change("alice", func(s *routedOutlookState) { s.missingDetail = false })
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	_ = f.messageID(t, "alice", "m1")
}

func TestUserOutlookRejectsIncompleteAndForeignGraphLinks(t *testing.T) {
	for _, kind := range []string{"incomplete", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			f := newUserOutlookFixture(t)
			f.api.change("alice", func(s *routedOutlookState) { s.incomplete = kind == "incomplete"; s.wrongLink = kind == "foreign" })
			if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err == nil {
				t.Fatal("invalid delta accepted")
			}
			f.local(t, "alice", func(db *storage.DB) error {
				var n int
				err := db.Read().QueryRow(`SELECT COUNT(*) FROM folders WHERE last_full_sync_at IS NOT NULL`).Scan(&n)
				if err == nil && n != 0 {
					return errors.New("invalid baseline finalized")
				}
				return err
			})
		})
	}
}
func TestUserOutlookRejectsStaleOrFailedBodyPublication(t *testing.T) {
	for _, change := range []string{"message", "subject", "deleted", "provider", "publication", "unavailable"} {
		t.Run(change, func(t *testing.T) {
			f := newUserOutlookFixture(t)
			if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
				t.Fatal(err)
			}
			id := f.messageID(t, "alice", "m1")
			if change == "unavailable" {
				f.api.change("alice", func(s *routedOutlookState) { s.mimeStatus = 503 })
			}
			block := f.api.stopAt("alice", "/me/messages/m1/$value")
			defer block.unblock()
			result := gmailAsync(func() error { return f.worker.EnsureBody(t.Context(), "alice", id) })
			gmailStarted(t, block)
			f.local(t, "alice", func(db *storage.DB) error {
				var err error
				switch change {
				case "message":
					_, err = db.Write().Exec(`UPDATE messages SET remote_message_id='different' WHERE id=?`, id)
				case "subject":
					_, err = db.Write().Exec(`UPDATE accounts SET provider_account_id='different' WHERE id=?`, f.ids["alice"])
				case "provider":
					_, err = db.Write().Exec(`UPDATE accounts SET provider='gmail' WHERE id=?`, f.ids["alice"])
				case "deleted":
					_, err = db.Write().Exec(`UPDATE message_folder_state SET is_deleted=1 WHERE message_id=?`, id)
				case "publication":
					_, err = db.Write().Exec(`CREATE TRIGGER reject_outlook_body AFTER UPDATE OF body_text_path ON messages BEGIN SELECT RAISE(ABORT,'injected body failure'); END`)
				}
				return err
			})
			block.unblock()
			if err := gmailAwait(t, result); err == nil {
				t.Fatal("invalid body publication succeeded")
			}
			f.local(t, "alice", func(db *storage.DB) error {
				if db.IsBodyFetchedInternal(t.Context(), id) {
					return errors.New("failed body marked cached")
				}
				var n int
				err := db.Read().QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id=?`, id).Scan(&n)
				if err == nil && n != 0 {
					return errors.New("partial attachment publication")
				}
				return err
			})
			files := 0
			err := filepath.WalkDir(filepath.Join(f.blobs, f.ids["alice"]), func(_ string, entry os.DirEntry, err error) error {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
				if !entry.IsDir() {
					files++
				}
				return nil
			})
			if err != nil || files != 0 {
				t.Fatalf("failed candidate remains: %d %v", files, err)
			}
			if change == "publication" || change == "unavailable" {
				if change == "publication" {
					f.local(t, "alice", func(db *storage.DB) error { _, err := db.Write().Exec(`DROP TRIGGER reject_outlook_body`); return err })
				} else {
					f.api.change("alice", func(s *routedOutlookState) { s.mimeStatus = 0 })
				}
				if err := f.worker.EnsureBody(t.Context(), "alice", id); err != nil {
					t.Fatalf("valid retry failed: %v", err)
				}
			}
		})
	}
}

func TestUserOutlookMetadataRejectsChangedMailboxIdentity(t *testing.T) {
	f := newUserOutlookFixture(t)
	block := f.api.stopAt("alice", "/me/mailFolders/inbox-id/messages/delta")
	defer block.unblock()
	result := gmailAsync(func() error { return f.worker.Sync(t.Context(), "alice", f.ids["alice"]) })
	gmailStarted(t, block)
	f.local(t, "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET provider_account_id='new-subject' WHERE id=?`, f.ids["alice"])
		return err
	})
	block.unblock()
	if err := gmailAwait(t, result); err == nil {
		t.Fatal("changed mailbox accepted stale metadata")
	}
	f.local(t, "alice", func(db *storage.DB) error {
		var n int
		err := db.Read().QueryRow(`SELECT (SELECT COUNT(*) FROM messages)+(SELECT COUNT(*) FROM folders WHERE sync_cursor<>'')`).Scan(&n)
		if err == nil && n != 0 {
			return errors.New("stale metadata/cursor published")
		}
		return err
	})
}

func TestUserOutlookDeletionCancelsBodyAndCleansCredentials(t *testing.T) {
	f := newUserOutlookFixture(t)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.worker.Sync(t.Context(), owner, f.ids[owner]); err != nil {
			t.Fatal(err)
		}
	}
	id := f.messageID(t, "alice", "m1")
	block := f.api.stopAt("alice", "/me/messages/m1/$value")
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

func TestUserOutlookManualAndPeriodicReceiveAvoidIMAPIdle(t *testing.T) {
	f := newUserOutlookFixture(t)
	if _, _, err := f.worker.StartManualSync(t.Context(), "bob", []string{f.ids["alice"]}); !errors.Is(err, storage.ErrAccountRoute) {
		t.Fatalf("foreign manual run: %v", err)
	}
	events := f.worker.Events().Subscribe()
	defer f.worker.Events().Unsubscribe(events)
	if _, started, err := f.worker.StartManualSync(t.Context(), "alice", []string{f.ids["alice"]}); err != nil || !started {
		t.Fatalf("Outlook manual run: %t %v", started, err)
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
			t.Fatal("manual Outlook receive did not complete")
		}
	}
	if err := f.worker.Start(UserIMAPBackgroundOptions{PollInterval: time.Hour, ScanInterval: 25 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for f.api.count("bob", "/me/mailFolders/inbox-id/messages/delta") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.api.count("bob", "/me/mailFolders/inbox-id/messages/delta") == 0 {
		t.Fatal("directory discovery skipped Outlook")
	}
	f.worker.mu.Lock()
	watchers := f.worker.idleCount
	f.worker.mu.Unlock()
	if watchers != 0 {
		t.Fatal("Outlook receive started IMAP IDLE watchers")
	}
}
func TestUserOutlookThrottlingDefersAccountAndStopsFurtherRequests(t *testing.T) {
	f := newUserOutlookFixture(t)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	childBefore := f.api.count("alice", "/me/mailFolders/child-id/messages/delta")
	trashBefore := f.api.count("alice", "/me/mailFolders/trash-id/messages/delta")
	f.api.change("alice", func(s *routedOutlookState) { s.throttled = true })
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err == nil {
		t.Fatal("throttle accepted")
	}
	if f.api.count("alice", "/me/mailFolders/child-id/messages/delta") != childBefore || f.api.count("alice", "/me/mailFolders/trash-id/messages/delta") != trashBefore {
		t.Fatal("Graph requests continued after Retry-After")
	}
	var due int64
	if err := f.system.Read().QueryRow(`SELECT next_due_ms FROM gofer_account_poll_schedule WHERE account_id=?`, f.ids["alice"]).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if time.UnixMilli(due).Before(time.Now().Add(4 * time.Minute)) {
		t.Fatal("polling ignored Retry-After")
	}
	// Another owner may continue immediately.
	if err := f.worker.Sync(t.Context(), "bob", f.ids["bob"]); err != nil {
		t.Fatal(err)
	}
	f.api.change("alice", func(s *routedOutlookState) { s.throttled = false })
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
}

func TestUserOutlookFailedCheckpointReplaysWithoutDuplicateMessages(t *testing.T) {
	f := newUserOutlookFixture(t)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	f.api.change("alice", func(s *routedOutlookState) { s.phase = 1 })
	f.local(t, "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`CREATE TRIGGER reject_graph_cursor BEFORE UPDATE OF sync_cursor ON folders WHEN OLD.provider_remote_id='inbox-id' AND NEW.sync_cursor LIKE '%cursor1%' BEGIN SELECT RAISE(ABORT,'injected cursor failure'); END`)
		return err
	})
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err == nil || !strings.Contains(err.Error(), "injected cursor failure") {
		t.Fatalf("checkpoint failure not reached: %v", err)
	}
	id := f.messageID(t, "alice", "m2")
	f.local(t, "alice", func(db *storage.DB) error {
		var cursor string
		if err := db.Read().QueryRow(`SELECT sync_cursor FROM folders WHERE provider_remote_id='inbox-id'`).Scan(&cursor); err != nil {
			return err
		}
		if !strings.Contains(cursor, "cursor0") {
			return errors.New("failed cursor advanced")
		}
		_, err := db.Write().Exec(`DROP TRIGGER reject_graph_cursor`)
		return err
	})
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	if f.messageID(t, "alice", "m2") != id {
		t.Fatal("replay created another message")
	}
	f.local(t, "alice", func(db *storage.DB) error {
		var n int
		err := db.Read().QueryRow(`SELECT COUNT(*) FROM messages WHERE remote_message_id='m2'`).Scan(&n)
		if err == nil && n != 1 {
			return errors.New("duplicate messages after replay")
		}
		return err
	})
}
func TestUserOutlookTokenThrottlingDefersAccountWithoutGraphRequests(t *testing.T) {
	f := newUserOutlookFixture(t)
	f.api.change("alice", func(s *routedOutlookState) { s.tokenThrottled = true })
	expired := time.Now().Add(-time.Hour)
	if err := f.credentials.UpsertForUser(t.Context(), "alice", f.ids["alice"], "microsoft", "same-subject", "alice-access", "alice-refresh", "Bearer", &expired, userOutlookScopes); err != nil {
		t.Fatal(err)
	}
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err == nil {
		t.Fatal("token throttle accepted")
	}
	if f.api.count("alice", "/me/mailFolders") != 0 {
		t.Fatal("Graph called without a token")
	}
	var due int64
	if err := f.system.Read().QueryRow(`SELECT next_due_ms FROM gofer_account_poll_schedule WHERE account_id=?`, f.ids["alice"]).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if time.UnixMilli(due).Before(time.Now().Add(4 * time.Minute)) {
		t.Fatal("token Retry-After ignored")
	}
	if err := f.worker.Sync(t.Context(), "bob", f.ids["bob"]); err != nil {
		t.Fatal(err)
	}
}
