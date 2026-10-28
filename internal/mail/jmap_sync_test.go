package mail

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// fakeJMAPServer is a small JMAP server for robin@example.test: Mailbox/get,
// Email/get (by ids or by a reference to a query) and Email/query newest
// first with anchor paging, over whatever mailboxes and emails the test sets.
type fakeJMAPServer struct {
	*httptest.Server
	mu              sync.Mutex
	mailboxes       []jmap.Mailbox
	emails          []jmap.Email
	emailState      string
	maxObjectsInGet int
	// emailSets counts Email/set calls.
	emailSets int
	// loseAnchorOnce answers the first anchored query with anchorNotFound, as
	// when the anchor email was deleted between two pages.
	loseAnchorOnce bool
	queries        int
	// failQuery makes that Email/query (counting from 1) fail with
	// serverFail. queryLog records each query's anchor and date filter.
	failQuery int
	queryLog  []fakeJMAPQuery
	// changeLog records each change made through addEmail, updateEmail and
	// deleteEmail; its states are "c1", "c2"... Email/changes answers from it.
	changeLog []fakeJMAPChange
	// oneChangePerCall makes Email/changes return one logged change at a time,
	// with hasMoreChanges while more remain.
	oneChangePerCall bool
	// forgetBefore makes states older than this change number unknown.
	forgetBefore int
	// blobs holds raw messages by blob id; downloads counts their downloads.
	blobs     map[string]string
	downloads int
	sets      int
	// Optional download barrier for testing account changes during HTTP waits.
	downloadStarted chan<- struct{}
	downloadRelease <-chan struct{}
	// pushes, when set, offers push: each event source connection writes
	// the StateChange JSON sent here. streams counts open connections.
	pushes  chan string
	streams int
	// mailboxLog records each change of the mailboxes; Mailbox/changes
	// answers from it, with states "m1", "m2"... fullMailboxGets counts
	// Mailbox/get calls for every mailbox. noMailboxChanges makes every
	// Mailbox/changes fail with cannotCalculateChanges.
	mailboxLog       []fakeMailboxChange
	fullMailboxGets  int
	noMailboxChanges bool
}

// emailIndex finds an email; the caller holds f.mu.
func (f *fakeJMAPServer) emailIndex(id string) int {
	for i, email := range f.emails {
		if email.ID == id {
			return i
		}
	}
	return -1
}

func (f *fakeJMAPServer) email(id string) (jmap.Email, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i := f.emailIndex(id); i >= 0 {
		return f.emails[i], true
	}
	return jmap.Email{}, false
}

type fakeJMAPQuery struct {
	anchor        string
	after, before *time.Time
}

type fakeJMAPChange struct {
	created, updated, destroyed string
}

func (f *fakeJMAPServer) logChange(change fakeJMAPChange) {
	f.changeLog = append(f.changeLog, change)
	f.emailState = fmt.Sprintf("c%d", len(f.changeLog))
}

func (f *fakeJMAPServer) addEmail(email jmap.Email) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emails = append(f.emails, email)
	f.logChange(fakeJMAPChange{created: email.ID})
}

func (f *fakeJMAPServer) updateEmail(id string, change func(*jmap.Email)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.emails {
		if f.emails[i].ID == id {
			change(&f.emails[i])
		}
	}
	f.logChange(fakeJMAPChange{updated: id})
}

func (f *fakeJMAPServer) deleteEmail(id string, logged bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.emails[:0]
	for _, email := range f.emails {
		if email.ID != id {
			kept = append(kept, email)
		}
	}
	f.emails = kept
	if logged {
		f.logChange(fakeJMAPChange{destroyed: id})
	} else {
		// The state still moves on; only the change itself is not kept.
		f.emailState = fmt.Sprintf("c%d", len(f.changeLog)+100)
	}
}

func newFakeJMAPServer(t *testing.T) *fakeJMAPServer {
	t.Helper()
	f := &fakeJMAPServer{emailState: "es1", maxObjectsInGet: 500, blobs: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pass, ok := r.BasicAuth(); !ok || user != "robin@example.test" || pass != "secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/.well-known/jmap":
			f.mu.Lock()
			max := f.maxObjectsInGet
			events := ""
			if f.pushes != nil {
				events = `"eventSourceUrl": "/jmap/events?types={types}&closeafter={closeafter}&ping={ping}",`
			}
			f.mu.Unlock()
			fmt.Fprintf(w, `{"capabilities": {"urn:ietf:params:jmap:core": {"maxCallsInRequest": 16, "maxObjectsInGet": %d}, "urn:ietf:params:jmap:mail": {}},
				"accounts": {"a1": {"name": "robin@example.test"}}, "primaryAccounts": {"urn:ietf:params:jmap:mail": "a1"},
				"username": "robin@example.test", "apiUrl": "/jmap/", "state": "s1", %s
				"downloadUrl": "/jmap/download/{accountId}/{blobId}/{name}?accept={type}"}`, max, events)
		case "/jmap/events":
			f.mu.Lock()
			pushes := f.pushes
			f.streams++
			f.mu.Unlock()
			defer func() { f.mu.Lock(); f.streams--; f.mu.Unlock() }()
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			for {
				select {
				case <-r.Context().Done():
					return
				case change := <-pushes:
					fmt.Fprintf(w, "event: state\ndata: %s\n\n", change)
					w.(http.Flusher).Flush()
				}
			}
		case "/jmap/":
			f.answer(w, r)
		default:
			var account, blob string
			if n, _ := fmt.Sscanf(r.URL.Path, "/jmap/download/%2s/%s", &account, &blob); n == 2 {
				blob = strings.SplitN(blob, "/", 2)[0]
				f.mu.Lock()
				raw, ok := f.blobs[blob]
				f.downloads++
				started, release := f.downloadStarted, f.downloadRelease
				f.mu.Unlock()
				if started != nil {
					select {
					case started <- struct{}{}:
					default:
					}
				}
				if release != nil {
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				if ok {
					_, _ = io.WriteString(w, raw)
					return
				}
			}
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Server.Close)
	return f
}

func (f *fakeJMAPServer) answer(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var request struct {
		MethodCalls [][3]json.RawMessage `json:"methodCalls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	queryIDs := map[string][]string{}
	var responses []any
	for _, call := range request.MethodCalls {
		var name, callID string
		var args map[string]json.RawMessage
		_ = json.Unmarshal(call[0], &name)
		_ = json.Unmarshal(call[1], &args)
		_ = json.Unmarshal(call[2], &callID)
		switch name {
		case "Mailbox/get":
			list, notFound := f.mailboxes, []string{}
			var ids []string
			if json.Unmarshal(args["ids"], &ids) != nil || ids == nil {
				f.fullMailboxGets++
			} else {
				list = []jmap.Mailbox{}
				for _, id := range ids {
					found := false
					for _, mailbox := range f.mailboxes {
						if mailbox.ID == id {
							list, found = append(list, mailbox), true
						}
					}
					if !found {
						notFound = append(notFound, id)
					}
				}
			}
			responses = append(responses, []any{name, map[string]any{"accountId": "a1", "state": fmt.Sprintf("m%d", len(f.mailboxLog)), "list": list, "notFound": notFound}, callID})
		case "Mailbox/changes":
			var since string
			_ = json.Unmarshal(args["sinceState"], &since)
			var from int
			if _, err := fmt.Sscanf(since, "m%d", &from); err != nil || from > len(f.mailboxLog) || f.noMailboxChanges {
				responses = append(responses, []any{"error", map[string]any{"type": "cannotCalculateChanges"}, callID})
				continue
			}
			created, updated, destroyed := []string{}, []string{}, []string{}
			countsOnly := true
			for _, change := range f.mailboxLog[from:] {
				created = append(created, change.created...)
				updated = append(updated, change.updated...)
				destroyed = append(destroyed, change.destroyed...)
				countsOnly = countsOnly && change.countsOnly
			}
			var properties []string
			if countsOnly {
				properties = jmap.MailboxCountProperties
			}
			responses = append(responses, []any{name, map[string]any{"accountId": "a1", "oldState": since, "newState": fmt.Sprintf("m%d", len(f.mailboxLog)),
				"hasMoreChanges": false, "created": created, "updated": updated, "destroyed": destroyed, "updatedProperties": properties}, callID})
		case "Email/get":
			var ids []string
			if ref, ok := args["#ids"]; ok {
				var reference jmap.ResultReference
				_ = json.Unmarshal(ref, &reference)
				ids = queryIDs[reference.ResultOf]
			} else {
				_ = json.Unmarshal(args["ids"], &ids)
			}
			list, notFound := []jmap.Email{}, []string{}
			for _, id := range ids {
				found := false
				for _, email := range f.emails {
					if email.ID == id {
						list = append(list, email)
						found = true
					}
				}
				if !found {
					notFound = append(notFound, id)
				}
			}
			responses = append(responses, []any{name, map[string]any{"accountId": "a1", "state": f.emailState, "list": list, "notFound": notFound}, callID})
		case "Email/query":
			f.queries++
			var anchor string
			var anchorOffset, limit int
			var filter jmap.EmailFilter
			_ = json.Unmarshal(args["anchor"], &anchor)
			_ = json.Unmarshal(args["anchorOffset"], &anchorOffset)
			_ = json.Unmarshal(args["limit"], &limit)
			_ = json.Unmarshal(args["filter"], &filter)
			f.queryLog = append(f.queryLog, fakeJMAPQuery{anchor: anchor, after: filter.After, before: filter.Before})
			if f.queries == f.failQuery {
				responses = append(responses, []any{"error", map[string]any{"type": "serverFail"}, callID})
				continue
			}
			if limit <= 0 || limit > f.maxObjectsInGet {
				limit = f.maxObjectsInGet
			}
			sorted := []jmap.Email{}
			for _, email := range f.emails {
				// After is inclusive and before exclusive (RFC 8621 4.4.1).
				if (filter.After == nil || !email.ReceivedAt.Before(*filter.After)) && (filter.Before == nil || email.ReceivedAt.Before(*filter.Before)) {
					sorted = append(sorted, email)
				}
			}
			sort.Slice(sorted, func(i, j int) bool { return sorted[i].ReceivedAt.After(sorted[j].ReceivedAt) })
			start := 0
			if anchor != "" {
				found := -1
				for i, email := range sorted {
					if email.ID == anchor {
						found = i
					}
				}
				if found < 0 || f.loseAnchorOnce {
					f.loseAnchorOnce = false
					responses = append(responses, []any{"error", map[string]any{"type": "anchorNotFound"}, callID})
					continue
				}
				start = found + anchorOffset
			}
			ids := []string{}
			for i := start; i < len(sorted) && len(ids) < limit; i++ {
				ids = append(ids, sorted[i].ID)
			}
			queryIDs[callID] = ids
			responses = append(responses, []any{name, map[string]any{"accountId": "a1", "queryState": "q1", "position": start, "ids": ids, "limit": limit}, callID})
		case "Email/set":
			f.emailSets++
			var update map[string]map[string]any
			var destroy []string
			_ = json.Unmarshal(args["update"], &update)
			_ = json.Unmarshal(args["destroy"], &destroy)
			oldState := f.emailState
			updated, notUpdated := map[string]any{}, map[string]any{}
			for id, patch := range update {
				index := f.emailIndex(id)
				if index < 0 {
					notUpdated[id] = map[string]string{"type": "notFound"}
					continue
				}
				email := &f.emails[index]
				for path, value := range patch {
					set := value != nil
					if keyword, ok := strings.CutPrefix(path, "keywords/"); ok {
						if set {
							email.Keywords[keyword] = true
						} else {
							delete(email.Keywords, keyword)
						}
					} else if mailbox, ok := strings.CutPrefix(path, "mailboxIds/"); ok {
						if set {
							email.MailboxIDs[mailbox] = true
						} else {
							delete(email.MailboxIDs, mailbox)
						}
					}
				}
				updated[id] = nil
				f.logChange(fakeJMAPChange{updated: id})
			}
			destroyed, notDestroyed := []string{}, map[string]any{}
			for _, id := range destroy {
				index := f.emailIndex(id)
				if index < 0 {
					notDestroyed[id] = map[string]string{"type": "notFound"}
					continue
				}
				f.emails = append(f.emails[:index], f.emails[index+1:]...)
				destroyed = append(destroyed, id)
				f.logChange(fakeJMAPChange{destroyed: id})
			}
			f.sets++
			responses = append(responses, []any{name, map[string]any{"accountId": "a1", "oldState": oldState, "newState": f.emailState,
				"updated": updated, "notUpdated": notUpdated, "destroyed": destroyed, "notDestroyed": notDestroyed}, callID})
		case "Email/changes":
			var since string
			_ = json.Unmarshal(args["sinceState"], &since)
			if since == f.emailState {
				responses = append(responses, []any{name, map[string]any{"accountId": "a1", "oldState": since, "newState": since, "hasMoreChanges": false, "created": []string{}, "updated": []string{}, "destroyed": []string{}}, callID})
				continue
			}
			var from int
			if _, err := fmt.Sscanf(since, "c%d", &from); err != nil || from < f.forgetBefore {
				responses = append(responses, []any{"error", map[string]any{"type": "cannotCalculateChanges"}, callID})
				continue
			}
			to := len(f.changeLog)
			if f.oneChangePerCall && from+1 < to {
				to = from + 1
			}
			created, updated, destroyed := []string{}, []string{}, []string{}
			for _, change := range f.changeLog[from:to] {
				for list, id := range map[*[]string]string{&created: change.created, &updated: change.updated, &destroyed: change.destroyed} {
					if id != "" {
						*list = append(*list, id)
					}
				}
			}
			responses = append(responses, []any{name, map[string]any{"accountId": "a1", "oldState": since, "newState": fmt.Sprintf("c%d", to),
				"hasMoreChanges": to < len(f.changeLog), "created": created, "updated": updated, "destroyed": destroyed}, callID})
		default:
			responses = append(responses, []any{"error", map[string]any{"type": "unknownMethod"}, callID})
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": responses, "sessionState": "s1"})
}

// setMailboxes replaces the mailboxes, logging which were created,
// destroyed or changed.
func (f *fakeJMAPServer) setMailboxes(mailboxes ...jmap.Mailbox) {
	f.mu.Lock()
	defer f.mu.Unlock()
	old := map[string]bool{}
	for _, mailbox := range f.mailboxes {
		old[mailbox.ID] = true
	}
	var change fakeMailboxChange
	for _, mailbox := range mailboxes {
		if old[mailbox.ID] {
			change.updated = append(change.updated, mailbox.ID)
			delete(old, mailbox.ID)
		} else {
			change.created = append(change.created, mailbox.ID)
		}
	}
	for id := range old {
		change.destroyed = append(change.destroyed, id)
	}
	f.mailboxes = mailboxes
	f.mailboxLog = append(f.mailboxLog, change)
}

// setMailboxCounts changes a mailbox's counters, a change Mailbox/changes
// reports as counters only.
func (f *fakeJMAPServer) setMailboxCounts(id string, total, unread int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.mailboxes {
		if f.mailboxes[i].ID == id {
			f.mailboxes[i].TotalEmails, f.mailboxes[i].UnreadEmails = total, unread
		}
	}
	f.mailboxLog = append(f.mailboxLog, fakeMailboxChange{updated: []string{id}, countsOnly: true})
}

type fakeMailboxChange struct {
	created, updated, destroyed []string
	countsOnly                  bool
}

func readable() jmap.MailboxRights { return jmap.MailboxRights{MayReadItems: true} }

// useFakeJMAPAccount turns alice's account in the managed fixture into a JMAP
// account on server, with the exceptions its loopback address needs.
func useFakeJMAPAccount(t *testing.T, f *userGmailFixture, server *fakeJMAPServer) {
	t.Helper()
	t.Setenv("GOFER_JMAP_EXPERIMENT", "true")
	sessionURL := server.URL + "/.well-known/jmap"
	endpoint, err := jmap.ParseEndpoint(sessionURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.system.AddPlaintextTransportException(t.Context(), "http", endpoint.Host, endpoint.Port, "administrator"); err != nil {
		t.Fatal(err)
	}
	if err := f.system.AddPrivateTargetException(t.Context(), "http", endpoint.Host, endpoint.Port, "administrator"); err != nil {
		t.Fatal(err)
	}
	if err := f.worker.accounts.UpdateAccount(t.Context(), "alice", f.ids["alice"], &models.CreateAccountRequest{
		Provider: "jmap", JMAPSessionURL: sessionURL, Username: "robin@example.test", Password: "secret",
	}); err != nil {
		t.Fatal(err)
	}
}

type jmapTestFolder struct {
	ID, ParentID, RemoteID, ProviderRemoteID, Name, Role string
	Total, Unread                                        int
	Selectable                                           bool
}

func jmapFolders(t *testing.T, f *userGmailFixture) map[string]jmapTestFolder {
	t.Helper()
	folders := map[string]jmapTestFolder{}
	f.local(t, "alice", func(db *storage.DB) error {
		rows, err := db.Read().Query(`SELECT id, COALESCE(parent_id,''), COALESCE(remote_id,''), provider_remote_id, name, role,
			COALESCE(total_count,0), COALESCE(unread_count,0), COALESCE(selectable,1) FROM folders WHERE account_id=? AND provider_remote_id != ''`, f.ids["alice"])
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var folder jmapTestFolder
			if err := rows.Scan(&folder.ID, &folder.ParentID, &folder.RemoteID, &folder.ProviderRemoteID, &folder.Name, &folder.Role, &folder.Total, &folder.Unread, &folder.Selectable); err != nil {
				return err
			}
			folders[folder.ProviderRemoteID] = folder
		}
		return rows.Err()
	})
	return folders
}

func TestSyncStoresJMAPMailboxesAsFolders(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.setMailboxes(
		jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", TotalEmails: 3, UnreadEmails: 1, MyRights: readable()},
		jmap.Mailbox{ID: "mb-sent", Name: "Sent Items", Role: "sent", TotalEmails: 2, MyRights: readable()},
		jmap.Mailbox{ID: "mb-proj", Name: "Projects", MyRights: readable()},
		jmap.Mailbox{ID: "mb-2026", Name: "2026", ParentID: "mb-proj", TotalEmails: 4, UnreadEmails: 4, MyRights: readable()},
	)
	useFakeJMAPAccount(t, f, server)

	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	folders := jmapFolders(t, f)
	inbox, sent, projects, year := folders["mb-in"], folders["mb-sent"], folders["mb-proj"], folders["mb-2026"]
	if inbox.Role != "inbox" || inbox.Name != "Inbox" || inbox.Total != 3 || inbox.Unread != 1 || !inbox.Selectable {
		t.Fatalf("inbox = %+v", inbox)
	}
	// The server's name for a special mailbox gives way to Gofer's.
	if sent.Role != "sent" || sent.Name != "Sent" {
		t.Fatalf("sent = %+v", sent)
	}
	if projects.Role != "custom" || projects.Name != "Projects" || projects.ParentID != "" {
		t.Fatalf("projects = %+v", projects)
	}
	if year.ParentID != projects.ID || year.RemoteID != "Projects/2026" || year.Unread != 4 {
		t.Fatalf("child = %+v (parent %q)", year, projects.ID)
	}

	// A renamed mailbox keeps its folder.
	server.setMailboxes(
		jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", TotalEmails: 3, MyRights: readable()},
		jmap.Mailbox{ID: "mb-sent", Name: "Sent Items", Role: "sent", TotalEmails: 2, MyRights: readable()},
		jmap.Mailbox{ID: "mb-proj", Name: "Work", MyRights: readable()},
		jmap.Mailbox{ID: "mb-2026", Name: "2026", ParentID: "mb-proj", MyRights: readable()},
	)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("second Sync() error = %v", err)
	}
	folders = jmapFolders(t, f)
	if renamed := folders["mb-proj"]; renamed.ID != projects.ID || renamed.Name != "Work" {
		t.Fatalf("renamed folder = %+v, was %+v", renamed, projects)
	}
	if child := folders["mb-2026"]; child.RemoteID != "Work/2026" || child.ParentID != projects.ID {
		t.Fatalf("child after rename = %+v", child)
	}
	if folders["mb-in"].Unread != 0 {
		t.Fatalf("inbox unread not updated: %+v", folders["mb-in"])
	}
}

type jmapTestMessage struct {
	Folders         map[string]bool
	Read, Flagged   bool
	Subject, From   string
	InternetMessage string
}

// jmapMessages reads alice's stored JMAP messages by provider id, with the
// mailboxes (by provider id) each shows in.
func jmapMessages(t *testing.T, f *userGmailFixture) map[string]*jmapTestMessage {
	t.Helper()
	messages := map[string]*jmapTestMessage{}
	f.local(t, "alice", func(db *storage.DB) error {
		rows, err := db.Read().Query(`SELECT COALESCE(mfs.remote_message_id, ''), f.provider_remote_id, mfs.is_read, mfs.is_flagged, m.subject, m.from_email, m.internet_message_id
			FROM messages m JOIN message_folder_state mfs ON mfs.message_id = m.id AND mfs.is_deleted = 0
			JOIN folders f ON f.id = mfs.folder_id WHERE m.account_id = ?`, f.ids["alice"])
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, mailbox, subject, from, internetID string
			var read, flagged bool
			if err := rows.Scan(&id, &mailbox, &read, &flagged, &subject, &from, &internetID); err != nil {
				return err
			}
			message := messages[id]
			if message == nil {
				message = &jmapTestMessage{Folders: map[string]bool{}, Read: read, Flagged: flagged, Subject: subject, From: from, InternetMessage: internetID}
				messages[id] = message
			}
			message.Folders[mailbox] = true
		}
		return rows.Err()
	})
	return messages
}

func jmapCursor(t *testing.T, f *userGmailFixture, scope string) string {
	t.Helper()
	var cursor string
	f.local(t, "alice", func(db *storage.DB) error {
		state, err := db.GetLabelSyncState(t.Context(), f.ids["alice"], storage.LabelProviderJMAP, scope)
		cursor = state.Cursor
		return err
	})
	return cursor
}

func testJMAPEmail(id, subject string, minutesAgo int, mailboxes ...string) jmap.Email {
	email := jmap.Email{
		ID: id, BlobID: "blob-" + id, ThreadID: "t-" + id, Subject: subject, Size: 1200,
		MailboxIDs: map[string]bool{}, Keywords: map[string]bool{},
		From:       []jmap.EmailAddress{{Name: "Sam Ortiz", Email: "sam@example.test"}},
		To:         []jmap.EmailAddress{{Email: "robin@example.test"}},
		MessageID:  []string{id + "@example.test"},
		ReceivedAt: time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC).Add(-time.Duration(minutesAgo) * time.Minute),
		Preview:    "About " + subject,
	}
	for _, mailbox := range mailboxes {
		email.MailboxIDs[mailbox] = true
	}
	return email
}

// The first sync imports every email page by page, one row per mailbox it is
// in, and only then saves the states read before the import as cursors.
func TestSyncImportsJMAPEmails(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.maxObjectsInGet = 2
	server.loseAnchorOnce = true
	server.emailState = "es-before"
	server.setMailboxes(
		jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()},
		jmap.Mailbox{ID: "mb-arch", Name: "Archive", Role: "archive", MyRights: readable()},
		jmap.Mailbox{ID: "mb-proj", Name: "Projects", MyRights: readable()},
	)
	unread := testJMAPEmail("e1", "Lunch on Friday?", 1, "mb-in")
	labelled := testJMAPEmail("e2", "Garden plans", 2, "mb-in", "mb-proj")
	labelled.Keywords = map[string]bool{"$seen": true, "$flagged": true}
	archived := testJMAPEmail("e3", "Old receipt", 3, "mb-arch")
	archived.Keywords = map[string]bool{"$seen": true}
	draft := testJMAPEmail("e4", "Notes", 4, "mb-in")
	elsewhere := testJMAPEmail("e5", "Somewhere unknown", 5, "mb-hidden")
	server.emails = []jmap.Email{unread, labelled, archived, draft, elsewhere}
	useFakeJMAPAccount(t, f, server)

	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	messages := jmapMessages(t, f)
	if len(messages) != 4 || messages["e5"] != nil {
		t.Fatalf("stored %d messages (%v), want e1-e4", len(messages), messages)
	}
	if m := messages["e1"]; m.Read || m.Flagged || !m.Folders["mb-in"] || m.Subject != "Lunch on Friday?" || m.From != "sam@example.test" || m.InternetMessage != "<e1@example.test>" {
		t.Fatalf("e1 = %+v", m)
	}
	if m := messages["e2"]; !m.Read || !m.Flagged || !m.Folders["mb-in"] || !m.Folders["mb-proj"] || len(m.Folders) != 2 {
		t.Fatalf("e2 = %+v", m)
	}
	if m := messages["e3"]; !m.Folders["mb-arch"] || len(m.Folders) != 1 {
		t.Fatalf("e3 = %+v", m)
	}
	// Five emails two at a time is three pages; the lost anchor restarts once.
	if server.queries < 4 {
		t.Fatalf("server answered %d queries, want paging with one restart", server.queries)
	}
	if got := jmapCursor(t, f, jmapEmailScope); got != "es-before" {
		t.Fatalf("email cursor = %q, want the state read before importing", got)
	}
	if got := jmapCursor(t, f, jmapMailboxScope); got != "m1" {
		t.Fatalf("mailbox cursor = %q", got)
	}
	if got := jmapCursor(t, f, jmapImportScope); got != "" {
		t.Fatalf("finished import left progress %q", got)
	}

	// With a cursor saved, the next sync does not import again.
	queries := server.queries
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	if server.queries != queries {
		t.Fatal("imported again although a cursor was saved")
	}
}

// jmapImportFixture is an account with five emails, e1 newest, that the
// server hands out two at a time.
func jmapImportFixture(t *testing.T) (*userGmailFixture, *fakeJMAPServer) {
	t.Helper()
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.maxObjectsInGet = 2
	server.emailState = "es-before"
	server.setMailboxes(jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()})
	for i := 1; i <= 5; i++ {
		server.emails = append(server.emails, testJMAPEmail(fmt.Sprintf("e%d", i), fmt.Sprintf("Message %d", i), i, "mb-in"))
	}
	useFakeJMAPAccount(t, f, server)
	return f, server
}

func storedJMAPIDs(t *testing.T, f *userGmailFixture) string {
	t.Helper()
	var ids []string
	for id := range jmapMessages(t, f) {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// An import that fails partway keeps what it stored and where it got to; the
// next sync continues from there with the state read before the first page.
func TestSyncResumesInterruptedJMAPImport(t *testing.T) {
	f, server := jmapImportFixture(t)
	server.failQuery = 2
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err == nil {
		t.Fatal("Sync() succeeded although a page failed")
	}
	if got := storedJMAPIDs(t, f); got != "e1,e2" {
		t.Fatalf("stored %s after the failed page", got)
	}
	if jmapCursor(t, f, jmapEmailScope) != "" || jmapCursor(t, f, jmapImportScope) == "" {
		t.Fatal("an unfinished import must keep its progress and save no cursor")
	}

	server.emailState = "es-later"
	server.queryLog = nil
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("resumed Sync() error = %v", err)
	}
	if got := storedJMAPIDs(t, f); got != "e1,e2,e3,e4,e5" {
		t.Fatalf("stored %s", got)
	}
	for _, query := range server.queryLog {
		if query.anchor == "" && query.after == nil {
			t.Fatalf("the resumed import started over: %+v", server.queryLog)
		}
	}
	if got := jmapCursor(t, f, jmapEmailScope); got != "es-before" {
		t.Fatalf("email cursor = %q, want the state read before the first attempt", got)
	}
	if got := jmapCursor(t, f, jmapImportScope); got != "" {
		t.Fatalf("finished import left progress %q", got)
	}
}

// When the email the import stopped at is deleted before it continues, the
// import continues from that email's date and skips nothing.
func TestSyncResumesJMAPImportAfterItsEmailIsDeleted(t *testing.T) {
	f, server := jmapImportFixture(t)
	server.failQuery = 2
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err == nil {
		t.Fatal("Sync() succeeded although a page failed")
	}
	server.deleteEmail("e2", false)
	server.queryLog = nil
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("resumed Sync() error = %v", err)
	}
	if got := storedJMAPIDs(t, f); got != "e1,e3,e4,e5" {
		t.Fatalf("stored %s", got)
	}
	dated := false
	for _, query := range server.queryLog {
		dated = dated || query.before != nil
	}
	if !dated {
		t.Fatalf("the import didn't continue from the deleted email's date: %+v", server.queryLog)
	}
}

// A new import of a known account that is interrupted still removes, once it
// finishes, the messages the server no longer has, and keeps the ones it
// saw before the interruption.
func TestResumedJMAPReimportRemovesOnlyLostMessages(t *testing.T) {
	f, server := jmapImportFixture(t)
	server.emailState = "c0"
	server.emails = append(server.emails, testJMAPEmail("e6", "Message 6", 6, "mb-in"))
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	// The server deletes e6 and can no longer give the changes since c0.
	server.deleteEmail("e6", false)
	server.forgetBefore = 1000
	server.failQuery = server.queries + 2
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err == nil {
		t.Fatal("Sync() succeeded although a page failed")
	}
	if got := storedJMAPIDs(t, f); got != "e1,e2,e3,e4,e5,e6" {
		t.Fatalf("an unfinished import removed messages: %s", got)
	}
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("resumed Sync() error = %v", err)
	}
	if got := storedJMAPIDs(t, f); got != "e1,e2,e3,e4,e5" {
		t.Fatalf("stored %s, want everything but e6", got)
	}
}

// Mail the server reports as created since the cursor is announced as new
// mail, per folder, like IMAP's; the first import and changes to known mail
// are not.
func TestSyncAnnouncesNewJMAPMail(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.emailState = "c0"
	server.setMailboxes(
		jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()},
		jmap.Mailbox{ID: "mb-sent", Name: "Sent", Role: "sent", MyRights: readable()},
	)
	server.emails = []jmap.Email{testJMAPEmail("e1", "Lunch on Friday?", 10, "mb-in")}
	useFakeJMAPAccount(t, f, server)
	events := f.worker.Events().Subscribe()
	defer f.worker.Events().Unsubscribe(events)
	newMail := func() map[string]Event {
		t.Helper()
		got := map[string]Event{}
		for len(events) > 0 {
			if event := <-events; event.Type == EventNewMail {
				got[event.FolderID] = event
			}
		}
		return got
	}
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	if got := newMail(); len(got) != 0 {
		t.Fatalf("the first import announced new mail: %v", got)
	}

	server.updateEmail("e1", func(e *jmap.Email) { e.Keywords = map[string]bool{"$seen": true} })
	server.addEmail(testJMAPEmail("e2", "Garden plans", 2, "mb-in"))
	latest := testJMAPEmail("e3", "Invoice 42", 1, "mb-in")
	latest.From = []jmap.EmailAddress{{Name: "Billing", Email: "billing@example.test"}}
	server.addEmail(latest)
	mine := testJMAPEmail("e4", "Re: Lunch", 0, "mb-sent")
	mine.Keywords = map[string]bool{"$seen": true}
	server.addEmail(mine)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	got := newMail()
	inbox := got[jmapLocalFolderID(f.ids["alice"], "mb-in")]
	if inbox.FolderRole != "inbox" || inbox.Payload["count"] != 2 || inbox.Payload["unread_count"] != 2 ||
		inbox.Payload["subject"] != "Invoice 42" || inbox.Payload["from_name"] != "Billing" || inbox.Payload["from_email"] != "billing@example.test" {
		t.Fatalf("inbox new mail = %+v", inbox)
	}
	if sent := got[jmapLocalFolderID(f.ids["alice"], "mb-sent")]; sent.FolderRole != "sent" || sent.Payload["unread_count"] != 0 {
		t.Fatalf("sent new mail = %+v", sent)
	}
}

// In per-user stores, mail rules run on the emails a sync finds created,
// not on the first import; what they move away isn't announced, and the next
// pass sends their changes to the server.
func TestSyncRunsMailRulesOnNewJMAPMail(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.emailState = "c0"
	server.setMailboxes(
		jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()},
		jmap.Mailbox{ID: "mb-arch", Name: "Archive", Role: "archive", MyRights: readable()},
	)
	oldOffers := testJMAPEmail("e1", "Last week's offers", 10, "mb-in")
	oldOffers.ListID = "Corner Shop offers <offers.corner-shop.example>"
	server.emails = []jmap.Email{oldOffers}
	useFakeJMAPAccount(t, f, server)
	f.local(t, "alice", func(db *storage.DB) error {
		_, err := db.SaveMailRuleGuarded(t.Context(), "alice", models.MailRule{Name: "Offers", Enabled: true,
			Conditions: []models.MailRuleCondition{{Field: models.MailRuleFieldListID, Operator: models.MailRuleOpContains, Value: "offers.corner-shop.example"}},
			Actions:    []models.MailRuleAction{{Kind: models.MailRuleActionLabel, Label: "Shop"}, {Kind: models.MailRuleActionArchive}},
		}, nil)
		return err
	})
	events := f.worker.Events().Subscribe()
	defer f.worker.Events().Unsubscribe(events)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	for len(events) > 0 {
		<-events
	}

	offers := testJMAPEmail("e2", "This week's offers", 1, "mb-in")
	offers.ListID = "Corner Shop offers <offers.corner-shop.example>"
	server.addEmail(offers)
	server.addEmail(testJMAPEmail("e3", "Your order is ready", 2, "mb-in"))
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	var announced []Event
	for len(events) > 0 {
		if event := <-events; event.Type == EventNewMail {
			announced = append(announced, event)
		}
	}
	if len(announced) != 1 || announced[0].Payload["count"] != 1 || announced[0].Payload["subject"] != "Your order is ready" {
		t.Fatalf("new mail events = %+v, want the order alone", announced)
	}
	f.local(t, "alice", func(db *storage.DB) error {
		var folder string
		if err := db.Read().QueryRow(`SELECT s.folder_id FROM message_folder_state s JOIN messages m ON m.id = s.message_id WHERE m.subject = 'This week''s offers' AND s.is_deleted = 0`).Scan(&folder); err != nil {
			return err
		}
		if folder != jmapLocalFolderID(f.ids["alice"], "mb-arch") {
			t.Errorf("the new offers are in %q, want the Archive", folder)
		}
		runs, err := db.ListMailRuleRuns(t.Context(), "alice", 10)
		if err != nil {
			return err
		}
		if len(runs) != 1 || runs[0].Subject != "This week's offers" {
			t.Errorf("rule runs = %+v, want the new offers alone", runs)
		}
		return nil
	})

	// The next pass sends the label and the move.
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	email, _ := server.email("e2")
	if !email.MailboxIDs["mb-arch"] || email.MailboxIDs["mb-in"] || !email.Keywords["Shop"] {
		t.Fatalf("server copy of the new offers: mailboxes %v, keywords %v", email.MailboxIDs, email.Keywords)
	}
	if old, _ := server.email("e1"); !old.MailboxIDs["mb-in"] {
		t.Fatalf("the imported offers moved: %v", old.MailboxIDs)
	}
}

// Later syncs apply the changes since the saved cursor, chunk by chunk,
// without querying the whole account again: read and flag changes, moves,
// deletions and new mail.
func TestSyncAppliesJMAPEmailChanges(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.emailState = "c0"
	server.setMailboxes(
		jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()},
		jmap.Mailbox{ID: "mb-arch", Name: "Archive", Role: "archive", MyRights: readable()},
	)
	server.emails = []jmap.Email{
		testJMAPEmail("e1", "Lunch on Friday?", 1, "mb-in"),
		testJMAPEmail("e2", "Garden plans", 2, "mb-in"),
		testJMAPEmail("e3", "Old receipt", 3, "mb-arch"),
	}
	useFakeJMAPAccount(t, f, server)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("first Sync() error = %v", err)
	}
	if got := jmapCursor(t, f, jmapEmailScope); got != "c0" {
		t.Fatalf("cursor after import = %q", got)
	}

	server.oneChangePerCall = true
	server.updateEmail("e1", func(e *jmap.Email) { e.Keywords = map[string]bool{"$seen": true, "$flagged": true} })
	server.updateEmail("e2", func(e *jmap.Email) { e.MailboxIDs = map[string]bool{"mb-arch": true} })
	server.deleteEmail("e3", true)
	server.addEmail(testJMAPEmail("e4", "New invoice", 0, "mb-in"))
	queries := server.queries
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("Sync() with changes error = %v", err)
	}
	if server.queries != queries {
		t.Fatal("applying changes queried the whole account")
	}
	messages := jmapMessages(t, f)
	if m := messages["e1"]; m == nil || !m.Read || !m.Flagged {
		t.Fatalf("e1 after read and flag = %+v", m)
	}
	if m := messages["e2"]; m == nil || !m.Folders["mb-arch"] || m.Folders["mb-in"] {
		t.Fatalf("e2 after move = %+v", m)
	}
	if messages["e3"] != nil {
		t.Fatalf("e3 still shown after deletion: %+v", messages["e3"])
	}
	if m := messages["e4"]; m == nil || m.Read || !m.Folders["mb-in"] {
		t.Fatalf("new e4 = %+v", m)
	}
	if got := jmapCursor(t, f, jmapEmailScope); got != "c4" {
		t.Fatalf("cursor after changes = %q, want c4", got)
	}

	// When the server forgets the saved state, everything is imported again,
	// and a message deleted meanwhile disappears.
	server.deleteEmail("e4", false)
	server.forgetBefore = 10
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("Sync() after a forgotten state error = %v", err)
	}
	if server.queries == queries {
		t.Fatal("did not import again after cannotCalculateChanges")
	}
	messages = jmapMessages(t, f)
	if messages["e4"] != nil || messages["e1"] == nil || messages["e2"] == nil {
		t.Fatalf("after importing again: %v", messages)
	}
}

// Keywords other than status ones are labels, named through the same aliases
// as IMAP keywords, and a later change on the server adds or removes them.
func TestSyncStoresJMAPKeywordsAsLabels(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.emailState = "c0"
	server.setMailboxes(jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()})
	tagged := testJMAPEmail("e1", "Garden plans", 1, "mb-in")
	tagged.Keywords = map[string]bool{"$seen": true, "$label2": true, "Client": true, "client": true, "$forwarded": true, "Unset": false}
	server.emails = []jmap.Email{tagged, testJMAPEmail("e2", "Lunch on Friday?", 2, "mb-in")}
	useFakeJMAPAccount(t, f, server)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if got := jmapLabels(t, f, "e1"); got != "Client,Work" {
		t.Fatalf("e1 labels = %q, want Client and $label2 shown as Work", got)
	}
	if got := jmapLabels(t, f, "e2"); got != "" {
		t.Fatalf("e2 labels = %q, want none", got)
	}

	server.updateEmail("e1", func(e *jmap.Email) { e.Keywords = map[string]bool{"$seen": true, "Client": true} })
	server.updateEmail("e2", func(e *jmap.Email) { e.Keywords = map[string]bool{"Receipts": true} })
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("Sync() with changes error = %v", err)
	}
	if got := jmapLabels(t, f, "e1"); got != "Client" {
		t.Fatalf("e1 labels after removing Work = %q", got)
	}
	if got := jmapLabels(t, f, "e2"); got != "Receipts" {
		t.Fatalf("e2 labels after tagging = %q", got)
	}
}

// Label changes queued in Gofer reach the server as keywords on the next
// sync: an alias like Work becomes $label2, and removing a label clears the
// keyword in the casing the server has.
func TestSyncReplaysJMAPLabelChanges(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.emailState = "c0"
	server.setMailboxes(jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()})
	tagged := testJMAPEmail("e1", "Garden plans", 1, "mb-in")
	tagged.Keywords = map[string]bool{"Client": true}
	server.emails = []jmap.Email{tagged, testJMAPEmail("e2", "Lunch on Friday?", 2, "mb-in")}
	useFakeJMAPAccount(t, f, server)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("first Sync() error = %v", err)
	}
	queue := func(emailID, name, operation string) {
		t.Helper()
		f.local(t, "alice", func(db *storage.DB) error {
			var id int64
			if err := db.Read().QueryRow(`SELECT DISTINCT s.message_id FROM message_folder_state s JOIN folders f ON f.id=s.folder_id WHERE f.account_id=? AND s.remote_message_id=?`, f.ids["alice"], emailID).Scan(&id); err != nil {
				return err
			}
			info, err := db.GetMessageMutationInfoForUser(t.Context(), id, "alice")
			if err != nil {
				return err
			}
			return db.QueueUserLabels(t.Context(), "alice", []storage.ThreadMessageMutationInfo{{MessageID: id, MessageMutationInfo: *info}}, name, operation)
		})
	}
	queue("e1", "Work", storage.LabelMutationAdd)
	queue("e1", "client", storage.LabelMutationRemove)
	queue("e2", "Projects", storage.LabelMutationAdd)

	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("Sync() replaying labels error = %v", err)
	}
	if e1, _ := server.email("e1"); len(e1.Keywords) != 1 || !e1.Keywords["$label2"] {
		t.Fatalf("e1 keywords on the server = %v, want only $label2", e1.Keywords)
	}
	if e2, _ := server.email("e2"); len(e2.Keywords) != 1 || !e2.Keywords["Projects"] {
		t.Fatalf("e2 keywords on the server = %v", e2.Keywords)
	}
	if got := jmapLabels(t, f, "e1"); got != "Work" {
		t.Fatalf("e1 labels = %q", got)
	}
	if got := jmapLabels(t, f, "e2"); got != "Projects" {
		t.Fatalf("e2 labels = %q", got)
	}
	var pending int
	f.local(t, "alice", func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT COUNT(*) FROM label_mutation_queue`).Scan(&pending)
	})
	if pending != 0 {
		t.Fatalf("%d label changes still queued", pending)
	}
}

// After the first sync, mailboxes are read again only when more than their
// counters changed: new counters are saved on their own, and a new, renamed
// or deleted mailbox, or a state the server no longer knows, reads them all.
func TestSyncFollowsJMAPMailboxChanges(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.emailState = "c0"
	inbox := jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable(), TotalEmails: 1, UnreadEmails: 1}
	server.setMailboxes(inbox, jmap.Mailbox{ID: "mb-proj", Name: "Projects", MyRights: readable()})
	server.emails = []jmap.Email{testJMAPEmail("e1", "Garden plans", 1, "mb-in")}
	useFakeJMAPAccount(t, f, server)
	account := f.ids["alice"]
	type folderState struct {
		name          string
		total, unread int
		listed        bool
	}
	folder := func(mailboxID string) folderState {
		t.Helper()
		var got folderState
		f.local(t, "alice", func(db *storage.DB) error {
			return db.Read().QueryRow(`SELECT name, total_count, unread_count, COALESCE(discovery_state, 'active') = 'active' FROM folders WHERE id = ?`,
				jmapLocalFolderID(account, mailboxID)).Scan(&got.name, &got.total, &got.unread, &got.listed)
		})
		return got
	}
	sync := func(step string, fullGets int) {
		t.Helper()
		if err := f.worker.Sync(t.Context(), "alice", account); err != nil {
			t.Fatalf("%s: Sync() error = %v", step, err)
		}
		server.mu.Lock()
		got := server.fullMailboxGets
		server.mu.Unlock()
		if got != fullGets {
			t.Fatalf("%s: mailboxes read in full %d times, want %d", step, got, fullGets)
		}
	}
	sync("first sync", 1)
	sync("nothing changed", 1)

	server.setMailboxCounts("mb-in", 4, 2)
	sync("counters changed", 1)
	if got := folder("mb-in"); got.total != 4 || got.unread != 2 {
		t.Fatalf("Inbox after its counters changed = %+v, want 4 and 2 unread", got)
	}

	server.setMailboxes(inbox, jmap.Mailbox{ID: "mb-proj", Name: "Clients", MyRights: readable()})
	sync("mailbox renamed", 2)
	if got := folder("mb-proj"); got.name != "Clients" {
		t.Fatalf("renamed mailbox = %+v, want Clients", got)
	}
	server.setMailboxes(inbox, jmap.Mailbox{ID: "mb-proj", Name: "Clients", MyRights: readable()}, jmap.Mailbox{ID: "mb-new", Name: "Receipts", MyRights: readable()})
	sync("mailbox created", 3)
	if got := folder("mb-new"); got.name != "Receipts" || !got.listed {
		t.Fatalf("new mailbox = %+v, want Receipts listed", got)
	}
	server.setMailboxes(inbox, jmap.Mailbox{ID: "mb-new", Name: "Receipts", MyRights: readable()})
	sync("mailbox deleted", 4)
	if got := folder("mb-proj"); got.listed {
		t.Fatalf("deleted mailbox = %+v, want no longer listed", got)
	}

	server.mu.Lock()
	server.noMailboxChanges = true
	server.mu.Unlock()
	server.setMailboxCounts("mb-in", 5, 0)
	sync("changes unavailable", 5)
	if got := folder("mb-in"); got.total != 5 || got.unread != 0 {
		t.Fatalf("Inbox read in full = %+v, want 5 and none unread", got)
	}
}

// A label change made in Gofer reaches every email of the message, so a
// later change on the server to one of them leaves the label the other has.
func TestSyncReplaysJMAPLabelChangesToEveryCopy(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.emailState = "c0"
	server.setMailboxes(
		jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()},
		jmap.Mailbox{ID: "mb-sent", Name: "Sent", Role: "sent", MyRights: readable()},
	)
	sent := testJMAPEmail("sent-1", "Reminder to self", 2, "mb-sent")
	received := testJMAPEmail("in-1", "Reminder to self", 1, "mb-in")
	sent.MessageID = []string{"reminder@example.test"}
	received.MessageID = []string{"reminder@example.test"}
	sent.Keywords = map[string]bool{"Client": true}
	received.Keywords = map[string]bool{"Client": true, "Family": true}
	server.emails = []jmap.Email{sent, received}
	useFakeJMAPAccount(t, f, server)
	account := f.ids["alice"]
	if err := f.worker.Sync(t.Context(), "alice", account); err != nil {
		t.Fatalf("first Sync() error = %v", err)
	}
	if got := jmapLabels(t, f, "in-1"); got != "Client,Family" {
		t.Fatalf("labels after import = %q, want Client and Family", got)
	}
	for _, change := range []struct{ name, operation string }{{"Work", storage.LabelMutationAdd}, {"client", storage.LabelMutationRemove}} {
		f.local(t, "alice", func(db *storage.DB) error {
			var id int64
			if err := db.Read().QueryRow(`SELECT id FROM messages WHERE account_id=? AND internet_message_id='<reminder@example.test>'`, account).Scan(&id); err != nil {
				return err
			}
			info, err := db.GetMessageMutationInfoForUser(t.Context(), id, "alice")
			if err != nil {
				return err
			}
			return db.QueueUserLabels(t.Context(), "alice", []storage.ThreadMessageMutationInfo{{MessageID: id, MessageMutationInfo: *info}}, change.name, change.operation)
		})
	}
	if err := f.worker.Sync(t.Context(), "alice", account); err != nil {
		t.Fatalf("Sync() replaying labels error = %v", err)
	}
	for id, want := range map[string]map[string]bool{"sent-1": {"$label2": true}, "in-1": {"$label2": true, "Family": true}} {
		if email, _ := server.email(id); !maps.Equal(email.Keywords, want) {
			t.Fatalf("%s keywords on the server = %v, want %v", id, email.Keywords, want)
		}
	}
	if got := jmapLabels(t, f, "in-1"); got != "Family,Work" {
		t.Fatalf("labels after the changes = %q, want Family and Work", got)
	}

	server.updateEmail("in-1", func(e *jmap.Email) { e.Keywords = map[string]bool{"Family": true} })
	if err := f.worker.Sync(t.Context(), "alice", account); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if got := jmapLabels(t, f, "in-1"); got != "Family,Work" {
		t.Fatalf("labels after Work left the Inbox email = %q, want Work kept by the Sent one", got)
	}
	server.updateEmail("sent-1", func(e *jmap.Email) { e.Keywords = map[string]bool{} })
	if err := f.worker.Sync(t.Context(), "alice", account); err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if got := jmapLabels(t, f, "in-1"); got != "Family" {
		t.Fatalf("labels after Work left both emails = %q, want Family", got)
	}
}

// Marking a JMAP message as spam moves it into Junk and sends the training
// keywords, which clear the opposite one in whatever casing the server has it
// and never show as labels; not spam does the reverse.
func TestSyncSendsJMAPSpamKeywords(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.emailState = "c0"
	server.setMailboxes(
		jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()},
		jmap.Mailbox{ID: "mb-junk", Name: "Junk", Role: "junk", MyRights: readable()},
	)
	offer := testJMAPEmail("e1", "A great offer", 1, "mb-in")
	offer.Keywords = map[string]bool{"$NotJunk": true}
	server.emails = []jmap.Email{offer}
	useFakeJMAPAccount(t, f, server)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("first Sync() error = %v", err)
	}
	inbox, junk := jmapLocalFolderID(f.ids["alice"], "mb-in"), jmapLocalFolderID(f.ids["alice"], "mb-junk")
	mark := func(spam bool, from, to string) {
		t.Helper()
		f.local(t, "alice", func(db *storage.DB) error {
			var id int64
			if err := db.Read().QueryRow(`SELECT DISTINCT s.message_id FROM message_folder_state s JOIN folders f ON f.id=s.folder_id WHERE f.account_id=? AND s.remote_message_id='e1'`, f.ids["alice"]).Scan(&id); err != nil {
				return err
			}
			info, err := db.GetMessageMutationInfoForFolder(t.Context(), id, from)
			if err != nil {
				return err
			}
			return db.QueueUserIMAPSpam(t.Context(), "alice", []storage.ThreadMessageMutationInfo{{MessageID: id, MessageMutationInfo: *info}}, to, spam)
		})
		if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
			t.Fatalf("Sync() error = %v", err)
		}
	}
	mark(true, inbox, junk)
	if e1, _ := server.email("e1"); len(e1.Keywords) != 1 || !e1.Keywords["$junk"] || !e1.MailboxIDs["mb-junk"] || e1.MailboxIDs["mb-in"] {
		t.Fatalf("e1 after spam = keywords %v, mailboxes %v", e1.Keywords, e1.MailboxIDs)
	}
	mark(false, junk, inbox)
	if e1, _ := server.email("e1"); len(e1.Keywords) != 1 || !e1.Keywords["$notjunk"] || !e1.MailboxIDs["mb-in"] {
		t.Fatalf("e1 after not spam = keywords %v, mailboxes %v", e1.Keywords, e1.MailboxIDs)
	}
	if got := jmapLabels(t, f, "e1"); got != "" {
		t.Fatalf("spam keywords showed as labels %q", got)
	}
	var pending int
	f.local(t, "alice", func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT COUNT(*) FROM label_mutation_queue`).Scan(&pending)
	})
	if pending != 0 {
		t.Fatalf("%d label changes still queued", pending)
	}
}

// In single-user mode, the next sync sends the spam keywords the spam action
// queued, without showing them as labels.
func TestSharedSyncSendsJMAPSpamKeywords(t *testing.T) {
	t.Setenv("GOFER_JMAP_EXPERIMENT", "true")
	ctx := t.Context()
	server := newFakeJMAPServer(t)
	server.emailState = "c0"
	server.setMailboxes(jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()})
	offer := testJMAPEmail("e1", "A great offer", 1, "mb-in")
	offer.Keywords = map[string]bool{"$NotJunk": true}
	server.emails = []jmap.Email{offer}
	db := newLabelSyncTestDB(t)
	endpoint, err := jmap.ParseEndpoint(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddPlaintextTransportException(ctx, "http", endpoint.Host, endpoint.Port, "default"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddPrivateTargetException(ctx, "http", endpoint.Host, endpoint.Port, "default"); err != nil {
		t.Fatal(err)
	}
	accounts, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	account, err := accounts.CreateAccount(ctx, "default", &models.CreateAccountRequest{
		Provider: "jmap", EmailAddress: "robin@example.test", JMAPSessionURL: server.URL + "/.well-known/jmap",
		Username: "robin@example.test", Password: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	orchestrator := NewSyncOrchestrator(db, accounts, nil, nil)
	if err := orchestrator.syncAccount(ctx, account.ID, true); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.Read().QueryRow(`SELECT DISTINCT s.message_id FROM message_folder_state s JOIN folders f ON f.id=s.folder_id WHERE f.account_id=? AND s.remote_message_id='e1'`, account.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	inbox := jmapLocalFolderID(account.ID, "mb-in")
	for _, change := range [][2]string{{storage.LabelMutationAdd, "$junk"}, {storage.LabelMutationRemove, "$notjunk"}} {
		if err := db.ReplaceLabelMutation(ctx, account.ID, id, inbox, storage.LabelProviderKeyword, change[0], change[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := orchestrator.syncAccount(ctx, account.ID, true); err != nil {
		t.Fatal(err)
	}
	if e1, _ := server.email("e1"); len(e1.Keywords) != 1 || !e1.Keywords["$junk"] {
		t.Fatalf("e1 keywords on the server = %v", e1.Keywords)
	}
	var labels, pending int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_labels WHERE message_id=?`, id).Scan(&labels); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM label_mutation_queue`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if labels != 0 || pending != 0 {
		t.Fatalf("labels = %d, still queued = %d", labels, pending)
	}
}

// jmapLabels lists the names of an email's labels, sorted and joined by commas.
func jmapLabels(t *testing.T, f *userGmailFixture, emailID string) string {
	t.Helper()
	var names []string
	f.local(t, "alice", func(db *storage.DB) error {
		rows, err := db.Read().Query(`SELECT l.name FROM messages m JOIN message_labels ml ON ml.message_id = m.id
			JOIN labels l ON l.id = ml.label_id WHERE m.account_id = ? AND m.id IN (SELECT s.message_id FROM message_folder_state s WHERE s.remote_message_id = ?) ORDER BY l.name`, f.ids["alice"], emailID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return err
			}
			names = append(names, name)
		}
		return rows.Err()
	})
	return strings.Join(names, ",")
}

// Opening a JMAP email downloads its original message by blob and stores the
// parsed body like any other; an email over the download limit is refused
// from its size, before downloading anything.
func TestEnsureBodyDownloadsJMAPMessages(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.setMailboxes(jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()})
	small := testJMAPEmail("e1", "Lunch on Friday?", 1, "mb-in")
	small.Size = 180
	large := testJMAPEmail("e2", "Holiday photos", 2, "mb-in")
	large.Size = 50 << 20
	server.emails = []jmap.Email{small, large}
	server.blobs["blob-e1"] = "From: Sam Ortiz <sam@example.test>\r\nTo: robin@example.test\r\nSubject: Lunch on Friday?\r\n" +
		"Message-ID: <e1@example.test>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nAre you free around noon on Friday?\r\n"
	useFakeJMAPAccount(t, f, server)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	localID := func(providerID string) int64 {
		var id int64
		f.local(t, "alice", func(db *storage.DB) error {
			return db.Read().QueryRow(`SELECT DISTINCT s.message_id FROM message_folder_state s JOIN folders f ON f.id=s.folder_id WHERE f.account_id=? AND s.remote_message_id=?`, f.ids["alice"], providerID).Scan(&id)
		})
		return id
	}

	if err := f.worker.EnsureBody(t.Context(), "alice", localID("e1")); err != nil {
		t.Fatalf("EnsureBody() error = %v", err)
	}
	var textPath string
	f.local(t, "alice", func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT COALESCE(body_text_path,'') FROM messages WHERE id=?`, localID("e1")).Scan(&textPath)
	})
	if textPath == "" {
		t.Fatal("no body stored for the opened JMAP email")
	}

	downloads := server.downloads
	err := f.worker.EnsureBody(t.Context(), "alice", localID("e2"))
	var tooLarge *message.TooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("EnsureBody() of a large email = %v, want TooLargeError", err)
	}
	if server.downloads != downloads {
		t.Fatal("downloaded an email already known to be too large")
	}
}

// Changes made in Gofer reach the server on the next sync: read and starred
// set keywords, a move swaps mailboxes, a permanent delete destroys the email,
// and deleting an email the server already lost simply completes.
func TestSyncReplaysJMAPMessageChanges(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.emailState = "c0"
	server.setMailboxes(
		jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()},
		jmap.Mailbox{ID: "mb-arch", Name: "Archive", Role: "archive", MyRights: readable()},
	)
	server.emails = []jmap.Email{
		testJMAPEmail("e1", "Lunch on Friday?", 1, "mb-in"),
		testJMAPEmail("e2", "Garden plans", 2, "mb-in"),
		testJMAPEmail("e3", "Old receipt", 3, "mb-in"),
		testJMAPEmail("e4", "Expired coupon", 4, "mb-in"),
	}
	useFakeJMAPAccount(t, f, server)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("first Sync() error = %v", err)
	}
	account := f.ids["alice"]
	inbox, archive := jmapLocalFolderID(account, "mb-in"), jmapLocalFolderID(account, "mb-arch")
	local := func(providerID string) int64 {
		var id int64
		f.local(t, "alice", func(db *storage.DB) error {
			return db.Read().QueryRow(`SELECT DISTINCT s.message_id FROM message_folder_state s JOIN folders f ON f.id=s.folder_id WHERE f.account_id=? AND s.remote_message_id=?`, account, providerID).Scan(&id)
		})
		return id
	}
	server.deleteEmail("e4", true)
	f.local(t, "alice", func(db *storage.DB) error {
		ctx := t.Context()
		return errors.Join(
			db.SetMessageReadAndQueue(ctx, local("e1"), true),
			db.SetMessageStarredAndQueue(ctx, local("e1"), true),
			db.MoveMessageAndQueue(ctx, local("e2"), inbox, archive),
			db.PermanentlyDeleteMessageAndQueue(ctx, local("e3"), inbox),
			db.PermanentlyDeleteMessageAndQueue(ctx, local("e4"), inbox),
		)
	})
	var queued []string
	f.local(t, "alice", func(db *storage.DB) error {
		rows, err := db.Read().Query(`SELECT DISTINCT provider_type FROM message_mutations`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var provider string
			_ = rows.Scan(&provider)
			queued = append(queued, provider)
		}
		return rows.Err()
	})
	if len(queued) != 1 || queued[0] != "jmap" {
		t.Fatalf("queued provider types = %v, want [jmap]", queued)
	}

	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("Sync() replaying changes error = %v", err)
	}
	if e1, _ := server.email("e1"); !e1.Keywords["$seen"] || !e1.Keywords["$flagged"] {
		t.Fatalf("e1 on the server = %v", e1.Keywords)
	}
	if e2, _ := server.email("e2"); !e2.MailboxIDs["mb-arch"] || e2.MailboxIDs["mb-in"] {
		t.Fatalf("e2 on the server = %v", e2.MailboxIDs)
	}
	if _, ok := server.email("e3"); ok {
		t.Fatal("e3 still on the server after a permanent delete")
	}
	var pending int
	f.local(t, "alice", func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE status != 'applied'`).Scan(&pending)
	})
	if pending != 0 {
		t.Fatalf("%d changes still queued after replay", pending)
	}
	if m := jmapMessages(t, f)["e2"]; m == nil || !m.Folders["mb-arch"] || m.Folders["mb-in"] {
		t.Fatalf("e2 in Gofer after the move = %+v", m)
	}
}

// Marking a whole mailbox read reaches the server as one Email/set naming
// every email, not one request per email.
func TestSyncSendsAFolderMarkedReadToJMAPInOneRequest(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.emailState = "c0"
	server.setMailboxes(
		jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()},
		jmap.Mailbox{ID: "mb-arch", Name: "Archive", Role: "archive", MyRights: readable()},
	)
	subjects := []string{"Book club picks", "Bake sale rota", "Trail cleanup", "Choir schedule", "Seed swap"}
	for i, subject := range subjects {
		server.emails = append(server.emails, testJMAPEmail(fmt.Sprintf("e%d", i+1), subject, i+1, "mb-in"))
	}
	server.emails = append(server.emails, testJMAPEmail("kept", "Library hours", 9, "mb-arch"))
	useFakeJMAPAccount(t, f, server)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("first Sync() error = %v", err)
	}
	inbox := jmapLocalFolderID(f.ids["alice"], "mb-in")
	f.local(t, "alice", func(db *storage.DB) error {
		result, err := db.MarkFolderReadForUser(t.Context(), inbox, "alice", nil)
		if err == nil && result.Marked != len(subjects) {
			err = fmt.Errorf("marked %d, want %d", result.Marked, len(subjects))
		}
		return err
	})
	server.mu.Lock()
	server.emailSets = 0
	server.mu.Unlock()

	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("Sync() replaying the change error = %v", err)
	}
	server.mu.Lock()
	sets := server.emailSets
	server.mu.Unlock()
	if sets != 1 {
		t.Fatalf("Email/set calls = %d, want 1 for the whole mailbox", sets)
	}
	for i := range subjects {
		if email, _ := server.email(fmt.Sprintf("e%d", i+1)); !email.Keywords["$seen"] {
			t.Fatalf("e%d on the server = %v, want $seen", i+1, email.Keywords)
		}
	}
	if kept, _ := server.email("kept"); kept.Keywords["$seen"] {
		t.Fatal("the Archive email was marked read too")
	}
	var pending int
	f.local(t, "alice", func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE status != 'applied'`).Scan(&pending)
	})
	if pending != 0 {
		t.Fatalf("%d changes still queued after replay", pending)
	}
}

// Emptying a Trash mailbox reaches the server as one Email/set destroying
// every email, not one request per email.
func TestSyncSendsAnEmptiedJMAPTrashInOneRequest(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.emailState = "c0"
	server.setMailboxes(
		jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()},
		jmap.Mailbox{ID: "mb-trash", Name: "Trash", Role: "trash", MyRights: readable()},
	)
	subjects := []string{"Old newsletter", "Cancelled meetup", "Draft flyer", "Stale reminder"}
	for i, subject := range subjects {
		server.emails = append(server.emails, testJMAPEmail(fmt.Sprintf("t%d", i+1), subject, i+1, "mb-trash"))
	}
	server.emails = append(server.emails, testJMAPEmail("keep", "Choir rehearsal", 9, "mb-in"))
	useFakeJMAPAccount(t, f, server)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("first Sync() error = %v", err)
	}
	trash := jmapLocalFolderID(f.ids["alice"], "mb-trash")
	f.local(t, "alice", func(db *storage.DB) error {
		result, err := db.EmptyFolderForUser(t.Context(), trash, "alice", nil)
		if err == nil && result.Removed != len(subjects) {
			err = fmt.Errorf("removed %d, want %d", result.Removed, len(subjects))
		}
		return err
	})
	server.mu.Lock()
	server.emailSets = 0
	server.mu.Unlock()

	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("Sync() replaying the deletes error = %v", err)
	}
	server.mu.Lock()
	sets := server.emailSets
	server.mu.Unlock()
	if sets != 1 {
		t.Fatalf("Email/set calls = %d, want 1 for the whole mailbox", sets)
	}
	for i := range subjects {
		if _, ok := server.email(fmt.Sprintf("t%d", i+1)); ok {
			t.Fatalf("t%d is still on the server", i+1)
		}
	}
	if _, ok := server.email("keep"); !ok {
		t.Fatal("the Inbox email was deleted too")
	}
	var pending int
	f.local(t, "alice", func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations WHERE status NOT IN ('applied')`).Scan(&pending)
	})
	if pending != 0 {
		t.Fatalf("%d deletes still queued after replay", pending)
	}
}

// jmapCopies maps each mailbox an account's message is shown in to the id of
// the email it is there as.
func jmapCopies(t *testing.T, f *userGmailFixture, internetMessageID string) map[string]string {
	t.Helper()
	copies := map[string]string{}
	f.local(t, "alice", func(db *storage.DB) error {
		rows, err := db.Read().Query(`SELECT f.provider_remote_id, COALESCE(mfs.remote_message_id, '')
			FROM messages m JOIN message_folder_state mfs ON mfs.message_id = m.id AND mfs.is_deleted = 0
			JOIN folders f ON f.id = mfs.folder_id WHERE m.account_id = ? AND m.internet_message_id = ?`, f.ids["alice"], internetMessageID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var mailbox, id string
			if err := rows.Scan(&mailbox, &id); err != nil {
				return err
			}
			copies[mailbox] = id
		}
		return rows.Err()
	})
	return copies
}

// Mail sent to oneself is two emails with one Message-ID, one in Sent and one
// in the Inbox. Both stay where they are as either changes, and deleting one
// leaves the other.
func TestSyncKeepsBothJMAPCopiesOfMailSentToOneself(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.emailState = "c0"
	server.setMailboxes(
		jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()},
		jmap.Mailbox{ID: "mb-sent", Name: "Sent", Role: "sent", MyRights: readable()},
	)
	sent := testJMAPEmail("sent-1", "Reminder to self", 2, "mb-sent")
	received := testJMAPEmail("in-1", "Reminder to self", 1, "mb-in")
	sent.MessageID = []string{"reminder@example.test"}
	received.MessageID = []string{"reminder@example.test"}
	server.emails = []jmap.Email{sent, received}
	useFakeJMAPAccount(t, f, server)
	const internetID = "<reminder@example.test>"
	want := map[string]string{"mb-sent": "sent-1", "mb-in": "in-1"}
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("first Sync() error = %v", err)
	}
	if got := jmapCopies(t, f, internetID); !maps.Equal(got, want) {
		t.Fatalf("copies after import = %v, want %v", got, want)
	}

	server.updateEmail("sent-1", func(e *jmap.Email) { e.Keywords = map[string]bool{"$seen": true} })
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("Sync() after reading the Sent copy error = %v", err)
	}
	if got := jmapCopies(t, f, internetID); !maps.Equal(got, want) {
		t.Fatalf("copies after the Sent copy changed = %v, want %v", got, want)
	}

	// Importing everything again keeps both.
	server.forgetBefore = 10
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("Sync() after a forgotten state error = %v", err)
	}
	if got := jmapCopies(t, f, internetID); !maps.Equal(got, want) {
		t.Fatalf("copies after importing again = %v, want %v", got, want)
	}

	server.forgetBefore = 0
	server.deleteEmail("in-1", true)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("Sync() after deleting the Inbox copy error = %v", err)
	}
	if got := jmapCopies(t, f, internetID); !maps.Equal(got, map[string]string{"mb-sent": "sent-1"}) {
		t.Fatalf("copies after deleting the Inbox copy = %v, want only Sent", got)
	}
}

// Reading mail sent to oneself marks both its emails read, and deleting the
// Inbox copy destroys only that email.
func TestSyncReplaysJMAPChangesOnEachCopy(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.emailState = "c0"
	server.setMailboxes(
		jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()},
		jmap.Mailbox{ID: "mb-sent", Name: "Sent", Role: "sent", MyRights: readable()},
	)
	sent := testJMAPEmail("sent-1", "Reminder to self", 2, "mb-sent")
	received := testJMAPEmail("in-1", "Reminder to self", 1, "mb-in")
	sent.MessageID = []string{"reminder@example.test"}
	received.MessageID = []string{"reminder@example.test"}
	server.emails = []jmap.Email{sent, received}
	useFakeJMAPAccount(t, f, server)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatalf("first Sync() error = %v", err)
	}
	account := f.ids["alice"]
	var msgID int64
	f.local(t, "alice", func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT id FROM messages WHERE account_id=? AND internet_message_id='<reminder@example.test>'`, account).Scan(&msgID)
	})
	f.local(t, "alice", func(db *storage.DB) error { return db.SetMessageReadAndQueue(t.Context(), msgID, true) })
	if err := f.worker.Sync(t.Context(), "alice", account); err != nil {
		t.Fatalf("Sync() replaying read error = %v", err)
	}
	for _, id := range []string{"sent-1", "in-1"} {
		if email, _ := server.email(id); !email.Keywords["$seen"] {
			t.Fatalf("%s on the server = %v, want $seen", id, email.Keywords)
		}
	}

	f.local(t, "alice", func(db *storage.DB) error {
		return db.PermanentlyDeleteMessageAndQueue(t.Context(), msgID, jmapLocalFolderID(account, "mb-in"))
	})
	if err := f.worker.Sync(t.Context(), "alice", account); err != nil {
		t.Fatalf("Sync() replaying delete error = %v", err)
	}
	if _, ok := server.email("in-1"); ok {
		t.Fatal("in-1 still on the server after deleting the Inbox copy")
	}
	if _, ok := server.email("sent-1"); !ok {
		t.Fatal("deleting the Inbox copy destroyed the Sent one")
	}
	if got := jmapCopies(t, f, "<reminder@example.test>"); !maps.Equal(got, map[string]string{"mb-sent": "sent-1"}) {
		t.Fatalf("copies after the delete = %v, want only Sent", got)
	}
}

// Deleting both copies of mail sent to oneself before Gofer syncs destroys
// both emails.
func TestSyncReplaysJMAPDeletesOfBothCopies(t *testing.T) {
	f := newUserGmailFixture(t)
	server := newFakeJMAPServer(t)
	server.emailState = "c0"
	server.setMailboxes(
		jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()},
		jmap.Mailbox{ID: "mb-sent", Name: "Sent", Role: "sent", MyRights: readable()},
	)
	sent := testJMAPEmail("sent-1", "Reminder to self", 2, "mb-sent")
	received := testJMAPEmail("in-1", "Reminder to self", 1, "mb-in")
	sent.MessageID = []string{"reminder@example.test"}
	received.MessageID = []string{"reminder@example.test"}
	server.emails = []jmap.Email{sent, received}
	useFakeJMAPAccount(t, f, server)
	account := f.ids["alice"]
	if err := f.worker.Sync(t.Context(), "alice", account); err != nil {
		t.Fatalf("first Sync() error = %v", err)
	}
	var msgID int64
	f.local(t, "alice", func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT id FROM messages WHERE account_id=? AND internet_message_id='<reminder@example.test>'`, account).Scan(&msgID)
	})
	f.local(t, "alice", func(db *storage.DB) error {
		return errors.Join(
			db.PermanentlyDeleteMessageAndQueue(t.Context(), msgID, jmapLocalFolderID(account, "mb-sent")),
			db.PermanentlyDeleteMessageAndQueue(t.Context(), msgID, jmapLocalFolderID(account, "mb-in")),
		)
	})
	if err := f.worker.Sync(t.Context(), "alice", account); err != nil {
		t.Fatalf("Sync() replaying deletes error = %v", err)
	}
	for _, id := range []string{"sent-1", "in-1"} {
		if _, ok := server.email(id); ok {
			t.Fatalf("%s still on the server after deleting both copies", id)
		}
	}
	if got := jmapCopies(t, f, "<reminder@example.test>"); len(got) != 0 {
		t.Fatalf("copies after both deletes = %v", got)
	}
}

// Both copies of mail sent to oneself can end up in one mailbox, such as
// Trash. Gofer shows the message there once, and changing it there reaches
// both emails.
func TestSyncReplaysJMAPChangesToBothCopiesInOneMailbox(t *testing.T) {
	inTrash := map[string][]string{"sent-1": {"mb-trash"}, "in-1": {"mb-trash"}}
	for _, tc := range []struct {
		name    string
		change  func(db *storage.DB, msgID int64, account string) error
		want    map[string][]string // email id -> mailboxes; absent is destroyed
		shown   []string            // mailboxes showing the message afterwards
		keyword string              // set on both emails afterwards
	}{
		{"read", func(db *storage.DB, msgID int64, account string) error {
			return db.SetMessageReadAndQueue(t.Context(), msgID, true)
		}, inTrash, []string{"mb-trash"}, "$seen"},
		{"starred", func(db *storage.DB, msgID int64, account string) error {
			return db.SetMessageStarredAndQueue(t.Context(), msgID, true)
		}, inTrash, []string{"mb-trash"}, "$flagged"},
		{"delete", func(db *storage.DB, msgID int64, account string) error {
			return db.PermanentlyDeleteMessageAndQueue(t.Context(), msgID, jmapLocalFolderID(account, "mb-trash"))
		}, map[string][]string{}, nil, ""},
		{"move", func(db *storage.DB, msgID int64, account string) error {
			return db.MoveMessageAndQueue(t.Context(), msgID, jmapLocalFolderID(account, "mb-trash"), jmapLocalFolderID(account, "mb-in"))
		}, map[string][]string{"sent-1": {"mb-in"}, "in-1": {"mb-in"}}, []string{"mb-in"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUserGmailFixture(t)
			server := newFakeJMAPServer(t)
			server.emailState = "c0"
			server.setMailboxes(
				jmap.Mailbox{ID: "mb-in", Name: "Inbox", Role: "inbox", MyRights: readable()},
				jmap.Mailbox{ID: "mb-sent", Name: "Sent", Role: "sent", MyRights: readable()},
				jmap.Mailbox{ID: "mb-trash", Name: "Trash", Role: "trash", MyRights: readable()},
			)
			sent := testJMAPEmail("sent-1", "Reminder to self", 2, "mb-trash")
			received := testJMAPEmail("in-1", "Reminder to self", 1, "mb-trash")
			sent.MessageID = []string{"reminder@example.test"}
			received.MessageID = []string{"reminder@example.test"}
			server.emails = []jmap.Email{sent, received}
			useFakeJMAPAccount(t, f, server)
			account := f.ids["alice"]
			if err := f.worker.Sync(t.Context(), "alice", account); err != nil {
				t.Fatalf("first Sync() error = %v", err)
			}
			if got := jmapCopies(t, f, "<reminder@example.test>"); len(got) != 1 || got["mb-trash"] == "" {
				t.Fatalf("copies after import = %v, want one in Trash", got)
			}
			var msgID int64
			f.local(t, "alice", func(db *storage.DB) error {
				return db.Read().QueryRow(`SELECT id FROM messages WHERE account_id=? AND internet_message_id='<reminder@example.test>'`, account).Scan(&msgID)
			})
			f.local(t, "alice", func(db *storage.DB) error { return tc.change(db, msgID, account) })
			if err := f.worker.Sync(t.Context(), "alice", account); err != nil {
				t.Fatalf("Sync() replaying %s error = %v", tc.name, err)
			}
			for _, id := range []string{"sent-1", "in-1"} {
				email, ok := server.email(id)
				mailboxes, want := tc.want[id]
				if ok != want {
					t.Fatalf("%s on the server = %v, want present %v", id, ok, want)
				}
				for _, mailbox := range mailboxes {
					if !email.MailboxIDs[mailbox] || len(email.MailboxIDs) != len(mailboxes) {
						t.Fatalf("%s mailboxes = %v, want %v", id, email.MailboxIDs, mailboxes)
					}
				}
				if tc.keyword != "" && !email.Keywords[tc.keyword] {
					t.Fatalf("%s keywords = %v, want %s", id, email.Keywords, tc.keyword)
				}
			}
			got := jmapCopies(t, f, "<reminder@example.test>")
			if len(got) != len(tc.shown) {
				t.Fatalf("copies after the %s = %v, want one in each of %v", tc.name, got, tc.shown)
			}
			for _, mailbox := range tc.shown {
				var others int
				f.local(t, "alice", func(db *storage.DB) error {
					return db.Read().QueryRow(`SELECT COUNT(*) FROM message_folder_copies WHERE message_id = ? AND folder_id = ? AND remote_message_id != ?`,
						msgID, jmapLocalFolderID(account, mailbox), got[mailbox]).Scan(&others)
				})
				if got[mailbox] == "" || others != 1 {
					t.Fatalf("%s after the %s shows %q with %d further copies, want one of each", mailbox, tc.name, got[mailbox], others)
				}
			}
		})
	}
}
