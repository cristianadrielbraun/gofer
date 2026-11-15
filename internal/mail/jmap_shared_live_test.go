package mail

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// TestLiveJMAPSharedMailboxes runs against a real JMAP server whose login
// has at least one mailbox shared with it, such as a Stalwart test account
// in a group. It is skipped unless GOFER_LIVE_JMAP_HOST,
// GOFER_LIVE_JMAP_USER and GOFER_LIVE_JMAP_PASSWORD are set. It syncs the
// account, saves and removes a draft in a shared mailbox, sends one message
// from the shared mailbox to the login's own address, moves the received
// copy into the shared mailbox and back, and removes what it made.
func TestLiveJMAPSharedMailboxes(t *testing.T) {
	host, user, password := os.Getenv("GOFER_LIVE_JMAP_HOST"), os.Getenv("GOFER_LIVE_JMAP_USER"), os.Getenv("GOFER_LIVE_JMAP_PASSWORD")
	if host == "" || user == "" || password == "" {
		t.Skip("set GOFER_LIVE_JMAP_HOST, GOFER_LIVE_JMAP_USER and GOFER_LIVE_JMAP_PASSWORD to run against a real JMAP server")
	}
	ctx := t.Context()
	f := newUserGmailFixture(t)
	sessionURL := "https://" + host + "/.well-known/jmap"
	cfg := &models.AccountConfig{Provider: "jmap", JMAPSessionURL: sessionURL, Username: user}
	if os.Getenv("HTTPS_PROXY") != "" {
		// Where outside hosts are reached only through a proxy, as in a
		// sandbox, a local forwarder carries the requests there.
		forwarder := liveJMAPForwarder(t, host)
		useFakeJMAPAccount(t, f, &fakeJMAPServer{Server: forwarder})
		sessionURL = forwarder.URL + "/.well-known/jmap"
		cfg.JMAPSessionURL, cfg.JMAPAllowPlaintext, cfg.JMAPAllowPrivate = sessionURL, true, true
	}
	if err := f.worker.accounts.UpdateAccount(ctx, "alice", f.ids["alice"], &models.CreateAccountRequest{
		Provider: "jmap", JMAPSessionURL: sessionURL, Username: user, Password: password,
	}); err != nil {
		t.Fatal(err)
	}
	sync := func() {
		t.Helper()
		if err := f.worker.Sync(ctx, "alice", f.ids["alice"]); err != nil {
			t.Fatalf("Sync() error = %v", err)
		}
	}
	sync()

	// The shared mailboxes, their folders and their identities.
	client, session, err := openJMAPSession(ctx, cfg, password)
	if err != nil {
		t.Fatal(err)
	}
	targets := jmapSharedAccounts(session)
	if len(targets) == 0 {
		t.Fatal("the login has no shared mailbox to test with")
	}
	shared := targets[0]
	t.Logf("shared mailboxes: %d, testing with %s (JMAP account %s)", len(targets), shared.mailbox, shared.account)
	var sentFolder, inboxFolder string
	var identities []models.Identity
	f.local(t, "alice", func(db *storage.DB) error {
		var err error
		if sentFolder, _, err = db.SentFolderFor(ctx, f.ids["alice"], shared.mailbox); err != nil {
			return err
		}
		if inboxFolder, _, _, err = db.SharedMailboxFolder(ctx, f.ids["alice"], shared.mailbox, "inbox"); err != nil {
			return err
		}
		identities, err = db.ListAccountIdentitiesInternal(ctx, f.ids["alice"])
		return err
	})
	var sentRemote string
	f.local(t, "alice", func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT provider_remote_id FROM folders WHERE id = ?`, sentFolder).Scan(&sentRemote)
	})
	if sentFolder == "" || inboxFolder == "" || !strings.HasPrefix(sentRemote, shared.account+":") {
		t.Fatalf("shared Sent %q (%q), shared Inbox %q", sentFolder, sentRemote, inboxFolder)
	}
	byEmail := map[string][]models.Identity{}
	for _, identity := range identities {
		byEmail[strings.ToLower(identity.Email)] = append(byEmail[strings.ToLower(identity.Email)], identity)
	}
	for _, target := range targets {
		got := byEmail[target.mailbox]
		if len(got) != 1 || got[0].SharedMailbox != target.mailbox {
			t.Fatalf("identities for %s = %+v", target.mailbox, got)
		}
	}
	// Each shared mailbox takes the name the server gives its sender
	// identity, as Stalwart a group's description.
	var registered []storage.SharedMailbox
	f.local(t, "alice", func(db *storage.DB) error {
		var err error
		registered, err = db.ListSharedMailboxes(ctx, f.ids["alice"])
		return err
	})
	for _, m := range registered {
		t.Logf("shared mailbox %s: server name %q, identity %q", m.Email, m.ServerName, byEmail[m.Email][0].Name)
		if m.ServerName != "" && byEmail[m.Email][0].Name != m.ServerName {
			t.Fatalf("identity of %s is named %q, not the server's %q", m.Email, byEmail[m.Email][0].Name, m.ServerName)
		}
	}

	// A draft from the shared address is kept in the shared mailbox.
	stamp := time.Now().UTC().Format("20060102T150405.000000000")
	drafts := NewJMAPDrafts(cfg, password)
	draftMIME := fmt.Sprintf("From: Gofer Live Test <%s>\r\nTo: %s\r\nSubject: Gofer JMAP shared draft %s\r\nMessage-ID: <draft-%s@gofer.test>\r\nX-Gofer-Draft-Revision: r1\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nA draft.\r\n",
		shared.mailbox, user, stamp, stamp)
	draft, err := drafts.SaveDraft(ctx, "", []byte(draftMIME))
	if err != nil {
		t.Fatalf("SaveDraft() error = %v", err)
	}
	if !strings.HasPrefix(draft.ID, shared.account+":") {
		t.Fatalf("draft %q was not saved in the shared mailbox", draft.ID)
	}
	if found, err := drafts.FindDrafts(ctx, draft.InternetMessageID); err != nil || len(found) != 1 || found[0].ID != draft.ID {
		t.Fatalf("FindDrafts() = %+v, %v", found, err)
	}
	if err := drafts.DeleteDraft(ctx, draft); err != nil {
		t.Fatalf("DeleteDraft() error = %v", err)
	}

	// Sent from the shared address, the message is stored in the shared
	// mailbox's Sent and reaches the login's own Inbox.
	messageID := "sent-" + stamp + "@gofer.test"
	raw := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: Gofer JMAP shared send %s\r\nMessage-ID: <%s>\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nSent by Gofer's live test; it removes this message.\r\n",
		shared.mailbox, user, stamp, messageID, time.Now().Format(time.RFC1123Z))
	sentID, err := SendJMAPMessage(ctx, cfg, password, shared.mailbox, []string{user}, []byte(raw), time.Time{})
	if err != nil {
		t.Fatalf("SendJMAPMessage() error = %v", err)
	}
	if !strings.HasPrefix(sentID, shared.account+":") {
		t.Fatalf("sent copy %q is not in the shared mailbox", sentID)
	}
	t.Cleanup(func() {
		if _, err := ApplyJMAPMutation(context.Background(), cfg, password, JMAPMutation{Kind: storage.MessageMutationDelete, EmailID: sentID}); err != nil {
			t.Logf("remove the sent copy: %v", err)
		}
	})
	own := session.PrimaryAccount("urn:ietf:params:jmap:mail")
	ownMailboxes, err := client.GetMailboxes(ctx, session, own, nil)
	if err != nil {
		t.Fatal(err)
	}
	ownInboxID := ""
	for _, mailbox := range ownMailboxes.List {
		if strings.EqualFold(mailbox.Role, "inbox") {
			ownInboxID = mailbox.ID
		}
	}
	var received string
	for i := 0; i < 20 && received == ""; i++ {
		time.Sleep(time.Second)
		ids, err := client.FindEmails(ctx, session, own, ownInboxID, messageID, []jmap.Window{{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour)}})
		if err == nil {
			for _, id := range ids {
				if id != "" {
					received = id
				}
			}
		}
	}
	if received == "" {
		t.Fatal("the message did not reach the login's own mailbox")
	}
	t.Cleanup(func() {
		if _, err := ApplyJMAPMutation(context.Background(), cfg, password, JMAPMutation{Kind: storage.MessageMutationDelete, EmailID: received}); err != nil {
			t.Logf("remove the received copy: %v", err)
		}
	})
	sync()
	var sentFolderOfCopy string
	f.local(t, "alice", func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT mfs.folder_id FROM message_folder_state mfs WHERE mfs.remote_message_id = ? AND mfs.is_deleted = 0`, sentID).Scan(&sentFolderOfCopy)
	})
	if sentFolderOfCopy != sentFolder {
		t.Fatalf("the sent copy is in folder %q, want the shared Sent %q", sentFolderOfCopy, sentFolder)
	}

	// Moved into the shared mailbox's Inbox and back, across JMAP accounts.
	var ownInbox, ownInboxRemote, sharedInboxRemote string
	f.local(t, "alice", func(db *storage.DB) error {
		if err := db.Read().QueryRow(`SELECT id, provider_remote_id FROM folders WHERE account_id = ? AND role = 'inbox' AND shared_mailbox = ''`, f.ids["alice"]).Scan(&ownInbox, &ownInboxRemote); err != nil {
			return err
		}
		return db.Read().QueryRow(`SELECT provider_remote_id FROM folders WHERE id = ?`, inboxFolder).Scan(&sharedInboxRemote)
	})
	gone, moved, err := ApplyJMAPMutationMoving(ctx, cfg, password, JMAPMutation{Kind: storage.MessageMutationMove, EmailID: received, FromMailboxID: ownInboxRemote, ToMailboxID: sharedInboxRemote})
	if err != nil || gone || !strings.HasPrefix(moved, shared.account+":") {
		t.Fatalf("move into the shared mailbox = %v %q %v", gone, moved, err)
	}
	received = moved
	gone, back, err := ApplyJMAPMutationMoving(ctx, cfg, password, JMAPMutation{Kind: storage.MessageMutationMove, EmailID: moved, FromMailboxID: sharedInboxRemote, ToMailboxID: ownInboxRemote})
	if err != nil || gone || back == "" || strings.Contains(back, ":") {
		t.Fatalf("move back = %v %q %v", gone, back, err)
	}
	received = back
	// The shared mailbox's own sent copy, the same message, stays in Sent.
	_, sentEmail := jmapSplitRef(sentID)
	still, err := client.GetEmails(ctx, session, shared.account, []string{sentEmail}, []string{"mailboxIds"})
	if err != nil || len(still.List) != 1 {
		t.Fatalf("the shared mailbox's sent copy after the moves = %+v, %v", still, err)
	}
	// The shared mailbox's calendars are listed with the login's own, and an
	// event saved in one goes to its account.
	calendars, err := JMAPCalendars(ctx, cfg, password)
	if err != nil {
		t.Fatalf("JMAPCalendars() error = %v", err)
	}
	sharedCalendar := ""
	for _, calendar := range calendars {
		if strings.HasPrefix(calendar.ID, shared.account+":") {
			sharedCalendar = calendar.ID
		}
	}
	if sharedCalendar == "" {
		t.Fatalf("no calendar of %s among %d calendars", shared.mailbox, len(calendars))
	}
	eventID, err := CreateJMAPCalendarEvent(ctx, cfg, password, sharedCalendar, jmap.CalendarEvent{
		"@type": "Event", "uid": "live-" + stamp + "@gofer.test", "title": "Gofer shared calendar test", "start": "2030-01-02T10:00:00", "timeZone": "Etc/UTC", "duration": "PT1H",
	})
	if err != nil || !strings.HasPrefix(eventID, shared.account+":") {
		t.Fatalf("CreateJMAPCalendarEvent() = %q, %v", eventID, err)
	}
	pulled, err := PullJMAPCalendarEvents(ctx, cfg, password, sharedCalendar, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("PullJMAPCalendarEvents() error = %v", err)
	}
	found := false
	for _, event := range pulled.Events {
		found = found || event.ID() == eventID
	}
	if !found {
		t.Fatalf("the new event is not among the shared calendar's %d events", len(pulled.Events))
	}
	if err := DeleteJMAPCalendarEvent(ctx, cfg, password, eventID); err != nil {
		t.Fatalf("DeleteJMAPCalendarEvent() error = %v", err)
	}
	// Its address book too: a contact saved there is read back from it.
	books, err := JMAPAddressBooks(ctx, cfg, password)
	if err != nil {
		t.Fatalf("JMAPAddressBooks() error = %v", err)
	}
	sharedBook := ""
	for _, book := range books {
		if strings.HasPrefix(book.ID, shared.account+":") {
			sharedBook = book.ID
		}
	}
	if sharedBook == "" {
		t.Fatalf("no address book of %s among %d", shared.mailbox, len(books))
	}
	cardID, err := PushJMAPContact(ctx, cfg, password, sharedBook, "", "live-"+stamp+"@gofer.test", models.Contact{Name: "Gofer Live Test", Email: "live-" + stamp + "@example.com"})
	if err != nil || !strings.HasPrefix(cardID, shared.account+":") {
		t.Fatalf("PushJMAPContact() = %q, %v", cardID, err)
	}
	t.Cleanup(func() {
		if err := DeleteJMAPContact(context.Background(), cfg, password, cardID); err != nil {
			t.Logf("remove the test contact: %v", err)
		}
	})
	page, err := PullJMAPContacts(ctx, cfg, password, "", []string{sharedBook})
	if err != nil {
		t.Fatalf("PullJMAPContacts() error = %v", err)
	}
	found = false
	for _, contact := range page.Contacts {
		found = found || (contact.ID == cardID && contact.AddressBookID == sharedBook)
	}
	if !found || !strings.HasPrefix(page.State, "{") {
		t.Fatalf("the test contact is not among %d pulled; state %q", len(page.Contacts), page.State)
	}
	if again, err := PullJMAPContacts(ctx, cfg, password, page.State, []string{sharedBook}); err != nil || again.Full {
		t.Fatalf("a second pull = full %v, %v", again != nil && again.Full, err)
	}
	t.Logf("draft, send (copy in %s's Sent), moves across accounts, its calendar and its address book all worked", shared.mailbox)
}

// liveJMAPForwarder serves, on a local address, what host serves, reaching it
// through the environment's proxy, and points the server's own URLs back at
// itself.
func liveJMAPForwarder(t *testing.T, host string) *httptest.Server {
	t.Helper()
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out, err := http.NewRequestWithContext(r.Context(), r.Method, "https://"+host+r.URL.RequestURI(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		out.Header = r.Header.Clone()
		resp, err := transport.RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		origin := "https://" + host
		if strings.Contains(resp.Header.Get("Content-Type"), "json") {
			body = []byte(strings.ReplaceAll(string(body), origin, server.URL))
		}
		for name, values := range resp.Header {
			if strings.EqualFold(name, "Content-Length") {
				continue
			}
			for _, value := range values {
				w.Header().Add(name, strings.ReplaceAll(value, origin, server.URL))
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server
}
