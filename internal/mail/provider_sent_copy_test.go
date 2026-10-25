package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	netmail "net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
)

// jmapSentServer is a JMAP server with a Sent mailbox, keeping the messages
// imported into it.
type jmapSentServer struct {
	mu       sync.Mutex
	blobs    map[string][]byte
	imported []jmapSentEmail
}

type jmapSentEmail struct {
	id, messageID string
	mailboxes     map[string]bool
	keywords      map[string]bool
}

func newJMAPSentServer(t *testing.T) (*jmapSentServer, string) {
	t.Helper()
	t.Setenv("GOFER_JMAP_EXPERIMENT", "true")
	f := &jmapSentServer{blobs: map[string][]byte{}}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/.well-known/jmap":
			_, _ = fmt.Fprintf(w, `{"capabilities": {%q: {}, %q: {}, %q: {}},
				"accounts": {"a1": {"name": "robin@example.test"}}, "primaryAccounts": {%q: "a1"},
				"username": "robin@example.test", "apiUrl": %q, "uploadUrl": %q, "state": "s1"}`,
				jmap.CapabilityCore, jmap.CapabilityMail, jmap.CapabilitySubmission, jmap.CapabilityMail,
				server.URL+"/jmap/", server.URL+"/upload/{accountId}/")
		case strings.HasPrefix(r.URL.Path, "/upload/"):
			body, _ := io.ReadAll(r.Body)
			blob := fmt.Sprintf("blob-%d", len(f.blobs))
			f.blobs[blob] = body
			_, _ = fmt.Fprintf(w, `{"accountId": "a1", "blobId": %q}`, blob)
		case r.URL.Path == "/jmap/":
			f.answer(t, w, decodeJMAPCalls(t, r))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return f, server.URL + "/.well-known/jmap"
}

func (f *jmapSentServer) answer(t *testing.T, w http.ResponseWriter, calls [][]json.RawMessage) {
	var responses []any
	queried := map[string][]string{}
	for _, call := range calls {
		var name, callID string
		_ = json.Unmarshal(call[0], &name)
		_ = json.Unmarshal(call[2], &callID)
		var response any
		switch name {
		case "Mailbox/get":
			response = map[string]any{"accountId": "a1", "list": []map[string]string{
				{"id": "mb-inbox", "name": "Inbox", "role": "inbox"}, {"id": "mb-sent", "name": "Sent", "role": "sent"}}}
		case "Email/query":
			var args jmap.EmailQuery
			_ = json.Unmarshal(call[1], &args)
			ids := []string{}
			for _, email := range f.imported {
				if email.mailboxes[args.Filter.InMailbox] && len(args.Filter.Header) == 2 && email.messageID == args.Filter.Header[1] {
					ids = append(ids, email.id)
				}
			}
			queried[callID] = ids
			response = map[string]any{"accountId": "a1", "ids": ids}
		case "Email/get":
			var args struct {
				RefIDs jmap.ResultReference `json:"#ids"`
			}
			_ = json.Unmarshal(call[1], &args)
			list := []map[string]any{}
			for _, email := range f.imported {
				for _, id := range queried[args.RefIDs.ResultOf] {
					if id == email.id {
						list = append(list, map[string]any{"id": email.id, "messageId": []string{email.messageID}})
					}
				}
			}
			response = map[string]any{"accountId": "a1", "list": list}
		case "Email/import":
			var args struct {
				Emails map[string]struct {
					BlobID     string          `json:"blobId"`
					MailboxIDs map[string]bool `json:"mailboxIds"`
					Keywords   map[string]bool `json:"keywords"`
				} `json:"emails"`
			}
			_ = json.Unmarshal(call[1], &args)
			created := map[string]any{}
			for key, email := range args.Emails {
				msg, _ := netmail.ReadMessage(bytes.NewReader(f.blobs[email.BlobID]))
				stored := jmapSentEmail{id: fmt.Sprintf("e%d", len(f.imported)+1), mailboxes: email.MailboxIDs, keywords: email.Keywords}
				if msg != nil {
					stored.messageID = strings.Trim(msg.Header.Get("Message-ID"), "<>")
				}
				f.imported = append(f.imported, stored)
				created[key] = map[string]string{"id": stored.id}
			}
			response = map[string]any{"accountId": "a1", "created": created}
		default:
			t.Errorf("unexpected call %s", name)
		}
		responses = append(responses, []any{name, response, callID})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": responses, "sessionState": "s1"})
}

func TestFileJMAPSentCopyFilesOnce(t *testing.T) {
	f, sessionURL := newJMAPSentServer(t)
	cfg := jmapTestConfig(sessionURL, true, true)
	raw := []byte("Message-ID: <shop-1@example.test>\r\nFrom: Shop <shop@example.org>\r\nTo: sam@example.test\r\nSubject: From the shop\r\n\r\nHello\r\n")
	date := time.Now().UTC().Truncate(time.Second)
	first, err := FileJMAPSentCopy(context.Background(), cfg, "secret", raw, "<shop-1@example.test>", date)
	if err != nil || first == "" {
		t.Fatalf("FileJMAPSentCopy() = %q, %v", first, err)
	}
	again, err := FileJMAPSentCopy(context.Background(), cfg, "secret", raw, "<shop-1@example.test>", date)
	if err != nil || again != first {
		t.Fatalf("second FileJMAPSentCopy() = %q, %v; want %q", again, err, first)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.imported) != 1 || !f.imported[0].mailboxes["mb-sent"] || len(f.imported[0].mailboxes) != 1 || !f.imported[0].keywords[jmap.KeywordSeen] {
		t.Fatalf("imported = %+v", f.imported)
	}
}
