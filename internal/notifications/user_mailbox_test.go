package notifications

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/handler"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func userAccountForm(email, name, password string) string {
	return url.Values{"provider": {"imap"}, "auth_method": {"plain"}, "email_address": {email}, "display_name": {name},
		"imap_host": {"imap.example.com"}, "imap_port": {"993"}, "smtp_host": {"smtp.example.com"}, "smtp_port": {"465"},
		"username": {email}, "password": {password}}.Encode()
}

func TestUserMailboxAccountHTTPLifecycleAndIsolation(t *testing.T) {
	var f *userStorageFixture
	created, updated, cleaned := make(chan string, 4), make(chan string, 4), make(chan string, 4)
	releaseCleanup := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseCleanup) })
	leaseFree := func(ctx context.Context) error {
		work, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return f.routing.WithUser(work, "bob", func(db *storage.DB) error { return db.SetSetting(work, "bob", "other_account_lifecycle", "ok") })
	}
	f = newUserStorageFixture(t, handler.UserAccountHooks{
		Created: func(ctx context.Context, id string) error {
			if err := leaseFree(ctx); err != nil {
				return err
			}
			created <- id
			return nil
		},
		Updated: func(ctx context.Context, id string) error {
			if err := leaseFree(ctx); err != nil {
				return err
			}
			updated <- id
			return nil
		},
		Cleanup: func(ctx context.Context, id string) error {
			if err := leaseFree(ctx); err != nil {
				return err
			}
			cleaned <- id
			select {
			case <-releaseCleanup:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	for _, owner := range []string{"alice", "bob"} {
		for _, path := range []string{"/api/accounts", "/settings/accounts", "/api/accounts/" + f.accounts[owner].ID + "/edit"} {
			rec := f.request(owner, http.MethodGet, path, "")
			other := "bob"
			if owner == "bob" {
				other = "alice"
			}
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), owner+"@example.com") || strings.Contains(rec.Body.String(), other+"@example.com") {
				t.Fatalf("account read %s %s: %d %s", owner, path, rec.Code, rec.Body.String())
			}
		}
	}
	rec := f.request("alice", http.MethodPost, "/api/accounts", userAccountForm("shared@example.com", "Alice extra", "alice-extra-password"))
	id := rec.Header().Get("X-Gofer-Account-ID")
	if rec.Code != http.StatusOK || id == "" || !strings.Contains(rec.Body.String(), "shared@example.com") {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case got := <-created:
		if got != id {
			t.Fatal(got)
		}
	default:
		t.Fatal("create hook not called")
	}
	rec = f.request("bob", http.MethodPost, "/api/accounts", userAccountForm("shared@example.com", "Bob extra", "bob-extra-password"))
	bobID := rec.Header().Get("X-Gofer-Account-ID")
	if rec.Code != http.StatusOK || bobID == "" || bobID == id {
		t.Fatalf("shared mailbox create: %d %s", rec.Code, rec.Body.String())
	}
	<-created
	for _, attempt := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/accounts/" + id + "/edit", ""},
		{http.MethodPost, "/api/accounts/" + id + "/edit", userAccountForm("hijacked@example.com", "Hijacked", "stolen")},
		{http.MethodPost, "/api/accounts/" + id + "/color", "color=%23ff0000"},
		{http.MethodDelete, "/api/accounts/" + id, ""},
		{http.MethodGet, "/api/accounts/" + id + "/deletion-status", ""},
	} {
		if rec := f.request("bob", attempt.method, attempt.path, attempt.body); rec.Code != http.StatusNotFound {
			t.Fatalf("foreign %s %s: %d", attempt.method, attempt.path, rec.Code)
		}
	}
	if rec := f.request("alice", http.MethodPost, "/api/accounts/"+id+"/edit", userAccountForm("ALICE@example.com", "Duplicate", "changed")); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate update: %d %s", rec.Code, rec.Body.String())
	}
	rec = f.request("alice", http.MethodPost, "/api/accounts/"+id+"/edit", userAccountForm("renamed@example.com", "Renamed Alice", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case got := <-updated:
		if got != id {
			t.Fatal(got)
		}
	default:
		t.Fatal("update hook not called")
	}
	if err := f.accountStore.WithAccountForUser(t.Context(), "alice", id, func(local *config.AccountStore, _ *storage.DB) error {
		data, err := local.GetEditData(t.Context(), id)
		if err != nil {
			return err
		}
		password, err := local.DecryptPassword(t.Context(), id)
		if err != nil {
			return err
		}
		if data.DisplayName != "Renamed Alice" || data.EmailAddress != "renamed@example.com" || password != "alice-extra-password" {
			t.Fatal("update lost configuration or retained password")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rec := f.request("alice", http.MethodPost, "/api/accounts/"+id+"/color", "color=%23abcdef"); rec.Code != http.StatusOK {
		t.Fatalf("color: %d", rec.Code)
	}
	// Canceling the HTTP request after acceptance must not cancel external cleanup.
	requestCtx, cancelRequest := context.WithCancel(t.Context())
	req := httptest.NewRequest(http.MethodDelete, "/api/accounts/"+id, nil).WithContext(requestCtx)
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	rec = httptest.NewRecorder()
	f.http.ServeHTTP(rec, req)
	cancelRequest()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case got := <-cleaned:
		if got != id {
			t.Fatal(got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not start outside lease")
	}
	if rec := f.request("alice", http.MethodGet, "/api/accounts/"+id+"/deletion-status", ""); !strings.Contains(rec.Body.String(), `"deleting"`) {
		t.Fatal(rec.Body.String())
	}
	if rec := f.request("alice", http.MethodGet, "/api/accounts/"+id+"/edit", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("deleting account still routed: %d", rec.Code)
	}
	if rec := f.request("alice", http.MethodDelete, "/api/accounts/"+id, ""); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Code)
	}
	select {
	case <-cleaned:
		t.Fatal("duplicate delete dispatched another cleanup")
	default:
	}
	releaseOnce.Do(func() { close(releaseCleanup) })
	waitUserAccountState(t, f, "alice", id, storage.AccountDeleted)
	if rec := f.request("alice", http.MethodDelete, "/api/accounts/"+id, ""); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Code)
	}
	select {
	case <-cleaned:
		t.Fatal("tombstone deletion repeated external cleanup")
	default:
	}
	if err := f.accountStore.WithAccountForUser(t.Context(), "bob", bobID, func(local *config.AccountStore, _ *storage.DB) error {
		password, err := local.DecryptPassword(t.Context(), bobID)
		if err == nil && password != "bob-extra-password" {
			t.Fatal("Alice deletion affected Bob's mailbox")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var centralAccounts int
	if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&centralAccounts); err != nil || centralAccounts != 0 {
		t.Fatalf("local lifecycle touched central accounts: %d %v", centralAccounts, err)
	}
}

func waitUserAccountState(t *testing.T, f *userStorageFixture, owner, id string, desired storage.AccountRouteState) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		state, err := f.routing.AccountStateForUser(t.Context(), owner, id)
		if err != nil {
			t.Fatal(err)
		}
		if state == desired {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("account %s did not reach %s; state=%s", id, desired, state)
		case <-tick.C:
		}
	}
}

func TestUserMailboxDeletionFailureCanBeRetriedThroughHTTP(t *testing.T) {
	failure := errors.New("external cleanup failure")
	var calls atomic.Int32
	failed := make(chan struct{})
	f := newUserStorageFixture(t, handler.UserAccountHooks{
		Created: func(context.Context, string) error { return nil }, Updated: func(context.Context, string) error { return nil },
		Cleanup: func(context.Context, string) error {
			if calls.Add(1) == 1 {
				close(failed)
				return failure
			}
			return nil
		},
	})
	id := f.accounts["alice"].ID
	if rec := f.request("alice", http.MethodDelete, "/api/accounts/"+id, ""); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Code)
	}
	select {
	case <-failed:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup never attempted")
	}
	state, err := f.routing.AccountStateForUser(t.Context(), "alice", id)
	if err != nil || state != storage.AccountDeleting {
		t.Fatalf("lost retry intent: %s %v", state, err)
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for calls.Load() < 2 {
		rec := f.request("alice", http.MethodDelete, "/api/accounts/"+id, "")
		if rec.Code != http.StatusAccepted {
			t.Fatal(rec.Code)
		}
		select {
		case <-deadline.C:
			t.Fatal("HTTP deletion retry never resumed")
		case <-tick.C:
		}
	}
	waitUserAccountState(t, f, "alice", id, storage.AccountDeleted)
}

func seedUserMailbox(t *testing.T, f *userStorageFixture) {
	t.Helper()
	for _, owner := range []string{"alice", "bob"} {
		err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			accountID := f.accounts[owner].ID
			if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{ID: "same-inbox", AccountID: accountID, Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
				return err
			}
			msgs := []storage.SyncMessage{}
			for i := 1; i <= 3; i++ {
				msgs = append(msgs, storage.SyncMessage{AccountID: accountID, FolderID: "same-inbox", RemoteUID: uint32(i), MessageID: fmt.Sprintf("<%s-%d@example.com>", owner, i), Subject: fmt.Sprintf("%s private project %d", owner, i), FromName: owner + " sender", FromEmail: owner + "-sender@example.com", DateSent: time.Date(2026, 10, i, 12, 0, 0, 0, time.UTC)})
			}
			if err := db.UpsertSyncMessages(t.Context(), msgs); err != nil {
				return err
			}
			if _, err := db.Write().Exec(`UPDATE messages SET thread_id = CASE WHEN id = 3 THEN 'separate-thread' ELSE 'same-thread' END`); err != nil {
				return err
			}
			path := filepath.Join(t.TempDir(), owner+"-body.html")
			if err := os.WriteFile(path, []byte("<p>"+owner+" private body</p>"), 0600); err != nil {
				return err
			}
			if err := db.UpdateMessageBodyInternal(t.Context(), 1, "", path, "", owner+" private preview"); err != nil {
				return err
			}
			return db.UpdateMessageOriginalHTMLPathInternal(t.Context(), 1, path)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// A same-owner/same-ID shared row must never become a fallback source.
	if _, err := f.system.Write().Exec(`INSERT INTO accounts(id,user_id,email_address) VALUES('central-mail','alice','central-sentinel@example.com'); INSERT INTO messages(id,account_id,internet_message_id,subject) VALUES(1,'central-mail','<central@example.com>','central-sentinel')`); err != nil {
		t.Fatal(err)
	}
}

func TestUserMailboxHTTPReadsIsolateOverlappingMessageFolderAndThreadIDs(t *testing.T) {
	f := newUserStorageFixture(t)
	seedUserMailbox(t, f)
	for _, owner := range []string{"alice", "bob"} {
		other := "bob"
		if owner == "bob" {
			other = "alice"
		}
		for _, path := range []string{"/folder/same-inbox/full", "/mail/folder/same-inbox/items", "/mail/folder/same-inbox/items?limit=1&start=1", "/mail/folder/same-inbox/items?around=1", "/folder/same-inbox/full?selected=1", "/search?q=project", "/email/1", "/email/1?single=1&folder_id=same-inbox", "/mail/thread/same-thread/subitems"} {
			rec := f.request(owner, http.MethodGet, path, "")
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), owner+" private") || strings.Contains(rec.Body.String(), other+" private") || strings.Contains(rec.Body.String(), "central-sentinel") {
				t.Fatalf("mail read %s %s: %d %s", owner, path, rec.Code, rec.Body.String())
			}
		}
		for _, path := range []string{"/", "/folder/same-inbox", "/folder/same-inbox/1", "/api/sidebar/mail", "/api/sidebar/accounts/" + f.accounts[owner].ID} {
			rec := f.request(owner, http.MethodGet, path, "")
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), f.accounts[owner].ID) || strings.Contains(rec.Body.String(), f.accounts[other].ID) || strings.Contains(rec.Body.String(), "central-sentinel") {
				t.Fatalf("shell %s %s: %d %s", owner, path, rec.Code, rec.Body.String())
			}
		}
		for _, request := range []struct{ path, target string }{
			{"/folder/same-inbox", "main-content"},
			{"/?folder=same-inbox&email=1", "mail-list"},
			{"/?folder=same-inbox&email=1", "app-shell"},
		} {
			req := httptest.NewRequest(http.MethodGet, request.path, nil)
			req.Header.Set("HX-Request", "true")
			req.Header.Set("HX-Target", request.target)
			req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions[owner].Token})
			rec := httptest.NewRecorder()
			f.http.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), f.accounts[owner].ID) || strings.Contains(rec.Body.String(), f.accounts[other].ID) {
				t.Fatalf("HTMX %s %s: %d", owner, request.target, rec.Code)
			}
			if request.target != "app-shell" && !strings.Contains(rec.Body.String(), owner+" private") {
				t.Fatal("HTMX list did not load local mail")
			}
		}
		first := f.request(owner, http.MethodGet, "/mail/folder/same-inbox/items?limit=1", "")
		cursor := regexp.MustCompile(`data-next-cursor="([^"]+)"`).FindStringSubmatch(first.Body.String())
		if first.Code != http.StatusOK || len(cursor) != 2 {
			t.Fatal("cursor pagination was not produced")
		}
		next := f.request(owner, http.MethodGet, "/mail/folder/same-inbox/items?limit=1&after="+url.QueryEscape(html.UnescapeString(cursor[1])), "")
		if next.Code != http.StatusOK || !strings.Contains(next.Body.String(), owner+" private") || strings.Contains(next.Body.String(), other+" private") {
			t.Fatalf("cursor page: %d", next.Code)
		}
		foreignFilter := f.request(owner, http.MethodGet, "/mail/folder/same-inbox/items?account_id="+url.QueryEscape(f.accounts[other].ID), "")
		if foreignFilter.Code != http.StatusOK || strings.Contains(foreignFilter.Body.String(), " private") {
			t.Fatalf("foreign account filter escaped owner: %d", foreignFilter.Code)
		}

		for _, path := range []string{"/email/1/body", "/email/1/body?mode=original"} {
			rec := f.request(owner, http.MethodGet, path, "")
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), owner+" private body") || strings.Contains(rec.Body.String(), other+" private body") || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("body %s %s: %d %s", owner, path, rec.Code, rec.Body.String())
			}
		}
		for _, path := range []string{"/email/999", "/email/999/body", "/folder/" + other + "-inbox/full", "/api/sidebar/accounts/" + f.accounts[other].ID} {
			if rec := f.request(owner, http.MethodGet, path, ""); rec.Code != http.StatusNotFound {
				t.Fatalf("foreign/missing %s %s: %d", owner, path, rec.Code)
			}
		}
		if rec := f.request(owner, http.MethodGet, "/email/2/body", ""); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("uncached body tried unconverted fetch: %d", rec.Code)
		}
		rec := f.request(owner, http.MethodGet, "/api/folders/unread", "")
		var counts map[string]int
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &counts) != nil || counts["inbox"] != 3 || counts[other+"-inbox"] != 0 {
			t.Fatalf("unread %s: %d %s", owner, rec.Code, rec.Body.String())
		}
	}
	if rec := f.request("alice", http.MethodGet, "/search?q=project&user_id=bob", ""); strings.Contains(rec.Body.String(), "bob private project") {
		t.Fatal("query selected a foreign owner")
	}
}

type mailboxWriteProbe struct {
	*httptest.ResponseRecorder
	once  sync.Once
	probe func()
}

func (w *mailboxWriteProbe) Write(data []byte) (int, error) {
	w.once.Do(w.probe)
	return w.ResponseRecorder.Write(data)
}

func TestUserMailboxRenderingReleasesDatabaseBeforeClientWrite(t *testing.T) {
	f := newUserStorageFixture(t)
	seedUserMailbox(t, f)
	for _, path := range []string{"/folder/same-inbox/full", "/email/1/body"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
		w := &mailboxWriteProbe{ResponseRecorder: httptest.NewRecorder(), probe: func() {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if err := f.routing.WithUser(ctx, "bob", func(db *storage.DB) error { return db.SetSetting(ctx, "bob", "during_response", "ok") }); err != nil {
				t.Errorf("response held sole cache slot: %v", err)
			}
		}}
		f.http.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("response %s: %d %s", path, w.Code, w.Body.String())
		}
	}
}

func TestUserMailboxWorkerStartFailureReportsSavedAccount(t *testing.T) {
	f := newUserStorageFixture(t, handler.UserAccountHooks{Created: func(context.Context, string) error { return errors.New("start failed") }, Updated: func(context.Context, string) error { return nil }, Cleanup: func(context.Context, string) error { return nil }})
	rec := f.request("alice", http.MethodPost, "/api/accounts", userAccountForm("persisted@example.com", "Persisted", "password"))
	id := rec.Header().Get("X-Gofer-Account-ID")
	if rec.Code != http.StatusServiceUnavailable || id == "" {
		t.Fatalf("start failure: %d %s", rec.Code, rec.Body.String())
	}
	state, err := f.routing.AccountStateForUser(t.Context(), "alice", id)
	if err != nil || state != storage.AccountActive {
		t.Fatalf("committed account lost: %s %v", state, err)
	}
	if rec := f.request("alice", http.MethodGet, "/api/accounts/"+id+"/edit", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "persisted@example.com") {
		t.Fatalf("saved account inaccessible: %d", rec.Code)
	}
}

func TestUserMailboxDeleteAcknowledgesIntentBeforeDrainingCallbacks(t *testing.T) {
	cleanup := make(chan struct{}, 1)
	f := newUserStorageFixture(t, handler.UserAccountHooks{Created: func(context.Context, string) error { return nil }, Updated: func(context.Context, string) error { return nil }, Cleanup: func(context.Context, string) error { cleanup <- struct{}{}; return nil }})
	id := f.accounts["alice"].ID
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	defer once.Do(func() { close(release) })
	go func() {
		done <- f.routing.WithAccountForUser(t.Context(), "alice", id, func(*storage.DB) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-t.Context().Done():
				return t.Context().Err()
			}
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("callback never started")
	}
	if rec := f.request("alice", http.MethodDelete, "/api/accounts/"+id, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("intent: %d %s", rec.Code, rec.Body.String())
	}
	select {
	case <-cleanup:
		t.Fatal("cleanup ran before account callback drained")
	default:
	}
	if rec := f.request("alice", http.MethodGet, "/api/accounts/"+id+"/edit", ""); rec.Code != http.StatusNotFound {
		t.Fatal("new account callback admitted after deletion intent")
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-cleanup:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup never resumed after drain")
	}
	waitUserAccountState(t, f, "alice", id, storage.AccountDeleted)
}

func TestUserMailboxRegistrationRejectsMissingLifecycleHooksBeforeMounting(t *testing.T) {
	f := newUserStorageFixture(t)
	mux := http.NewServeMux()
	if err := f.base.RegisterUserStorageRoutes(t.Context(), mux, f.routing, handler.UserStorageOptions{Accounts: f.accountStore}); err == nil {
		t.Fatal("account writes registered without converted lifecycle hooks")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/settings/ui", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatal("invalid registration partially mounted routes")
	}
}

func TestUserMailboxOwnerWideReadsRefuseToRecreateMissingActiveStore(t *testing.T) {
	f := newUserStorageFixture(t)
	// Bob occupies the only slot, checkpointing/closing Alice before removal.
	if err := f.routing.WithUser(t.Context(), "bob", func(*storage.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("alice"))
	path := filepath.Join(f.system.Path()+".users", fmt.Sprintf("%x.db", hash))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"/api/accounts", "/api/settings/ui", "/folder/inbox/full"} {
		rec := f.request("alice", http.MethodGet, endpoint, "")
		if rec.Code == http.StatusOK {
			t.Fatalf("missing active store silently replaced for %s", endpoint)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lost store recreated: %v", err)
	}
}

func TestUserMailboxDeletionWaitsForExternalUpdateHookWithoutLeasingStore(t *testing.T) {
	updating, cleanup, release := make(chan struct{}), make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	f := newUserStorageFixture(t, handler.UserAccountHooks{
		Created: func(context.Context, string) error { return nil },
		Updated: func(ctx context.Context, _ string) error {
			close(updating)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		Cleanup: func(context.Context, string) error { cleanup <- struct{}{}; return nil },
	})
	id := f.accounts["alice"].ID
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- f.request("alice", http.MethodPost, "/api/accounts/"+id+"/edit", userAccountForm("alice@example.com", "Updated Alice", ""))
	}()
	select {
	case <-updating:
	case <-time.After(5 * time.Second):
		t.Fatal("update hook did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := f.routing.WithUser(ctx, "bob", func(db *storage.DB) error { return db.SetSetting(ctx, "bob", "while_update_runs", "ok") }); err != nil {
		t.Fatalf("external hook pinned database: %v", err)
	}
	if rec := f.request("alice", http.MethodDelete, "/api/accounts/"+id, ""); rec.Code != http.StatusAccepted {
		t.Fatalf("deletion intent: %d", rec.Code)
	}
	select {
	case <-cleanup:
		t.Fatal("cleanup ran before in-flight worker update ended")
	default:
	}
	once.Do(func() { close(release) })
	select {
	case rec := <-response:
		if rec.Code != http.StatusOK {
			t.Fatalf("completed update response: %d %s", rec.Code, rec.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("update did not complete")
	}
	select {
	case <-cleanup:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not resume")
	}
	waitUserAccountState(t, f, "alice", id, storage.AccountDeleted)
}
