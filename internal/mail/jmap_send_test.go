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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
)

// jmapSendServer is a JMAP server that keeps the emails and submissions a
// send leaves, and answers the send request as mode says:
//   - "accept": stores, submits and answers
//   - "sentThen502": stores and submits, then a gateway answers 502
//   - "drop": stores and submits, then drops the connection
//   - "storedThen502": stores the draft but doesn't submit, then 502
//   - "503": refuses with 503 before doing anything
//   - "importError:<type>": answers the import with a method error
//   - "submitError:<type>": stores the draft, then answers the submission
//     with a method error
//   - "partialFail": stores and submits, then answers the submission with
//     serverPartialFail
type jmapSendServer struct {
	mu          sync.Mutex
	mode        string
	blobs       map[string][]byte
	emails      []*jmapSendEmail
	submissions map[string]bool
	sends       int
	destroyed   []string
	// lookupFails answers every lookup with a 500.
	lookupFails bool
}

type jmapSendEmail struct {
	id, messageID, revision string
	mailboxes, keywords     map[string]bool
	size                    int
	receivedAt              time.Time
}

func newJMAPSendServer(t *testing.T, mode string) (*jmapSendServer, *httptest.Server) {
	t.Helper()
	f := &jmapSendServer{mode: mode, blobs: map[string][]byte{}, submissions: map[string]bool{}}
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
	return f, server
}

// store adds an email the way Email/import does.
func (f *jmapSendServer) store(raw []byte, mailbox string, keywords ...string) *jmapSendEmail {
	msg, _ := netmail.ReadMessage(bytes.NewReader(raw))
	email := &jmapSendEmail{id: fmt.Sprintf("e%d", len(f.emails)+1), mailboxes: map[string]bool{mailbox: true}, keywords: map[string]bool{},
		size: len(raw), receivedAt: time.Now().UTC().Truncate(time.Second)}
	if msg != nil {
		email.messageID = strings.Trim(msg.Header.Get("Message-ID"), "<>")
		email.revision = msg.Header.Get(jmap.DraftRevisionHeader)
	}
	for _, keyword := range keywords {
		email.keywords[keyword] = true
	}
	f.emails = append(f.emails, email)
	return email
}

func (f *jmapSendServer) answer(t *testing.T, w http.ResponseWriter, calls [][]json.RawMessage) {
	var first string
	_ = json.Unmarshal(calls[0][0], &first)
	switch first {
	case "Identity/get":
		_, _ = io.WriteString(w, `{"methodResponses": [["Identity/get", {"accountId": "a1", "list": [{"id": "i1", "email": "robin@example.test"}]}, "0"]], "sessionState": "s1"}`)
		return
	case "Mailbox/get":
		_, _ = io.WriteString(w, `{"methodResponses": [["Mailbox/get", {"accountId": "a1", "list": [
			{"id": "mb-drafts", "name": "Drafts", "role": "drafts"}, {"id": "mb-sent", "name": "Sent", "role": "sent"}]}, "0"]], "sessionState": "s1"}`)
		return
	case "Email/import":
		f.sends++
		var args struct {
			Emails map[string]struct {
				BlobID string `json:"blobId"`
			} `json:"emails"`
		}
		_ = json.Unmarshal(calls[0][1], &args)
		if f.mode == "503" {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		if kind, ok := strings.CutPrefix(f.mode, "importError:"); ok {
			_, _ = fmt.Fprintf(w, `{"methodResponses": [["error", {"type": %q}, "0"], ["error", {"type": "invalidResultReference"}, "1"]], "sessionState": "s1"}`, kind)
			return
		}
		email := f.store(f.blobs[args.Emails["draft"].BlobID], "mb-drafts", jmap.KeywordDraft, jmap.KeywordSeen)
		if kind, ok := strings.CutPrefix(f.mode, "submitError:"); ok {
			_, _ = fmt.Fprintf(w, `{"methodResponses": [["Email/import", {"accountId": "a1", "created": {"draft": {"id": %q}}}, "0"],
				["error", {"type": %q}, "1"]], "sessionState": "s1"}`, email.id, kind)
			return
		}
		if f.mode != "storedThen502" {
			f.submissions[email.id] = true
			email.mailboxes = map[string]bool{"mb-sent": true}
			delete(email.keywords, jmap.KeywordDraft)
		}
		switch f.mode {
		case "drop":
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		case "sentThen502", "storedThen502":
			http.Error(w, "bad gateway", http.StatusBadGateway)
		case "partialFail":
			_, _ = fmt.Fprintf(w, `{"methodResponses": [["Email/import", {"accountId": "a1", "created": {"draft": {"id": %q}}}, "0"],
				["error", {"type": "serverPartialFail"}, "1"]], "sessionState": "s1"}`, email.id)
		default:
			_, _ = fmt.Fprintf(w, `{"methodResponses": [["Email/import", {"accountId": "a1", "created": {"draft": {"id": %q}}}, "0"],
				["EmailSubmission/set", {"accountId": "a1", "created": {"send": {"id": "sub-%s"}}}, "1"]], "sessionState": "s1"}`, email.id, email.id)
		}
		return
	}
	if f.lookupFails {
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	var responses []any
	queried := map[string][]string{}
	for _, call := range calls {
		var name, callID string
		_ = json.Unmarshal(call[0], &name)
		_ = json.Unmarshal(call[2], &callID)
		var response any
		switch name {
		case "Email/query":
			var args jmap.EmailQuery
			_ = json.Unmarshal(call[1], &args)
			var ids []string
			for _, email := range f.emails {
				switch {
				case !email.mailboxes[args.Filter.InMailbox],
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
				IDs    []string             `json:"ids"`
				RefIDs jmap.ResultReference `json:"#ids"`
			}
			_ = json.Unmarshal(call[1], &args)
			ids := args.IDs
			if args.RefIDs.ResultOf != "" {
				ids = queried[args.RefIDs.ResultOf]
			}
			var list []map[string]any
			for _, email := range f.emails {
				if slices.Contains(ids, email.id) {
					var revision any
					if email.revision != "" {
						revision = email.revision
					}
					list = append(list, map[string]any{"id": email.id, "messageId": []string{email.messageID}, "mailboxIds": email.mailboxes,
						"keywords": email.keywords, "size": email.size, "header:" + jmap.DraftRevisionHeader + ":asText": revision})
				}
			}
			response = map[string]any{"accountId": "a1", "list": list}
		case "EmailSubmission/query":
			var args struct {
				Filter struct {
					EmailIDs []string `json:"emailIds"`
				} `json:"filter"`
			}
			_ = json.Unmarshal(call[1], &args)
			ids := []string{}
			for _, id := range args.Filter.EmailIDs {
				if f.submissions[id] {
					ids = append(ids, "sub-"+id)
				}
			}
			response = map[string]any{"accountId": "a1", "ids": ids}
		case "Email/set":
			var args jmap.EmailSet
			_ = json.Unmarshal(call[1], &args)
			f.destroyed = append(f.destroyed, args.Destroy...)
			f.emails = slices.DeleteFunc(f.emails, func(email *jmapSendEmail) bool { return slices.Contains(args.Destroy, email.id) })
			response = map[string]any{"accountId": "a1", "destroyed": args.Destroy}
		default:
			t.Errorf("unexpected call %s", name)
		}
		responses = append(responses, []any{name, response, callID})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": responses, "sessionState": "s1"})
}

const jmapSendRaw = "Message-ID: <lunch-1@example.test>\r\nFrom: robin@example.test\r\nTo: sam@example.test\r\nSubject: Lunch?\r\n\r\nNoon?\r\n"

// A send whose request got no clear answer is settled by what the server
// stored: in Sent means sent, a stored but unsubmitted copy is removed and
// the send retried, and nothing stored means nothing was sent. When the
// server can't be asked, the send stays possibly sent.
func TestSendJMAPMessageSettlesUnclearOutcomes(t *testing.T) {
	for _, tc := range []struct {
		mode          string
		lookupFails   bool
		sent, retry   bool
		failed        bool
		destroyedCopy bool
	}{
		{mode: "accept", sent: true},
		{mode: "sentThen502", sent: true},
		{mode: "drop", sent: true},
		{mode: "storedThen502", retry: true, destroyedCopy: true},
		{mode: "503", retry: true},
		{mode: "sentThen502", lookupFails: true},
		{mode: "drop", lookupFails: true},
		// A method error means the server made none of that method's
		// changes: not sent, retried only when the server says it may work
		// later, and a stored copy is removed.
		{mode: "importError:accountReadOnly", failed: true},
		{mode: "importError:serverUnavailable", retry: true},
		{mode: "submitError:forbidden", failed: true, destroyedCopy: true},
		{mode: "submitError:serverFail", retry: true, destroyedCopy: true},
		// serverPartialFail may have submitted it, so the server is asked.
		{mode: "partialFail", sent: true},
	} {
		t.Run(fmt.Sprintf("%s lookupFails=%v", tc.mode, tc.lookupFails), func(t *testing.T) {
			f, server := newJMAPSendServer(t, tc.mode)
			f.lookupFails = tc.lookupFails
			// The user's saved draft of this message shares its Message-ID
			// and must survive whatever happens.
			draft := f.store([]byte("Message-ID: <lunch-1@example.test>\r\n"+jmap.DraftRevisionHeader+": r1\r\nSubject: Lunch?\r\n\r\nNoon?\r\n"),
				"mb-drafts", jmap.KeywordDraft)
			cfg := jmapTestConfig(server.URL+"/.well-known/jmap", true, true)
			id, err := SendJMAPMessage(context.Background(), cfg, "secret", "robin@example.test", []string{"sam@example.test"}, []byte(jmapSendRaw), time.Time{})
			var notSent *JMAPNotSentError
			switch {
			case tc.sent:
				if err != nil || id == "" || id == draft.id {
					t.Fatalf("SendJMAPMessage = %q, %v; want the sent email", id, err)
				}
			case tc.retry:
				if !errors.As(err, &notSent) || !notSent.Retryable {
					t.Fatalf("SendJMAPMessage = %q, %v; want not sent, retryable", id, err)
				}
			case tc.failed:
				if !errors.As(err, &notSent) || notSent.Retryable {
					t.Fatalf("SendJMAPMessage = %q, %v; want not sent, final", id, err)
				}
			default:
				if err == nil || errors.As(err, &notSent) {
					t.Fatalf("SendJMAPMessage = %q, %v; want possibly sent", id, err)
				}
			}
			if f.sends != 1 {
				t.Fatalf("sent %d times", f.sends)
			}
			if slices.Contains(f.destroyed, draft.id) {
				t.Fatal("the user's draft was removed")
			}
			if tc.destroyedCopy != (len(f.destroyed) == 1) {
				t.Fatalf("removed %v", f.destroyed)
			}
		})
	}
}

// A retry first asks whether an earlier attempt went out, and doesn't send
// again when it did; when it didn't, the retry sends.
func TestSendJMAPMessageRetryChecksTheEarlierAttempt(t *testing.T) {
	since := time.Now().Add(-time.Hour)
	f, server := newJMAPSendServer(t, "accept")
	earlier := f.store([]byte(jmapSendRaw), "mb-sent", jmap.KeywordSeen)
	earlier.receivedAt = since.Add(time.Minute).UTC().Truncate(time.Second)
	cfg := jmapTestConfig(server.URL+"/.well-known/jmap", true, true)
	id, err := SendJMAPMessage(context.Background(), cfg, "secret", "robin@example.test", []string{"sam@example.test"}, []byte(jmapSendRaw), since)
	if err != nil || id != earlier.id || f.sends != 0 {
		t.Fatalf("retry = %q, %v after %d sends; want the earlier email and no send", id, err, f.sends)
	}

	f, server = newJMAPSendServer(t, "accept")
	cfg = jmapTestConfig(server.URL+"/.well-known/jmap", true, true)
	if _, err := SendJMAPMessage(context.Background(), cfg, "secret", "robin@example.test", []string{"sam@example.test"}, []byte(jmapSendRaw), since); err != nil || f.sends != 1 {
		t.Fatalf("retry with nothing sent = %v after %d sends", err, f.sends)
	}

	// A retry that can't check doesn't send.
	f, server = newJMAPSendServer(t, "accept")
	f.lookupFails = true
	cfg = jmapTestConfig(server.URL+"/.well-known/jmap", true, true)
	_, err = SendJMAPMessage(context.Background(), cfg, "secret", "robin@example.test", []string{"sam@example.test"}, []byte(jmapSendRaw), since)
	var notSent *JMAPNotSentError
	if !errors.As(err, &notSent) || !notSent.Retryable || f.sends != 0 {
		t.Fatalf("unchecked retry = %v after %d sends", err, f.sends)
	}
}
