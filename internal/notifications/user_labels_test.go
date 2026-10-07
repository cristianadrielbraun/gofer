package notifications

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (f *userStorageFixture) pendingLabels(t *testing.T, owner string) int {
	t.Helper()
	var count int
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT count(*) FROM label_mutation_queue`).Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	return count
}
func (f *userStorageFixture) labelCount(t *testing.T, owner, name, provider string) int {
	t.Helper()
	var count int
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT count(*) FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=1 AND lower(l.name)=lower(?) AND (?='' OR l.provider_type=?)`, name, provider, provider).Scan(&count)
	}); err != nil {
		t.Fatal(err)
	}
	return count
}
func TestUserLabelsProviderHTTPCreateBulkAndRemove(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderActionFixture(t, provider)
			if rec := f.request("alice", "POST", "/api/messages/1/label", url.Values{"label": {"Projects"}}.Encode()); rec.Code != 200 {
				t.Fatalf("single label: %d %s", rec.Code, rec.Body.String())
			}
			if rec := f.request("bob", "POST", "/api/messages/label", `{"targets":[{"id":"1","thread":true}],"label":"Projects"}`); rec.Code != 200 {
				t.Fatalf("bulk label: %d %s", rec.Code, rec.Body.String())
			}
			waitUserIMAP(t, func() bool { return f.pendingLabels(t, "alice") == 0 && f.pendingLabels(t, "bob") == 0 })
			for _, owner := range []string{"alice", "bob"} {
				if f.labelCount(t, owner, "Projects", provider) != 1 || f.labelCount(t, owner, "Projects", "local") != 0 {
					t.Fatal("provider label publication incomplete")
				}
			}
			api.mu.Lock()
			valid := api.appliedLabels["alice"]["Projects"] && api.appliedLabels["bob"]["Projects"] && api.appliedLabels["alice"]["Existing"] && api.appliedLabels["bob"]["Existing"]
			api.mu.Unlock()
			if !valid {
				t.Fatal("label or existing category was lost/crossed owners")
			}
			if rec := f.request("alice", "POST", "/api/messages/unlabel", `{"targets":[{"id":"1"}],"label":"Projects"}`); rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			waitUserIMAP(t, func() bool { return f.pendingLabels(t, "alice") == 0 })
			api.mu.Lock()
			valid = !api.appliedLabels["alice"]["Projects"] && api.appliedLabels["bob"]["Projects"] && api.appliedLabels["alice"]["Existing"]
			api.mu.Unlock()
			if !valid || f.labelCount(t, "alice", "Projects", "") != 0 {
				t.Fatal("owned removal did not preserve other labels/users")
			}
			body := fmt.Sprintf(`{"targets":[{"id":"1"}],"label":"Projects","folder_id":%q}`, f.accounts["bob"].ID+"-inbox")
			if rec := f.request("alice", "POST", "/api/messages/label", body); rec.Code != 404 {
				t.Fatalf("foreign folder label: %d", rec.Code)
			}
		})
	}
}
func TestUserLabelsBlockedReplyCannotRestoreRemovedLabel(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderActionFixture(t, provider)
			api.blockOwner = "alice"
			if rec := f.request("alice", "POST", "/api/messages/1/label", `{"label":"Projects"}`); rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			awaitIMAP(t, api.entered)
			if f.labelCount(t, "alice", "Projects", "local") != 1 {
				t.Fatal("label was not saved optimistically")
			}
			if rec := f.request("alice", "POST", "/api/messages/1/unlabel", `{"label":"Projects"}`); rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			if f.labelCount(t, "alice", "Projects", "") != 0 {
				t.Fatal("local removal was delayed by provider")
			}
			if rec := f.request("bob", "POST", "/api/messages/1/label", `{"label":"Projects"}`); rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			waitUserIMAP(t, func() bool { return f.pendingLabels(t, "bob") == 0 })
			api.unblock()
			waitUserIMAP(t, func() bool { return f.pendingLabels(t, "alice") == 0 })
			api.mu.Lock()
			alice, bob := api.appliedLabels["alice"]["Projects"], api.appliedLabels["bob"]["Projects"]
			api.mu.Unlock()
			if alice || !bob || f.labelCount(t, "alice", "Projects", "") != 0 {
				t.Fatal("old reply restored a removed label")
			}
		})
	}
}
func TestUserLabelsPublicationFailureRetriesWithoutCreatingDuplicateLabel(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderActionFixture(t, provider)
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`CREATE TRIGGER reject_user_label BEFORE INSERT ON message_labels WHEN (SELECT provider_type FROM labels WHERE id=NEW.label_id)!='local' BEGIN SELECT RAISE(ABORT,'injected provider label publication failure'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if rec := f.request("alice", "POST", "/api/messages/1/label", `{"label":"Projects"}`); rec.Code != 200 {
				t.Fatalf("local save: %d %s", rec.Code, rec.Body.String())
			}
			waitUserIMAP(t, func() bool {
				var n int
				_ = f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					return db.Read().QueryRow(`SELECT count(*) FROM label_mutation_queue WHERE attempts=1`).Scan(&n)
				})
				return n == 1
			})
			if f.labelCount(t, "alice", "Projects", provider) != 0 || f.labelCount(t, "alice", "Projects", "local") != 1 {
				t.Fatal("partial publication escaped rollback")
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				if _, err := db.Write().Exec(`DROP TRIGGER reject_user_label`); err != nil {
					return err
				}
				_, err := db.Write().Exec(`UPDATE label_mutation_queue SET next_attempt_at=datetime('now','-1 minute')`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			path := "/users/me/labels"
			if provider == "outlook" {
				path = "/me/outlook/masterCategories"
			}
			created := api.calls["alice|POST|"+path]
			api.mu.Unlock()
			if created != 1 || f.pendingLabels(t, "alice") != 0 || f.labelCount(t, "alice", "Projects", provider) != 1 || f.labelCount(t, "alice", "Projects", "local") != 0 {
				t.Fatalf("publication recovery: creates=%d", created)
			}
		})
	}
}
func TestUserLabelsStaleFailureCannotDelayNewIntent(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderActionFixture(t, provider)
			api.blockOwner = "alice"
			if rec := f.request("alice", "POST", "/api/messages/1/label", `{"label":"Projects"}`); rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			awaitIMAP(t, api.entered)
			if rec := f.request("alice", "POST", "/api/messages/1/unlabel", `{"label":"Projects"}`); rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			api.mu.Lock()
			api.throttle = true
			api.mu.Unlock()
			api.unblock()
			waitUserIMAP(t, func() bool {
				until, err := f.routing.ProviderRetryUntil(t.Context(), "alice", f.accounts["alice"].ID)
				return err == nil && until.After(time.Now().Add(time.Minute))
			})
			_ = f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID)
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var operation string
				var attempts int
				if err := db.Read().QueryRow(`SELECT operation,attempts FROM label_mutation_queue`).Scan(&operation, &attempts); err != nil {
					return err
				}
				if operation != "remove" || attempts != 0 {
					return fmt.Errorf("stale error rewrote new intent: %s %d", operation, attempts)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestUserLabelsDeletionCancelsProviderWait(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderActionFixture(t, provider)
			api.blockOwner = "alice"
			if rec := f.request("alice", "POST", "/api/messages/1/label", `{"label":"Projects"}`); rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			awaitIMAP(t, api.entered)
			if err := f.accountStore.DeleteAccount(t.Context(), "alice", f.accounts["alice"].ID, f.imap.Cleanup); err != nil {
				t.Fatal(err)
			}
			if rec := f.request("bob", "POST", "/api/messages/1/label", `{"label":"Projects"}`); rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			waitUserIMAP(t, func() bool { return f.pendingLabels(t, "bob") == 0 })
		})
	}
}
func TestUserLabelsIMAPAliasesAndSpamTraining(t *testing.T) {
	f, server := newUserMutationFixture(t)
	// Discover a selectable Junk folder through the real receiving path.
	server.mu.Lock()
	server.mutationMailboxLocked("alice", "Junk")
	server.mu.Unlock()
	if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	junk := f.remoteFolderID(t, "alice", "Junk")
	if rec := f.request("alice", http.MethodPost, "/api/messages/1/label", `{"label":"Important"}`); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	waitUserIMAP(t, func() bool { return f.pendingLabels(t, "alice") == 0 })
	if remote, local := server.remoteFlag("alice", "INBOX", 2, `$Label1`), f.labelCount(t, "alice", "Important", "imap_keyword"); !remote || local != 1 {
		t.Fatalf("IMAP alias did not reach remote keyword: remote=%t local=%d", remote, local)
	}
	if rec := f.request("alice", http.MethodPost, "/api/messages/spam", `{"targets":[{"id":"1"}]}`); rec.Code != 200 {
		t.Fatalf("spam: %d %s", rec.Code, rec.Body.String())
	}
	waitUserIMAP(t, func() bool { return f.pendingLabels(t, "alice") == 0 && f.pendingMutations(t, "alice") == 0 })
	if !server.remoteFlag("alice", "Junk", 2, `$Junk`) || server.remoteFlag("alice", "Junk", 2, `$NotJunk`) {
		t.Fatal("spam move lost training flags")
	}
	// A second report in the same folder still trains; it does not queue a move.
	if rec := f.request("alice", http.MethodPost, "/api/messages/spam", `{"targets":[{"id":"1"}]}`); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	waitUserIMAP(t, func() bool { return f.pendingLabels(t, "alice") == 0 })
	body := fmt.Sprintf(`{"targets":[{"id":"1"}],"folder_id":%q}`, junk)
	if rec := f.request("alice", http.MethodPost, "/api/messages/not-spam", body); rec.Code != 200 {
		t.Fatalf("not-spam: %d %s", rec.Code, rec.Body.String())
	}
	waitUserIMAP(t, func() bool { return f.pendingLabels(t, "alice") == 0 && f.pendingMutations(t, "alice") == 0 })
	if !server.remoteFlag("alice", "INBOX", 2, `$NotJunk`) || server.remoteFlag("alice", "INBOX", 2, `$Junk`) {
		t.Fatal("not-spam move lost training flags")
	}
	if f.labelCount(t, "alice", "$Junk", "") != 0 || f.labelCount(t, "alice", "$NotJunk", "") != 0 {
		t.Fatal("spam training keywords appeared as user labels")
	}
	if rec := f.request("alice", http.MethodPost, "/api/messages/1/unlabel", `{"label":"Important"}`); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	waitUserIMAP(t, func() bool { return f.pendingLabels(t, "alice") == 0 })
	if server.remoteFlag("alice", "INBOX", 2, `$Label1`) || f.labelCount(t, "alice", "Important", "") != 0 {
		t.Fatal("keyword removal failed after moves")
	}
	for _, label := range []string{"bad label", `\Seen`, "$Junk"} {
		body := fmt.Sprintf(`{"label":%q}`, label)
		if rec := f.request("alice", http.MethodPost, "/api/messages/1/label", body); rec.Code != 502 {
			t.Fatalf("invalid keyword %q: %d", label, rec.Code)
		}
	}
	if f.pendingLabels(t, "alice") != 0 {
		t.Fatal("invalid label left intent")
	}
}
func TestUserLabelsIMAPNewIntentSurvivesBlockedStore(t *testing.T) {
	f, server := newUserMutationFixture(t)
	blocked, release := server.setBlock("alice", "store")
	if rec := f.request("alice", "POST", "/api/messages/1/label", `{"label":"Projects"}`); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	awaitIMAP(t, blocked)
	if rec := f.request("alice", "POST", "/api/messages/1/unlabel", `{"label":"Projects"}`); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if rec := f.request("bob", "POST", "/api/messages/1/label", `{"label":"Projects"}`); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	waitUserIMAP(t, func() bool { return f.pendingLabels(t, "bob") == 0 })
	close(release)
	waitUserIMAP(t, func() bool { return f.pendingLabels(t, "alice") == 0 })
	if server.remoteFlag("alice", "INBOX", 2, "Projects") || !server.remoteFlag("bob", "INBOX", 2, "Projects") || f.labelCount(t, "alice", "Projects", "") != 0 {
		t.Fatal("stale keyword publication overwrote newer intent")
	}
}

func TestUserLabelsIMAPMissingEpochAndAmbiguousIdentity(t *testing.T) {
	for _, tc := range []struct {
		name        string
		epoch       uint32
		duplicate   bool
		wantSuccess bool
	}{{"resolve unknown epoch", 0, false, true}, {"reject changed epoch", 101, false, false}, {"reject ambiguous message ID", 0, true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			f, server := newUserMutationFixture(t)
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				if _, err := db.Write().Exec(`UPDATE accounts SET email_sync_enabled=0`); err != nil {
					return err
				}
				if _, err := db.Write().Exec(`UPDATE folders SET uid_validity=? WHERE id=?`, tc.epoch, f.remoteFolderIDUnchecked(t, "alice", db, "INBOX")); err != nil {
					return err
				}
				// UID 1000000 belongs to different mail; only Message-ID resolution may
				// safely identify the intended UID 2 when the epoch is absent.
				// Clear the other membership first to model a stale historical cache.
				if _, err := db.Write().Exec(`UPDATE message_folder_state SET remote_uid=NULL WHERE message_id!=1`); err != nil {
					return err
				}
				_, err := db.Write().Exec(`UPDATE message_folder_state SET remote_uid=1000000 WHERE message_id=1`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			server.mu.Lock()
			if tc.duplicate {
				server.mutationMailboxLocked("alice", "INBOX").origins[1000000] = 2
			}
			server.commands = nil
			server.mu.Unlock()
			if rec := f.request("alice", "POST", "/api/messages/1/label", `{"label":"Projects"}`); rec.Code != 200 {
				t.Fatal(rec.Code)
			}
			if tc.wantSuccess {
				waitUserIMAP(t, func() bool {
					var attempts int
					var detail string
					if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
						return db.Read().QueryRow(`SELECT COALESCE(max(attempts),0),COALESCE(max(last_error),'') FROM label_mutation_queue`).Scan(&attempts, &detail)
					}); err != nil {
						t.Fatal(err)
					}
					if attempts > 0 {
						server.mu.Lock()
						commands := append([]string(nil), server.commands...)
						server.mu.Unlock()
						t.Fatalf("identity recovery failed: %s; commands=%v", detail, commands)
					}
					return f.pendingLabels(t, "alice") == 0
				})
				if !server.remoteFlag("alice", "INBOX", 2, "Projects") || server.remoteFlag("alice", "INBOX", 1000000, "Projects") {
					t.Fatal("unknown epoch used the stale cached UID")
				}
			} else {
				waitUserIMAP(t, func() bool {
					var attempts int
					if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
						return db.Read().QueryRow(`SELECT attempts FROM label_mutation_queue`).Scan(&attempts)
					}); err != nil {
						t.Fatal(err)
					}
					return attempts > 0
				})
				server.mu.Lock()
				defer server.mu.Unlock()
				for _, cmd := range server.commands {
					if strings.Contains(strings.ToUpper(cmd), " UID STORE ") {
						t.Fatal("unsafe identity reached STORE:", cmd)
					}
				}
				if f.labelCount(t, "alice", "Projects", "imap_keyword") != 0 {
					t.Fatal("unsafe identity published provider label")
				}
			}
		})
	}
}

func TestUserLabelsMalformedCreateRetainsIntentAndRetriesExistingLabel(t *testing.T) {
	f, api := newProviderActionFixture(t, "gmail")
	api.badLabelCreate = true
	if rec := f.request("alice", "POST", "/api/messages/1/label", `{"label":"Projects"}`); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	waitUserIMAP(t, func() bool {
		var attempts int
		if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
			return db.Read().QueryRow(`SELECT attempts FROM label_mutation_queue`).Scan(&attempts)
		}); err != nil {
			t.Fatal(err)
		}
		return attempts > 0
	})
	if f.labelCount(t, "alice", "Projects", "gmail") != 0 || f.labelCount(t, "alice", "Projects", "local") != 1 {
		t.Fatal("malformed reply consumed optimistic intent")
	}
	api.mu.Lock()
	api.badLabelCreate = false
	api.mu.Unlock()
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE label_mutation_queue SET next_attempt_at='2000-01-01'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	if f.pendingLabels(t, "alice") != 0 || f.labelCount(t, "alice", "Projects", "gmail") != 1 {
		t.Fatal("retry did not recover existing remote label")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.calls["alice|POST|/users/me/labels"] != 1 {
		t.Fatal("retry duplicated remote label creation")
	}
}
