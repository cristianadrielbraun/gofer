package notifications

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type routedMutationMailbox struct {
	uids     []uint32
	validity uint32
	flags    map[uint32]map[string]bool
	origins  map[uint32]uint32
	raw      map[uint32]string
}

func (s *routedIMAPServer) mutationMailboxLocked(owner, folder string) *routedMutationMailbox {
	if s.mutationMailboxes == nil {
		s.mutationMailboxes = make(map[string]*routedMutationMailbox)
	}
	key := owner + ":" + folder
	box := s.mutationMailboxes[key]
	if box == nil {
		box = &routedMutationMailbox{validity: s.validity, flags: make(map[uint32]map[string]bool), origins: make(map[uint32]uint32)}
		if folder == "INBOX" {
			box.uids = append([]uint32(nil), s.uids...)
			if owned, ok := s.ownerUIDs[owner]; ok {
				box.uids = append([]uint32(nil), owned...)
			}
		}
		for _, uid := range box.uids {
			box.origins[uid] = uid
		}
		s.mutationMailboxes[key] = box
	}
	return box
}

func (s *routedIMAPServer) serveMutation(w *bufio.Writer, owner, folder, tag string, parts []string, upper string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	box := s.mutationMailboxLocked(owner, folder)
	if strings.Contains(upper, " UID STORE ") {
		if s.failStore {
			fmt.Fprintf(w, "%s NO temporary store failure\r\n", tag)
			return
		}
		for _, uid := range box.uids {
			if !testUIDSetContains(parts[3], uid) {
				continue
			}
			if box.flags[uid] == nil {
				box.flags[uid] = make(map[string]bool)
			}
			for _, flag := range []string{`\Seen`, `\Flagged`, `\Deleted`, `$Label1`, `$Label2`, `$Junk`, `$NotJunk`, `Projects`, `Later`} {
				if strings.Contains(strings.ToLower(upper), strings.ToLower(flag)) {
					box.flags[uid][flag] = strings.HasPrefix(parts[4], "+")
				}
			}
		}
		fmt.Fprintf(w, "%s OK stored\r\n", tag)
		return
	}
	if strings.Contains(upper, " UID MOVE ") {
		destination := s.mutationMailboxLocked(owner, strings.Trim(parts[4], `"`))
		for index, uid := range box.uids {
			if !testUIDSetContains(parts[3], uid) {
				continue
			}
			destinationUID := uint32(10)
			for _, n := range destination.uids {
				destinationUID = max(destinationUID, n+1)
			}
			destination.uids = append(destination.uids, destinationUID)
			destination.origins[destinationUID] = box.origins[uid]
			destination.flags[destinationUID] = make(map[string]bool)
			for flag, value := range box.flags[uid] {
				destination.flags[destinationUID][flag] = value
			}
			box.uids = slices.Delete(box.uids, index, index+1)
			fmt.Fprintf(w, "* %d EXPUNGE\r\n%s OK [COPYUID %d %d %d] moved\r\n", index+1, tag, destination.validity, uid, destinationUID)
			return
		}
		fmt.Fprintf(w, "%s OK nothing to move\r\n", tag)
		return
	}
	for i := 0; i < len(box.uids); {
		uid := box.uids[i]
		if testUIDSetContains(parts[3], uid) && box.flags[uid][`\Deleted`] {
			box.uids = slices.Delete(box.uids, i, i+1)
			fmt.Fprintf(w, "* %d EXPUNGE\r\n", i+1)
		} else {
			i++
		}
	}
	fmt.Fprintf(w, "%s OK expunged\r\n", tag)
}

func newUserMutationFixture(t *testing.T) (*userStorageFixture, *routedIMAPServer) {
	t.Helper()
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	server.mu.Lock()
	server.mutationMode = true
	server.mu.Unlock()
	f.useIMAPServer(t, server)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.imap.Sync(t.Context(), owner, f.accounts[owner].ID); err != nil {
			t.Fatal(err)
		}
		// The generic HTTP fixture's placeholder is not a remote mailbox. Once
		// receiving discovers INBOX, retain only the actual provider folders.
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			_, err := db.Write().Exec(`DELETE FROM folders WHERE account_id=? AND COALESCE(remote_id,'')=''`, f.accounts[owner].ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, server
}

func (f *userStorageFixture) remoteFolderID(t *testing.T, owner, remote string) string {
	t.Helper()
	var id string
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		return db.Read().QueryRow("SELECT id FROM folders WHERE account_id=? AND remote_id=?", f.accounts[owner].ID, remote).Scan(&id)
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *userStorageFixture) pendingMutations(t *testing.T, owner string) int {
	t.Helper()
	var count int
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		return db.Read().QueryRow("SELECT COUNT(*) FROM message_mutations WHERE status!='applied'").Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func (s *routedIMAPServer) remoteFlag(owner, folder string, origin uint32, flag string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	box := s.mutationMailboxLocked(owner, folder)
	for _, uid := range box.uids {
		if box.origins[uid] == origin {
			return box.flags[uid][flag]
		}
	}
	return false
}

func TestUserIMAPMutationsHTTPFlagsRequeueEvictionAndOwnedEvents(t *testing.T) {
	f, server := newUserMutationFixture(t)
	events := openUserEventStream(t, f, "alice")
	blocked, release := server.setBlock("alice", "store")
	if rec := f.request("alice", http.MethodPost, "/api/messages/1/read?state=read", ""); rec.Code != 200 {
		t.Fatalf("read: %d %s", rec.Code, rec.Body.String())
	}
	awaitIMAP(t, blocked)
	// While Alice is blocked in STORE, a second local action and Bob's actions
	// can acquire the single cache slot. New intent must survive the old result.
	for _, action := range []struct{ owner, path string }{
		{"alice", "/api/messages/1/read?state=unread"},
		{"alice", "/api/messages/1/star"},
		{"bob", "/api/messages/1/read?state=read"},
	} {
		if rec := f.request(action.owner, http.MethodPost, action.path, ""); rec.Code != 200 {
			t.Fatalf("action: %d %s", rec.Code, rec.Body.String())
		}
	}
	waitUserIMAP(t, func() bool { return server.remoteFlag("bob", "INBOX", 2, `\Seen`) })
	awaitUserEvent(t, events, func(event map[string]any) bool {
		if event["type"] == string(mail.EventMutation) && event["account_id"] == f.accounts["bob"].ID {
			t.Fatal("Bob mutation leaked into Alice's stream")
		}
		return event["type"] == string(mail.EventMutation) && event["account_id"] == f.accounts["alice"].ID
	})
	f.events.Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: "alice", Payload: map[string]any{"mutation_barrier": true}})
	awaitUserEvent(t, events, func(event map[string]any) bool {
		if event["type"] == string(mail.EventMutation) && event["account_id"] == f.accounts["bob"].ID {
			t.Fatal("Bob mutation leaked before the event barrier")
		}
		return event["mutation_barrier"] == true
	})
	close(release)
	waitUserIMAP(t, func() bool { return f.pendingMutations(t, "alice") == 0 })
	if server.remoteFlag("alice", "INBOX", 2, `\Seen`) || !server.remoteFlag("alice", "INBOX", 2, `\Flagged`) {
		t.Fatal("older in-flight result overwrote new flag intent")
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		var read, star int
		if err := db.Read().QueryRow("SELECT is_read,is_starred FROM message_folder_state WHERE message_id=1 AND folder_id=?", f.remoteFolderIDUnchecked(t, "alice", db, "INBOX")).Scan(&read, &star); err != nil {
			return err
		}
		if read != 0 || star != 1 {
			t.Fatal("local flags disagree with final remote flags")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var central int
	if err := f.system.Read().QueryRow("SELECT COUNT(*) FROM message_mutations").Scan(&central); err != nil || central != 0 {
		t.Fatal("routed mutations reached the shared mailbox database")
	}
}

func (f *userStorageFixture) remoteFolderIDUnchecked(t *testing.T, owner string, db *storage.DB, remote string) string {
	t.Helper()
	var id string
	if err := db.Read().QueryRow("SELECT id FROM folders WHERE account_id=? AND remote_id=?", f.accounts[owner].ID, remote).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUserIMAPMutationsArchiveMoveTrashAndPermanentDelete(t *testing.T) {
	f, server := newUserMutationFixture(t)
	archive, inbox, trash := f.remoteFolderID(t, "alice", "Archive"), f.inboxID(t, "alice"), f.remoteFolderID(t, "alice", "Trash")
	for _, action := range []struct{ method, path, body, destination string }{
		{http.MethodPost, "/api/messages/1/thread/archive", "", archive},
		{http.MethodPost, "/api/messages/1/move", url.Values{"folder_id": {inbox}}.Encode(), inbox},
		{http.MethodDelete, "/api/messages/1", "", trash},
	} {
		if rec := f.request("alice", action.method, action.path, action.body); rec.Code != 200 {
			t.Fatalf("%s: %d %s", action.path, rec.Code, rec.Body.String())
		}
		waitUserIMAP(t, func() bool { return f.pendingMutations(t, "alice") == 0 })
		if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
			var uid, deleted int
			if err := db.Read().QueryRow("SELECT COALESCE(remote_uid,0),is_deleted FROM message_folder_state WHERE message_id=1 AND folder_id=?", action.destination).Scan(&uid, &deleted); err != nil {
				return err
			}
			if uid == 0 || deleted != 0 {
				t.Fatal("move did not publish destination UID and visible state")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if rec := f.request("alice", http.MethodDelete, "/api/messages/1?folder_id="+url.QueryEscape(trash), ""); rec.Code != 200 {
		t.Fatalf("permanent delete: %d %s", rec.Code, rec.Body.String())
	}
	waitUserIMAP(t, func() bool { return f.pendingMutations(t, "alice") == 0 })
	server.mu.Lock()
	remaining := len(server.mutationMailboxLocked("alice", "Trash").uids)
	bob := len(server.mutationMailboxLocked("bob", "INBOX").uids)
	server.mu.Unlock()
	if remaining != 0 || bob != 2 {
		t.Fatal("delete did not expunge only Alice's mail")
	}
}

func TestUserIMAPMutationsForeignTargetsAndUnsupportedProvidersDoNotWrite(t *testing.T) {
	f, _ := newUserMutationFixture(t)
	if err := f.imap.WakeMutations(t.Context(), "alice", f.accounts["bob"].ID); err != storage.ErrAccountRoute {
		t.Fatalf("foreign worker wake: %v", err)
	}
	bobInbox := f.inboxID(t, "bob")
	for _, request := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/messages/999999/read", ""},
		{http.MethodPost, "/api/messages/read", `{"targets":[{"id":"1"},{"id":"999999"}]}`},
		{http.MethodPost, "/api/messages/1/move", url.Values{"folder_id": {bobInbox}}.Encode()},
		{http.MethodDelete, "/api/messages/1?folder_id=" + url.QueryEscape(bobInbox), ""},
	} {
		rec := f.request("alice", request.method, request.path, request.body)
		if rec.Code != 404 && rec.Code != 400 {
			t.Fatalf("foreign/invalid target: %d %s", rec.Code, rec.Body.String())
		}
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec("UPDATE accounts SET provider='outlook' WHERE id=?", f.accounts["alice"].ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rec := f.request("alice", http.MethodPost, "/api/messages/1/star", ""); rec.Code != 501 {
		t.Fatalf("unconverted provider: %d %s", rec.Code, rec.Body.String())
	}
	for _, owner := range []string{"alice", "bob"} {
		if f.pendingMutations(t, owner) != 0 {
			t.Fatal("rejected request left durable queue intent")
		}
	}
}

func TestUserIMAPMutationsRetryMetadataRecoveryAndDisabledReceiving(t *testing.T) {
	f, server := newUserMutationFixture(t)
	id := f.accounts["alice"].ID
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		if _, err := db.Write().Exec("UPDATE accounts SET email_sync_enabled=0 WHERE id=?", id); err != nil {
			return err
		}
		return db.SetSetting(t.Context(), "alice", "sync_interval_minutes", "1440")
	}); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	server.failStore = true
	server.mu.Unlock()
	if rec := f.request("alice", http.MethodPost, "/api/messages/1/read?state=read", ""); rec.Code != 200 {
		t.Fatalf("queued flags: %d %s", rec.Code, rec.Body.String())
	}
	var next time.Time
	waitUserIMAP(t, func() bool {
		var failed bool
		if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
			var status string
			if err := db.Read().QueryRow("SELECT status,next_attempt_at FROM message_mutations WHERE message_id=1 AND kind='read'").Scan(&status, &next); err != nil {
				return err
			}
			failed = status == storage.MessageMutationFailed
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return failed
	})
	// Wait for the serialized pass/deadline commit, then check central scheduling.
	_ = f.imap.Sync(t.Context(), "alice", id)
	var deadline int64
	if err := f.system.Read().QueryRow("SELECT next_due_ms FROM gofer_account_poll_schedule WHERE account_id=?", id).Scan(&deadline); err != nil || deadline != next.UnixMilli() {
		t.Fatalf("retry hidden behind daily poll: %d vs %d (%v)", deadline, next.UnixMilli(), err)
	}
	server.mu.Lock()
	server.failStore = false
	server.mu.Unlock()
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		// Simulate a durable processing claim left by an interrupted process.
		_, err := db.Write().Exec("UPDATE message_mutations SET status='processing',locked_at=CURRENT_TIMESTAMP WHERE message_id=1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.imap.Sync(t.Context(), "alice", id); err != nil {
		t.Fatal(err)
	}
	if f.pendingMutations(t, "alice") != 0 || !server.remoteFlag("alice", "INBOX", 2, `\Seen`) {
		t.Fatal("interrupted claim was not recovered while receiving disabled")
	}
}

func TestUserIMAPMutationsUIDResetNeverStoresMovesOrDeletesReusedUID(t *testing.T) {
	for _, kind := range []string{"read", "move", "delete"} {
		for _, validity := range []uint32{101, 0} {
			t.Run(fmt.Sprintf("%s/validity-%d", kind, validity), func(t *testing.T) {
				f, server := newUserMutationFixture(t)
				inbox, archive := f.inboxID(t, "alice"), f.remoteFolderID(t, "alice", "Archive")
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					if _, err := db.Write().Exec("UPDATE accounts SET email_sync_enabled=0 WHERE id=?", f.accounts["alice"].ID); err != nil {
						return err
					}
					switch kind {
					case "read":
						return db.SetMessageReadAndQueueForUser(t.Context(), 1, true, "alice")
					case "move":
						return db.MoveMessageAndQueueForUser(t.Context(), 1, inbox, archive, "alice")
					default:
						return db.PermanentlyDeleteMessageAndQueueForUser(t.Context(), 1, inbox, "alice")
					}
				}); err != nil {
					t.Fatal(err)
				}
				server.mu.Lock()
				server.mutationMailboxLocked("alice", "INBOX").validity = validity
				server.commands = nil
				server.mu.Unlock()
				if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err == nil {
					t.Fatal("stale UID was treated as safely applied")
				}
				server.mu.Lock()
				defer server.mu.Unlock()
				for _, command := range server.commands {
					if strings.Contains(command, " UID STORE ") || strings.Contains(command, " UID MOVE ") || strings.Contains(command, " UID EXPUNGE ") {
						t.Fatalf("changed UIDVALIDITY allowed mutation: %s", command)
					}
				}
			})
		}
	}
}

func TestUserIMAPMutationsAccountDeletionCancelsProtocolWait(t *testing.T) {
	f, server := newUserMutationFixture(t)
	blocked, release := server.setBlock("alice", "store")
	defer close(release)
	if rec := f.request("alice", http.MethodPost, "/api/messages/1/read?state=read", ""); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	awaitIMAP(t, blocked)
	id := f.accounts["alice"].ID
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := f.accountStore.DeleteAccount(ctx, "alice", id, f.imap.Cleanup); err != nil {
		t.Fatalf("deletion did not cancel/drain mutation: %v", err)
	}
	if state, err := f.routing.AccountStateForUser(t.Context(), "alice", id); err != nil || state != storage.AccountDeleted {
		t.Fatal("account did not reach deleted state")
	}
	if rec := f.request("bob", http.MethodPost, "/api/messages/1/star", ""); rec.Code != 200 {
		t.Fatal("Alice deletion blocked Bob mail changes")
	}
}

func TestUserIMAPMutationsBulkLimitAndThreadFlags(t *testing.T) {
	f, server := newUserMutationFixture(t)
	var ids []string
	for i := 1; i <= 257; i++ {
		ids = append(ids, `{"id":"`+strconv.Itoa(i)+`"}`)
	}
	if rec := f.request("alice", http.MethodPost, "/api/messages/read", `{"targets":[`+strings.Join(ids, ",")+`]}`); rec.Code != 400 || f.pendingMutations(t, "alice") != 0 {
		t.Fatal("oversized selection was not rejected before writes")
	}
	if rec := f.request("alice", http.MethodPost, "/api/messages/star", `{"targets":[{"id":"1"},{"id":"2"}],"state":"starred"}`); rec.Code != 200 {
		t.Fatalf("bulk star: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.request("alice", http.MethodPost, "/api/messages/1/thread/read?state=read", ""); rec.Code != 200 {
		t.Fatalf("thread read: %d %s", rec.Code, rec.Body.String())
	}
	waitUserIMAP(t, func() bool { return f.pendingMutations(t, "alice") == 0 })
	if !server.remoteFlag("alice", "INBOX", 2, `\Seen`) || !server.remoteFlag("alice", "INBOX", 2, `\Flagged`) || !server.remoteFlag("alice", "INBOX", 1000000, `\Flagged`) {
		t.Fatal("bulk/thread intent did not reach provider")
	}
}

func TestUserIMAPMutationsBackgroundRetryAndBacklogBeyondOnePass(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	server.mu.Lock()
	server.mutationMode = true
	server.uids = nil
	for i := uint32(1); i <= 40; i++ {
		server.uids = append(server.uids, i)
	}
	server.mu.Unlock()
	f.useIMAPServer(t, server)
	id := f.accounts["alice"].ID
	if err := f.imap.Sync(t.Context(), "alice", id); err != nil {
		t.Fatal(err)
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec("UPDATE accounts SET email_sync_enabled=0 WHERE id=?", id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.imap.Start(mail.UserIMAPBackgroundOptions{PollInterval: time.Hour, ScanInterval: 20 * time.Millisecond, DisableIDLE: true}); err != nil {
		t.Fatal(err)
	}
	var targets []string
	for i := 1; i <= 40; i++ {
		targets = append(targets, fmt.Sprintf(`{"id":"%d"}`, i))
	}
	if rec := f.request("alice", http.MethodPost, "/api/messages/read", `{"targets":[`+strings.Join(targets, ",")+`]}`); rec.Code != 200 {
		t.Fatalf("backlog: %d %s", rec.Code, rec.Body.String())
	}
	waitUserIMAP(t, func() bool { return f.pendingMutations(t, "alice") == 0 })
	for i := uint32(1); i <= 40; i++ {
		if !server.remoteFlag("alice", "INBOX", i, `\Seen`) {
			t.Fatal("bounded replay dropped the discovery tail")
		}
	}
	server.mu.Lock()
	server.failStore = true
	server.mu.Unlock()
	if rec := f.request("alice", http.MethodPost, "/api/messages/1/star", ""); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	waitUserIMAP(t, func() bool {
		var failed int
		if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
			return db.Read().QueryRow("SELECT COUNT(*) FROM message_mutations WHERE kind='starred' AND status='failed'").Scan(&failed)
		}); err != nil {
			t.Fatal(err)
		}
		return failed == 1
	})
	// Move the saved deadline forward for a fast deterministic retry. No direct
	// QueueAccount/manual Sync follows: periodic central discovery must deliver it.
	server.mu.Lock()
	server.failStore = false
	server.mu.Unlock()
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec("UPDATE message_mutations SET next_attempt_at=? WHERE kind='starred' AND status='failed'", time.Now().Add(-time.Second).UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.routing.ResetAccountPolling(t.Context(), "alice", id); err != nil {
		t.Fatal(err)
	}
	waitUserIMAP(t, func() bool {
		return f.pendingMutations(t, "alice") == 0 && server.remoteFlag("alice", "INBOX", 1, `\Flagged`)
	})
}

func TestUserIMAPMutationsAtomicQueueRollbackAndSavedResultAfterWakeFailure(t *testing.T) {
	f, server := newUserMutationFixture(t)
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`CREATE TRIGGER reject_mail_intent BEFORE INSERT ON message_mutations
			BEGIN SELECT RAISE(ABORT,'injected queue failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rec := f.request("alice", http.MethodPost, "/api/messages/1/star", ""); rec.Code != 500 {
		t.Fatalf("failed transaction: %d %s", rec.Code, rec.Body.String())
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		var starred int
		if err := db.Read().QueryRow("SELECT is_starred FROM message_folder_state WHERE message_id=1 AND is_deleted=0").Scan(&starred); err != nil {
			return err
		}
		if starred != 0 || server.remoteFlag("alice", "INBOX", 2, `\Flagged`) {
			t.Fatal("failed queue write changed local or remote flags")
		}
		_, err := db.Write().Exec("DROP TRIGGER reject_mail_intent")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.system.Write().Exec(`CREATE TRIGGER reject_mail_wake BEFORE INSERT ON gofer_account_poll_schedule
		WHEN NEW.revision=1
		BEGIN SELECT RAISE(ABORT,'injected wake failure'); END`); err != nil {
		t.Fatal(err)
	}
	defer f.system.Write().Exec("DROP TRIGGER reject_mail_wake")
	rec := f.request("alice", http.MethodPost, "/api/messages/1/star", "")
	if rec.Code != 200 || rec.Header().Get("X-Gofer-Mail-Delivery") != "delayed" || !strings.Contains(rec.Body.String(), `"is_starred":true`) {
		t.Fatalf("saved toggle reported as failed: %d %s", rec.Code, rec.Body.String())
	}
	waitUserIMAP(t, func() bool {
		return server.remoteFlag("alice", "INBOX", 2, `\Flagged`) && f.pendingMutations(t, "alice") == 0
	})
}

func TestUserIMAPMutationsMoveRecoveryAfterLocalPublicationFailure(t *testing.T) {
	f, server := newUserMutationFixture(t)
	id := f.accounts["alice"].ID
	archive := f.remoteFolderID(t, "alice", "Archive")
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		if _, err := db.Write().Exec("UPDATE accounts SET email_sync_enabled=0 WHERE id=?", id); err != nil {
			return err
		}
		_, err := db.Write().Exec(`CREATE TRIGGER reject_move_publication BEFORE UPDATE OF remote_uid ON message_folder_state
			WHEN NEW.folder_id='` + archive + `' BEGIN SELECT RAISE(ABORT,'injected move publication failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rec := f.request("alice", http.MethodPost, "/api/messages/1/thread/archive", ""); rec.Code != 200 {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body.String())
	}
	waitUserIMAP(t, func() bool {
		var failed int
		if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
			return db.Read().QueryRow("SELECT COUNT(*) FROM message_mutations WHERE kind='move' AND status='failed'").Scan(&failed)
		}); err != nil {
			t.Fatal(err)
		}
		return failed == 1
	})
	server.mu.Lock()
	remote := len(server.mutationMailboxLocked("alice", "Archive").uids)
	server.mu.Unlock()
	if remote != 1 {
		t.Fatal("injected failure occurred before remote move")
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		if _, err := db.Write().Exec("DROP TRIGGER reject_move_publication"); err != nil {
			return err
		}
		_, err := db.Write().Exec("UPDATE message_mutations SET next_attempt_at=? WHERE kind='move'", time.Now().Add(-time.Second).UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.imap.Sync(t.Context(), "alice", id); err != nil {
		t.Fatal(err)
	}
	if f.pendingMutations(t, "alice") != 0 {
		t.Fatal("already moved message was not recovered by Message-ID")
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.mutationMailboxLocked("alice", "Archive").uids) != 1 {
		t.Fatal("recovered move created a duplicate")
	}
}
