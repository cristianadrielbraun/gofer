package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/translation"
)

func newOwnedMessageContentFixture(t *testing.T) *userContactPushFixture {
	t.Helper()
	f := newUserContactPushFixture(t, "carddav", http.NotFoundHandler())
	f.h.blobStore = f.h.userIMAP.Blobs()
	f.h.db = f.system
	for _, owner := range []string{"alice", "bob"} {
		candidate, err := f.h.blobStore.NewMessageVersion()
		if err != nil {
			t.Fatal(err)
		}
		path, err := candidate.StoreBodyHTML(t.Context(), f.accounts[owner].ID, 1, []byte(`<p>`+owner+` private body</p><img src="" data-remote-src="https://images.example/private.png">`))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.h.withUserDB(t.Context(), owner, func(db *storage.DB) error {
			if err := db.UpsertFolders(t.Context(), []storage.UpsertFolderInput{{ID: "inbox", AccountID: f.accounts[owner].ID, RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true}}); err != nil {
				return err
			}
			if _, err := db.Write().Exec(`INSERT INTO messages(id,account_id,internet_message_id,thread_id,subject,from_email,body_html_path) VALUES(1,?,'<same@mail.test>','thread','private','sender@example.com',?)`, f.accounts[owner].ID, path); err != nil {
				return err
			}
			if _, err := db.Write().Exec(`INSERT INTO message_folder_state(message_id,folder_id,remote_uid) VALUES(1,'inbox',1)`); err != nil {
				return err
			}
			return db.SetUISettings(t.Context(), owner, map[string]string{"translation_target_language": map[string]string{"alice": "es", "bob": "de"}[owner]})
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func ownedContentRequest(owner, path, body string) *http.Request {
	method := "GET"
	if body != "" {
		method = "POST"
	}
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.SetPathValue("id", "1")
	r.SetPathValue("messageID", "1")
	return r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: owner}))
}

func contentTranslationConnector(t *testing.T, before func(*http.Request)) *translation.GoogleWebConnector {
	t.Helper()
	return translation.NewGoogleWebConnector(&http.Client{Transport: calendarActionTransportFunc(func(r *http.Request) (*http.Response, error) {
		data := `{"x-goog-api-key":"abcdefghijklmnopqrstuvwx"}`
		if r.Method == "POST" {
			if before != nil {
				before(r)
			}
			if err := r.Context().Err(); err != nil {
				return nil, err
			}
			var payload []json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				return nil, err
			}
			var inner []json.RawMessage
			_ = json.Unmarshal(payload[0], &inner)
			var texts []string
			_ = json.Unmarshal(inner[0], &texts)
			var target string
			_ = json.Unmarshal(inner[2], &target)
			for i := range texts {
				texts[i] = "translated-" + target + " " + texts[i]
			}
			encoded, _ := json.Marshal([]any{texts, []string{"en"}})
			data = string(encoded)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(data)), Request: r}, nil
	})})
}

func TestOwnedMessageTranslationResultsAndConsent(t *testing.T) {
	f := newOwnedMessageContentFixture(t)
	f.h.googleTranslator = contentTranslationConnector(t, nil)
	for _, owner := range []string{"alice", "bob"} {
		r := httptest.NewRecorder()
		f.h.handleUserTranslateMessage(r, ownedContentRequest(owner, "/api/messages/1/translate", `{}`))
		if r.Code != 200 || !strings.Contains(r.Body.String(), owner+" private body") || !strings.Contains(r.Body.String(), "translated-"+map[string]string{"alice": "es", "bob": "de"}[owner]) {
			t.Fatal(owner, r.Code, r.Body.String())
		}
	}
	if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error { return db.AllowRemoteContentForMessageForUser(t.Context(), 1, "alice") }); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		r := httptest.NewRecorder()
		f.h.handleUserTranslatedEmailBody(r, ownedContentRequest(owner, "/email/1/body/translated", ""))
		if r.Code != 200 || !strings.Contains(r.Body.String(), owner+" private body") || (strings.Contains(r.Body.String(), ` src="https://images.example/private.png"`) != (owner == "alice")) {
			t.Fatal("translated document", owner, r.Code, r.Body.String())
		}
	}
}

func TestOwnedMessageTranslationNetworkReleaseAndStaleResult(t *testing.T) {
	for _, mode := range []string{"body-change", "owner-disable", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnedMessageContentFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			f.h.googleTranslator = contentTranslationConnector(t, func(r *http.Request) {
				once.Do(func() { close(entered) })
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})
			r := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				f.h.handleUserTranslateMessage(r, ownedContentRequest("alice", "/api/messages/1/translate", `{}`))
			}()
			var cleanup sync.Once
			unblock := func() { cleanup.Do(func() { close(release) }) }
			t.Cleanup(func() { unblock(); <-done })
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("no translation request")
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if err := f.h.withUserDB(ctx, "bob", func(*storage.DB) error { return nil }); err != nil {
				t.Fatal("network held store", err)
			}
			switch mode {
			case "body-change":
				if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE messages SET body_html_path='changed' WHERE id=1`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			case "owner-disable":
				if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				f.cancel()
			}
			unblock()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("translation did not stop")
			}
			if r.Code == 200 || strings.Contains(r.Body.String(), "alice private body") {
				t.Fatal("stale translation exposed", r.Code, r.Body.String())
			}
		})
	}
}

func TestOwnedRemoteContentPublicationAssetsAndRepeatedConsent(t *testing.T) {
	f := newOwnedMessageContentFixture(t)
	downloads := 0
	f.h.remoteResourceDownloader = func(ctx context.Context, url string) ([]byte, error) { downloads++; return []byte("owned image"), nil }
	for _, mode := range []string{"once", "email", "sender"} {
		r := httptest.NewRecorder()
		f.h.handleUserAllowRemoteContent(r, ownedContentRequest("alice", "/api/remote-content/1/allow", `{"mode":"`+mode+`"}`))
		if r.Code != 200 {
			t.Fatal(mode, r.Code, r.Body.String())
		}
		if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error {
			if db.IsRemoteContentAllowedForMessageForUser(t.Context(), 1, "alice") != (mode != "once") {
				return fmt.Errorf("wrong message approval")
			}
			if db.IsRemoteContentAllowedForSenderForUser(t.Context(), "sender@example.com", "alice") != (mode == "sender") {
				return fmt.Errorf("wrong sender approval")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if downloads != 1 {
		t.Fatal("repeated approval fetched again", downloads)
	}
	var filename string
	if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error {
		s, err := db.GetMessageContentSnapshotForUser(t.Context(), "alice", 1)
		if err != nil {
			return err
		}
		entries, err := filepath.Glob(filepath.Join(filepath.Dir(s.BodyHTMLPath), "remote_assets", "*"))
		if err != nil {
			return err
		}
		if len(entries) != 1 {
			return fmt.Errorf("missing published assets")
		}
		filename = filepath.Base(entries[0])
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		r := httptest.NewRecorder()
		req := ownedContentRequest(owner, "/api/remote-assets/1/"+filename, "")
		req.SetPathValue("filename", filename)
		f.h.handleUserRemoteAsset(r, req)
		if owner == "alice" {
			if r.Code != 200 || r.Body.String() != "owned image" {
				t.Fatal("asset missing", r.Code, r.Body.String())
			}
		} else if r.Code != 404 {
			t.Fatal("foreign asset exposed", r.Code)
		}
	}
	if err := f.h.withUserDB(t.Context(), "bob", func(db *storage.DB) error {
		if db.IsRemoteContentAllowedForSenderForUser(t.Context(), "sender@example.com", "bob") || db.IsRemoteContentAllowedForMessageForUser(t.Context(), 1, "bob") {
			return fmt.Errorf("consent crossed owner")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedRemoteContentDeniedPublicationDiscardsCandidate(t *testing.T) {
	for _, mode := range []string{"body-change", "owner-disable", "rollback", "late-principal"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnedMessageContentFixture(t)
			var before *storage.MessageContentSnapshot
			if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error {
				var err error
				before, err = db.GetMessageContentSnapshotForUser(t.Context(), "alice", 1)
				if err == nil && mode == "rollback" {
					_, err = db.Write().Exec(`CREATE TRIGGER fail_remote_consent BEFORE INSERT ON remote_content_messages BEGIN SELECT RAISE(ABORT,'injected consent failure'); END`)
				}
				if err == nil && mode == "late-principal" {
					_, err = db.Write().Exec(`CREATE TRIGGER change_remote_principal AFTER UPDATE OF body_html_path ON messages BEGIN UPDATE accounts SET provider_account_id='changed' WHERE id=NEW.account_id; END`)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			f.h.remoteResourceDownloader = func(ctx context.Context, url string) ([]byte, error) {
				if mode == "owner-disable" {
					_, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`)
					return []byte("image"), err
				}
				if mode == "body-change" {
					err := f.h.withUserDB(ctx, "alice", func(db *storage.DB) error {
						_, err := db.Write().Exec(`UPDATE messages SET subject='newer' WHERE id=1`)
						return err
					})
					return []byte("image"), err
				}
				return []byte("image"), nil
			}
			r := httptest.NewRecorder()
			f.h.handleUserAllowRemoteContent(r, ownedContentRequest("alice", "/api/remote-content/1/allow", `{"mode":"sender"}`))
			if r.Code == 200 {
				t.Fatal("denied publication succeeded", r.Body.String())
			}
			if mode == "owner-disable" {
				if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error {
				s, err := db.GetMessageContentSnapshotForUser(t.Context(), "alice", 1)
				if err != nil {
					return err
				}
				if s.BodyHTMLPath != before.BodyHTMLPath || s.ProviderAccountID != before.ProviderAccountID || db.IsRemoteContentAllowedForMessageForUser(t.Context(), 1, "alice") || db.IsRemoteContentAllowedForSenderForUser(t.Context(), "sender@example.com", "alice") {
					return fmt.Errorf("partial publication")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			paths, err := filepath.Glob(filepath.Join(filepath.Dir(filepath.Dir(before.BodyHTMLPath)), "*", "body_remote.html"))
			if err != nil || len(paths) != 0 {
				t.Fatal("unpublished candidate left behind", paths, err)
			}
		})
	}
}
