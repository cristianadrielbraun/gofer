package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	netmail "net/mail"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
)

// jmapImportServer is a JMAP server that keeps the mailboxes it is asked to
// create and the emails it is asked to import.
type jmapImportServer struct {
	mu        sync.Mutex
	mailboxes []jmap.Mailbox
	imported  []map[string]any
	sessions  int
	failNext  bool
	blobs     map[string][]byte
	stored    []jmapImportStored
	// hangUp stores the next import and drops the connection unanswered.
	hangUp bool
	// noHeaderSearch refuses header filters, and redate ignores the
	// receivedAt given, as servers may.
	noHeaderSearch, redate bool
	queries                int
}

type jmapImportStored struct {
	id, mailbox, messageID string
	receivedAt             time.Time
}

func newJMAPImportServer(t *testing.T, maxUpload int, mailboxes ...jmap.Mailbox) (*jmapImportServer, *httptest.Server) {
	t.Helper()
	f := &jmapImportServer{mailboxes: mailboxes, blobs: map[string][]byte{}}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/.well-known/jmap":
			f.sessions++
			_, _ = fmt.Fprintf(w, `{"capabilities": {%q: {"maxSizeUpload": %d}, %q: {}},
				"accounts": {"a1": {"name": "robin@example.test", "isPersonal": true}},
				"primaryAccounts": {%q: "a1"}, "username": "robin@example.test",
				"apiUrl": %q, "uploadUrl": %q, "state": "s1"}`,
				jmap.CapabilityCore, maxUpload, jmap.CapabilityMail, jmap.CapabilityMail,
				server.URL+"/jmap/", server.URL+"/upload/{accountId}/")
		case "/upload/a1/":
			body, _ := io.ReadAll(r.Body)
			blob := fmt.Sprintf("blob-%d", len(f.blobs))
			f.blobs[blob] = body
			_, _ = fmt.Fprintf(w, `{"accountId": "a1", "blobId": %q, "type": "message/rfc822", "size": %d}`, blob, len(body))
		case "/jmap/":
			if f.failNext {
				f.failNext = false
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			f.answer(t, w, decodeJMAPCalls(t, r))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return f, server
}

func decodeJMAPCalls(t *testing.T, r *http.Request) [][]json.RawMessage {
	t.Helper()
	var request struct {
		MethodCalls [][]json.RawMessage `json:"methodCalls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Fatalf("request is not JSON: %v", err)
	}
	return request.MethodCalls
}

func (f *jmapImportServer) answer(t *testing.T, w http.ResponseWriter, calls [][]json.RawMessage) {
	var responses []any
	queried := map[string][]string{}
	for _, call := range calls {
		var name, callID string
		_ = json.Unmarshal(call[0], &name)
		_ = json.Unmarshal(call[2], &callID)
		var response any
		switch name {
		case "Mailbox/get":
			response = map[string]any{"accountId": "a1", "state": "s1", "list": f.mailboxes}
		case "Mailbox/set":
			var args struct {
				Create map[string]struct {
					Name     string  `json:"name"`
					ParentID *string `json:"parentId"`
				} `json:"create"`
			}
			_ = json.Unmarshal(call[1], &args)
			created := map[string]any{}
			for key, mailbox := range args.Create {
				id := fmt.Sprintf("mb-new-%d", len(f.mailboxes))
				parent := ""
				if mailbox.ParentID != nil {
					parent = *mailbox.ParentID
				}
				f.mailboxes = append(f.mailboxes, jmap.Mailbox{ID: id, Name: mailbox.Name, ParentID: parent})
				created[key] = map[string]string{"id": id}
			}
			response = map[string]any{"accountId": "a1", "created": created}
		case "Email/import":
			var args struct {
				Emails map[string]map[string]any `json:"emails"`
			}
			_ = json.Unmarshal(call[1], &args)
			created := map[string]any{}
			for key, email := range args.Emails {
				f.imported = append(f.imported, email)
				id := fmt.Sprintf("e%d", len(f.imported))
				received := time.Now().UTC().Truncate(time.Second)
				if at, ok := email["receivedAt"].(string); ok && !f.redate {
					received, _ = time.Parse(time.RFC3339, at)
				}
				var messageID string
				if blob, ok := email["blobId"].(string); ok {
					if msg, err := netmail.ReadMessage(bytes.NewReader(f.blobs[blob])); err == nil {
						messageID = strings.Trim(msg.Header.Get("Message-ID"), "<>")
					}
				}
				for mailbox := range email["mailboxIds"].(map[string]any) {
					f.stored = append(f.stored, jmapImportStored{id: id, mailbox: mailbox, messageID: messageID, receivedAt: received})
				}
				created[key] = map[string]string{"id": id}
			}
			if f.hangUp {
				f.hangUp = false
				conn, _, _ := w.(http.Hijacker).Hijack()
				_ = conn.Close()
				return
			}
			response = map[string]any{"accountId": "a1", "created": created}
		case "Email/query":
			f.queries++
			var args jmap.EmailQuery
			_ = json.Unmarshal(call[1], &args)
			if len(args.Filter.Header) > 0 && f.noHeaderSearch {
				name, response = "error", map[string]string{"type": "unsupportedFilter"}
				break
			}
			var ids []string
			for _, email := range f.stored {
				switch {
				case email.mailbox != args.Filter.InMailbox,
					len(args.Filter.Header) == 2 && !strings.Contains(email.messageID, args.Filter.Header[1]),
					args.Filter.After != nil && email.receivedAt.Before(*args.Filter.After),
					args.Filter.Before != nil && !email.receivedAt.Before(*args.Filter.Before):
					continue
				}
				ids = append(ids, email.id)
			}
			queried[callID] = ids
			response = map[string]any{"accountId": "a1", "ids": ids}
		case "Email/get":
			var args struct {
				IDs jmap.ResultReference `json:"#ids"`
			}
			_ = json.Unmarshal(call[1], &args)
			ids, ok := queried[args.IDs.ResultOf]
			if !ok {
				name, response = "error", map[string]string{"type": "invalidResultReference"}
				break
			}
			var list []map[string]any
			for _, email := range f.stored {
				if slices.Contains(ids, email.id) {
					list = append(list, map[string]any{"id": email.id, "messageId": []string{email.messageID}})
				}
			}
			response = map[string]any{"accountId": "a1", "state": "s1", "list": list}
		default:
			t.Errorf("unexpected call %s", name)
		}
		responses = append(responses, []any{name, response, callID})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": responses, "sessionState": "s1"})
}

// A path finds the mailboxes it names, including the inbox by its role, and
// creates the ones missing, each inside the one before.
func TestJMAPImportEnsuresMailboxPaths(t *testing.T) {
	f, server := newJMAPImportServer(t, 0,
		jmap.Mailbox{ID: "mb-inbox", Name: "Inbox", Role: "inbox"},
		jmap.Mailbox{ID: "mb-work", Name: "Work"},
	)
	imports := NewJMAPImport(jmapTestConfig(server.URL+"/.well-known/jmap", true, true), "secret")
	ctx := context.Background()
	for path, want := range map[string]string{"INBOX": "mb-inbox", "Work": "mb-work"} {
		if id, err := imports.EnsureMailbox(ctx, path); err != nil || id != want {
			t.Fatalf("EnsureMailbox(%q) = %q, %v", path, id, err)
		}
	}
	id, err := imports.EnsureMailbox(ctx, "Work/2024/Receipts")
	if err != nil {
		t.Fatal(err)
	}
	again, err := imports.EnsureMailbox(ctx, "Work/2024/Receipts")
	if err != nil || again != id {
		t.Fatalf("second EnsureMailbox = %q, %v; first %q", again, err, id)
	}
	want := []jmap.Mailbox{
		{ID: "mb-inbox", Name: "Inbox", Role: "inbox"},
		{ID: "mb-work", Name: "Work"},
		{ID: "mb-new-2", Name: "2024", ParentID: "mb-work"},
		{ID: "mb-new-3", Name: "Receipts", ParentID: "mb-new-2"},
	}
	if !reflect.DeepEqual(f.mailboxes, want) || id != "mb-new-3" {
		t.Fatalf("mailboxes = %+v, id %q", f.mailboxes, id)
	}
	if f.sessions != 1 {
		t.Fatalf("signed in %d times", f.sessions)
	}
}

// Import keeps the keywords and date, signs in again after a failed request,
// and refuses a message over the upload limit as final.
func TestJMAPImportStoresMessages(t *testing.T) {
	f, server := newJMAPImportServer(t, 64)
	imports := NewJMAPImport(jmapTestConfig(server.URL+"/.well-known/jmap", true, true), "secret")
	ctx := context.Background()
	raw := []byte("Subject: hi\r\n\r\nhello\r\n")
	date := time.Date(2023, 7, 1, 8, 0, 0, 0, time.UTC)
	if err := imports.Import(ctx, "mb-inbox", raw, "", []string{jmap.KeywordSeen}, date); err != nil {
		t.Fatal(err)
	}
	f.failNext = true
	if err := imports.Import(ctx, "mb-inbox", raw, "", nil, time.Time{}); err == nil {
		t.Fatal("a failed request was not reported")
	}
	if err := imports.Import(ctx, "mb-inbox", raw, "", nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if f.sessions != 2 {
		t.Fatalf("signed in %d times, want again after the failure", f.sessions)
	}
	first := f.imported[0]
	if first["blobId"] != "blob-0" || !reflect.DeepEqual(first["mailboxIds"], map[string]any{"mb-inbox": true}) ||
		!reflect.DeepEqual(first["keywords"], map[string]any{"$seen": true}) || first["receivedAt"] != "2023-07-01T08:00:00Z" {
		t.Fatalf("import = %v", first)
	}
	err := imports.Import(ctx, "mb-inbox", make([]byte, 65), "", nil, time.Time{})
	var refused *jmap.ImportError
	if !errors.As(err, &refused) || refused.Type != "tooLarge" || len(f.imported) != 2 {
		t.Fatalf("too large = %v after %d imports", err, len(f.imported))
	}
}

// A retry after an import that got no answer finds the copy the server
// stored instead of storing another, by header search or by date, whichever
// the server supports; a message the server didn't store is imported.
func TestJMAPImportRetriesAnUnansweredImportOnce(t *testing.T) {
	raw := []byte("Message-ID: <m1@example.test>\r\nSubject: hi\r\n\r\nhello\r\n")
	date := time.Date(2023, 7, 1, 8, 0, 0, 0, time.UTC)
	for _, server := range []struct {
		name                   string
		noHeaderSearch, redate bool
		date                   time.Time
	}{
		{name: "header search"},
		{name: "date", noHeaderSearch: true, date: date},
		{name: "server's date", noHeaderSearch: true, redate: true, date: date},
		{name: "no date", noHeaderSearch: true},
	} {
		t.Run(server.name, func(t *testing.T) {
			f, httpServer := newJMAPImportServer(t, 0)
			f.noHeaderSearch, f.redate = server.noHeaderSearch, server.redate
			imports := NewJMAPImport(jmapTestConfig(httpServer.URL+"/.well-known/jmap", true, true), "secret")
			ctx := context.Background()
			// Another copy of the message elsewhere doesn't count.
			if err := imports.Import(ctx, "mb-other", raw, "m1@example.test", nil, server.date); err != nil {
				t.Fatal(err)
			}
			f.hangUp = true
			if err := imports.Import(ctx, "mb-inbox", raw, "m1@example.test", nil, server.date); err == nil {
				t.Fatal("the dropped connection was not reported")
			}
			if err := imports.Import(ctx, "mb-inbox", raw, "m1@example.test", nil, server.date); err != nil {
				t.Fatal(err)
			}
			if len(f.imported) != 2 || f.queries == 0 {
				t.Fatalf("imported %d times after %d searches, want 2", len(f.imported), f.queries)
			}
		})
	}
	t.Run("not stored", func(t *testing.T) {
		f, httpServer := newJMAPImportServer(t, 0)
		imports := NewJMAPImport(jmapTestConfig(httpServer.URL+"/.well-known/jmap", true, true), "secret")
		ctx := context.Background()
		f.failNext = true
		if err := imports.Import(ctx, "mb-inbox", raw, "m1@example.test", nil, date); err == nil {
			t.Fatal("the failure was not reported")
		}
		if err := imports.Import(ctx, "mb-inbox", raw, "m1@example.test", nil, date); err != nil {
			t.Fatal(err)
		}
		if len(f.imported) != 1 || f.queries == 0 {
			t.Fatalf("imported %d times after %d searches, want 1", len(f.imported), f.queries)
		}
	})
}
