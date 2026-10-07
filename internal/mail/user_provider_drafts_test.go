package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	stdmail "net/mail"
	"strings"
	"sync"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
)

type userDraftAPI struct {
	mu                            sync.Mutex
	provider                      string
	entries                       map[string]map[string]UserProviderDraft
	raw                           map[string]map[string][]byte
	created, versions             map[string]int
	loseAck, badReply, repeatPage bool
}

func (a *userDraftAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
	if a.provider == "outlook" && !strings.Contains(r.Header.Get("Prefer"), `IdType="ImmutableId"`) {
		http.Error(w, "missing immutable identity", 400)
		return
	}
	wire, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	defer a.mu.Unlock()
	base := "/users/me/drafts"
	if a.provider == "outlook" {
		base = "/me/messages"
	}
	w.Header().Set("Content-Type", "application/json")
	if (r.Method == "POST" || r.Method == "PUT") && strings.HasPrefix(r.URL.Path, base) {
		var raw []byte
		var err error
		if a.provider == "gmail" {
			var body struct{ Message struct{ Raw string } }
			_ = json.Unmarshal(wire, &body)
			raw, err = base64.RawURLEncoding.DecodeString(body.Message.Raw)
		} else {
			if r.Header.Get("Content-Type") != "text/plain" {
				http.Error(w, "wrong MIME type", 400)
				return
			}
			raw, err = base64.StdEncoding.DecodeString(string(wire))
		}
		if err != nil {
			http.Error(w, "bad base64", 400)
			return
		}
		mime, err := stdmail.ReadMessage(bytes.NewReader(raw))
		if err != nil || mime.Header.Get("Bcc") == "" || !strings.Contains(mime.Header.Get("From"), owner+"@mail.test") || !bytes.Contains(raw, []byte(owner+" private body")) {
			http.Error(w, "bad MIME", 400)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, base+"/")
		if r.Method == "POST" {
			a.created[owner]++
			id = fmt.Sprintf("draft-%d", a.created[owner])
		}
		if r.Method == "PUT" && a.entries[owner][id].ID == "" {
			http.NotFound(w, r)
			return
		}
		a.versions[owner]++
		msgID := id
		if a.provider == "gmail" {
			msgID = fmt.Sprintf("message-%d", a.versions[owner])
		}
		d := UserProviderDraft{ID: id, MessageID: msgID, InternetMessageID: mime.Header.Get("Message-ID"), RevisionToken: mime.Header.Get("X-Gofer-Draft-Revision")}
		if a.entries[owner] == nil {
			a.entries[owner] = map[string]UserProviderDraft{}
			a.raw[owner] = map[string][]byte{}
		}
		a.entries[owner][id], a.raw[owner][id] = d, raw
		if a.loseAck {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		if a.badReply {
			_, _ = fmt.Fprint(w, `{`)
			return
		}
		a.reply(w, d)
		return
	}
	if r.Method == "GET" && (r.URL.Path == base || r.URL.Path == "/me/mailFolders/drafts/messages") {
		if (a.provider == "gmail" && r.URL.Query().Get("q") != "rfc822msgid:<same-draft@mail.test>") || (a.provider == "outlook" && r.URL.Query().Get("$filter") != "internetMessageId eq '<same-draft@mail.test>' and isDraft eq true") {
			http.Error(w, "wrong draft identity filter", 400)
			return
		}
		var ids []map[string]string
		for id := range a.entries[owner] {
			ids = append(ids, map[string]string{"id": id})
		}
		key := "drafts"
		if a.provider == "outlook" {
			key = "value"
		}
		reply := map[string]any{key: ids}
		if a.repeatPage {
			// Repeated empty pages must never be accepted as a complete search.
			reply[key] = []map[string]string{}
			if a.provider == "gmail" {
				reply["nextPageToken"] = "again"
			} else {
				reply["@odata.nextLink"] = outlookGraphBaseURL + r.URL.Path + "?again=1"
			}
		}
		_ = json.NewEncoder(w).Encode(reply)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, base+"/")
	d := a.entries[owner][id]
	if d.ID == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method == "GET" {
		a.reply(w, d)
		return
	}
	if r.Method == "DELETE" {
		delete(a.entries[owner], id)
		delete(a.raw[owner], id)
		w.WriteHeader(204)
		return
	}
	http.NotFound(w, r)
}

func (a *userDraftAPI) reply(w http.ResponseWriter, d UserProviderDraft) {
	headers := []map[string]string{{"name": "Message-ID", "value": d.InternetMessageID}, {"name": "x-gofer-draft-revision", "value": d.RevisionToken}}
	if a.provider == "gmail" {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": d.ID, "message": map[string]any{"id": d.MessageID, "payload": map[string]any{"headers": headers}}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": d.ID, "internetMessageId": d.InternetMessageID, "isDraft": true, "internetMessageHeaders": headers})
}

type userDraftFixture struct {
	worker *UserIMAP
	ids    map[string]string
	api    *userDraftAPI
}

func newUserDraftFixture(t *testing.T, provider string) *userDraftFixture {
	t.Helper()
	f := &userDraftFixture{api: &userDraftAPI{provider: provider, entries: map[string]map[string]UserProviderDraft{}, raw: map[string]map[string][]byte{}, created: map[string]int{}, versions: map[string]int{}}}
	if provider == "gmail" {
		prior := newUserGmailFixture(t)
		f.worker, f.ids = prior.worker, prior.ids
	} else {
		prior := newUserOutlookFixture(t)
		f.worker, f.ids = prior.worker, prior.ids
	}
	server := httptest.NewServer(f.api)
	t.Cleanup(server.Close)
	if provider == "gmail" {
		prior := gmailAPIBaseURL
		gmailAPIBaseURL = server.URL
		t.Cleanup(func() { gmailAPIBaseURL = prior })
	} else {
		prior := outlookGraphBaseURL
		outlookGraphBaseURL = server.URL
		t.Cleanup(func() { outlookGraphBaseURL = prior })
	}
	return f
}

func (f *userDraftFixture) run(t *testing.T, owner string, fn func(context.Context, *UserProviderMail) error) error {
	t.Helper()
	return f.worker.operation(t.Context(), owner, f.ids[owner], 0, manualSyncTimeout, func(ctx context.Context) error {
		scope, err := f.worker.snapshot(ctx, owner, f.ids[owner])
		if err != nil {
			return err
		}
		return fn(ctx, &UserProviderMail{scope: scope, lifetime: ctx})
	})
}

func draftMIME(t *testing.T, owner, revision string) []byte {
	t.Helper()
	raw, err := message.BuildMIMEMessageForIMAPDraft(&message.OutgoingMessage{MessageID: "<same-draft@mail.test>", FromEmail: owner + "@mail.test", To: []*stdmail.Address{{Address: "to@mail.test"}}, Bcc: []*stdmail.Address{{Address: "hidden@mail.test"}}, TextBody: owner + " private body", Subject: owner + " private draft"}, revision)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestUserProviderDraftOwnedRevisionsFindAndDelete(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f := newUserDraftFixture(t, provider)
			for _, owner := range []string{"alice", "bob"} {
				if err := f.run(t, owner, func(ctx context.Context, p *UserProviderMail) error {
					first, err := p.SaveDraft(ctx, "", draftMIME(t, owner, "first"))
					if err != nil {
						return err
					}
					id := ""
					if provider == "gmail" {
						id = first.ID
					}
					second, err := p.SaveDraft(ctx, id, draftMIME(t, owner, "second"))
					if err != nil {
						return err
					}
					if provider == "gmail" && (first.ID != second.ID || first.MessageID == second.MessageID) {
						return errors.New("Gmail container/message identities conflated")
					}
					found, err := p.FindDrafts(ctx, second.InternetMessageID)
					if err != nil {
						return err
					}
					want := 1
					if provider == "outlook" {
						want = 2
					}
					if len(found) != want {
						return fmt.Errorf("found %d drafts want %d", len(found), want)
					}
					if provider == "outlook" {
						if err := p.DeleteDraft(ctx, first); err != nil {
							return err
						}
					}
					changed := second
					changed.RevisionToken = "stale"
					if err := p.DeleteDraft(ctx, changed); err == nil {
						return errors.New("stale draft deletion was accepted")
					}
					if err := p.DeleteDraft(ctx, second); err != nil {
						return err
					}
					return p.DeleteDraft(ctx, second) // A missing acknowledged revision stays deleted.
				}); err != nil {
					t.Fatal(err)
				}
			}
			if f.api.created["alice"] != f.api.created["bob"] {
				t.Fatal("owner isolation changed provider IDs")
			}
		})
	}
}

func TestUserProviderDraftUncertainCreateReconcilesRevision(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, failure := range []string{"lost-ack", "malformed-reply"} {
			t.Run(provider+"/"+failure, func(t *testing.T) {
				f := newUserDraftFixture(t, provider)
				f.api.loseAck, f.api.badReply = failure == "lost-ack", failure == "malformed-reply"
				if err := f.run(t, "alice", func(ctx context.Context, p *UserProviderMail) error {
					_, err := p.SaveDraft(ctx, "", draftMIME(t, "alice", "uncertain"))
					if !errors.Is(err, ErrUserProviderDraftUncertain) {
						return fmt.Errorf("expected uncertain creation, got %v", err)
					}
					found, err := p.FindDrafts(ctx, "<same-draft@mail.test>")
					if err != nil {
						return err
					}
					if len(found) != 1 || found[0].RevisionToken != "uncertain" {
						return errors.New("accepted revision could not be recovered")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if f.api.created["alice"] != 1 {
					t.Fatal("revision recovery created a duplicate")
				}
			})
		}
	}
}

func TestUserProviderDraftSearchAndSessionBoundaries(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f := newUserDraftFixture(t, provider)
			f.api.repeatPage = true
			if err := f.run(t, "alice", func(ctx context.Context, p *UserProviderMail) error {
				if _, err := p.FindDrafts(ctx, "<same-draft@mail.test>"); err == nil {
					return errors.New("incomplete search accepted")
				}
				life, cancel := context.WithCancel(ctx)
				cancel()
				p.lifetime = life
				_, err := p.SaveDraft(ctx, "", draftMIME(t, "alice", "expired"))
				if !errors.Is(err, context.Canceled) {
					return fmt.Errorf("expired capability accepted: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if f.api.created["alice"] != 0 {
				t.Fatal("expired session made a provider request")
			}
		})
	}
}
