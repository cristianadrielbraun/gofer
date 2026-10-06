package notifications

import (
	"context"
	"fmt"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (s *routedIMAPServer) idlePeers(owner string) map[net.Conn]*routedIdleClient {
	s.mu.Lock()
	copy := make(map[net.Conn]*routedIdleClient)
	for conn, client := range s.idleClients {
		copy[conn] = client
	}
	s.mu.Unlock()
	peers := make(map[net.Conn]*routedIdleClient)
	for conn, client := range copy {
		client.mu.Lock()
		if client.owner == owner && client.idle {
			peers[conn] = client
		}
		client.mu.Unlock()
	}
	return peers
}
func (s *routedIMAPServer) notifyIdle(owner, line string) {
	for _, client := range s.idlePeers(owner) {
		client.mu.Lock()
		if client.idle {
			fmt.Fprint(client.writer, line+"\r\n")
			client.writer.Flush()
		}
		client.mu.Unlock()
	}
}
func waitUserIMAP(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("background IMAP condition did not become true")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func (f *userStorageFixture) messageCount(t *testing.T, owner string) int {
	t.Helper()
	count := 0
	if err := f.routing.WithAccountForUser(t.Context(), owner, f.accounts[owner].ID, func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT COUNT(*) FROM messages WHERE account_id=?`, f.accounts[owner].ID).Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	return count
}
func startBackgroundIMAP(t *testing.T, f *userStorageFixture, s *routedIMAPServer, options mail.UserIMAPBackgroundOptions) {
	t.Helper()
	f.useIMAPServer(t, s)
	if err := f.imap.Start(options); err != nil {
		t.Fatal(err)
	}
}
func TestUserIMAPBackgroundStartupAndPeriodicPolling(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	s := newRoutedIMAPServer(t)
	startBackgroundIMAP(t, f, s, mail.UserIMAPBackgroundOptions{PollInterval: 50 * time.Millisecond, DisableIDLE: true})
	waitUserIMAP(t, func() bool { return f.messageCount(t, "alice") == 2 && f.messageCount(t, "bob") == 2 })
	s.mu.Lock()
	s.ownerUIDs["alice"] = []uint32{2, 1000000, 1000001}
	s.mu.Unlock()
	waitUserIMAP(t, func() bool { return f.messageCount(t, "alice") == 3 })
	if f.messageCount(t, "bob") != 2 {
		t.Fatal("polling crossed owner boundaries")
	}
	if err := f.routing.WithAccountForUser(t.Context(), "alice", f.accounts["alice"].ID, func(db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET email_sync_enabled=0 WHERE id=?`, f.accounts["alice"].ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Configuration edits cancel sessions that copied the previous setting.
	if err := f.imap.RestartAccount(t.Context(), f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.ownerUIDs["alice"] = []uint32{2, 1000000, 1000001, 1000002}
	s.ownerUIDs["bob"] = []uint32{2, 1000000, 1000001}
	s.mu.Unlock()
	waitUserIMAP(t, func() bool { return f.messageCount(t, "bob") == 3 })
	if f.messageCount(t, "alice") != 3 {
		t.Fatal("disabled account continued polling")
	}
	if err := f.imap.Start(mail.UserIMAPBackgroundOptions{}); err == nil {
		t.Fatal("duplicate background start accepted")
	}
}

func TestUserIMAPBackgroundIDLENotificationsReleaseDatabaseAndReconcile(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	s := newRoutedIMAPServer(t)
	s.idleSupported = true
	startBackgroundIMAP(t, f, s, mail.UserIMAPBackgroundOptions{PollInterval: time.Hour, MaxIdleWatchers: 2})
	waitUserIMAP(t, func() bool { return len(s.idlePeers("alice")) == 1 && len(s.idlePeers("bob")) == 1 })
	// A watcher is still listening when another owner evicts its database.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.routing.WithUser(ctx, "bob", func(db *storage.DB) error { return db.SetSetting(ctx, "bob", "idle_eviction_probe", "ok") }); err != nil {
		t.Fatalf("IDLE retained Alice database lease: %v", err)
	}
	states, err := f.imap.IdleStatusesForUser(t.Context(), "bob")
	if err != nil || len(states) != 1 || states[0].AccountID != f.accounts["bob"].ID {
		t.Fatalf("owned IDLE status: %#v %v", states, err)
	}
	s.mu.Lock()
	s.ownerUIDs["alice"] = []uint32{2, 1000000, 1000001}
	s.mu.Unlock()
	for i := 0; i < 50; i++ {
		s.notifyIdle("alice", "* 3 EXISTS")
	}
	waitUserIMAP(t, func() bool { return f.messageCount(t, "alice") == 3 })
	if f.messageCount(t, "bob") != 2 {
		t.Fatal("Alice IDLE notification updated Bob")
	}
	s.mu.Lock()
	s.ownerUIDs["alice"] = []uint32{1000000, 1000001}
	s.seen = true
	s.mu.Unlock()
	s.notifyIdle("alice", "* 1 EXPUNGE")
	waitUserIMAP(t, func() bool { return f.messageCount(t, "alice") == 2 })
	s.notifyIdle("alice", "* 1 FETCH (UID 1000000 FLAGS (\\Seen))")
	waitUserIMAP(t, func() bool {
		read := 0
		if err := f.routing.WithAccountForUser(t.Context(), "alice", f.accounts["alice"].ID, func(db *storage.DB) error {
			return db.Read().QueryRow(`SELECT SUM(is_read) FROM message_folder_state`).Scan(&read)
		}); err != nil {
			t.Fatal(err)
		}
		return read == 1
	})
}

func TestUserIMAPBackgroundWatcherLimitAndPollingFallback(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	s := newRoutedIMAPServer(t)
	s.idleSupported = true
	startBackgroundIMAP(t, f, s, mail.UserIMAPBackgroundOptions{PollInterval: 50 * time.Millisecond, MaxIdleWatchers: 1})
	waitUserIMAP(t, func() bool {
		return len(s.idlePeers("alice"))+len(s.idlePeers("bob")) == 1 && f.messageCount(t, "alice") == 2 && f.messageCount(t, "bob") == 2
	})
	s.mu.Lock()
	s.uids = []uint32{2, 1000000, 1000001}
	s.mu.Unlock()
	waitUserIMAP(t, func() bool { return f.messageCount(t, "alice") == 3 && f.messageCount(t, "bob") == 3 })
	if count := len(s.idlePeers("alice")) + len(s.idlePeers("bob")); count > 1 {
		t.Fatalf("watcher bound exceeded: %d", count)
	}
}

func TestUserIMAPBackgroundReconnectCatchesMissedMail(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	s := newRoutedIMAPServer(t)
	s.idleSupported = true
	startBackgroundIMAP(t, f, s, mail.UserIMAPBackgroundOptions{PollInterval: time.Hour, MaxIdleWatchers: 2})
	waitUserIMAP(t, func() bool { return len(s.idlePeers("alice")) == 1 })
	old := s.idlePeers("alice")
	for conn := range old {
		conn.Close()
	}
	s.mu.Lock()
	s.ownerUIDs["alice"] = []uint32{2, 1000000, 1000001}
	s.mu.Unlock()
	waitUserIMAP(t, func() bool { return f.messageCount(t, "alice") == 3 })
	waitUserIMAP(t, func() bool {
		for conn := range s.idlePeers("alice") {
			if old[conn] == nil {
				return true
			}
		}
		return false
	})
}

func TestUserIMAPBackgroundDeletionDisablingAndShutdownDrainIDLE(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	s := newRoutedIMAPServer(t)
	s.idleSupported = true
	startBackgroundIMAP(t, f, s, mail.UserIMAPBackgroundOptions{PollInterval: time.Hour, MaxIdleWatchers: 2})
	waitUserIMAP(t, func() bool { return len(s.idlePeers("alice")) == 1 && len(s.idlePeers("bob")) == 1 })
	rec := f.request("alice", http.MethodDelete, "/api/accounts/"+f.accounts["alice"].ID, "")
	if rec.Code != 202 {
		t.Fatalf("delete with IDLE: %d", rec.Code)
	}
	waitUserIMAP(t, func() bool {
		state, err := f.routing.AccountStateForUser(t.Context(), "alice", f.accounts["alice"].ID)
		if err != nil {
			t.Fatal(err)
		}
		return state == storage.AccountDeleted
	})
	if len(s.idlePeers("alice")) != 0 {
		t.Fatal("deleted account watcher remained")
	}
	if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	waitUserIMAP(t, func() bool { return len(s.idlePeers("bob")) == 0 })
	f.stopIMAP()
	done := make(chan struct{})
	go func() { f.imap.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not join IDLE watchers")
	}
}

func TestUserIMAPBackgroundEditRestartsWatcher(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	s := newRoutedIMAPServer(t)
	s.idleSupported = true
	f.useIMAPServer(t, s)
	if _, err := f.system.Write().Exec("UPDATE users SET status='disabled' WHERE id='bob'"); err != nil {
		t.Fatal(err)
	}
	if err := f.imap.Start(mail.UserIMAPBackgroundOptions{PollInterval: time.Hour, MaxIdleWatchers: 1}); err != nil {
		t.Fatal(err)
	}
	waitUserIMAP(t, func() bool { return len(s.idlePeers("alice")) == 1 })
	old := s.idlePeers("alice")
	if err := f.imap.RestartAccount(t.Context(), f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	waitUserIMAP(t, func() bool {
		for conn := range s.idlePeers("alice") {
			if old[conn] == nil {
				return true
			}
		}
		return false
	})
	for conn := range s.idlePeers("alice") {
		if old[conn] != nil {
			t.Fatal("old watcher survived restart")
		}
	}
}

// Notifications after the sync snapshot must trigger a follow-up receive.
func TestUserIMAPBackgroundBurstDuringBlockedSyncPreservesFollowUp(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	s := newRoutedIMAPServer(t)
	s.idleSupported = true
	startBackgroundIMAP(t, f, s, mail.UserIMAPBackgroundOptions{PollInterval: time.Hour, MaxIdleWatchers: 2})
	waitUserIMAP(t, func() bool { return len(s.idlePeers("alice")) == 1 })
	s.mu.Lock()
	s.ownerUIDs["alice"] = []uint32{2, 1000000, 1000001}
	s.mu.Unlock()
	blocked, release := s.setBlock("alice", "headers")
	s.notifyIdle("alice", "* 3 EXISTS")
	awaitIMAP(t, blocked)
	s.mu.Lock()
	s.ownerUIDs["alice"] = []uint32{2, 1000000, 1000001, 1000002}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				s.notifyIdle("alice", "* 4 EXISTS")
			}
		}()
	}
	wg.Wait()
	close(release)
	waitUserIMAP(t, func() bool { return f.messageCount(t, "alice") == 4 })
	if f.messageCount(t, "bob") != 2 {
		t.Fatal("notification burst crossed ownership")
	}
}

// Full discovery backlog and notifications during blocked sessions must neither
// drop tail accounts nor deadlock workers enqueueing their follow-up.
func TestUserIMAPBackgroundFullQueueRetainsDiscoveryAndFollowUps(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	f.useIMAPServer(t, server)
	host, rawPort, err := net.SplitHostPort(server.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{f.accounts["alice"].ID}
	for i := 0; i < 36; i++ {
		account, err := f.accountStore.CreateAccount(t.Context(), "alice", &models.CreateAccountRequest{
			Provider: "imap", EmailAddress: fmt.Sprintf("alice-%d@example.com", i),
			DisplayName: "queue test", IMAPHost: host, IMAPPort: port,
			IMAPTLSMode: "plaintext", AuthMethod: "plain", SMTPHost: "smtp.example.com", Username: "alice", Password: "synthetic-only",
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, account.ID)
	}
	blocked, release := server.setBlock("alice", "list")
	if err := f.imap.Start(mail.UserIMAPBackgroundOptions{PollInterval: time.Hour, DisableIDLE: true}); err != nil {
		t.Fatal(err)
	}
	awaitIMAP(t, blocked)
	// Let all four worker sessions block and discovery fill its bounded queue.
	waitUserIMAP(t, func() bool {
		server.mu.Lock()
		defer server.mu.Unlock()
		count := 0
		for _, line := range server.commands {
			if strings.HasPrefix(line, "alice: ") && strings.Contains(line, " LIST ") {
				count++
			}
		}
		return count >= 4
	})
	// Saturation is observable through the public API. Existing running entries
	// record dirty follow-ups, while accounts not admitted yet report backpressure.
	full := false
	waitUserIMAP(t, func() bool {
		for _, id := range ids {
			err := f.imap.QueueAccount(t.Context(), id)
			if err != nil {
				if err.Error() != "IMAP sync queue is full" {
					t.Fatal(err)
				}
				full = true
			}
		}
		return full
	})
	close(release)
	waitUserIMAP(t, func() bool {
		count := 0
		if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
			return db.Read().QueryRow("SELECT COUNT(DISTINCT account_id) FROM messages").Scan(&count)
		}); err != nil {
			t.Fatal(err)
		}
		return count == len(ids) && f.messageCount(t, "bob") == 2
	})
	// Each blocked account was marked dirty. More than one LIST per account
	// proves the follow-ups progressed even while the queue started full.
	waitUserIMAP(t, func() bool {
		server.mu.Lock()
		defer server.mu.Unlock()
		count := 0
		for _, line := range server.commands {
			if strings.HasPrefix(line, "alice: ") && strings.Contains(line, " LIST ") {
				count++
			}
		}
		return count >= len(ids)+4
	})
}
