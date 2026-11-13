package handler

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	stdmail "net/mail"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

// fakeJMAPSender is a JMAP server for alice@example.com that answers a send
// as mode says: "accept", "forbiddenFrom" or "rateLimit" (refusals), or "drop",
// which closes the connection without answering the send request. It can't
// answer the lookup that follows, so a dropped send stays possibly sent.
type fakeJMAPSender struct {
	*httptest.Server
	mode     string
	mu       sync.Mutex
	uploaded string
	envelope map[string]any
	sends    int
}

func TestUserJMAPQueuedDraftEditKeepsBcc(t *testing.T) {
	f := newUserContactPushFixture(t, "carddav", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	id := f.accounts["alice"].ID
	if err := f.h.userAccounts.UpdateAccount(t.Context(), "alice", id, &models.CreateAccountRequest{
		Provider: "jmap", JMAPSessionURL: "https://mail.example.test/.well-known/jmap", Username: "alice@example.com", Password: "secret",
	}); err != nil {
		t.Fatal(err)
	}
	ctx := auth.ContextWithUser(t.Context(), &auth.User{ID: "alice"})
	err := f.h.userAccounts.WithUser(ctx, "alice", func(local *config.AccountStore, db *storage.DB) error {
		h := &Handler{db: db, accountStore: local, blobStore: store.NewBlobStore(t.TempDir()), syncer: mail.NewSyncOrchestrator(db, nil, nil, nil)}
		if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{{ID: "drafts", AccountID: id, RemoteID: "mb-drafts", Name: "Drafts", Role: "drafts", Selectable: true}}); err != nil {
			return err
		}
		form := url.Values{"account_id": {id}, "draft_id": {"<queued-bcc@example.com>"}, "to": {"sam@example.test"}, "bcc": {"old-hidden@example.test"}, "subject": {"Queued draft"}, "body": {"Original body"}}
		save := func() composeDraftSaveResult {
			r := httptest.NewRequest(http.MethodPost, "/compose/draft", strings.NewReader(form.Encode())).WithContext(ctx)
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			saved, failure := h.saveUserComposeDraft(ctx, r)
			if failure != nil {
				t.Fatalf("save queued draft: %+v", failure)
			}
			return saved
		}
		saved := save()
		msg, err := h.outgoingMessageFromDraft(ctx, saved.MessageID)
		if err != nil {
			return err
		}
		queued, err := h.queueOutgoingMessage(ctx, id, saved.MessageID, saved.DraftID, msg, time.Now().Add(time.Hour), true)
		if err != nil {
			return err
		}
		form.Set("bcc", `"New hidden recipient" <new-hidden@example.test>`)
		form.Set("body", "Edited body")
		save()
		edited, err := db.GetOutgoingSend(ctx, queued.ID)
		if err != nil {
			return err
		}
		stored, err := stdmail.ReadMessage(strings.NewReader(string(edited.MIMEData)))
		if err != nil {
			return err
		}
		bcc, err := stored.Header.AddressList("Bcc")
		if err != nil || len(bcc) != 1 || bcc[0].Address != "new-hidden@example.test" || bcc[0].Name != "New hidden recipient" {
			t.Fatalf("edited queued Bcc = %q, %v", stored.Header.Get("Bcc"), err)
		}
		if stored.Header.Get("Message-ID") != msg.MessageID || !strings.Contains(string(edited.MIMEData), "Edited body") || strings.Contains(string(edited.MIMEData), "old-hidden@example.test") {
			t.Fatalf("edited queued MIME = %s", edited.MIMEData)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func newFakeJMAPSender(t *testing.T, mode string) *fakeJMAPSender {
	t.Helper()
	f := &fakeJMAPSender{mode: mode}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/.well-known/jmap":
			_, _ = io.WriteString(w, `{"capabilities": {"urn:ietf:params:jmap:core": {"maxSizeUpload": 1000000}, "urn:ietf:params:jmap:mail": {}, "urn:ietf:params:jmap:submission": {}},
				"accounts": {"a1": {"name": "alice@example.com"}}, "primaryAccounts": {"urn:ietf:params:jmap:mail": "a1"},
				"username": "alice@example.com", "apiUrl": "/jmap/", "uploadUrl": "/upload/{accountId}/", "state": "s1"}`)
		case strings.HasPrefix(r.URL.Path, "/upload/"):
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.uploaded = string(body)
			f.mu.Unlock()
			_, _ = io.WriteString(w, `{"accountId": "a1", "blobId": "b-out"}`)
		case r.URL.Path == "/jmap/":
			var request struct {
				MethodCalls [][3]json.RawMessage `json:"methodCalls"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			var name string
			_ = json.Unmarshal(request.MethodCalls[0][0], &name)
			switch name {
			case "Identity/get":
				_, _ = io.WriteString(w, `{"methodResponses": [["Identity/get", {"accountId": "a1", "list": [{"id": "i-alice", "email": "alice@example.com"}]}, "0"]], "sessionState": "s1"}`)
			case "Mailbox/get":
				_, _ = io.WriteString(w, `{"methodResponses": [["Mailbox/get", {"accountId": "a1", "state": "m1", "list": [
					{"id": "mb-drafts", "name": "Drafts", "role": "drafts"}, {"id": "mb-sent", "name": "Sent", "role": "sent"}]}, "0"]], "sessionState": "s1"}`)
			case "Email/set":
				_, _ = io.WriteString(w, `{"methodResponses": [["Email/set", {"accountId": "a1", "destroyed": ["e-out"]}, "0"]], "sessionState": "s1"}`)
			case "Email/import":
				var submit map[string]any
				_ = json.Unmarshal(request.MethodCalls[1][1], &submit)
				f.mu.Lock()
				f.sends++
				f.envelope = submit["create"].(map[string]any)["send"].(map[string]any)["envelope"].(map[string]any)
				f.mu.Unlock()
				if f.mode == "drop" {
					conn, _, _ := w.(http.Hijacker).Hijack()
					_ = conn.Close()
					return
				}
				submission := `["EmailSubmission/set", {"accountId": "a1", "created": {"send": {"id": "sub-1"}}}, "1"]`
				if f.mode != "accept" {
					submission = `["EmailSubmission/set", {"accountId": "a1", "notCreated": {"send": {"type": "` + f.mode + `"}}}, "1"]`
				}
				_, _ = io.WriteString(w, `{"methodResponses": [["Email/import", {"accountId": "a1", "created": {"draft": {"id": "e-out"}}}, "0"], `+submission+`], "sessionState": "s1"}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Server.Close)
	return f
}

// The routed delivery worker sends a JMAP account's queued mail and records
// the outcome as the queue expects: sent (with its Sent copy to publish),
// retried later, failed for good, or ambiguous when the connection dropped
// mid-send and the server can't say whether it went out, which must never be
// resent automatically.
func TestUserJMAPDeliveryOutcomes(t *testing.T) {
	for _, tc := range []struct{ mode, want string }{
		{"accept", storage.OutgoingSendSent},
		{"rateLimit", storage.OutgoingSendPending},
		{"forbiddenFrom", storage.OutgoingSendFailed},
		{"drop", storage.OutgoingSendAmbiguous},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			server := newFakeJMAPSender(t, tc.mode)
			f := newUserContactPushFixture(t, "carddav", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			host, portText, _ := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
			port, _ := strconv.Atoi(portText)
			if err := f.system.AddPlaintextTransportException(t.Context(), "http", host, port, "fixture"); err != nil {
				t.Fatal(err)
			}
			if err := f.system.AddPrivateTargetException(t.Context(), "http", host, port, "fixture"); err != nil {
				t.Fatal(err)
			}
			id := f.accounts["alice"].ID
			if err := f.h.userAccounts.UpdateAccount(t.Context(), "alice", id, &models.CreateAccountRequest{
				Provider: "jmap", JMAPSessionURL: server.URL + "/.well-known/jmap", Username: "alice@example.com", Password: "secret",
			}); err != nil {
				t.Fatal(err)
			}
			msg := &message.OutgoingMessage{
				FromEmail: "alice@example.com", MessageID: "<lunch-1@example.com>", Subject: "Lunch on Friday?",
				To: []*stdmail.Address{{Address: "sam@example.test"}}, Bcc: []*stdmail.Address{{Name: "Hidden recipient", Address: "hidden@example.test"}},
				TextBody: "Are you free around noon?",
			}
			var raw string
			var sendID string
			s := &userMailDeliveryScope{runner: &userMailDelivery{h: f.h}, owner: "alice", id: id}
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(local *config.AccountStore, db *storage.DB) error {
				localHandler := &Handler{db: db, accountStore: local}
				queued, err := localHandler.queueOutgoingMessage(t.Context(), id, 0, "", msg, time.Now().Add(-time.Minute), false)
				if err != nil {
					return err
				}
				sendID = queued.ID
				raw = string(queued.MIMEData)
				if s.cfg, err = local.GetConfig(t.Context(), id); err != nil {
					return err
				}
				s.password, err = local.DecryptPassword(t.Context(), id)
				return err
			}); err != nil {
				t.Fatal(err)
			}

			more, err := s.send(t.Context())
			if !more || (err == nil) != (tc.want == storage.OutgoingSendSent) {
				t.Fatalf("send() = %v, %v", more, err)
			}
			if server.sends != 1 || server.uploaded != raw {
				t.Fatalf("server saw %d sends, upload %q", server.sends, server.uploaded)
			}
			stored, err := stdmail.ReadMessage(strings.NewReader(server.uploaded))
			if err != nil {
				t.Fatal(err)
			}
			bcc, err := stored.Header.AddressList("Bcc")
			if err != nil || len(bcc) != 1 || bcc[0].Address != "hidden@example.test" || bcc[0].Name != "Hidden recipient" {
				t.Fatalf("uploaded Sent copy lost Bcc: %q, %v", stored.Header.Get("Bcc"), err)
			}
			rcpt, _ := json.Marshal(server.envelope["rcptTo"])
			if !strings.Contains(string(rcpt), "hidden@example.test") {
				t.Fatalf("envelope lost a hidden recipient: %s", rcpt)
			}
			if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
				send, err := db.GetOutgoingSend(t.Context(), sendID)
				if err != nil {
					return err
				}
				if send.Status != tc.want || send.AttemptCount != 1 {
					t.Fatalf("send status = %s after %d attempts, want %s", send.Status, send.AttemptCount, tc.want)
				}
				if tc.want == storage.OutgoingSendSent && (send.SentCopyStatus != storage.SentCopyPending || send.SentMessageID != "<lunch-1@example.com>") {
					t.Fatalf("Sent copy = %s %q", send.SentCopyStatus, send.SentMessageID)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// Accepted, refused and ambiguous mail is never sent again by itself.
			if more, err := s.send(t.Context()); more || err != nil {
				t.Fatalf("immediate resend = %v, %v", more, err)
			}
			if server.sends != 1 {
				t.Fatal("sent the same mail twice")
			}
		})
	}
}
