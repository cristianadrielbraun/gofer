package notifications

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/handler"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// SMTP is real TCP, with acceptance and loss of final acknowledgement under
// test control. It records raw accepted bytes, rather than mocking SendRaw.
type routedSMTPServer struct {
	listener         net.Listener
	mu               sync.Mutex
	wg               sync.WaitGroup
	connections      map[net.Conn]bool
	accepted         map[string][]string
	acceptedAt       map[string][]time.Time
	loseAck          bool
	rejectSender     int
	blockOwner       string
	blocked, release chan struct{}
	blockOnce        sync.Once
	done             chan struct{}
}

func newRoutedSMTPServer(t *testing.T) *routedSMTPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &routedSMTPServer{listener: listener, connections: make(map[net.Conn]bool), accepted: make(map[string][]string), acceptedAt: make(map[string][]time.Time), done: make(chan struct{})}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			select {
			case <-s.done:
				s.mu.Unlock()
				conn.Close()
				return
			default:
			}
			s.connections[conn] = true
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer conn.Close()
				s.serve(conn)
				s.mu.Lock()
				delete(s.connections, conn)
				s.mu.Unlock()
			}()
		}
	}()
	t.Cleanup(func() {
		close(s.done)
		listener.Close()
		s.mu.Lock()
		for conn := range s.connections {
			conn.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	})
	return s
}
func (s *routedSMTPServer) serve(conn net.Conn) {
	r, w := bufio.NewReader(conn), bufio.NewWriter(conn)
	fmt.Fprint(w, "220 localhost SMTP ready\r\n")
	w.Flush()
	owner := ""
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			fmt.Fprint(w, "250-localhost\r\n250-AUTH PLAIN\r\n250 SIZE 67108864\r\n")
		case strings.HasPrefix(upper, "AUTH "):
			fmt.Fprint(w, "235 authenticated\r\n")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			address := strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
			address = strings.Trim(strings.Fields(address)[0], "<>")
			owner = strings.Split(address, "@")[0]
			s.mu.Lock()
			reject := s.rejectSender
			s.mu.Unlock()
			if reject > 0 {
				fmt.Fprintf(w, "%d rejected sender\r\n", reject)
			} else {
				fmt.Fprint(w, "250 sender ok\r\n")
			}
		case strings.HasPrefix(upper, "RCPT TO:"):
			fmt.Fprint(w, "250 recipient ok\r\n")
		case strings.HasPrefix(upper, "DATA"):
			fmt.Fprint(w, "354 send data\r\n")
			w.Flush()
			raw, err := io.ReadAll(textproto.NewReader(r).DotReader())
			if err != nil {
				return
			}
			s.mu.Lock()
			s.accepted[owner] = append(s.accepted[owner], string(raw))
			s.acceptedAt[owner] = append(s.acceptedAt[owner], time.Now())
			lose := s.loseAck
			s.loseAck = false
			block := s.blockOwner == owner
			blocked, release := s.blocked, s.release
			if block {
				s.blockOnce.Do(func() { close(blocked) })
			}
			s.mu.Unlock()
			if block {
				select {
				case <-release:
				case <-s.done:
					return
				}
			}
			if lose {
				return
			}
			fmt.Fprint(w, "250 accepted\r\n")
		case strings.HasPrefix(upper, "QUIT"):
			fmt.Fprint(w, "221 bye\r\n")
			w.Flush()
			return
		case strings.HasPrefix(upper, "RSET"):
			fmt.Fprint(w, "250 reset\r\n")
		default:
			fmt.Fprint(w, "500 unexpected command\r\n")
		}
		if w.Flush() != nil {
			return
		}
	}
}
func (s *routedSMTPServer) count(owner string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.accepted[owner])
}

func newUserComposeFixture(t *testing.T) (*userStorageFixture, *routedIMAPServer, *routedSMTPServer) {
	t.Helper()
	f := newUserStorageFixtureMode(t, true)
	imap := newRoutedIMAPServer(t)
	smtp := newRoutedSMTPServer(t)
	imap.mu.Lock()
	imap.mutationMode = true
	imap.deliveryMode = true
	imap.mu.Unlock()
	f.useIMAPServer(t, imap)
	host, portText, _ := net.SplitHostPort(smtp.listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	if err := f.system.AddPlaintextTransportException(t.Context(), "smtp", host, port, "alice"); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.accountStore.UpdateAccount(t.Context(), owner, f.accounts[owner].ID, &models.CreateAccountRequest{Provider: "imap", AuthMethod: "plain", SMTPHost: host, SMTPPort: port, SMTPTLSMode: "plaintext"}); err != nil {
			t.Fatal(err)
		}
		if err := f.imap.Sync(t.Context(), owner, f.accounts[owner].ID); err != nil {
			t.Fatal(err)
		}
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE accounts SET email_sync_enabled=0 WHERE id=?`, f.accounts[owner].ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, imap, smtp
}
func (f *userStorageFixture) compose(t *testing.T, owner, path string, form url.Values) map[string]any {
	t.Helper()
	if form.Get("account_id") == "" {
		form.Set("account_id", f.accounts[owner].ID)
	}
	rec := f.request(owner, http.MethodPost, path, form.Encode())
	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func (f *userStorageFixture) sendStatus(t *testing.T, owner, id string) storage.OutgoingSend {
	t.Helper()
	var send storage.OutgoingSend
	err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error { var err error; send, err = db.GetOutgoingSend(t.Context(), id); return err })
	if err != nil {
		t.Fatal(err)
	}
	return send
}
func (s *routedIMAPServer) rawCopies(owner, folder string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	box := s.mutationMailboxLocked(owner, folder)
	var raw []string
	for _, uid := range box.uids {
		if text := box.raw[uid]; text != "" {
			raw = append(raw, text)
		}
	}
	return raw
}

func TestUserComposeDraftAttachmentsIsolationAndDiscard(t *testing.T) {
	f, server, _ := newUserComposeFixture(t)
	id, _, err := f.blobs.StoreComposeAttachment(t.Context(), "alice", "report.txt", strings.NewReader("private attachment"))
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"draft_id": {"<same-draft@example.com>"}, "to": {"recipient@example.com"}, "subject": {"Alice draft"}, "body": {"alice draft body"}, "attachment_id": {id}, "attachment_filename": {"report.txt"}, "attachment_content_type": {"text/plain"}}
	f.compose(t, "alice", "/compose/draft", form)
	if rec := f.request("bob", http.MethodPost, "/compose/draft", form.Encode()); rec.Code != 404 {
		t.Fatalf("foreign staged attachment accepted: %d %s", rec.Code, rec.Body.String())
	}
	f.compose(t, "bob", "/compose/draft", url.Values{"draft_id": {"<same-draft@example.com>"}, "to": {"recipient@example.com"}, "subject": {"Bob draft"}, "body": {"bob draft body"}})
	for _, owner := range []string{"alice", "bob"} {
		if err := f.imap.Sync(t.Context(), owner, f.accounts[owner].ID); err != nil {
			t.Fatal(err)
		}
	}
	if len(server.rawCopies("alice", "Drafts")) != 1 || len(server.rawCopies("bob", "Drafts")) != 1 {
		t.Fatal("draft copies were not separated by owner")
	}
	if err := f.blobs.DeleteComposeAttachment("alice", id); err != nil {
		t.Fatal(err)
	}
	var localID int64
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		var err error
		localID, err = db.GetMessageLocalIDByInternetIDInternal(t.Context(), f.accounts["alice"].ID, form.Get("draft_id"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rec := f.request("alice", http.MethodGet, fmt.Sprintf("/api/drafts/%d", localID), "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "alice draft body") {
		t.Fatalf("draft read: %d %s", rec.Code, rec.Body.String())
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		email, err := db.GetEmailByIDInternal(t.Context(), fmt.Sprint(localID))
		if err != nil {
			return err
		}
		if len(email.Attachments) != 1 {
			return fmt.Errorf("attachments=%d", len(email.Attachments))
		}
		data, err := os.ReadFile(email.Attachments[0].StoragePath)
		if string(data) != "private attachment" {
			return fmt.Errorf("attachment lost: %q", data)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.compose(t, "alice", "/compose/draft/discard", url.Values{"draft_id": {form.Get("draft_id")}})
	if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	if len(server.rawCopies("alice", "Drafts")) != 0 || len(server.rawCopies("bob", "Drafts")) != 1 {
		t.Fatal("discard crossed owners or failed remotely")
	}
}

func TestUserComposeSMTPAndSentCopyReleaseLeaseAndOwnEvents(t *testing.T) {
	f, imap, smtp := newUserComposeFixture(t)
	smtp.mu.Lock()
	smtp.blockOwner = "alice"
	smtp.blocked = make(chan struct{})
	smtp.release = make(chan struct{})
	blocked, release := smtp.blocked, smtp.release
	smtp.mu.Unlock()
	events := openUserEventStream(t, f, "alice")
	a := f.compose(t, "alice", "/compose", url.Values{"to": {"recipient@example.com"}, "subject": {"Alice outgoing"}, "body": {"Alice body"}})
	awaitIMAP(t, blocked)
	b := f.compose(t, "bob", "/compose", url.Values{"to": {"recipient@example.com"}, "subject": {"Bob outgoing"}, "body": {"Bob body"}})
	waitUserIMAP(t, func() bool {
		return f.sendStatus(t, "bob", b["send_id"].(string)).SentCopyStatus == storage.SentCopyComplete
	})
	if smtp.count("bob") != 1 || len(imap.rawCopies("bob", "Sent")) != 1 {
		t.Fatal("Bob could not deliver while Alice held SMTP open")
	}
	if rec := f.request("bob", http.MethodGet, "/api/outgoing-sends/"+a["send_id"].(string), ""); rec.Code != 404 {
		t.Fatal("foreign outgoing payload visible")
	}
	close(release)
	waitUserIMAP(t, func() bool {
		return f.sendStatus(t, "alice", a["send_id"].(string)).SentCopyStatus == storage.SentCopyComplete
	})
	awaitUserEvent(t, events, func(event map[string]any) bool {
		if event["account_id"] == f.accounts["bob"].ID {
			t.Fatal("foreign send event leaked")
		}
		if event["type"] == "send-result" {
			if event["status"] != "sent" {
				t.Fatalf("send event schema/status changed: %v", event)
			}
			return true
		}
		return false
	})
	if smtp.count("alice") != 1 || len(imap.rawCopies("alice", "Sent")) != 1 {
		t.Fatal("Alice duplicate or missing delivery")
	}
	if send := f.sendStatus(t, "alice", a["send_id"].(string)); len(send.MIMEData) != 0 {
		t.Fatal("completed payload retained")
	}
}

func TestUserComposeSentCopyRecoveryNeverResendsSMTP(t *testing.T) {
	f, imap, smtp := newUserComposeFixture(t)
	imap.mu.Lock()
	imap.loseAppendAck = true
	imap.mu.Unlock()
	result := f.compose(t, "alice", "/compose", url.Values{"to": {"recipient@example.com"}, "body": {"durably accepted"}})
	id := result["send_id"].(string)
	waitUserIMAP(t, func() bool { return f.sendStatus(t, "alice", id).SentCopyStatus == storage.SentCopyAmbiguous })
	if send := f.sendStatus(t, "alice", id); send.Status != storage.OutgoingSendSent {
		t.Fatal("SMTP acceptance was not durable before Sent copy")
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`CREATE TRIGGER reject_sent_body BEFORE UPDATE OF body_text_path ON messages BEGIN SELECT RAISE(ABORT,'injected Sent cache failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	releaseCopy := func() {
		t.Helper()
		if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE outgoing_sends SET sent_copy_next_attempt_at=datetime('now','-1 second') WHERE id=?`, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	releaseCopy()
	if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err == nil {
		t.Fatal("injected Sent cache failure hidden")
	}
	if smtp.count("alice") != 1 || len(imap.rawCopies("alice", "Sent")) != 1 {
		t.Fatal("Sent cache recovery duplicated delivery")
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error { _, err := db.Write().Exec(`DROP TRIGGER reject_sent_body`); return err }); err != nil {
		t.Fatal(err)
	}
	releaseCopy()
	if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	if send := f.sendStatus(t, "alice", id); send.SentCopyStatus != storage.SentCopyComplete {
		t.Fatal("Sent cache not recovered")
	}
	if smtp.count("alice") != 1 || len(imap.rawCopies("alice", "Sent")) != 1 {
		t.Fatal("Sent reconciliation repeated accepted bytes")
	}
}

func TestUserComposeDraftEditDuringAppendAndReceiving(t *testing.T) {
	f, server, _ := newUserComposeFixture(t)
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE accounts SET email_sync_enabled=1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	blocked, release := server.setBlock("alice", "append")
	form := url.Values{"draft_id": {"<editing@example.com>"}, "to": {"recipient@example.com"}, "subject": {"first subject"}, "body": {"first body"}}
	f.compose(t, "alice", "/compose/draft", form)
	awaitIMAP(t, blocked)
	form.Set("subject", "new subject")
	form.Set("body", "new body")
	f.compose(t, "alice", "/compose/draft", form)
	f.compose(t, "bob", "/compose/draft", url.Values{"to": {"recipient@example.com"}, "body": {"Bob independent draft"}})
	close(release)
	if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	if copies := server.rawCopies("alice", "Drafts"); len(copies) != 1 || !strings.Contains(copies[0], "new body") {
		t.Fatalf("draft remote revisions=%q", copies)
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		id, err := db.GetMessageLocalIDByInternetIDInternal(t.Context(), f.accounts["alice"].ID, form.Get("draft_id"))
		if err != nil {
			return err
		}
		email, err := db.GetEmailByIDInternal(t.Context(), fmt.Sprint(id))
		if err != nil {
			return err
		}
		if email.Subject != "new subject" || email.TextBody != "new body" || !email.IsDraft {
			return fmt.Errorf("receiving overwrote draft: %+v", email)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserComposeSMTPTemporaryAndPermanentRejections(t *testing.T) {
	for _, code := range []int{451, 550} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f, _, smtp := newUserComposeFixture(t)
			smtp.mu.Lock()
			smtp.rejectSender = code
			smtp.mu.Unlock()
			result := f.compose(t, "alice", "/compose", url.Values{"to": {"recipient@example.com"}, "body": {"rejection test"}})
			id := result["send_id"].(string)
			waitUserIMAP(t, func() bool {
				send := f.sendStatus(t, "alice", id)
				return send.AttemptCount == 1 && send.LastError != ""
			})
			send := f.sendStatus(t, "alice", id)
			if code == 550 {
				if send.Status != storage.OutgoingSendFailed {
					t.Fatal("permanent rejection was retried")
				}
				return
			}
			if send.Status != storage.OutgoingSendPending || !send.NextAttemptAt.After(time.Now()) {
				t.Fatal("temporary failure did not back off")
			}
			if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			if again := f.sendStatus(t, "alice", id); again.AttemptCount != 1 {
				t.Fatal("temporary failure was retried before deadline")
			}
			smtp.mu.Lock()
			smtp.rejectSender = 0
			smtp.mu.Unlock()
			if rec := f.request("alice", http.MethodPost, "/api/outgoing-sends/"+id+"/retry-now", ""); rec.Code != 200 {
				t.Fatalf("retry temporary failure: %d %s", rec.Code, rec.Body.String())
			}
			waitUserIMAP(t, func() bool { return f.sendStatus(t, "alice", id).SentCopyStatus == storage.SentCopyComplete })
			if smtp.count("alice") != 1 {
				t.Fatal("temporary rejection retry duplicated acceptance")
			}
		})
	}
}

func TestUserComposeUncertainSMTPRequiresOwnedConfirmation(t *testing.T) {
	f, _, smtp := newUserComposeFixture(t)
	smtp.mu.Lock()
	smtp.loseAck = true
	smtp.mu.Unlock()
	result := f.compose(t, "alice", "/compose", url.Values{"to": {"recipient@example.com"}, "subject": {"ambiguous"}, "body": {"accepted remotely"}})
	id := result["send_id"].(string)
	waitUserIMAP(t, func() bool { return f.sendStatus(t, "alice", id).Status == storage.OutgoingSendAmbiguous })
	if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	if smtp.count("alice") != 1 {
		t.Fatal("uncertain send was automatically repeated")
	}
	for _, action := range []struct {
		owner, body string
		status      int
	}{{"bob", `{"confirm":true}`, 404}, {"alice", `{"confirm":false}`, 409}, {"alice", `{"confirm":true}`, 200}} {
		rec := f.request(action.owner, http.MethodPost, "/api/outgoing-sends/"+id+"/retry", action.body)
		if rec.Code != action.status {
			t.Fatalf("retry: %d %s", rec.Code, rec.Body.String())
		}
	}
	waitUserIMAP(t, func() bool { return f.sendStatus(t, "alice", id).SentCopyStatus == storage.SentCopyComplete })
	if smtp.count("alice") != 2 {
		t.Fatal("confirmed retry did not deliver exactly once more")
	}
}

func TestUserComposeLostDraftAppendReconcilesBeforeNewRevision(t *testing.T) {
	f, server, _ := newUserComposeFixture(t)
	server.mu.Lock()
	server.loseAppendAck = true
	server.mu.Unlock()
	form := url.Values{"draft_id": {"<revision@example.com>"}, "to": {"recipient@example.com"}, "subject": {"revision"}, "body": {"first revision"}}
	f.compose(t, "alice", "/compose/draft", form)
	waitUserIMAP(t, func() bool {
		var count int
		err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
			return db.Read().QueryRow(`SELECT COUNT(*) FROM imap_draft_operations WHERE status='ambiguous'`).Scan(&count)
		})
		return err == nil && count == 1
	})
	form.Set("body", "second revision")
	f.compose(t, "alice", "/compose/draft", form)
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE imap_draft_operations SET next_attempt_at=datetime('now','-1 second')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	copies := server.rawCopies("alice", "Drafts")
	if len(copies) != 1 || !strings.Contains(copies[0], "second revision") {
		t.Fatalf("draft revisions=%q", copies)
	}
}

func TestUserComposeScheduledEditsAndRecoveryControls(t *testing.T) {
	f, _, smtp := newUserComposeFixture(t)
	at := time.Now().UTC().Add(time.Hour).Truncate(5 * time.Minute)
	form := url.Values{"draft_id": {"<schedule@example.com>"}, "to": {"recipient@example.com"}, "subject": {"scheduled"}, "body": {"first schedule"}, "schedule_timezone": {"UTC"}, "schedule_date": {at.Format("2006-01-02")}, "schedule_hour": {at.Format("15")}, "schedule_minute": {at.Format("04")}}
	f.compose(t, "alice", "/compose/schedule", form)
	var send storage.OutgoingSend
	load := func() {
		t.Helper()
		if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
			var id int64
			var err error
			id, err = db.GetMessageLocalIDByInternetIDInternal(t.Context(), f.accounts["alice"].ID, form.Get("draft_id"))
			if err != nil {
				return err
			}
			send, err = db.GetOutgoingSendByMessageID(t.Context(), id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	load()
	var original map[string]any
	json.Unmarshal(send.MessageJSON, &original)
	form.Set("body", "updated schedule")
	f.compose(t, "alice", "/compose/draft", form)
	load()
	var updated map[string]any
	json.Unmarshal(send.MessageJSON, &updated)
	if updated["text_body"] != "updated schedule" || updated["message_id"] != original["message_id"] {
		t.Fatalf("waiting snapshot not refreshed: %v", updated)
	}
	if smtp.count("alice") != 0 {
		t.Fatal("schedule delivered early")
	}
	if rec := f.request("bob", http.MethodPost, "/api/outgoing-sends/"+send.ID+"/retry-now", ""); rec.Code != 404 {
		t.Fatal("foreign schedule changed")
	}
	if rec := f.request("alice", http.MethodPost, "/api/outgoing-sends/"+send.ID+"/retry-now", ""); rec.Code != 200 {
		t.Fatalf("retry now: %d %s", rec.Code, rec.Body.String())
	}
	waitUserIMAP(t, func() bool { return f.sendStatus(t, "alice", send.ID).SentCopyStatus == storage.SentCopyComplete })
	smtp.mu.Lock()
	raw := smtp.accepted["alice"][0]
	smtp.mu.Unlock()
	if !strings.Contains(raw, "updated schedule") {
		t.Fatal("old schedule contents were sent")
	}
	form.Set("draft_id", "<cancel@example.com>")
	f.compose(t, "alice", "/compose/schedule", form)
	load()
	if rec := f.request("alice", http.MethodPost, "/api/outgoing-sends/"+send.ID+"/cancel", ""); rec.Code != 200 {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	if f.sendStatus(t, "alice", send.ID).Status != storage.OutgoingSendCanceled || smtp.count("alice") != 1 {
		t.Fatal("canceled send delivered")
	}
}

func TestUserComposeShutdownLeavesUncertainSMTPForRecovery(t *testing.T) {
	f, _, smtp := newUserComposeFixture(t)
	smtp.mu.Lock()
	smtp.blockOwner = "alice"
	smtp.blocked = make(chan struct{})
	smtp.release = make(chan struct{})
	blocked, release := smtp.blocked, smtp.release
	smtp.mu.Unlock()
	result := f.compose(t, "alice", "/compose", url.Values{"to": {"recipient@example.com"}, "body": {"shutdown acceptance"}})
	id := result["send_id"].(string)
	awaitIMAP(t, blocked)
	f.stopIMAP()
	done := make(chan struct{})
	go func() { f.imap.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown waited on remote SMTP")
	}
	close(release)
	if send := f.sendStatus(t, "alice", id); send.Status != storage.OutgoingSendSending {
		t.Fatalf("unexpected interrupted status %s", send.Status)
	}
	ctx, cancel := context.WithCancel(t.Context())
	restarted, err := mail.NewUserIMAP(ctx, f.accountStore, f.blobs, f.events)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); restarted.Wait() })
	if err := f.base.RegisterUserStorageRoutes(ctx, http.NewServeMux(), f.routing, handler.UserStorageOptions{Accounts: f.accountStore, IMAP: restarted}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Sync(ctx, "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	if send := f.sendStatus(t, "alice", id); send.Status != storage.OutgoingSendAmbiguous || smtp.count("alice") != 1 {
		t.Fatal("interrupted acceptance was not retained as uncertain")
	}
}

func TestUserComposeBackgroundUsesDurableScheduledDeadline(t *testing.T) {
	f, _, smtp := newUserComposeFixture(t)
	if err := f.imap.Start(mail.UserIMAPBackgroundOptions{PollInterval: 24 * time.Hour, ScanInterval: 20 * time.Millisecond, DisableIDLE: true}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(time.Hour).Truncate(5 * time.Minute)
	form := url.Values{"draft_id": {"<background-schedule@example.com>"}, "to": {"recipient@example.com"}, "body": {"durable future send"}, "schedule_timezone": {"UTC"}, "schedule_date": {at.Format("2006-01-02")}, "schedule_hour": {at.Format("15")}, "schedule_minute": {at.Format("04")}}
	f.compose(t, "alice", "/compose/schedule", form)
	// Advance the durable schedule for this clock-controlled test, then follow
	// the same central discovery wake used by HTTP edits, with no manual Sync.
	due := time.Now().UTC().Add(2 * time.Second)
	var id string
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		if err := db.Read().QueryRow(`SELECT id FROM outgoing_sends WHERE draft_id=?`, form.Get("draft_id")).Scan(&id); err != nil {
			return err
		}
		_, err := db.Write().Exec(`UPDATE outgoing_sends SET send_after=? WHERE id=?`, due, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.imap.WakeMutations(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	waitUserIMAP(t, func() bool { return f.sendStatus(t, "alice", id).SentCopyStatus == storage.SentCopyComplete })
	smtp.mu.Lock()
	accepted := append([]time.Time(nil), smtp.acceptedAt["alice"]...)
	smtp.mu.Unlock()
	if len(accepted) != 1 || accepted[0].Before(due) {
		t.Fatalf("schedule accepted at %v before %s", accepted, due)
	}
}
