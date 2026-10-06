package notifications

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func openUserEventStream(t *testing.T, f *userStorageFixture, owner string) <-chan map[string]any {
	t.Helper()
	server := httptest.NewServer(f.http)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions[owner].Token})
	response, err := server.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		response.Body.Close()
		cancel()
		t.Fatalf("event stream: %d", response.StatusCode)
	}
	t.Cleanup(func() { cancel(); response.Body.Close() })
	events := make(chan map[string]any, 64)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			if !strings.HasPrefix(scanner.Text(), "data: ") {
				continue
			}
			var event map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &event) != nil {
				continue
			}
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return events
}

func awaitUserEvent(t *testing.T, events <-chan map[string]any, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event, open := <-events:
			if !open {
				t.Fatal("event stream ended before expected event")
			}
			if match(event) {
				return event
			}
		case <-deadline.C:
			t.Fatal("expected event did not arrive")
		}
	}
}

func TestUserIMAPControlsManualHTTPDetachDeduplicateCancelAndSSEIsolation(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	f.useIMAPServer(t, server)
	blocked, release := server.setBlock("alice", "list")
	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequest(http.MethodPost, "/api/mail/sync/accounts/"+f.accounts["alice"].ID, nil).WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	recorder := httptest.NewRecorder()
	f.http.ServeHTTP(recorder, req)
	cancel()
	runID := recorder.Header().Get("X-Gofer-Mail-Sync-Run-ID")
	if recorder.Code != 200 || runID == "" {
		t.Fatalf("manual start: %d %s", recorder.Code, recorder.Body.String())
	}
	awaitIMAP(t, blocked)
	duplicate := f.request("alice", http.MethodPost, "/api/mail/sync", "")
	if duplicate.Header().Get("X-Gofer-Mail-Sync-Running") != "true" || duplicate.Header().Get("X-Gofer-Mail-Sync-Run-ID") != runID {
		t.Fatal("duplicate manual run was not coalesced")
	}
	if foreign := f.request("bob", http.MethodPost, "/api/mail/sync/accounts/"+f.accounts["alice"].ID, ""); foreign.Code != 404 {
		t.Fatalf("foreign sync: %d", foreign.Code)
	}
	events := openUserEventStream(t, f, "alice")
	awaitUserEvent(t, events, func(event map[string]any) bool {
		return event["type"] == string(mail.EventManualSyncStarted) && event["run_id"] == runID
	})
	if bob := f.request("bob", http.MethodPost, "/api/mail/sync", ""); bob.Code != 200 {
		t.Fatalf("Bob sync: %d", bob.Code)
	}
	waitUserIMAP(t, func() bool { return f.messageCount(t, "bob") == 2 })
	if snapshot, err := f.imap.ManualSyncSnapshot(t.Context(), "alice"); err != nil || len(snapshot) == 0 {
		t.Fatal("request cancellation stopped detached run")
	}
	f.events.Publish(mail.Event{Type: mail.EventManualSyncProgress, UserID: "bob", AccountID: f.accounts["bob"].ID, Payload: map[string]any{"run_id": "bob-secret"}})
	f.events.Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: "alice", Payload: map[string]any{"barrier": true}})
	awaitUserEvent(t, events, func(event map[string]any) bool {
		if event["run_id"] == "bob-secret" || event["account_id"] == f.accounts["bob"].ID {
			t.Fatal("foreign event reached Alice stream")
		}
		return event["barrier"] == true
	})
	if rec := f.request("alice", http.MethodPost, "/api/mail/sync/cancel", ""); rec.Code != 200 {
		t.Fatalf("cancel: %d", rec.Code)
	}
	complete := awaitUserEvent(t, events, func(event map[string]any) bool {
		return event["type"] == string(mail.EventManualSyncComplete) && event["run_id"] == runID
	})
	if complete["status"] != "cancelled" {
		t.Fatalf("completion: %#v", complete)
	}
	waitUserIMAP(t, func() bool {
		snapshot, err := f.imap.ManualSyncSnapshot(t.Context(), "alice")
		return err == nil && len(snapshot) == 0
	})
	close(release)
	if _, err := f.system.Write().Exec("UPDATE users SET status='disabled' WHERE id='alice'"); err != nil {
		t.Fatal(err)
	}
	awaitClosed := time.NewTimer(8 * time.Second)
	defer awaitClosed.Stop()
	for {
		select {
		case _, open := <-events:
			if !open {
				return
			}
		case <-awaitClosed.C:
			t.Fatal("disabled owner's event stream remained open")
		}
	}
}

func (f *userStorageFixture) inboxID(t *testing.T, owner string) string {
	t.Helper()
	var id string
	if err := f.routing.WithAccountForUser(t.Context(), owner, f.accounts[owner].ID, func(db *storage.DB) error {
		return db.Read().QueryRow("SELECT id FROM folders WHERE account_id=? AND remote_id='INBOX'", f.accounts[owner].ID).Scan(&id)
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUserIMAPControlsSettingsAtomicOwnedModesAndEmailToggle(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	server.idleSupported = true
	startBackgroundIMAP(t, f, server, mail.UserIMAPBackgroundOptions{PollInterval: time.Hour, MaxIdleWatchers: 2})
	waitUserIMAP(t, func() bool { return len(server.idlePeers("alice")) == 1 && len(server.idlePeers("bob")) == 1 })
	alice, bob := f.accounts["alice"].ID, f.accounts["bob"].ID
	inbox, bobInbox := f.inboxID(t, "alice"), f.inboxID(t, "bob")
	form := url.Values{"sync_interval_minutes": {"1"}, "account_ids": {alice}}
	if rec := f.request("alice", http.MethodPost, "/api/settings/sync", form.Encode()); rec.Code != 200 {
		t.Fatalf("settings save: %d %s", rec.Code, rec.Body.String())
	}
	waitUserIMAP(t, func() bool { return len(server.idlePeers("alice")) == 0 })
	if len(server.idlePeers("bob")) != 1 {
		t.Fatal("Alice settings restarted Bob watcher")
	}
	for _, bad := range []url.Values{
		{"sync_interval_minutes": {"10"}, "account_ids": {bob}},
		{"sync_interval_minutes": {"10"}, "account_ids": {alice}, "idle_folders": {alice + ":" + bobInbox}},
	} {
		if rec := f.request("alice", http.MethodPost, "/api/settings/sync", bad.Encode()); rec.Code != 404 {
			t.Fatalf("foreign selection: %d %s", rec.Code, rec.Body.String())
		}
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		if db.GetSyncInterval(t.Context(), "alice") != 1 || len(db.GetIdleFolderIDsForAccount(t.Context(), "alice", alice)) != 0 {
			t.Fatal("rejected update changed saved settings")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	form.Set("sync_interval_minutes", "5")
	form.Set("idle_folders", alice+":"+inbox)
	if rec := f.request("alice", http.MethodPost, "/api/settings/sync", form.Encode()); rec.Code != 200 {
		t.Fatalf("IDLE save: %d %s", rec.Code, rec.Body.String())
	}
	waitUserIMAP(t, func() bool {
		states, err := f.imap.IdleStatusesForUser(t.Context(), "alice")
		return err == nil && len(states) == 1 && states[0].Healthy
	})
	view := f.request("alice", http.MethodGet, "/settings/sync", "")
	if view.Code != 200 || !strings.Contains(view.Body.String(), `data-effective-idle="true"`) || strings.Contains(view.Body.String(), bobInbox) {
		t.Fatalf("owned runtime settings view: %d", view.Code)
	}
	bobEvents := openUserEventStream(t, f, "bob")
	awaitUserEvent(t, bobEvents, func(event map[string]any) bool {
		return event["type"] == string(mail.EventIDLEFolderStatus) && event["account_id"] == bob && event["effective_mode"] == "idle"
	})
	if rec := f.request("alice", http.MethodPost, "/api/settings/sync", "sync_interval_minutes=15"); rec.Code != 200 {
		t.Fatalf("interval-only save: %d", rec.Code)
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		if !db.GetIdleFolderIDsForAccount(t.Context(), "alice", alice)[inbox] {
			t.Fatal("interval-only save erased folder modes")
		}
		_, err := db.Write().Exec(`CREATE TRIGGER fail_settings_interval BEFORE INSERT ON app_settings WHEN NEW.key='sync_interval_minutes' BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rec := f.request("alice", http.MethodPost, "/api/settings/sync", url.Values{"sync_interval_minutes": {"30"}, "account_ids": {alice}}.Encode()); rec.Code != 503 {
		t.Fatalf("transaction failure: %d", rec.Code)
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		if db.GetSyncInterval(t.Context(), "alice") != 15 || !db.GetIdleFolderIDsForAccount(t.Context(), "alice", alice)[inbox] {
			t.Fatal("settings escaped rollback")
		}
		_, err := db.Write().Exec("DROP TRIGGER fail_settings_interval")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []string{"false", "true"} {
		rec := f.request("alice", http.MethodPost, "/api/accounts/"+alice+"/service", "service=email&enabled="+enabled)
		if rec.Code != 200 {
			t.Fatalf("email toggle: %d %s", rec.Code, rec.Body.String())
		}
		want := 0
		if enabled == "true" {
			want = 1
		}
		waitUserIMAP(t, func() bool { return len(server.idlePeers("alice")) == want })
	}
	if rec := f.request("bob", http.MethodPost, "/api/accounts/"+alice+"/service", "service=email&enabled=false"); rec.Code != 404 {
		t.Fatal("foreign email toggle accepted")
	}
	if rec := f.request("alice", http.MethodPost, "/api/accounts/"+alice+"/service", "service=contacts&enabled=true"); rec.Code != 501 {
		t.Fatal("unconverted service was silently enabled")
	}
	if err := f.routing.WithUser(t.Context(), "bob", func(db *storage.DB) error {
		if db.GetSyncInterval(t.Context(), "bob") != 5 {
			t.Fatal("Alice interval changed Bob")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *userStorageFixture) nextPoll(t *testing.T, owner string) time.Time {
	t.Helper()
	var milliseconds int64
	if err := f.system.Read().QueryRow("SELECT next_due_ms FROM gofer_account_poll_schedule WHERE account_id=?", f.accounts[owner].ID).Scan(&milliseconds); err != nil {
		t.Fatal(err)
	}
	return time.UnixMilli(milliseconds)
}

func TestUserIMAPControlsPollingUsesIndependentUserIntervals(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	for owner, minutes := range map[string]string{"alice": "1", "bob": "10"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error { return db.SetSetting(t.Context(), owner, "sync_interval_minutes", minutes) }); err != nil {
			t.Fatal(err)
		}
	}
	startBackgroundIMAP(t, f, server, mail.UserIMAPBackgroundOptions{PollInterval: 50 * time.Millisecond, DisableIDLE: true})
	waitUserIMAP(t, func() bool { return f.messageCount(t, "alice") == 2 && f.messageCount(t, "bob") == 2 })
	waitUserIMAP(t, func() bool { return f.nextPoll(t, "alice").After(time.Now()) && f.nextPoll(t, "bob").After(time.Now()) })
	if gap := f.nextPoll(t, "bob").Sub(f.nextPoll(t, "alice")); gap < 8*time.Minute || gap > 10*time.Minute {
		t.Fatalf("user interval gap: %s", gap)
	}
	server.mu.Lock()
	server.ownerUIDs["alice"] = []uint32{2, 1000000, 1000001}
	server.ownerUIDs["bob"] = []uint32{2, 1000000, 1000001}
	server.mu.Unlock()
	revision, err := f.routing.PollRevision(t.Context(), "alice", f.accounts["alice"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated, err := f.routing.DeferAccountPoll(t.Context(), "alice", f.accounts["alice"].ID, revision, time.Now().Add(-time.Second)); err != nil || !updated {
		t.Fatalf("make Alice due: %v %v", updated, err)
	}
	waitUserIMAP(t, func() bool { return f.messageCount(t, "alice") == 3 })
	if f.messageCount(t, "bob") != 2 {
		t.Fatal("not-due owner was polled")
	}
}

func TestUserIMAPControlsSettingsDuringSyncCannotRestoreOldDeadline(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error { return db.SetSetting(t.Context(), owner, "sync_interval_minutes", "10") }); err != nil {
			t.Fatal(err)
		}
	}
	startBackgroundIMAP(t, f, server, mail.UserIMAPBackgroundOptions{PollInterval: time.Hour, ScanInterval: 50 * time.Millisecond, DisableIDLE: true})
	waitUserIMAP(t, func() bool { return f.messageCount(t, "alice") == 2 && f.messageCount(t, "bob") == 2 })
	waitUserIMAP(t, func() bool { return f.nextPoll(t, "alice").After(time.Now()) && f.nextPoll(t, "bob").After(time.Now()) })
	blocked, release := server.setBlock("alice", "headers")
	server.mu.Lock()
	server.ownerUIDs["alice"] = []uint32{2, 1000000, 1000001}
	server.mu.Unlock()
	revision, err := f.routing.PollRevision(t.Context(), "alice", f.accounts["alice"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.routing.DeferAccountPoll(t.Context(), "alice", f.accounts["alice"].ID, revision, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	awaitIMAP(t, blocked)
	if rec := f.request("alice", http.MethodPost, "/api/settings/sync", "sync_interval_minutes=1"); rec.Code != 200 {
		t.Fatalf("save during network wait: %d %s", rec.Code, rec.Body.String())
	}
	server.mu.Lock()
	server.ownerUIDs["alice"] = []uint32{2, 1000000, 1000001, 1000002}
	server.mu.Unlock()
	close(release)
	waitUserIMAP(t, func() bool { return f.messageCount(t, "alice") == 4 && f.nextPoll(t, "alice").After(time.Now()) })
	if due := f.nextPoll(t, "alice").Sub(time.Now()); due > 2*time.Minute {
		t.Fatalf("old deadline restored: %s", due)
	}
	if f.messageCount(t, "bob") != 2 {
		t.Fatal("settings refresh queued another owner")
	}
}

func TestUserIMAPControlsManualCapacityAndShutdown(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	f.useIMAPServer(t, server)
	host, rawPort, _ := net.SplitHostPort(server.listener.Addr().String())
	port, _ := strconv.Atoi(rawPort)
	accounts := map[string]string{"alice": f.accounts["alice"].ID, "bob": f.accounts["bob"].ID}
	request := func(owner string) *models.CreateAccountRequest {
		return &models.CreateAccountRequest{Provider: "imap", EmailAddress: owner + "@example.com", IMAPHost: host, IMAPPort: port, IMAPTLSMode: "plaintext", SMTPHost: "smtp.example.com", Username: "alice", Password: "synthetic-only", AuthMethod: "plain"}
	}
	if err := f.accountStore.UpdateAccount(t.Context(), "bob", accounts["bob"], request("bob")); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"third", "fourth", "fifth"} {
		if _, err := f.system.Write().Exec("INSERT INTO users(id,username,username_normalized) VALUES(?,?,?)", owner, owner, owner); err != nil {
			t.Fatal(err)
		}
		account, err := f.accountStore.CreateAccount(t.Context(), owner, request(owner))
		if err != nil {
			t.Fatal(err)
		}
		accounts[owner] = account.ID
	}
	blocked, _ := server.setBlock("alice", "list")
	for _, owner := range []string{"alice", "bob", "third", "fourth"} {
		if _, started, err := f.imap.StartManualSync(t.Context(), owner, []string{accounts[owner]}); err != nil || !started {
			t.Fatalf("admit %s: %v %v", owner, started, err)
		}
	}
	awaitIMAP(t, blocked)
	if _, _, err := f.imap.StartManualSync(t.Context(), "fifth", []string{accounts["fifth"]}); !errors.Is(err, mail.ErrUserIMAPManualCapacity) {
		t.Fatalf("capacity: %v", err)
	}
	if active, err := f.imap.CancelManualSync(t.Context(), "third"); err != nil || !active {
		t.Fatalf("cancel third: %v %v", active, err)
	}
	waitUserIMAP(t, func() bool {
		snapshot, err := f.imap.ManualSyncSnapshot(t.Context(), "third")
		return err == nil && len(snapshot) == 0
	})
	if _, started, err := f.imap.StartManualSync(t.Context(), "fifth", []string{accounts["fifth"]}); err != nil || !started {
		t.Fatalf("capacity not released: %v %v", started, err)
	}
	f.stopIMAP()
	done := make(chan struct{})
	go func() { f.imap.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("shutdown failed to join manual runs")
	}
}

func TestUserIMAPControlsManualRunSkipsAccountDisabledWhileQueued(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	f.useIMAPServer(t, server)
	host, rawPort, _ := net.SplitHostPort(server.listener.Addr().String())
	port, _ := strconv.Atoi(rawPort)
	second, err := f.accountStore.CreateAccount(t.Context(), "alice", &models.CreateAccountRequest{Provider: "imap", EmailAddress: "second@example.com", IMAPHost: host, IMAPPort: port, IMAPTLSMode: "plaintext", SMTPHost: "smtp.example.com", Username: "alice", Password: "synthetic-only", AuthMethod: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	blocked, release := server.setBlock("alice", "list")
	events := f.events.Subscribe()
	defer f.events.Unsubscribe(events)
	id, started, err := f.imap.StartManualSync(t.Context(), "alice", []string{f.accounts["alice"].ID, second.ID})
	if err != nil || !started {
		t.Fatalf("start: %v %v", started, err)
	}
	awaitIMAP(t, blocked)
	if rec := f.request("alice", http.MethodPost, "/api/accounts/"+second.ID+"/service", "service=email&enabled=false"); rec.Code != 200 {
		t.Fatalf("disable queued account: %d", rec.Code)
	}
	close(release)
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.Type == mail.EventManualSyncComplete && event.Payload["run_id"] == id {
				if event.Payload["skipped"] != 1 || event.Payload["accounts_done"] != 2 || event.Payload["status"] != "partial" {
					t.Fatalf("disabled mailbox reported synced: %#v", event.Payload)
				}
				return
			}
		case <-timer.C:
			t.Fatal("manual run did not complete")
		}
	}
}
