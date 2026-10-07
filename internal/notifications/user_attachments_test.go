package notifications

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func attachmentFixtureMIME(owner string) string {
	return "Message-ID: <" + owner + "@provider.test>\r\nSubject: private\r\nFrom: sender@provider.test\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=parts\r\n\r\n--parts\r\nContent-Type: text/plain\r\n\r\nprivate body\r\n--parts\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=report.txt\r\n\r\n" + owner + " report\r\n--parts\r\nContent-Type: image/png\r\nContent-Disposition: inline; filename=inline.png\r\nContent-ID: <shared-cid>\r\n\r\n" + owner + " image\r\n--parts--\r\n"
}

func (a *providerActionAPI) serveAttachmentLocked(w http.ResponseWriter, r *http.Request, owner string) bool {
	if (r.URL.Path == "/users/me/messages/m1" && r.URL.Query().Get("format") == "raw") || r.URL.Path == "/me/messages/m1/$value" {
		raw := attachmentFixtureMIME(owner)
		if a.duplicateAttachments {
			raw = strings.Replace(raw, "Content-Type: image/png\r\nContent-Disposition: inline; filename=inline.png\r\nContent-ID: <shared-cid>", "Content-Type: text/plain\r\nContent-Disposition: attachment; filename=report.txt", 1)
			raw = strings.Replace(raw, owner+" image", owner+" second", 1)
		}
		if a.badAttachment {
			raw = strings.Replace(raw, "<"+owner+"@provider.test>", "<different@provider.test>", 1)
		}
		if a.provider == "gmail" {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "m1", "raw": base64.RawURLEncoding.EncodeToString([]byte(raw))})
		} else {
			w.Header().Set("Content-Type", "message/rfc822")
			_, _ = fmt.Fprint(w, raw)
		}
		return true
	}
	if !strings.Contains(r.URL.Path, "/messages/m1/attachments/") {
		return false
	}
	content := owner + " report"
	if strings.Contains(r.URL.Path, "inline-part") {
		content = owner + " image"
	}
	if a.badAttachment {
		content = "wrong size"
	}
	if a.provider == "gmail" {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": base64.RawURLEncoding.EncodeToString([]byte(content)), "size": len(content)})
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = fmt.Fprint(w, content)
	}
	return true
}

func newUserAttachmentFixture(t *testing.T, provider string, direct bool) (*userStorageFixture, *providerActionAPI) {
	t.Helper()
	f, api := newProviderActionFixture(t, provider)
	api.attachmentAPI = true
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			for i, part := range []struct {
				name, typ, cid, remote, content string
				inline                          bool
			}{{"report.txt", "text/plain", "", "report-part", owner + " report", false}, {"inline.png", "image/png", "shared-cid", "inline-part", owner + " image", true}} {
				remote := part.remote
				if !direct {
					remote = ""
				}
				if _, err := db.Write().Exec(`INSERT INTO attachments(id,message_id,filename,content_type,size_bytes,content_id,inline,storage_path,provider_remote_id) VALUES(?,1,?,?,?,?,?,'',?)`, i+1, part.name, part.typ, len(part.content), part.cid, part.inline, remote); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, api
}

func (f *userStorageFixture) attachmentPath(t *testing.T, owner string, id int64) string {
	t.Helper()
	var path string
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT storage_path FROM attachments WHERE id=?`, id).Scan(&path)
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUserAttachmentRecoveryHTTPPreservesOwnedIDsAndViews(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, direct := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/direct=%v", provider, direct), func(t *testing.T) {
				f, _ := newUserAttachmentFixture(t, provider, direct)
				for _, owner := range []string{"alice", "bob"} {
					for _, view := range []struct{ path, content string }{{"/api/attachments/1/download", owner + " report"}, {"/api/inline-content/1/shared-cid", owner + " image"}, {"/api/attachments/2/preview", owner + " image"}} {
						rec := f.request(owner, "GET", view.path, "")
						if rec.Code != 200 || rec.Body.String() != view.content {
							t.Fatalf("%s %s: %d %q", owner, view.path, rec.Code, rec.Body.String())
						}
						if rec.Header().Get("Cache-Control") != "private, no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
							t.Fatal("private attachment response headers lost")
						}
					}
					if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
						var count int
						if err := db.Read().QueryRow(`SELECT count(*) FROM attachments WHERE message_id=1 AND id IN(1,2)`).Scan(&count); err != nil {
							return err
						}
						if count != 2 {
							return fmt.Errorf("recovery changed attachment IDs: %d", count)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if rec := f.request(owner, "GET", "/api/attachments/1/preview", ""); rec.Code != 404 {
						t.Fatal("non-image preview accepted", rec.Code)
					}
				}
				if rec := f.request("alice", "GET", "/api/attachments/999/download", ""); rec.Code != 404 {
					t.Fatal("unknown attachment exposed", rec.Code)
				}
			})
		}
	}
}

func TestUserAttachmentRecoverySavedMIMEWorksDuringDraftAndCooldown(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newUserAttachmentFixture(t, provider, true)
			api.missing = true
			version, err := f.blobs.NewMessageVersion()
			if err != nil {
				t.Fatal(err)
			}
			path, err := version.StoreRaw(t.Context(), f.accounts["alice"].ID, 1, []byte(attachmentFixtureMIME("alice")))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				if _, err := db.Write().Exec(`UPDATE messages SET raw_path=?,remote_message_id='' WHERE id=1`, path); err != nil {
					return err
				}
				if _, err := db.Write().Exec(`INSERT INTO gofer_provider_draft_states(account_id,draft_key,provider,mailbox_subject,local_message_id,folder_id,local_revision) VALUES(?, '<alice@provider.test>',?,'same-subject',1,?,'pending')`, f.accounts["alice"].ID, provider, f.accounts["alice"].ID+"-inbox"); err != nil {
					return err
				}
				_, err := db.Write().Exec(`INSERT INTO gofer_provider_draft_operations(account_id,draft_key,kind,revision_token,mime_data) VALUES(?,'<alice@provider.test>','upsert','pending','pending')`, f.accounts["alice"].ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.routing.DeferProviderRetry(t.Context(), "alice", f.accounts["alice"].ID, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			for _, view := range []struct{ path, content string }{{"/api/attachments/1/download", "alice report"}, {"/api/inline-content/1/shared-cid", "alice image"}} {
				rec := f.request("alice", "GET", view.path, "")
				if rec.Code != 200 || rec.Body.String() != view.content {
					t.Fatalf("saved MIME %s: %d %q", view.path, rec.Code, rec.Body.String())
				}
			}
			api.mu.Lock()
			calls := len(api.calls)
			api.mu.Unlock()
			if calls != 0 {
				t.Fatal("saved MIME recovery called unavailable provider", calls)
			}
		})
	}
}

func TestUserAttachmentRecoveryFailureDiscardsCandidateAndRetries(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, _ := newUserAttachmentFixture(t, provider, false)
			probe, err := f.blobs.StoreBodyText(t.Context(), f.accounts["alice"].ID, 1, []byte("probe"))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`CREATE TRIGGER reject_attachment_path BEFORE UPDATE OF storage_path ON attachments BEGIN SELECT RAISE(ABORT,'injected attachment publication failure'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if rec := f.request("alice", "GET", "/api/attachments/1/download", ""); rec.Code != 503 {
				t.Fatal("failed attachment publication reported success", rec.Code)
			}
			if f.attachmentPath(t, "alice", 1) != "" {
				t.Fatal("failed publication left cached path")
			}
			files := []string{}
			err = filepath.WalkDir(filepath.Dir(probe), func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !entry.IsDir() {
					files = append(files, path)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 1 || files[0] != probe {
				t.Fatal("failed candidate leaked files", files)
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`DROP TRIGGER reject_attachment_path`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if rec := f.request("alice", "GET", "/api/attachments/1/download", ""); rec.Code != 200 || rec.Body.String() != "alice report" {
				t.Fatal("attachment recovery did not retry", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestUserAttachmentRecoveryCooldownAndRefresh(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, direct := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/direct=%v", provider, direct), func(t *testing.T) {
				f, api := newUserAttachmentFixture(t, provider, direct)
				api.throttle = true
				if rec := f.request("alice", "GET", "/api/attachments/1/download", ""); rec.Code != 503 {
					t.Fatal("throttled attachment reported success", rec.Code)
				}
				until, err := f.routing.ProviderRetryUntil(t.Context(), "alice", f.accounts["alice"].ID)
				if err != nil || !until.After(time.Now()) {
					t.Fatal("attachment cooldown was not persisted", until, err)
				}
				api.mu.Lock()
				api.throttle = false
				before := len(api.calls)
				api.rejectOld = true
				api.mu.Unlock()
				if rec := f.request("alice", "GET", "/api/attachments/1/download", ""); rec.Code != 503 {
					t.Fatal("attachment bypassed cooldown", rec.Code)
				}
				api.mu.Lock()
				after := len(api.calls)
				api.mu.Unlock()
				if after != before {
					t.Fatal("cooldown made attachment request")
				}
				if _, err := f.system.Write().Exec(`UPDATE gofer_account_provider_retry SET retry_until_ms=0 WHERE account_id=?`, f.accounts["alice"].ID); err != nil {
					t.Fatal(err)
				}
				if rec := f.request("alice", "GET", "/api/attachments/1/download", ""); rec.Code != 200 || rec.Body.String() != "alice report" {
					t.Fatal("attachment refresh/retry failed", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

func TestUserAttachmentRecoveryLateResponseCannotChangeIdentity(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, change := range []string{"mailbox", "message", "part", "delete"} {
			t.Run(provider+"/"+change, func(t *testing.T) {
				f, api := newUserAttachmentFixture(t, provider, true)
				api.blockOwner = "alice"
				api.blockPath = "/users/me/messages/m1/attachments/report-part"
				if provider == "outlook" {
					api.blockPath = "/me/messages/m1/attachments/report-part/$value"
				}
				response := make(chan *httptest.ResponseRecorder, 1)
				go func() { response <- f.request("alice", "GET", "/api/attachments/1/download", "") }()
				awaitIMAP(t, api.entered)
				if rec := f.request("bob", "GET", "/api/attachments/1/download", ""); rec.Code != 200 || rec.Body.String() != "bob report" {
					t.Fatal("provider wait retained store lease", rec.Code, rec.Body.String())
				}
				if change == "delete" {
					if err := f.accountStore.DeleteAccount(t.Context(), "alice", f.accounts["alice"].ID, f.imap.Cleanup); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
						q := `UPDATE accounts SET provider_account_id='replacement'`
						if change == "message" {
							q = `UPDATE messages SET remote_message_id='replacement' WHERE id=1`
						}
						if change == "part" {
							q = `UPDATE attachments SET filename='replacement.txt' WHERE id=1`
						}
						_, err := db.Write().Exec(q)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				api.unblock()
				select {
				case rec := <-response:
					if rec.Code == 200 {
						t.Fatal("stale attachment accepted")
					}
				case <-time.After(15 * time.Second):
					t.Fatal("stale attachment request did not finish")
				}
				if change != "delete" && f.attachmentPath(t, "alice", 1) != "" {
					t.Fatal("stale attachment path published")
				}
			})
		}
	}
}

func TestUserAttachmentRecoveryLookupAndInvalidContent(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, direct := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/direct=%v", provider, direct), func(t *testing.T) {
				f, api := newUserAttachmentFixture(t, provider, direct)
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE messages SET remote_message_id='' WHERE id=1`)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				api.lookupNextPage = true
				if rec := f.request("alice", "GET", "/api/attachments/1/download", ""); rec.Code != 503 {
					t.Fatal("incomplete lookup accepted", rec.Code)
				}
				api.mu.Lock()
				api.lookupNextPage = false
				api.badAttachment = true
				api.mu.Unlock()
				if rec := f.request("alice", "GET", "/api/attachments/1/download", ""); rec.Code != 503 {
					t.Fatal("invalid attachment identity/content accepted", rec.Code)
				}
				if f.attachmentPath(t, "alice", 1) != "" {
					t.Fatal("invalid bytes published")
				}
				api.mu.Lock()
				api.badAttachment = false
				api.mu.Unlock()
				if rec := f.request("alice", "GET", "/api/attachments/1/download", ""); rec.Code != 200 || rec.Body.String() != "alice report" {
					t.Fatal("verified lookup did not recover", rec.Code, rec.Body.String())
				}
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					var id string
					if err := db.Read().QueryRow(`SELECT remote_message_id FROM messages WHERE id=1`).Scan(&id); err != nil {
						return err
					}
					if id != "m1" {
						return fmt.Errorf("verified lookup not published: %s", id)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestUserAttachmentRecoveryDuplicateFilenamesKeepBytesAndIDs(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newUserAttachmentFixture(t, provider, false)
			api.duplicateAttachments = true
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE attachments SET filename='report.txt',content_type='text/plain',content_id='',size_bytes=?,inline=0 WHERE id=2`, len("alice second"))
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if rec := f.request("alice", "GET", "/api/attachments/2/download", ""); rec.Code != 200 || rec.Body.String() != "alice second" {
				t.Fatal("same-name MIME sibling mixed up", rec.Code, rec.Body.String())
			}
			if f.attachmentPath(t, "alice", 1) != "" {
				t.Fatal("recovering one part published its sibling")
			}
		})
	}
}

func TestUserAttachmentRecoveryConcurrentReadersShareRequest(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newUserAttachmentFixture(t, provider, true)
			api.blockOwner = "alice"
			api.blockPath = "/users/me/messages/m1/attachments/report-part"
			if provider == "outlook" {
				api.blockPath = "/me/messages/m1/attachments/report-part/$value"
			}
			responses := make(chan *httptest.ResponseRecorder, 2)
			go func() { responses <- f.request("alice", "GET", "/api/attachments/1/download", "") }()
			awaitIMAP(t, api.entered)
			go func() { responses <- f.request("alice", "GET", "/api/attachments/1/download", "") }()
			if rec := f.request("bob", "GET", "/api/attachments/1/download", ""); rec.Code != 200 || rec.Body.String() != "bob report" {
				t.Fatal("blocked readers retained user-store lease", rec.Code)
			}
			api.unblock()
			for i := 0; i < 2; i++ {
				select {
				case rec := <-responses:
					if rec.Code != 200 || rec.Body.String() != "alice report" {
						t.Fatal("coalesced reader failed", rec.Code, rec.Body.String())
					}
				case <-time.After(15 * time.Second):
					t.Fatal("coalesced reader did not finish")
				}
			}
			api.mu.Lock()
			count := api.calls["alice|GET|"+api.blockPath]
			api.mu.Unlock()
			if count != 1 {
				t.Fatal("concurrent recovery repeated provider request", count)
			}
		})
	}
}

func TestUserAttachmentRecoveryCorruptSavedMIMEDoesNotHideRemoteSource(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, direct := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/direct=%v", provider, direct), func(t *testing.T) {
				f, _ := newUserAttachmentFixture(t, provider, direct)
				version, err := f.blobs.NewMessageVersion()
				if err != nil {
					t.Fatal(err)
				}
				path, err := version.StoreRaw(t.Context(), f.accounts["alice"].ID, 1, []byte("broken saved MIME"))
				if err != nil {
					t.Fatal(err)
				}
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE messages SET raw_path=? WHERE id=1`, path)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if rec := f.request("alice", "GET", "/api/attachments/1/download", ""); rec.Code != 200 || rec.Body.String() != "alice report" {
					t.Fatal("corrupt saved MIME prevented verified provider recovery", rec.Code, rec.Body.String())
				}
			})
		}
	}
}
