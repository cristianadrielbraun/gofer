package notifications

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// jmapDraftServer is a JMAP server for robin@example.test keeping whole
// messages, enough for Gofer to save, replace, find, remove and send drafts.
type jmapDraftServer struct {
	*httptest.Server
	mu     sync.Mutex
	blobs  map[string]string
	emails map[string]*jmapDraftEmail
	nextID int
	// login, password and identity are the account's sign-in and the address
	// it may send from: robin@example.test with "secret" unless changed.
	login, password, identity string
}

type jmapDraftEmail struct {
	raw, mailbox string
	keywords     map[string]bool
}

func (e *jmapDraftEmail) header(name string) string {
	msg, err := mail.ReadMessage(strings.NewReader(e.raw))
	if err != nil {
		return ""
	}
	return msg.Header.Get(name)
}

// drafts gives the raw messages in the Drafts mailbox; sent those in Sent.
func (s *jmapDraftServer) in(mailbox string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var raws []string
	for _, email := range s.emails {
		if email.mailbox == mailbox {
			raws = append(raws, email.raw)
		}
	}
	return raws
}

func newJMAPDraftServer(t *testing.T) *jmapDraftServer {
	t.Helper()
	s := &jmapDraftServer{blobs: map[string]string{}, emails: map[string]*jmapDraftEmail{}, login: "robin@example.test", password: "secret", identity: "robin@example.test"}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if user, pass, ok := r.BasicAuth(); !ok || user != s.login || pass != s.password {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/.well-known/jmap":
			_, _ = io.WriteString(w, `{"capabilities": {"urn:ietf:params:jmap:core": {"maxObjectsInGet": 50}, "urn:ietf:params:jmap:mail": {}, "urn:ietf:params:jmap:submission": {}},
				"accounts": {"a1": {"name": "robin@example.test"}}, "primaryAccounts": {"urn:ietf:params:jmap:mail": "a1"},
				"apiUrl": "/jmap/", "uploadUrl": "/upload/{accountId}/", "state": "s1"}`)
		case strings.HasPrefix(r.URL.Path, "/upload/"):
			body, _ := io.ReadAll(r.Body)
			s.nextID++
			blob := fmt.Sprintf("b%d", s.nextID)
			s.blobs[blob] = string(body)
			fmt.Fprintf(w, `{"accountId": "a1", "blobId": %q}`, blob)
		case r.URL.Path == "/jmap/":
			s.answer(t, w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Server.Close)
	return s
}

func (s *jmapDraftServer) answer(t *testing.T, w http.ResponseWriter, r *http.Request) {
	var request struct {
		MethodCalls [][3]json.RawMessage `json:"methodCalls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Errorf("decode request: %v", err)
	}
	var responses []any
	results := map[string]any{}
	for _, call := range request.MethodCalls {
		var name, callID string
		var args map[string]json.RawMessage
		_ = json.Unmarshal(call[0], &name)
		_ = json.Unmarshal(call[1], &args)
		_ = json.Unmarshal(call[2], &callID)
		var result any
		switch name {
		case "Mailbox/get":
			result = map[string]any{"accountId": "a1", "state": "m1", "list": []map[string]string{
				{"id": "mb-in", "name": "Inbox", "role": "inbox"}, {"id": "mb-drafts", "name": "Drafts", "role": "drafts"}, {"id": "mb-sent", "name": "Sent", "role": "sent"}}}
		case "Identity/get":
			result = map[string]any{"accountId": "a1", "list": []map[string]string{{"id": "i1", "email": s.identity}}}
		case "Email/changes":
			result = map[string]any{"accountId": "a1", "oldState": "s1", "newState": "s1", "hasMoreChanges": false,
				"created": []string{}, "updated": []string{}, "destroyed": []string{}}
		case "Mailbox/changes":
			result = map[string]any{"accountId": "a1", "oldState": "m1", "newState": "m1", "hasMoreChanges": false,
				"created": []string{}, "updated": []string{}, "destroyed": []string{}}
		case "Email/import":
			var emails map[string]struct {
				BlobID     string          `json:"blobId"`
				MailboxIDs map[string]bool `json:"mailboxIds"`
				Keywords   map[string]bool `json:"keywords"`
			}
			_ = json.Unmarshal(args["emails"], &emails)
			created := map[string]any{}
			for key, email := range emails {
				s.nextID++
				id := fmt.Sprintf("e%d", s.nextID)
				var mailbox string
				for m := range email.MailboxIDs {
					mailbox = m
				}
				s.emails[id] = &jmapDraftEmail{raw: s.blobs[email.BlobID], mailbox: mailbox, keywords: email.Keywords}
				created[key] = map[string]string{"id": id}
				results["#"+key] = id
			}
			result = map[string]any{"accountId": "a1", "created": created}
		case "EmailSubmission/set":
			var create map[string]struct {
				EmailID string `json:"emailId"`
			}
			_ = json.Unmarshal(args["create"], &create)
			for key, submission := range create {
				if email := s.emails[fmt.Sprint(results[submission.EmailID])]; email != nil {
					// onSuccessUpdateEmail: the sent email moves to Sent.
					email.mailbox, email.keywords = "mb-sent", map[string]bool{jmap.KeywordSeen: true}
				}
				result = map[string]any{"accountId": "a1", "created": map[string]any{key: map[string]string{"id": "sub-" + key}}}
			}
		case "Email/query":
			var filter struct {
				InMailbox string `json:"inMailbox"`
			}
			_ = json.Unmarshal(args["filter"], &filter)
			ids := []string{}
			for id, email := range s.emails {
				if email.mailbox == filter.InMailbox {
					ids = append(ids, id)
				}
			}
			results["query"] = ids
			result = map[string]any{"accountId": "a1", "ids": ids}
		case "Email/get":
			var ids []string
			if raw, ok := args["ids"]; ok {
				_ = json.Unmarshal(raw, &ids)
			} else {
				ids, _ = results["query"].([]string)
			}
			list := []map[string]any{}
			for _, id := range ids {
				email := s.emails[id]
				if email == nil {
					continue
				}
				revision := strings.TrimSpace(email.header(jmap.DraftRevisionHeader))
				list = append(list, map[string]any{
					"id": id, "keywords": email.keywords,
					"messageId":                            []string{strings.Trim(email.header("Message-ID"), "<> ")},
					"header:X-Gofer-Draft-Revision:asText": revision,
				})
			}
			result = map[string]any{"accountId": "a1", "state": "s1", "list": list}
		case "Email/set":
			var destroy []string
			_ = json.Unmarshal(args["destroy"], &destroy)
			destroyed, notDestroyed := []string{}, map[string]any{}
			for _, id := range destroy {
				if s.emails[id] == nil {
					notDestroyed[id] = map[string]string{"type": "notFound"}
					continue
				}
				delete(s.emails, id)
				destroyed = append(destroyed, id)
			}
			result = map[string]any{"accountId": "a1", "destroyed": destroyed, "notDestroyed": notDestroyed}
		default:
			t.Errorf("unexpected JMAP method %s", name)
			result = map[string]any{}
		}
		responses = append(responses, []any{name, result, callID})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": responses, "sessionState": "s1"})
}

// newJMAPDraftFixture gives Alice a JMAP account on a fake server, with mail
// sync off so only her queued drafts and sends reach it.
func newJMAPDraftFixture(t *testing.T) (*userStorageFixture, *jmapDraftServer) {
	t.Helper()
	server := newJMAPDraftServer(t)
	f := newUserStorageFixtureMode(t, true)
	endpoint, err := jmap.ParseEndpoint(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.system.AddPlaintextTransportException(t.Context(), "http", endpoint.Host, endpoint.Port, "administrator"); err != nil {
		t.Fatal(err)
	}
	if err := f.system.AddPrivateTargetException(t.Context(), "http", endpoint.Host, endpoint.Port, "administrator"); err != nil {
		t.Fatal(err)
	}
	account, err := f.accountStore.CreateAccount(t.Context(), "alice", &models.CreateAccountRequest{
		Provider: "jmap", EmailAddress: "robin@example.test", JMAPSessionURL: server.URL + "/.well-known/jmap",
		Username: "robin@example.test", Password: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.accounts["alice"] = account
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		if _, err := db.Write().Exec(`UPDATE accounts SET email_sync_enabled=0 WHERE id=?`, account.ID); err != nil {
			return err
		}
		for _, folder := range []struct{ remote, role string }{{"mb-in", "inbox"}, {"mb-drafts", "drafts"}, {"mb-sent", "sent"}} {
			if _, err := db.Write().Exec(`INSERT INTO folders(id,account_id,remote_id,provider_remote_id,name,role) VALUES(?,?,?,?,?,?)`, account.ID+"-"+folder.role, account.ID, folder.remote, folder.remote, folder.role, folder.role); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return f, server
}

// A draft written in Gofer reaches the JMAP account's Drafts mailbox, an edit
// replaces it there, discarding removes it, and sending a draft leaves the
// sent message in Sent and no draft behind.
func TestUserJMAPDraftsSyncEditDiscardAndSend(t *testing.T) {
	f, server := newJMAPDraftFixture(t)
	account := f.accounts["alice"].ID
	draft := func(key, body string) {
		t.Helper()
		f.compose(t, "alice", "/compose/draft", url.Values{"account_id": {account}, "draft_id": {key}, "to": {"sam@example.test"}, "subject": {"Notes for Friday"}, "body": {body}})
	}
	synced := func(want ...string) func() bool {
		return func() bool {
			drafts := server.in("mb-drafts")
			if f.pendingProviderDrafts(t, "alice") != 0 || len(drafts) != len(want) {
				return false
			}
			for i := range want {
				if !strings.Contains(drafts[i], want[i]) {
					return false
				}
			}
			return true
		}
	}

	draft("<notes-1@example.test>", "Bring the slides")
	waitUserIMAP(t, synced("Bring the slides"))
	draft("<notes-1@example.test>", "Bring the slides and the cake")
	waitUserIMAP(t, synced("Bring the slides and the cake"))

	f.compose(t, "alice", "/compose/draft/discard", url.Values{"account_id": {account}, "draft_id": {"<notes-1@example.test>"}})
	waitUserIMAP(t, synced())

	draft("<invite-1@example.test>", "Lunch on Friday?")
	waitUserIMAP(t, synced("Lunch on Friday?"))
	rec := f.request("alice", http.MethodPost, "/compose", url.Values{"account_id": {account}, "draft_id": {"<invite-1@example.test>"}, "to": {"sam@example.test"}, "subject": {"Notes for Friday"}, "body": {"Lunch on Friday?"}}.Encode())
	var sent struct {
		ID string `json:"send_id"`
	}
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &sent) != nil || sent.ID == "" {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	waitUserIMAP(t, func() bool {
		status, copy, _ := f.providerSendStatus(t, "alice", sent.ID)
		return status == "sent" && copy == "complete" && synced()()
	})
	if sentMail := server.in("mb-sent"); len(sentMail) != 1 || !strings.Contains(sentMail[0], "Lunch on Friday?") {
		t.Fatalf("Sent holds %d messages", len(sentMail))
	}
}
