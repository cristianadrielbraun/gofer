package notifications

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	stdmail "net/mail"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// This server speaks IMAP over loopback, including SASL password authentication,
// sparse UID search/fetch, folder discovery and MIME literals. Worker/protocol and
// SQL implementations are real; the remote mail provider is scripted.
type routedIdleClient struct {
	mu         sync.Mutex
	writer     *bufio.Writer
	owner, tag string
	idle       bool
}

type routedIMAPServer struct {
	listener                 net.Listener
	done                     chan struct{}
	wg                       sync.WaitGroup
	mu                       sync.Mutex
	connections              map[net.Conn]bool
	uids                     []uint32
	validity                 uint32
	seen                     bool
	bodyCount                map[string]int
	commands                 []string
	blockOwner, blockCommand string
	blocked, release         chan struct{}
	blockOnce                sync.Once
	rejectBody               bool
	bodyOverride             string
	idleSupported            bool
	idleClients              map[net.Conn]*routedIdleClient
	ownerUIDs                map[string][]uint32
	mutationMode             bool
	mutationMailboxes        map[string]*routedMutationMailbox
	failStore                bool
	deliveryMode             bool
	loseAppendAck            bool
}

func newRoutedIMAPServer(t *testing.T) *routedIMAPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &routedIMAPServer{listener: listener, done: make(chan struct{}), connections: make(map[net.Conn]bool), uids: []uint32{2, 1000000}, validity: 100, bodyCount: make(map[string]int), idleClients: make(map[net.Conn]*routedIdleClient), ownerUIDs: make(map[string][]uint32)}
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
				s.serve(conn)
				s.mu.Lock()
				delete(s.connections, conn)
				delete(s.idleClients, conn)
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
func (s *routedIMAPServer) setBlock(owner, command string) (<-chan struct{}, chan<- struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blockOwner = owner
	s.blockCommand = command
	s.blocked = make(chan struct{})
	s.release = make(chan struct{})
	s.blockOnce = sync.Once{}
	return s.blocked, s.release
}
func (s *routedIMAPServer) bodyRequests(owner string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodyCount[owner]
}
func (s *routedIMAPServer) serve(conn net.Conn) {
	defer conn.Close()
	writer := bufio.NewWriter(conn)
	reader := bufio.NewReader(conn)
	entry := &routedIdleClient{writer: writer}
	s.mu.Lock()
	s.idleClients[conn] = entry
	supported := s.idleSupported
	mutations := s.mutationMode
	delivery := s.deliveryMode
	s.mu.Unlock()
	caps := "IMAP4rev1 AUTH=PLAIN SASL-IR UNSELECT"
	if supported {
		caps += " IDLE"
	}
	if mutations {
		caps += " MOVE UIDPLUS"
	}
	fmt.Fprintf(writer, "* OK [CAPABILITY %s] test mail server\r\n", caps)
	writer.Flush()
	owner := ""
	selected := "INBOX"
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		if strings.EqualFold(strings.TrimSpace(line), "DONE") {
			entry.mu.Lock()
			fmt.Fprintf(writer, "%s OK idle finished\r\n", entry.tag)
			entry.idle = false
			writer.Flush()
			entry.mu.Unlock()
			continue
		}
		parts := strings.Fields(strings.TrimSpace(line))
		if len(parts) < 2 {
			return
		}
		tag := parts[0]
		upper := strings.ToUpper(line)
		if strings.Contains(upper, " SELECT ") {
			selected = strings.Trim(parts[2], `"`)
		}
		s.mu.Lock()
		s.commands = append(s.commands, owner+": "+strings.TrimSpace(line))
		uids := append([]uint32(nil), s.uids...)
		if owned, ok := s.ownerUIDs[owner]; ok {
			uids = append([]uint32(nil), owned...)
		}
		validity, seen := s.validity, s.seen
		origins := make(map[uint32]uint32)
		mailFlags := make(map[uint32]string)
		rawMessages := make(map[uint32]string)
		if mutations && owner != "" {
			box := s.mutationMailboxLocked(owner, selected)
			uids = append([]uint32(nil), box.uids...)
			validity = box.validity
			for uid, origin := range box.origins {
				origins[uid] = origin
			}
			for uid, flags := range box.flags {
				for _, flag := range []string{`\Seen`, `\Flagged`, `\Deleted`, `\Draft`, `$Label1`, `$Label2`, `$Junk`, `$NotJunk`, `Projects`, `Later`} {
					if flags[flag] {
						mailFlags[uid] += flag + " "
					}
				}
			}
			for uid, raw := range box.raw {
				rawMessages[uid] = raw
			}
		}
		body := strings.Contains(upper, " UID FETCH ") && strings.Contains(upper, "BODY.PEEK[]")
		if body {
			s.bodyCount[owner]++
		}
		block := owner == s.blockOwner && s.blockCommand != "" && (s.blockCommand == "body" && body || s.blockCommand == "list" && strings.Contains(upper, " LIST ") || s.blockCommand == "headers" && strings.Contains(upper, "ENVELOPE") || s.blockCommand == "store" && strings.Contains(upper, " UID STORE ") || s.blockCommand == "move" && strings.Contains(upper, " UID MOVE ") || s.blockCommand == "append" && strings.Contains(upper, " APPEND "))
		blocked, release, reject := s.blocked, s.release, s.rejectBody
		bodyOverride := s.bodyOverride
		hasJunk := s.mutationMailboxes[owner+":Junk"] != nil
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
		entry.mu.Lock()
		switch {
		case strings.Contains(upper, " CAPABILITY"):
			fmt.Fprintf(writer, "* CAPABILITY %s\r\n%s OK capability\r\n", caps, tag)
		case strings.Contains(upper, " AUTHENTICATE PLAIN"):
			token := ""
			if len(parts) >= 4 {
				token = parts[3]
			} else {
				fmt.Fprint(writer, "+ \r\n")
				writer.Flush()
				token, _ = reader.ReadString('\n')
			}
			decoded, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(token))
			credentials := strings.Split(string(decoded), "\x00")
			if len(credentials) != 3 || credentials[2] != "synthetic-only" {
				fmt.Fprintf(writer, "%s NO invalid credentials\r\n", tag)
			} else {
				owner = credentials[1]
				entry.owner = owner
				fmt.Fprintf(writer, "%s OK authenticated\r\n", tag)
			}
		case strings.Contains(upper, " LIST "):
			if delivery {
				fmt.Fprint(writer, "* LIST (\\Drafts) \"/\" Drafts\r\n* LIST (\\Sent) \"/\" Sent\r\n")
			}
			if mutations {
				fmt.Fprint(writer, "* LIST (\\Archive) \"/\" Archive\r\n* LIST (\\Trash) \"/\" Trash\r\n")
				if hasJunk {
					fmt.Fprint(writer, "* LIST (\\Junk) \"/\" Junk\r\n")
				}
			}
			fmt.Fprintf(writer, "* LIST (\\Inbox) \"/\" INBOX\r\n%s OK list\r\n", tag)
		case strings.Contains(upper, " SELECT "):
			if mutations {
				fmt.Fprint(writer, "* OK [PERMANENTFLAGS (\\Seen \\Flagged \\Deleted \\*)] persistent flags\r\n")
			}
			next := uint32(1)
			for _, uid := range uids {
				if uid >= next {
					next = uid + 1
				}
			}
			fmt.Fprintf(writer, "* FLAGS (\\Seen \\Flagged)\r\n* %d EXISTS\r\n* OK [UIDVALIDITY %d] valid\r\n* OK [UIDNEXT %d] next\r\n%s OK [READ-WRITE] selected\r\n", len(uids), validity, next, tag)
		case strings.Contains(upper, " UID SEARCH "):
			fmt.Fprint(writer, "* SEARCH")
			for _, uid := range uids {
				if raw := rawMessages[uid]; raw != "" && strings.Contains(upper, "HEADER ") {
					msg, e := stdmail.ReadMessage(strings.NewReader(raw))
					if e != nil {
						continue
					}
					header := "Message-ID"
					if strings.Contains(upper, "X-GOFER-DRAFT-REVISION") {
						header = "X-Gofer-Draft-Revision"
					}
					if !strings.Contains(line, strings.Trim(msg.Header.Get(header), "<>")) || msg.Header.Get(header) == "" {
						continue
					}
					fmt.Fprintf(writer, " %d", uid)
					continue
				}
				if strings.Contains(upper, "X-GOFER-DRAFT-REVISION") {
					continue
				}
				if strings.Contains(strings.ReplaceAll(upper, `"`, ""), "HEADER MESSAGE-ID") {
					origin := uid
					if origins[uid] != 0 {
						origin = origins[uid]
					}
					if !strings.Contains(line, fmt.Sprintf("%s-%d@example.com", owner, origin)) {
						continue
					}
				}
				fmt.Fprintf(writer, " %d", uid)
			}
			fmt.Fprintf(writer, "\r\n%s OK search\r\n", tag)
		case strings.Contains(upper, " UID FETCH "):
			if body && reject {
				fmt.Fprintf(writer, "%s NO temporary fetch failure\r\n", tag)
				break
			}
			for index, uid := range uids {
				if !testUIDSetContains(parts[3], uid) {
					continue
				}
				flags := ""
				if seen && uid == uids[0] {
					flags = "\\Seen"
				}
				if mutations {
					flags = strings.TrimSpace(mailFlags[uid])
				}
				origin := uid
				if origins[uid] != 0 {
					origin = origins[uid]
				}
				if body {
					raw := testUserMIME(owner, origin)
					if rawMessages[uid] != "" {
						raw = rawMessages[uid]
					}
					if bodyOverride != "" {
						raw = bodyOverride
					}
					fmt.Fprintf(writer, "* %d FETCH (UID %d BODY[] {%d}\r\n%s)\r\n", index+1, uid, len(raw), raw)
				} else if strings.Contains(upper, "ENVELOPE") {
					if rawMessages[uid] != "" {
						msg, err := stdmail.ReadMessage(strings.NewReader(rawMessages[uid]))
						if err != nil {
							continue
						}
						fmt.Fprintf(writer, `* %d FETCH (UID %d FLAGS (%s) INTERNALDATE "06-Oct-2026 10:00:00 +0000" RFC822.SIZE %d ENVELOPE ("Tue, 6 Oct 2026 10:00:00 +0000" %s (("Sender" NIL "%s" "example.com")) NIL NIL ((NIL NIL "recipient" "example.com")) NIL NIL NIL %s))`+"\r\n", index+1, uid, flags, len(rawMessages[uid]), strconv.Quote(msg.Header.Get("Subject")), owner, strconv.Quote(msg.Header.Get("Message-ID")))
						continue
					}
					fmt.Fprintf(writer, `* %d FETCH (UID %d FLAGS (%s) INTERNALDATE "06-Oct-2026 10:00:00 +0000" RFC822.SIZE 900 ENVELOPE ("Tue, 6 Oct 2026 10:00:00 +0000" "%s private subject %d" (("Sender" NIL "%s" "example.com")) NIL NIL ((NIL NIL "%s" "example.com")) NIL NIL NIL "<%s-%d@example.com>"))`+"\r\n", index+1, uid, flags, owner, origin, owner, owner, owner, origin)
				} else {
					fmt.Fprintf(writer, "* %d FETCH (UID %d FLAGS (%s))\r\n", index+1, uid, flags)
				}
			}
			fmt.Fprintf(writer, "%s OK fetch\r\n", tag)
		case mutations && (strings.Contains(upper, " UID STORE ") || strings.Contains(upper, " UID MOVE ") || strings.Contains(upper, " UID EXPUNGE ")):
			s.serveMutation(writer, owner, selected, tag, parts, upper)
		case delivery && strings.Contains(upper, " APPEND "):
			literal := strings.Trim(parts[len(parts)-1], "{}+\r\n")
			size, e := strconv.Atoi(literal)
			if e != nil || size < 0 || size > 64<<20 {
				entry.mu.Unlock()
				return
			}
			if !strings.HasSuffix(strings.TrimSpace(line), "+}") {
				fmt.Fprint(writer, "+ ready\r\n")
				writer.Flush()
			}
			raw := make([]byte, size)
			if _, e := io.ReadFull(reader, raw); e != nil {
				entry.mu.Unlock()
				return
			}
			reader.ReadString('\n')
			s.mu.Lock()
			box := s.mutationMailboxLocked(owner, strings.Trim(parts[2], `"`))
			uid := uint32(10)
			for _, n := range box.uids {
				uid = max(uid, n+1)
			}
			box.uids = append(box.uids, uid)
			if box.raw == nil {
				box.raw = make(map[uint32]string)
			}
			box.raw[uid] = string(raw)
			box.flags[uid] = map[string]bool{`\Seen`: true, `\Draft`: strings.Contains(upper, `\DRAFT`)}
			lose := s.loseAppendAck
			s.loseAppendAck = false
			validity := box.validity
			s.mu.Unlock()
			if lose {
				entry.mu.Unlock()
				return
			}
			fmt.Fprintf(writer, "%s OK [APPENDUID %d %d] appended\r\n", tag, validity, uid)
		case strings.Contains(upper, " IDLE"):
			entry.idle = true
			entry.tag = tag
			fmt.Fprint(writer, "+ idling\r\n")
		case strings.Contains(upper, " UNSELECT"):
			fmt.Fprintf(writer, "%s OK unselected\r\n", tag)
		case strings.Contains(upper, " LOGOUT"):
			fmt.Fprintf(writer, "* BYE closing\r\n%s OK logout\r\n", tag)
			writer.Flush()
			entry.mu.Unlock()
			return
		default:
			fmt.Fprintf(writer, "%s BAD unexpected command\r\n", tag)
		}
		err = writer.Flush()
		entry.mu.Unlock()
		if err != nil {
			return
		}
	}
}
func testUIDSetContains(set string, uid uint32) bool {
	for _, group := range strings.Split(set, ",") {
		bounds := strings.Split(group, ":")
		low, _ := strconv.ParseUint(bounds[0], 10, 32)
		high := low
		if len(bounds) == 2 {
			if bounds[1] == "*" {
				high = 1<<32 - 1
			} else {
				high, _ = strconv.ParseUint(bounds[1], 10, 32)
			}
		}
		if uint64(uid) >= low && uint64(uid) <= high {
			return true
		}
	}
	return false
}
func testUserMIME(owner string, uid uint32) string {
	return fmt.Sprintf("From: Sender <%s@example.com>\r\nTo: %s@example.com\r\nSubject: %s private subject %d\r\nMessage-ID: <%s-%d@example.com>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=parts\r\n\r\n--parts\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>%sbodyuniquetoken private body</p><img src=\"cid:shared-cid\"><img src=\"https://images.example/private.png\"><script>alert('unsafe')</script>\r\n--parts\r\nContent-Type: image/png\r\nContent-Disposition: inline; filename=inline.png\r\nContent-ID: <shared-cid>\r\n\r\n%s-inline-image\r\n--parts\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=private.txt\r\n\r\n%s-private-attachment\r\n--parts--\r\n", owner, owner, owner, uid, owner, uid, owner, owner, owner)
}
func (f *userStorageFixture) useIMAPServer(t *testing.T, s *routedIMAPServer) {
	t.Helper()
	host, portText, _ := net.SplitHostPort(s.listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	if err := f.system.AddPlaintextTransportException(t.Context(), "imap", host, port, "alice"); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.accountStore.UpdateAccount(t.Context(), owner, f.accounts[owner].ID, &models.CreateAccountRequest{Provider: "imap", EmailAddress: owner + "@example.com", DisplayName: owner, IMAPHost: host, IMAPPort: port, IMAPTLSMode: "plaintext", SMTPHost: "smtp.example.com", Username: owner, AuthMethod: "plain"}); err != nil {
			t.Fatal(err)
		}
	}
}
func awaitIMAP(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	awaitIMAPFor(t, ch, 5*time.Second)
}

func awaitIMAPFor(t *testing.T, ch <-chan struct{}, timeout time.Duration) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatal("IMAP operation did not reach block")
	}
}
func awaitIMAPResult(t *testing.T, ch <-chan error) {
	t.Helper()
	select {
	case err := <-ch:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("IMAP operation timed out")
	}
}

func TestUserIMAPReceiveAndConcurrentHTTPBodyIsolation(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	f.useIMAPServer(t, server)
	blocked, release := server.setBlock("alice", "list")
	aliceSync := make(chan error, 1)
	go func() { aliceSync <- f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID) }()
	awaitIMAP(t, blocked)
	// Bob receives while Alice is waiting on a remote server, with one cache slot.
	if err := f.imap.Sync(t.Context(), "bob", f.accounts["bob"].ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	awaitIMAPResult(t, aliceSync)
	for _, owner := range []string{"alice", "bob"} {
		rec := f.request(owner, http.MethodGet, "/mail/folder/inbox/items", "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), owner+" private subject") {
			t.Fatalf("received mail %s: %d %s", owner, rec.Code, rec.Body.String()[:min(250, rec.Body.Len())])
		}
	}
	blocked, release = server.setBlock("alice", "body")
	var wg sync.WaitGroup
	responses := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := f.request("alice", http.MethodGet, "/email/1/body", "")
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), "alicebodyuniquetoken") || strings.Contains(rec.Body.String(), "bobbodyuniquetoken") || strings.Contains(rec.Body.String(), "alert('unsafe')") {
				responses <- fmt.Sprintf("%d %s", rec.Code, rec.Body.String()[:min(250, rec.Body.Len())])
			}
		}()
	}
	awaitIMAP(t, blocked)
	rec := f.request("bob", http.MethodGet, "/email/1/body", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "bobbodyuniquetoken") || strings.Contains(rec.Body.String(), "alicebodyuniquetoken") {
		t.Fatalf("Bob body: %d %s", rec.Code, rec.Body.String()[:min(250, rec.Body.Len())])
	}
	close(release)
	wg.Wait()
	close(responses)
	for err := range responses {
		t.Error(err)
	}
	if server.bodyRequests("alice") != 1 || server.bodyRequests("bob") != 1 {
		t.Fatalf("body fetches: Alice %d Bob %d", server.bodyRequests("alice"), server.bodyRequests("bob"))
	}
	for _, owner := range []string{"alice", "bob"} {
		rec := f.request(owner, http.MethodGet, "/email/1/body?mode=original", "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), owner+"bodyuniquetoken") {
			t.Fatalf("original %s: %d %s", owner, rec.Code, rec.Body.String()[:min(250, rec.Body.Len())])
		}
		for _, path := range []string{"/api/inline-content/1/shared-cid", "/api/attachments/2/download"} {
			rec := f.request(owner, http.MethodGet, path, "")
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), owner+"-") {
				t.Fatalf("attachment %s: %d %s", owner, rec.Code, rec.Body.String()[:min(250, rec.Body.Len())])
			}
		}
		rec = f.request(owner, http.MethodGet, "/search?q="+owner+"bodyuniquetoken", "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), owner+" private subject") {
			t.Fatalf("body search %s: %d %s", owner, rec.Code, rec.Body.String()[:min(250, rec.Body.Len())])
		}
	}
}

func TestUserIMAPReconcileFlagsExpungeAndUIDReset(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	f.useIMAPServer(t, server)
	id := f.accounts["alice"].ID
	if err := f.imap.Sync(t.Context(), "alice", id); err != nil {
		t.Fatal(err)
	}
	events := f.events.Subscribe()
	defer f.events.Unsubscribe(events)
	server.mu.Lock()
	server.uids = []uint32{1000000, 1000001}
	server.seen = true
	server.mu.Unlock()
	if err := f.imap.Sync(t.Context(), "alice", id); err != nil {
		t.Fatal(err)
	}
	if err := f.routing.WithAccountForUser(t.Context(), "alice", id, func(db *storage.DB) error {
		var count, read int
		if err := db.Read().QueryRow(`SELECT COUNT(*),SUM(mfs.is_read) FROM messages m JOIN message_folder_state mfs ON mfs.message_id=m.id WHERE m.account_id=?`, id).Scan(&count, &read); err != nil {
			return err
		}
		if count != 2 || read != 1 {
			return fmt.Errorf("after reconcile count=%d read=%d", count, read)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	found := false
	for len(events) > 0 {
		event := <-events
		if event.UserID != "alice" {
			t.Fatalf("event owner: %#v", event)
		}
		if event.Type == mail.EventNewMail {
			found = true
		}
	}
	if !found {
		t.Fatal("incremental receive did not publish new mail")
	}
	server.mu.Lock()
	server.validity = 200
	server.uids = []uint32{2}
	server.mu.Unlock()
	if err := f.imap.Sync(t.Context(), "alice", id); err != nil {
		t.Fatal(err)
	}
	if err := f.routing.WithAccountForUser(t.Context(), "alice", id, func(db *storage.DB) error {
		var count, validity int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM messages WHERE account_id=?`, id).Scan(&count); err != nil {
			return err
		}
		if err := db.Read().QueryRow(`SELECT uid_validity FROM folders WHERE account_id=? AND remote_id='INBOX'`, id).Scan(&validity); err != nil {
			return err
		}
		if count != 1 || validity != 200 {
			return fmt.Errorf("after UID reset count=%d validity=%d", count, validity)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserIMAPDeletionCancelsBlockedBodyAndKeepsOtherOwner(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	f.useIMAPServer(t, server)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.imap.Sync(t.Context(), owner, f.accounts[owner].ID); err != nil {
			t.Fatal(err)
		}
	}
	blocked, _ := server.setBlock("alice", "body")
	result := make(chan error, 1)
	go func() { result <- f.imap.EnsureBody(t.Context(), "alice", 1) }()
	awaitIMAP(t, blocked)
	rec := f.request("alice", http.MethodDelete, "/api/accounts/"+f.accounts["alice"].ID, "")
	if rec.Code != 202 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String()[:min(250, rec.Body.Len())])
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("deleted account body fetch succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deletion did not cancel blocked IMAP command")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		state, err := f.routing.AccountStateForUser(t.Context(), "alice", f.accounts["alice"].ID)
		if err != nil {
			t.Fatal(err)
		}
		if state == storage.AccountDeleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deletion did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec = f.request("bob", http.MethodGet, "/email/1/body", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "bobbodyuniquetoken") {
		t.Fatalf("Bob after delete: %d %s", rec.Code, rec.Body.String()[:min(250, rec.Body.Len())])
	}
}

func TestUserIMAPBodyFailureRetryAndUIDValidityProtection(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	f.useIMAPServer(t, server)
	if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	server.validity = 200
	server.mu.Unlock()
	rec := f.request("alice", http.MethodGet, "/email/1/body", "")
	if rec.Code != 503 {
		t.Fatalf("UID mismatch: %d %s", rec.Code, rec.Body.String()[:min(250, rec.Body.Len())])
	}
	if server.bodyRequests("alice") != 0 {
		t.Fatal("fetched reused UID before checking validity")
	}
	server.mu.Lock()
	server.validity = 100
	server.rejectBody = true
	server.mu.Unlock()
	rec = f.request("alice", http.MethodGet, "/email/1/body", "")
	if rec.Code != 503 {
		t.Fatalf("remote failure: %d %s", rec.Code, rec.Body.String()[:min(250, rec.Body.Len())])
	}
	server.mu.Lock()
	server.rejectBody = false
	server.mu.Unlock()
	rec = f.request("alice", http.MethodGet, "/email/1/body", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "alicebodyuniquetoken") {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body.String()[:min(250, rec.Body.Len())])
	}
	if err := f.imap.Sync(context.Background(), "bob", f.accounts["alice"].ID); err == nil {
		t.Fatal("foreign owner sync accepted")
	}
}

func TestUserIMAPBodyPublicationRollbackRemovesCandidateFiles(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	f.useIMAPServer(t, server)
	account := f.accounts["alice"].ID
	if err := f.imap.Sync(t.Context(), "alice", account); err != nil {
		t.Fatal(err)
	}
	var beforeRecipients int
	if err := f.routing.WithAccountForUser(t.Context(), "alice", account, func(db *storage.DB) error {
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id=1`).Scan(&beforeRecipients); err != nil {
			return err
		}
		_, err := db.Write().Exec(`CREATE TRIGGER fail_body_attachment BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT,'injected body publication failure'); END`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rec := f.request("alice", http.MethodGet, "/email/1/body", "")
	if rec.Code != 503 {
		t.Fatalf("publication failure: %d", rec.Code)
	}
	if err := f.routing.WithAccountForUser(t.Context(), "alice", account, func(db *storage.DB) error {
		if db.IsBodyFetchedInternal(t.Context(), 1) {
			return fmt.Errorf("failed publication marked body fetched")
		}
		var attachments, recipients int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id=1`).Scan(&attachments); err != nil {
			return err
		}
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id=1`).Scan(&recipients); err != nil {
			return err
		}
		if attachments != 0 || recipients != beforeRecipients {
			return fmt.Errorf("partial rows: attachments %d recipients %d, previously %d", attachments, recipients, beforeRecipients)
		}
		_, err := db.Write().Exec(`DROP TRIGGER fail_body_attachment`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Candidate files live below account/messages/1/<version>; none remain after
	// rollback. The parent directories may exist, but are not published cache data.
	entries, err := filepath.Glob(filepath.Join(f.blobs.RemoteAssetsDir(account, 1), "..", "*", "raw.eml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unpublished raw candidates remain: %v", entries)
	}
	rec = f.request("alice", http.MethodGet, "/email/1/body", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "alicebodyuniquetoken") {
		t.Fatalf("retry after rollback: %d", rec.Code)
	}
	if err := f.routing.WithAccountForUser(t.Context(), "alice", account, func(db *storage.DB) error {
		var attachments, recipients int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id=1`).Scan(&attachments); err != nil {
			return err
		}
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_recipients WHERE message_id=1`).Scan(&recipients); err != nil {
			return err
		}
		if attachments != 2 || recipients != 1 {
			return fmt.Errorf("retry duplicated metadata: attachments %d recipients %d", attachments, recipients)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserIMAPAccountCreateHookStartsRealReceiveWorker(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	f.useIMAPServer(t, server)
	host, portText, _ := net.SplitHostPort(server.listener.Addr().String())
	form := url.Values{"provider": {"imap"}, "email_address": {"extra@example.com"}, "display_name": {"Extra"}, "imap_host": {host}, "imap_port": {portText}, "imap_tls_mode": {"plaintext"}, "smtp_host": {"smtp.example.com"}, "smtp_port": {"465"}, "smtp_tls_mode": {"tls"}, "username": {"alice"}, "password": {"synthetic-only"}, "auth_method": {"plain"}}
	rec := f.request("alice", http.MethodPost, "/api/accounts", form.Encode())
	if rec.Code != 200 {
		t.Fatalf("account create: %d %s", rec.Code, rec.Body.String()[:min(250, rec.Body.Len())])
	}
	id := rec.Header().Get("X-Gofer-Account-ID")
	if id == "" {
		t.Fatal("account create did not return ID")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var count int
		err := f.routing.WithAccountForUser(t.Context(), "alice", id, func(db *storage.DB) error {
			return db.Read().QueryRow(`SELECT COUNT(*) FROM messages WHERE account_id=?`, id).Scan(&count)
		})
		if err != nil {
			t.Fatal(err)
		}
		if count == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("create hook did not receive mail")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec := f.request("bob", http.MethodGet, "/api/accounts/"+id+"/edit", ""); rec.Code != 404 {
		t.Fatalf("foreign queued account became visible: %d", rec.Code)
	}
}

func TestUserIMAPBodyDoesNotWaitForSameAccountSyncAndRejectsStalePublication(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	f.useIMAPServer(t, server)
	account := f.accounts["alice"].ID
	if err := f.imap.Sync(t.Context(), "alice", account); err != nil {
		t.Fatal(err)
	}
	blocked, release := server.setBlock("alice", "list")
	result := make(chan error, 1)
	go func() { result <- f.imap.Sync(t.Context(), "alice", account) }()
	awaitIMAP(t, blocked)
	// A long mailbox sync must not block opening a cached or cold message.
	rec := f.request("alice", http.MethodGet, "/email/1/body", "")
	if rec.Code != 200 {
		t.Fatalf("read during same-account sync: %d", rec.Code)
	}
	close(release)
	awaitIMAPResult(t, result)
	blocked, release = server.setBlock("alice", "body")
	bodyResult := make(chan error, 1)
	go func() { bodyResult <- f.imap.EnsureBody(t.Context(), "alice", 2) }()
	awaitIMAP(t, blocked)
	// Expunge the message while its old MIME literal is still in flight. Its
	// candidate must not be published into any remaining or new local row.
	server.mu.Lock()
	server.uids = []uint32{2}
	server.mu.Unlock()
	if err := f.imap.Sync(t.Context(), "alice", account); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-bodyResult:
		if err == nil {
			t.Fatal("expunged message body was published")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale body fetch did not finish")
	}
	if err := f.routing.WithAccountForUser(t.Context(), "alice", account, func(db *storage.DB) error {
		var count int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM messages WHERE id=2`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("expunged message recreated")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserIMAPAccountEditHookCancelsOldSessionAndQueuesFreshSync(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	f.useIMAPServer(t, server)
	id := f.accounts["alice"].ID
	blocked, release := server.setBlock("alice", "list")
	result := make(chan error, 1)
	go func() { result <- f.imap.Sync(t.Context(), "alice", id) }()
	awaitIMAP(t, blocked)
	host, portText, _ := net.SplitHostPort(server.listener.Addr().String())
	form := url.Values{"provider": {"imap"}, "email_address": {"alice@example.com"}, "display_name": {"Alice updated"}, "imap_host": {host}, "imap_port": {portText}, "imap_tls_mode": {"plaintext"}, "smtp_host": {"smtp.example.com"}, "smtp_port": {"465"}, "smtp_tls_mode": {"tls"}, "username": {"alice"}, "password": {""}, "auth_method": {"plain"}}
	rec := f.request("alice", http.MethodPost, "/api/accounts/"+id+"/edit", form.Encode())
	if rec.Code != 200 {
		t.Fatalf("account edit: %d %s", rec.Code, rec.Body.String()[:min(250, rec.Body.Len())])
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("edit did not cancel old sync")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old sync did not stop")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var count int
		if err := f.routing.WithAccountForUser(t.Context(), "alice", id, func(db *storage.DB) error {
			return db.Read().QueryRow(`SELECT COUNT(*) FROM messages WHERE account_id=?`, id).Scan(&count)
		}); err != nil {
			t.Fatal(err)
		}
		if count == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("edit hook did not start new sync")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUserIMAPColdBodyRechecksRemoteApprovalAfterFetch(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	f.useIMAPServer(t, server)
	account := f.accounts["alice"].ID
	if err := f.imap.Sync(t.Context(), "alice", account); err != nil {
		t.Fatal(err)
	}
	if err := f.routing.WithAccountForUser(t.Context(), "alice", account, func(db *storage.DB) error { return db.AllowRemoteContentForMessageForUser(t.Context(), 1, "alice") }); err != nil {
		t.Fatal(err)
	}
	blocked, release := server.setBlock("alice", "body")
	type response struct {
		code int
		body string
	}
	result := make(chan response, 1)
	go func() {
		rec := f.request("alice", http.MethodGet, "/email/1/body", "")
		result <- response{rec.Code, rec.Body.String()}
	}()
	awaitIMAP(t, blocked)
	if err := f.routing.WithAccountForUser(t.Context(), "alice", account, func(db *storage.DB) error {
		_, err := db.Write().Exec("DELETE FROM remote_content_messages WHERE message_id=1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case rec := <-result:
		if rec.code != 200 || strings.Contains(rec.body, ` src="https://images.example/private.png"`) || !strings.Contains(rec.body, `data-remote-src="https://images.example/private.png"`) {
			t.Fatalf("revoked remote approval was not respected: status %d", rec.code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("body response did not finish")
	}
}

func TestUserIMAPEmptyBodyIsCachedWithoutRepeatedFetch(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	server := newRoutedIMAPServer(t)
	f.useIMAPServer(t, server)
	if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	server.bodyOverride = "From: alice@example.com\r\nSubject: Empty message\r\nContent-Type: text/plain\r\n\r\n"
	server.mu.Unlock()
	for i := 0; i < 2; i++ {
		rec := f.request("alice", http.MethodGet, "/email/1/body", "")
		if rec.Code != 200 {
			t.Fatalf("empty body: %d", rec.Code)
		}
	}
	if count := server.bodyRequests("alice"); count != 1 {
		t.Fatalf("empty message was fetched %d times", count)
	}
}
