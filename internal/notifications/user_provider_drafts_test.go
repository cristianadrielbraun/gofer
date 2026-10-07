package notifications

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	stdmail "net/mail"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	gomail "github.com/emersion/go-message/mail"
)

type providerDraftFixtureEntry struct {
	ID, MessageID, InternetID, Revision string
	Raw                                 []byte
}

func (a *providerActionAPI) serveDraftLocked(w http.ResponseWriter, r *http.Request, owner string, wire []byte, payload map[string]json.RawMessage) bool {
	base := "/users/me/drafts"
	if a.provider == "outlook" {
		base = "/me/messages"
	}
	creating := r.Method == "POST" && r.URL.Path == base
	updating := a.provider == "gmail" && r.Method == "PUT" && strings.HasPrefix(r.URL.Path, base+"/")
	if creating || updating {
		if a.draftStatus != 0 {
			http.Error(w, "synthetic draft rejection", a.draftStatus)
			return true
		}
		var raw []byte
		var err error
		if a.provider == "gmail" {
			var msg struct{ Raw string }
			_ = json.Unmarshal(payload["message"], &msg)
			raw, err = base64.RawURLEncoding.DecodeString(msg.Raw)
		} else {
			raw, err = base64.StdEncoding.DecodeString(string(wire))
			if r.Header.Get("Content-Type") != "text/plain" {
				http.Error(w, "wrong draft MIME type", 400)
				return true
			}
		}
		mime, parseErr := stdmail.ReadMessage(bytes.NewReader(raw))
		if err != nil || parseErr != nil || mime.Header.Get("Message-ID") == "" || mime.Header.Get("X-Gofer-Draft-Revision") == "" {
			http.Error(w, "invalid draft MIME", 400)
			return true
		}
		if a.drafts == nil {
			a.drafts = map[string]map[string]providerDraftFixtureEntry{}
			a.draftCreates = map[string]int{}
			a.draftVersions = map[string]int{}
		}
		if a.drafts[owner] == nil {
			a.drafts[owner] = map[string]providerDraftFixtureEntry{}
		}
		id := strings.TrimPrefix(r.URL.Path, base+"/")
		if creating {
			a.draftCreates[owner]++
			id = fmt.Sprintf("draft-%d", a.draftCreates[owner])
		}
		if updating && a.drafts[owner][id].ID == "" {
			http.NotFound(w, r)
			return true
		}
		a.draftVersions[owner]++
		messageID := id
		if a.provider == "gmail" {
			messageID = fmt.Sprintf("draft-message-%d", a.draftVersions[owner])
		}
		d := providerDraftFixtureEntry{ID: id, MessageID: messageID, InternetID: mime.Header.Get("Message-ID"), Revision: mime.Header.Get("X-Gofer-Draft-Revision"), Raw: raw}
		a.drafts[owner][id] = d
		if a.loseDraftAck {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return true
		}
		if a.badDraftReply {
			_, _ = fmt.Fprint(w, `{`)
			return true
		}
		a.replyDraftLocked(w, d)
		return true
	}
	if r.Method == "GET" && (r.URL.Path == "/users/me/drafts" || r.URL.Path == "/me/mailFolders/drafts/messages") {
		internetID := strings.TrimPrefix(r.URL.Query().Get("q"), "rfc822msgid:")
		key := "drafts"
		if a.provider == "outlook" {
			key = "value"
			internetID = strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(r.URL.Query().Get("$filter"), "internetMessageId eq '"), "' and isDraft eq true"), "''", "'")
		}
		ids := []map[string]string{}
		for _, d := range a.drafts[owner] {
			if a.hideDrafts {
				continue
			}
			if d.InternetID == internetID {
				ids = append(ids, map[string]string{"id": d.ID})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{key: ids})
		return true
	}
	id := strings.TrimPrefix(r.URL.Path, base+"/")
	d := a.drafts[owner][id]
	if !strings.HasPrefix(r.URL.Path, base+"/") || (a.provider == "outlook" && d.ID == "" && !strings.HasPrefix(id, "draft-")) {
		return false
	}
	if d.ID == "" {
		http.NotFound(w, r)
		return true
	}
	if r.Method == "GET" {
		a.replyDraftLocked(w, d)
		return true
	}
	if r.Method == "DELETE" {
		if a.deleteDraftStatus != 0 {
			http.Error(w, "synthetic draft delete rejection", a.deleteDraftStatus)
			return true
		}
		delete(a.drafts[owner], id)
		w.WriteHeader(204)
		return true
	}
	return false
}

func (a *providerActionAPI) replyDraftLocked(w http.ResponseWriter, d providerDraftFixtureEntry) {
	headers := []map[string]string{{"name": "Message-ID", "value": d.InternetID}, {"name": "X-Gofer-Draft-Revision", "value": d.Revision}}
	if a.provider == "gmail" {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": d.ID, "message": map[string]any{"id": d.MessageID, "payload": map[string]any{"headers": headers}}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": d.ID, "internetMessageId": d.InternetID, "isDraft": true, "internetMessageHeaders": headers})
}

func newProviderDraftFixture(t *testing.T, provider string) (*userStorageFixture, *providerActionAPI) {
	t.Helper()
	f, api := newProviderSendFixture(t, provider)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			_, err := db.Write().Exec(`INSERT INTO folders(id,account_id,remote_id,provider_remote_id,name,role) VALUES(?,?,?,?,?,'drafts')`, f.accounts[owner].ID+"-drafts", f.accounts[owner].ID, "DRAFT", "DRAFT", "Drafts")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, api
}

func (a *providerActionAPI) draftCount(owner string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.drafts[owner])
}

func (f *userStorageFixture) saveProviderDraft(t *testing.T, owner, key, body string) int64 {
	t.Helper()
	f.compose(t, owner, "/compose/draft", url.Values{"account_id": {f.accounts[owner].ID}, "draft_id": {key}, "to": {"to@mail.test"}, "bcc": {"hidden@mail.test"}, "subject": {owner + " private draft"}, "body": {body}})
	var id int64
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		var err error
		id, err = db.GetMessageLocalIDByInternetIDInternal(t.Context(), f.accounts[owner].ID, key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *userStorageFixture) pendingProviderDrafts(t *testing.T, owner string) int {
	t.Helper()
	var n int
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT count(*) FROM gofer_provider_draft_operations`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestUserProviderDraftHTTPIsolationEditAndDiscard(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderDraftFixture(t, provider)
			key := "<overlapping-draft@mail.test>"
			api.blockOwner = "alice"
			alice := f.saveProviderDraft(t, "alice", key, "first alice")
			awaitIMAP(t, api.entered)
			bob := f.saveProviderDraft(t, "bob", key, "private bob")
			waitUserIMAP(t, func() bool { return f.pendingProviderDrafts(t, "bob") == 0 })
			if alice != bob {
				t.Fatal("local draft IDs did not overlap")
			}
			f.saveProviderDraft(t, "alice", key, "newest alice")
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.UpsertProviderSyncMessages(t.Context(), []storage.ProviderSyncMessage{{AccountID: f.accounts["alice"].ID, FolderID: f.accounts["alice"].ID + "-drafts", ProviderMessageID: "older-provider-id", InternetMessageID: key, Subject: "stale remote subject", IsDraft: true}})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			api.unblock()
			waitUserIMAP(t, func() bool { return f.pendingProviderDrafts(t, "alice") == 0 })
			if api.draftCount("alice") != 1 || api.draftCount("bob") != 1 {
				t.Fatal("replacement duplicated a draft")
			}
			api.mu.Lock()
			entries := make([]providerDraftFixtureEntry, 0, len(api.drafts["alice"]))
			for _, d := range api.drafts["alice"] {
				entries = append(entries, d)
			}
			api.mu.Unlock()
			for _, d := range entries {
				if !bytes.Contains(d.Raw, []byte("newest alice")) || !bytes.Contains(d.Raw, []byte("hidden@mail.test")) {
					t.Fatal("newer draft MIME/Bcc lost")
				}
			}
			for _, owner := range []string{"alice", "bob"} {
				rec := f.request(owner, "GET", fmt.Sprintf("/api/drafts/%d", alice), "")
				if rec.Code != 200 || !strings.Contains(rec.Body.String(), owner+" private draft") {
					t.Fatalf("owned draft read: %d %s", rec.Code, rec.Body.String())
				}
			}
			if rec := f.request("bob", "POST", "/compose/draft/discard", url.Values{"account_id": {f.accounts["alice"].ID}, "draft_id": {key}}.Encode()); rec.Code != 404 {
				t.Fatal("foreign discard accepted")
			}
			if rec := f.request("alice", "DELETE", fmt.Sprintf("/api/drafts/%d", alice), ""); rec.Code != 200 {
				t.Fatalf("discard: %d %s", rec.Code, rec.Body.String())
			}
			waitUserIMAP(t, func() bool { return f.pendingProviderDrafts(t, "alice") == 0 && api.draftCount("alice") == 0 })
			if api.draftCount("bob") != 1 {
				t.Fatal("discard crossed owner boundary")
			}
		})
	}
}

func TestUserProviderDraftLostCreateAckReconcilesWithoutDuplicate(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderDraftFixture(t, provider)
			api.loseDraftAck = true
			f.saveProviderDraft(t, "alice", "<lost-draft@mail.test>", "uncertain first")
			waitUserIMAP(t, func() bool { return api.draftCount("alice") == 1 })
			if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			var id int64
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				return db.Read().QueryRow(`SELECT id FROM gofer_provider_draft_operations WHERE account_id=?`, f.accounts["alice"].ID).Scan(&id)
			}); err != nil {
				t.Fatal(err)
			}
			if response := f.request("alice", "POST", fmt.Sprintf("/api/mail-operations/provider_draft:%d/retry", id), ""); response.Code != 200 {
				t.Fatal("owned draft reconciliation retry", response.Code, response.Body.String())
			}
			if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			if f.pendingProviderDrafts(t, "alice") != 0 {
				t.Fatal("acknowledged draft was not reconciled")
			}
			api.mu.Lock()
			n := api.draftCreates["alice"]
			api.mu.Unlock()
			if n != 1 {
				t.Fatal("lost acknowledgment caused another POST")
			}
		})
	}
}

func TestUserProviderDraftScheduledEditSendAndCleanup(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderDraftFixture(t, provider)
			at := time.Now().UTC().Add(time.Hour).Truncate(5 * time.Minute)
			form := url.Values{"account_id": {f.accounts["alice"].ID}, "draft_id": {"<scheduled-provider@mail.test>"}, "to": {"to@mail.test"}, "subject": {"scheduled provider"}, "body": {"original scheduled"}, "schedule_timezone": {"UTC"}, "schedule_date": {at.Format("2006-01-02")}, "schedule_hour": {at.Format("15")}, "schedule_minute": {at.Format("04")}}
			f.compose(t, "alice", "/compose/schedule", form)
			waitUserIMAP(t, func() bool { return f.pendingProviderDrafts(t, "alice") == 0 })
			var send storage.OutgoingSend
			load := func() {
				t.Helper()
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					id, err := db.GetMessageLocalIDByInternetIDInternal(t.Context(), f.accounts["alice"].ID, form.Get("draft_id"))
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
			_ = json.Unmarshal(send.MessageJSON, &original)
			form.Set("body", "updated scheduled")
			f.compose(t, "alice", "/compose/draft", form)
			load()
			var updated map[string]any
			_ = json.Unmarshal(send.MessageJSON, &updated)
			if updated["message_id"] != original["message_id"] || updated["text_body"] != "updated scheduled" {
				t.Fatal("waiting provider send snapshot was not refreshed")
			}
			if api.acceptedCount("alice") != 0 {
				t.Fatal("provider schedule sent early")
			}
			if rec := f.request("alice", "POST", "/api/outgoing-sends/"+send.ID+"/retry-now", ""); rec.Code != 200 {
				t.Fatalf("retry now: %d %s", rec.Code, rec.Body.String())
			}
			waitUserIMAP(t, func() bool {
				_, copy, _ := f.providerSendStatus(t, "alice", send.ID)
				return copy == "complete" && f.pendingProviderDrafts(t, "alice") == 0
			})
			if api.acceptedCount("alice") != 1 || api.draftCount("alice") != 0 {
				t.Fatal("delivery did not remove the matching remote draft")
			}
			if rec := f.request("alice", "GET", fmt.Sprintf("/api/drafts/%d", send.MessageID), ""); rec.Code != 404 {
				t.Fatal("matching delivered draft remains visible")
			}
		})
	}
}

func (f *userStorageFixture) providerDraftsDue(t *testing.T, owner string) {
	t.Helper()
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE gofer_provider_draft_operations SET next_attempt_at=CURRENT_TIMESTAMP`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserProviderDraftCandidateSurvivesPublicationFailure(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderDraftFixture(t, provider)
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`CREATE TRIGGER reject_provider_draft_completion BEFORE DELETE ON gofer_provider_draft_operations BEGIN SELECT RAISE(ABORT,'injected completion failure'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			f.saveProviderDraft(t, "alice", "<publication-failure@mail.test>", "confirmed remote MIME")
			waitUserIMAP(t, func() bool {
				var n int
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					return db.Read().QueryRow(`SELECT count(*) FROM gofer_provider_draft_operations WHERE status='ambiguous' AND candidate_id!='' AND length(mime_data)>0`).Scan(&n)
				}); err != nil {
					t.Fatal(err)
				}
				return n == 1
			})
			if api.draftCount("alice") != 1 {
				t.Fatal("remote revision not retained")
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`DROP TRIGGER reject_provider_draft_completion`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			f.saveProviderDraft(t, "bob", "<eviction@mail.test>", "evict Alice's store")
			waitUserIMAP(t, func() bool { return f.pendingProviderDrafts(t, "bob") == 0 })
			f.providerDraftsDue(t, "alice")
			if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			n := api.draftCreates["alice"]
			api.mu.Unlock()
			if f.pendingProviderDrafts(t, "alice") != 0 || n != 1 {
				t.Fatal("cache eviction/publication failure created another remote draft")
			}
		})
	}
}

func TestUserProviderDraftGraphReplacementRetainsCandidateUntilOldDelete(t *testing.T) {
	f, api := newProviderDraftFixture(t, "outlook")
	key := "<replace-graph@mail.test>"
	f.saveProviderDraft(t, "alice", key, "first")
	waitUserIMAP(t, func() bool { return f.pendingProviderDrafts(t, "alice") == 0 })
	api.mu.Lock()
	api.deleteDraftStatus = 503
	api.mu.Unlock()
	f.saveProviderDraft(t, "alice", key, "replacement")
	waitUserIMAP(t, func() bool { return api.draftCount("alice") == 2 })
	// Wait for the serialized pass so changing the fixture cannot race its response.
	_ = f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID)
	api.mu.Lock()
	api.deleteDraftStatus = 0
	api.mu.Unlock()
	f.providerDraftsDue(t, "alice")
	if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	created := api.draftCreates["alice"]
	api.mu.Unlock()
	if f.pendingProviderDrafts(t, "alice") != 0 || api.draftCount("alice") != 1 || created != 2 {
		t.Fatal("old deletion retry recreated replacement")
	}
}

func TestUserProviderDraftDiscardDuringCreateKeepsCleanup(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderDraftFixture(t, provider)
			api.blockOwner = "alice"
			id := f.saveProviderDraft(t, "alice", "<discard-in-flight@mail.test>", "discarded MIME")
			awaitIMAP(t, api.entered)
			if rec := f.request("alice", "DELETE", fmt.Sprintf("/api/drafts/%d", id), ""); rec.Code != 200 {
				t.Fatalf("discard: %d %s", rec.Code, rec.Body.String())
			}
			api.unblock()
			waitUserIMAP(t, func() bool { return f.pendingProviderDrafts(t, "alice") == 0 && api.draftCount("alice") == 0 })
			if rec := f.request("alice", "GET", fmt.Sprintf("/api/drafts/%d", id), ""); rec.Code != 404 {
				t.Fatal("late create restored discarded draft")
			}
		})
	}
}

func TestUserProviderDraftEditDuringSendPreservesNewerDraft(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderDraftFixture(t, provider)
			key := "<edit-during-send@mail.test>"
			id := f.saveProviderDraft(t, "alice", key, "original body")
			waitUserIMAP(t, func() bool { return f.pendingProviderDrafts(t, "alice") == 0 })
			api.mu.Lock()
			api.blockOwner = "alice"
			api.mu.Unlock()
			result := f.compose(t, "alice", "/compose", url.Values{"account_id": {f.accounts["alice"].ID}, "draft_id": {key}, "to": {"to@mail.test"}, "bcc": {"hidden@mail.test"}, "subject": {"alice private draft"}, "body": {"original body"}})
			awaitIMAP(t, api.entered)
			f.saveProviderDraft(t, "alice", key, "newer unsent body")
			api.unblock()
			waitUserIMAP(t, func() bool {
				_, copy, _ := f.providerSendStatus(t, "alice", result["send_id"].(string))
				return copy == "complete" && f.pendingProviderDrafts(t, "alice") == 0
			})
			if rec := f.request("alice", "GET", fmt.Sprintf("/api/drafts/%d", id), ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "newer unsent body") {
				t.Fatalf("newer draft lost: %d %s", rec.Code, rec.Body.String())
			}
			if api.draftCount("alice") != 1 || api.acceptedCount("alice") != 1 {
				t.Fatal("newer remote draft removed or delivery duplicated")
			}
		})
	}
}

func TestUserProviderDraftBacklogBeyondBoundedPass(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderDraftFixture(t, provider)
			api.blockOwner = "alice"
			f.saveProviderDraft(t, "alice", "<backlog-0@mail.test>", "first")
			awaitIMAP(t, api.entered)
			for i := 1; i < 12; i++ {
				f.saveProviderDraft(t, "alice", fmt.Sprintf("<backlog-%d@mail.test>", i), "queued body")
			}
			f.saveProviderDraft(t, "bob", "<other-owner@mail.test>", "independent")
			waitUserIMAP(t, func() bool { return f.pendingProviderDrafts(t, "bob") == 0 })
			if _, err := f.system.Write().Exec(`INSERT INTO gofer_account_poll_schedule(account_id,next_due_ms) SELECT account_id,9999999999999 FROM gofer_account_directory WHERE account_id!=? ON CONFLICT(account_id) DO UPDATE SET next_due_ms=excluded.next_due_ms`, f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			if err := f.imap.Start(mail.UserIMAPBackgroundOptions{PollInterval: 24 * time.Hour, ScanInterval: 20 * time.Millisecond, DisableIDLE: true}); err != nil {
				t.Fatal(err)
			}
			api.unblock()
			waitUserIMAP(t, func() bool { return f.pendingProviderDrafts(t, "alice") == 0 })
			if api.draftCount("alice") != 12 || api.draftCount("bob") != 1 {
				t.Fatal("bounded passes lost or duplicated drafts")
			}
		})
	}
}

func TestUserProviderDraftNormalTextSendRemovesMatchingDraft(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderDraftFixture(t, provider)
			key := "<normal-text-draft@mail.test>"
			id := f.saveProviderDraft(t, "alice", key, "same <text> & body")
			waitUserIMAP(t, func() bool { return f.pendingProviderDrafts(t, "alice") == 0 })
			result := f.compose(t, "alice", "/compose", url.Values{"account_id": {f.accounts["alice"].ID}, "draft_id": {key}, "to": {"to@mail.test"}, "bcc": {"hidden@mail.test"}, "subject": {"alice private draft"}, "body": {"same <text> & body"}})
			waitUserIMAP(t, func() bool {
				_, copy, _ := f.providerSendStatus(t, "alice", result["send_id"].(string))
				return copy == "complete" && f.pendingProviderDrafts(t, "alice") == 0
			})
			if rec := f.request("alice", "GET", fmt.Sprintf("/api/drafts/%d", id), ""); rec.Code != 404 || api.draftCount("alice") != 0 {
				t.Fatalf("unchanged sent draft retained: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestUserProviderDraftInvisibleUncertainCreateWaitsForRevision(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderDraftFixture(t, provider)
			api.badDraftReply = true
			key := "<hidden-uncertain@mail.test>"
			f.saveProviderDraft(t, "alice", key, "older revision")
			waitUserIMAP(t, func() bool {
				var n int
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					return db.Read().QueryRow(`SELECT count(*) FROM gofer_provider_draft_operations WHERE create_attempted=1 AND status='ambiguous'`).Scan(&n)
				}); err != nil {
					t.Fatal(err)
				}
				return n == 1
			})
			api.mu.Lock()
			api.hideDrafts = true
			api.badDraftReply = false
			api.mu.Unlock()
			f.saveProviderDraft(t, "alice", key, "newer revision")
			f.providerDraftsDue(t, "alice")
			// Saving also wakes the account worker. It may consume this due
			// attempt before manual Sync acquires the gate, so inspect the
			// durable result rather than requiring that particular call to fail.
			_ = f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID)
			waitUserIMAP(t, func() bool {
				var n int
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					return db.Read().QueryRow(`SELECT count(*) FROM gofer_provider_draft_operations WHERE create_attempted=1 AND status='ambiguous' AND attempt_count>=2`).Scan(&n)
				}); err != nil {
					t.Fatal(err)
				}
				return n == 1
			})
			api.mu.Lock()
			created := api.draftCreates["alice"]
			api.hideDrafts = false
			api.mu.Unlock()
			if created != 1 || f.pendingProviderDrafts(t, "alice") != 2 {
				t.Fatal("uncertain create was repeated or newer revision overwritten")
			}
			f.providerDraftsDue(t, "alice")
			if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			if f.pendingProviderDrafts(t, "alice") != 0 || api.draftCount("alice") != 1 {
				t.Fatal("visible revision did not release later edit")
			}
		})
	}
}

func TestUserProviderDraftReceivedIdentityIsPreservedDuringReplacement(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderDraftFixture(t, provider)
			key := "<received-provider-draft@mail.test>"
			messageID := "legacy-draft"
			if provider == "gmail" {
				messageID = "legacy-message"
			}
			api.mu.Lock()
			api.drafts = map[string]map[string]providerDraftFixtureEntry{"alice": {"legacy-draft": {ID: "legacy-draft", MessageID: messageID, InternetID: key}}, "bob": {}}
			api.draftCreates = map[string]int{}
			api.draftVersions = map[string]int{"alice": 1}
			api.mu.Unlock()
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.UpsertProviderSyncMessages(t.Context(), []storage.ProviderSyncMessage{{AccountID: f.accounts["alice"].ID, FolderID: f.accounts["alice"].ID + "-drafts", ProviderMessageID: messageID, InternetMessageID: key, Subject: "received draft", IsDraft: true}})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			id := f.saveProviderDraft(t, "alice", key, "edited received body")
			waitUserIMAP(t, func() bool { return f.pendingProviderDrafts(t, "alice") == 0 })
			api.mu.Lock()
			created := api.draftCreates["alice"]
			_, legacy := api.drafts["alice"]["legacy-draft"]
			api.mu.Unlock()
			if provider == "gmail" && (created != 0 || !legacy) {
				t.Fatal("Gmail received draft container was not reused")
			}
			if provider == "outlook" && (created != 1 || legacy) {
				t.Fatal("Graph received draft was not safely replaced")
			}
			if api.draftCount("alice") != 1 {
				t.Fatal("received draft replacement duplicated remote content")
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var remote string
				if err := db.Read().QueryRow(`SELECT remote_message_id FROM messages WHERE id=?`, id).Scan(&remote); err != nil {
					return err
				}
				if remote == messageID {
					return fmt.Errorf("replacement kept stale message identity")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserProviderDraftCooldownAndAccountDeletion(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderDraftFixture(t, provider)
			api.throttle = true
			f.saveProviderDraft(t, "alice", "<cooldown-draft@mail.test>", "waiting")
			waitUserIMAP(t, func() bool {
				until, err := f.routing.ProviderRetryUntil(t.Context(), "alice", f.accounts["alice"].ID)
				if err != nil {
					t.Fatal(err)
				}
				return until.After(time.Now())
			})
			api.mu.Lock()
			api.throttle = false
			api.mu.Unlock()
			if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err == nil {
				t.Fatal("manual sync bypassed durable provider cooldown")
			}
			if api.draftCount("alice") != 0 {
				t.Fatal("manual sync bypassed durable provider cooldown")
			}
			if _, err := f.system.Write().Exec(`UPDATE gofer_account_provider_retry SET retry_until_ms=0 WHERE account_id=?`, f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			f.providerDraftsDue(t, "alice")
			if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			if api.draftCount("alice") != 1 {
				t.Fatal("expired cooldown did not permit draft synchronization")
			}
			api.mu.Lock()
			api.blockOwner = "alice"
			api.mu.Unlock()
			f.saveProviderDraft(t, "alice", "<deleting-draft@mail.test>", "will be canceled")
			awaitIMAP(t, api.entered)
			if err := f.accountStore.DeleteAccount(t.Context(), "alice", f.accounts["alice"].ID, f.imap.Cleanup); err != nil {
				t.Fatal(err)
			}
			if api.draftCount("alice") != 1 {
				t.Fatal("deleting account accepted the blocked creation")
			}
			f.saveProviderDraft(t, "bob", "<remaining-owner@mail.test>", "continues independently")
			waitUserIMAP(t, func() bool { return f.pendingProviderDrafts(t, "bob") == 0 })
		})
	}
}

func TestUserProviderDraftAttachmentsRemainOwnedAndImmutable(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderDraftFixture(t, provider)
			api.blockOwner = "alice"
			key := "<owned-attachment@mail.test>"
			for _, owner := range []string{"alice", "bob"} {
				id, _, err := f.blobs.StoreComposeAttachment(t.Context(), owner, "private.txt", strings.NewReader(owner+" private bytes"))
				if err != nil {
					t.Fatal(err)
				}
				form := url.Values{"account_id": {f.accounts[owner].ID}, "draft_id": {key}, "to": {"to@mail.test"}, "body": {owner + " body"}, "attachment_id": {id}, "attachment_filename": {"private.txt"}, "attachment_content_type": {"text/plain"}}
				f.compose(t, owner, "/compose/draft", form)
				if owner == "alice" {
					awaitIMAP(t, api.entered)
					foreign := url.Values{"account_id": {f.accounts["bob"].ID}, "draft_id": {key}, "attachment_id": {id}, "attachment_filename": {"private.txt"}, "attachment_content_type": {"text/plain"}}
					if rec := f.request("bob", "POST", "/compose/draft", foreign.Encode()); rec.Code != 404 {
						t.Fatalf("foreign provider draft attachment accepted: %d %s", rec.Code, rec.Body.String())
					}
				}
				// Delivery must use the published immutable version even after
				// the original compose upload has been removed.
				if err := f.blobs.DeleteComposeAttachment(owner, id); err != nil {
					t.Fatal(err)
				}
			}
			waitUserIMAP(t, func() bool { return f.pendingProviderDrafts(t, "bob") == 0 })
			api.unblock()
			waitUserIMAP(t, func() bool { return f.pendingProviderDrafts(t, "alice") == 0 })
			for _, owner := range []string{"alice", "bob"} {
				api.mu.Lock()
				var raw []byte
				for _, d := range api.drafts[owner] {
					raw = append([]byte(nil), d.Raw...)
				}
				api.mu.Unlock()
				reader, err := gomail.CreateReader(bytes.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				attachments := 0
				for {
					part, err := reader.NextPart()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if header, ok := part.Header.(*gomail.AttachmentHeader); ok {
						name, err := header.Filename()
						data, readErr := io.ReadAll(part.Body)
						if err != nil || readErr != nil || name != "private.txt" || string(data) != owner+" private bytes" {
							t.Fatalf("wrong owned remote attachment: %q %q %v %v", name, data, err, readErr)
						}
						attachments++
					}
				}
				if attachments != 1 {
					t.Fatalf("remote attachments=%d", attachments)
				}
				if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
					id, err := db.GetMessageLocalIDByInternetIDInternal(t.Context(), f.accounts[owner].ID, key)
					if err != nil {
						return err
					}
					email, err := db.GetEmailByIDInternal(t.Context(), fmt.Sprint(id))
					if err != nil {
						return err
					}
					if len(email.Attachments) != 1 {
						return fmt.Errorf("local attachments=%d", len(email.Attachments))
					}
					data, err := os.ReadFile(email.Attachments[0].StoragePath)
					if err != nil || string(data) != owner+" private bytes" {
						return fmt.Errorf("wrong owned local attachment: %q %v", data, err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestUserProviderDraftMailboxChangeRejectsLateAcceptance(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderDraftFixture(t, provider)
			api.blockOwner = "alice"
			id := f.saveProviderDraft(t, "alice", "<old-mailbox@mail.test>", "old mailbox draft")
			awaitIMAP(t, api.entered)
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE accounts SET provider_account_id='replacement-subject' WHERE id=?`, f.accounts["alice"].ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			api.unblock()
			waitUserIMAP(t, func() bool { return api.draftCount("alice") == 1 })
			// Wait for the old gated session and verify its accepted response
			// cannot be published into the replacement mailbox's local rows.
			if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var bound, candidates int
				if err := db.Read().QueryRow(`SELECT count(*) FROM messages WHERE id=? AND COALESCE(remote_message_id,'')!=''`, id).Scan(&bound); err != nil {
					return err
				}
				if err := db.Read().QueryRow(`SELECT count(*) FROM gofer_provider_draft_operations WHERE candidate_id!=''`).Scan(&candidates); err != nil {
					return err
				}
				if bound != 0 || candidates != 0 {
					return fmt.Errorf("old mailbox result published: bound=%d candidates=%d", bound, candidates)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			f.saveProviderDraft(t, "bob", "<independent-mailbox@mail.test>", "continues independently")
			waitUserIMAP(t, func() bool { return f.pendingProviderDrafts(t, "bob") == 0 })
		})
	}
}
