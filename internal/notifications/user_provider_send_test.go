package notifications

import (
	"encoding/json"
	"net/http"
	stdmail "net/mail"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newProviderSendFixture(t *testing.T, provider string) (*userStorageFixture, *providerActionAPI) {
	t.Helper()
	f, api := newProviderActionFixture(t, provider)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			_, err := db.Write().Exec(`INSERT INTO folders(id,account_id,remote_id,provider_remote_id,name,role) VALUES(?,?,?,?,?,'sent')`, f.accounts[owner].ID+"-sent", f.accounts[owner].ID, "SENT", "SENT", "Sent")
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, api
}

func (f *userStorageFixture) queueProviderMessage(t *testing.T, owner string) string {
	t.Helper()
	form := url.Values{"account_id": {f.accounts[owner].ID}, "to": {"recipient@example.test"}, "bcc": {"hidden@example.test"}, "subject": {owner + " private delivery"}, "body": {owner + " private body"}}
	rec := f.request(owner, "POST", "/compose", form.Encode())
	var result struct {
		ID string `json:"send_id"`
	}
	if rec.Code != 202 || json.Unmarshal(rec.Body.Bytes(), &result) != nil || result.ID == "" {
		t.Fatalf("compose: %d %s", rec.Code, rec.Body.String())
	}
	return result.ID
}

func (f *userStorageFixture) providerSendStatus(t *testing.T, owner, id string) (string, string, int) {
	t.Helper()
	var status, copy string
	var attempt int
	if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT status,sent_copy_status,attempt_count FROM outgoing_sends WHERE id=?`, id).Scan(&status, &copy, &attempt)
	}); err != nil {
		t.Fatal(err)
	}
	return status, copy, attempt
}

func (a *providerActionAPI) acceptedCount(owner string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.accepted[owner])
}

func TestUserProviderSendHTTPAndSentCacheOwnedEvents(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderSendFixture(t, provider)
			events := openUserEventStream(t, f, "alice")
			api.blockOwner = "alice"
			alice := f.queueProviderMessage(t, "alice")
			awaitIMAP(t, api.entered)
			bob := f.queueProviderMessage(t, "bob")
			waitUserIMAP(t, func() bool {
				status, copy, _ := f.providerSendStatus(t, "bob", bob)
				return status == "sent" && copy == "complete"
			})
			if f.request("bob", "GET", "/api/outgoing-sends/"+alice, "").Code != 404 {
				t.Fatal("foreign outgoing ID exposed")
			}
			api.unblock()
			waitUserIMAP(t, func() bool {
				status, copy, _ := f.providerSendStatus(t, "alice", alice)
				return status == "sent" && copy == "complete"
			})
			awaitUserEvent(t, events, func(event map[string]any) bool {
				if event["type"] == string(mail.EventSendResult) && event["account_id"] == f.accounts["bob"].ID {
					t.Fatal("foreign send event leaked")
				}
				return event["type"] == string(mail.EventSendResult) && event["account_id"] == f.accounts["alice"].ID && event["status"] == "sent"
			})
			for _, owner := range []string{"alice", "bob"} {
				if api.acceptedCount(owner) != 1 {
					t.Fatal("provider accepted wrong number of sends")
				}
				api.mu.Lock()
				raw := append([]byte(nil), api.accepted[owner][0]...)
				api.mu.Unlock()
				msg, err := stdmail.ReadMessage(strings.NewReader(string(raw)))
				if err != nil || msg.Header.Get("Subject") != owner+" private delivery" || !strings.Contains(msg.Header.Get("Bcc"), "hidden@example.test") {
					t.Fatal("MIME identity/recipient lost", err)
				}
				if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
					var subject, remote, bodyPath string
					if err := db.Read().QueryRow(`SELECT m.subject,COALESCE(m.remote_message_id,''),m.body_text_path FROM messages m JOIN message_folder_state s ON s.message_id=m.id JOIN folders f ON f.id=s.folder_id WHERE f.role='sent'`).Scan(&subject, &remote, &bodyPath); err != nil {
						return err
					}
					if subject != owner+" private delivery" || bodyPath == "" || (provider == "gmail" && remote != "same-sent-provider-id") {
						t.Fatal("owned Sent cache incomplete")
					}
					var retained int
					if err := db.Read().QueryRow(`SELECT (SELECT count(*) FROM outgoing_sends WHERE length(mime_data)>0)+(SELECT count(*) FROM gofer_provider_send_receipts)`).Scan(&retained); err != nil {
						return err
					}
					if retained != 0 {
						t.Fatal("completed provider snapshot retained")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestUserProviderSendSentPublicationFailureNeverResends(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderSendFixture(t, provider)
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`CREATE TRIGGER fail_sent_cache BEFORE UPDATE OF body_text_path ON messages WHEN NEW.subject='alice private delivery' BEGIN SELECT RAISE(ABORT,'injected Sent publication failure'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			id := f.queueProviderMessage(t, "alice")
			waitUserIMAP(t, func() bool {
				status, copy, _ := f.providerSendStatus(t, "alice", id)
				return status == "sent" && copy == "ambiguous"
			})
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var receipts int
				if err := db.Read().QueryRow(`SELECT count(*) FROM gofer_provider_send_receipts WHERE send_id=?`, id).Scan(&receipts); err != nil {
					return err
				}
				if receipts != 1 {
					t.Fatal("confirmed receipt lost during cache failure")
				}
				if _, err := db.Write().Exec(`DROP TRIGGER fail_sent_cache`); err != nil {
					return err
				}
				_, err := db.Write().Exec(`UPDATE outgoing_sends SET sent_copy_next_attempt_at='2000-01-01' WHERE id=?`, id)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			// Bob evicts Alice's handle. Recovery must survive that reopen too.
			bob := f.queueProviderMessage(t, "bob")
			waitUserIMAP(t, func() bool { _, copy, _ := f.providerSendStatus(t, "bob", bob); return copy == "complete" })
			if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			if status, copy, attempt := f.providerSendStatus(t, "alice", id); status != "sent" || copy != "complete" || attempt != 1 || api.acceptedCount("alice") != 1 {
				t.Fatalf("cache recovery resent mail: %s %s %d", status, copy, attempt)
			}
		})
	}
}

func TestUserProviderSendLostAckRequiresOwnedConfirmation(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderSendFixture(t, provider)
			api.loseSendAck = true
			id := f.queueProviderMessage(t, "alice")
			waitUserIMAP(t, func() bool { status, _, _ := f.providerSendStatus(t, "alice", id); return status == "ambiguous" })
			if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			if api.acceptedCount("alice") != 1 {
				t.Fatal("uncertain send retried automatically")
			}
			if rec := f.request("alice", "POST", "/api/outgoing-sends/"+id+"/retry", `{}`); rec.Code != 409 {
				t.Fatal("ambiguous resend needs confirmation", rec.Code)
			}
			if rec := f.request("bob", "POST", "/api/outgoing-sends/"+id+"/retry", `{"confirm":true}`); rec.Code != 404 {
				t.Fatal("foreign resend allowed", rec.Code)
			}
			api.mu.Lock()
			api.loseSendAck = false
			api.mu.Unlock()
			if rec := f.request("alice", "POST", "/api/outgoing-sends/"+id+"/retry", `{"confirm":true}`); rec.Code != 200 {
				t.Fatal("confirmed retry failed", rec.Code, rec.Body.String())
			}
			waitUserIMAP(t, func() bool { _, copy, _ := f.providerSendStatus(t, "alice", id); return copy == "complete" })
			if api.acceptedCount("alice") != 2 {
				t.Fatal("confirmed retry did not send once")
			}
		})
	}
}

func TestUserProviderSendRetryCooldownAnd401Refresh(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderSendFixture(t, provider)
			api.throttle = true
			id := f.queueProviderMessage(t, "alice")
			waitUserIMAP(t, func() bool {
				status, _, attempt := f.providerSendStatus(t, "alice", id)
				return status == "pending" && attempt == 1
			})
			until, err := f.routing.ProviderRetryUntil(t.Context(), "alice", f.accounts["alice"].ID)
			if err != nil || !until.After(time.Now().Add(time.Minute)) {
				t.Fatal("send cooldown lost", err)
			}
			api.mu.Lock()
			before := len(api.calls)
			api.throttle = false
			api.rejectOld = true
			api.mu.Unlock()
			if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err == nil {
				t.Fatal("manual sync bypassed cooldown")
			}
			api.mu.Lock()
			after := len(api.calls)
			api.mu.Unlock()
			if before != after {
				t.Fatal("cooldown called provider")
			}
			if _, err := f.system.Write().Exec(`UPDATE gofer_account_provider_retry SET retry_until_ms=0`); err != nil {
				t.Fatal(err)
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE outgoing_sends SET next_attempt_at='2000-01-01' WHERE id=?`, id)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			if status, copy, attempt := f.providerSendStatus(t, "alice", id); status != "sent" || copy != "complete" || attempt != 2 || api.acceptedCount("alice") != 1 {
				t.Fatalf("provider retry failed: %s %s %d", status, copy, attempt)
			}
		})
	}
}

func TestUserProviderSendDeletionCancelsWait(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderSendFixture(t, provider)
			api.blockOwner = "alice"
			f.queueProviderMessage(t, "alice")
			awaitIMAP(t, api.entered)
			if err := f.accountStore.DeleteAccount(t.Context(), "alice", f.accounts["alice"].ID, f.imap.Cleanup); err != nil {
				t.Fatal(err)
			}
			bob := f.queueProviderMessage(t, "bob")
			waitUserIMAP(t, func() bool { _, copy, _ := f.providerSendStatus(t, "bob", bob); return copy == "complete" })
			if api.acceptedCount("alice") != 0 {
				t.Fatal("deleted account delivered after cancellation")
			}
		})
	}
}

func TestUserProviderSendSuccessWithMalformedIDBodyDoesNotResend(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderSendFixture(t, provider)
			api.badSendReply = true
			id := f.queueProviderMessage(t, "alice")
			waitUserIMAP(t, func() bool {
				status, copy, _ := f.providerSendStatus(t, "alice", id)
				return status == "sent" && copy == "complete"
			})
			if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			if api.acceptedCount("alice") != 1 {
				t.Fatal("successful send with absent ID was repeated")
			}
		})
	}
}

func TestUserProviderSendAcceptancePublicationFailureRequiresRecovery(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderSendFixture(t, provider)
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`CREATE TRIGGER reject_acceptance BEFORE UPDATE OF status ON outgoing_sends WHEN NEW.status='sent' BEGIN SELECT RAISE(ABORT,'injected acceptance publication failure'); END`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			id := f.queueProviderMessage(t, "alice")
			waitUserIMAP(t, func() bool { return api.acceptedCount("alice") == 1 })
			// The account gate waits for the first operation before recovery marks
			// the unresolved sending claim ambiguous. It must not issue another POST.
			if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			if status, _, attempt := f.providerSendStatus(t, "alice", id); status != "ambiguous" || attempt != 1 {
				t.Fatalf("lost acceptance was unsafe: %s %d", status, attempt)
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error { _, err := db.Write().Exec(`DROP TRIGGER reject_acceptance`); return err }); err != nil {
				t.Fatal(err)
			}
			if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			if api.acceptedCount("alice") != 1 {
				t.Fatal("local acceptance failure resent mail")
			}
		})
	}
}

func TestUserProviderSendDefinitiveRejectionRetriesOnlyTemporaryFailures(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		for _, status := range []int{400, 503} {
			t.Run(provider+"/"+http.StatusText(status), func(t *testing.T) {
				f, api := newProviderSendFixture(t, provider)
				api.sendStatus = status
				id := f.queueProviderMessage(t, "alice")
				want := "failed"
				if status == 503 {
					want = "pending"
				}
				waitUserIMAP(t, func() bool {
					state, _, attempt := f.providerSendStatus(t, "alice", id)
					return state == want && attempt == 1
				})
				if api.acceptedCount("alice") != 0 {
					t.Fatal("rejected request accepted mail")
				}
				if status == 400 {
					if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
						t.Fatal(err)
					}
					if state, _, attempt := f.providerSendStatus(t, "alice", id); state != "failed" || attempt != 1 {
						t.Fatal("permanent rejection retried")
					}
					return
				}
				api.mu.Lock()
				api.sendStatus = 0
				api.mu.Unlock()
				if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE outgoing_sends SET next_attempt_at='2000-01-01' WHERE id=?`, id)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
					t.Fatal(err)
				}
				if state, copy, attempt := f.providerSendStatus(t, "alice", id); state != "sent" || copy != "complete" || attempt != 2 || api.acceptedCount("alice") != 1 {
					t.Fatal("temporary failure did not recover safely")
				}
			})
		}
	}
}

func TestUserProviderSendBackgroundHonorsDurableRetryDeadline(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderSendFixture(t, provider)
			api.sendStatus = 503
			id := f.queueProviderMessage(t, "alice")
			waitUserIMAP(t, func() bool {
				status, _, attempt := f.providerSendStatus(t, "alice", id)
				return status == "pending" && attempt == 1
			})
			// Leave unrelated fixture accounts asleep, including the original plain
			// IMAP placeholders. No test can reach an external provider endpoint.
			if _, err := f.system.Write().Exec(`INSERT INTO gofer_account_poll_schedule(account_id,next_due_ms) SELECT account_id,9999999999999 FROM gofer_account_directory WHERE account_id!=? ON CONFLICT(account_id) DO UPDATE SET next_due_ms=excluded.next_due_ms`, f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			due := time.Now().UTC().Add(2 * time.Second).Truncate(time.Millisecond)
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE outgoing_sends SET next_attempt_at=? WHERE id=?`, due, id)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			api.sendStatus = 0
			api.mu.Unlock()
			if err := f.imap.Start(mail.UserIMAPBackgroundOptions{PollInterval: 24 * time.Hour, ScanInterval: 20 * time.Millisecond, DisableIDLE: true}); err != nil {
				t.Fatal(err)
			}
			if err := f.imap.WakeMutations(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			waitUserIMAP(t, func() bool {
				var next int64
				if err := f.system.Read().QueryRow(`SELECT next_due_ms FROM gofer_account_poll_schedule WHERE account_id=?`, f.accounts["alice"].ID).Scan(&next); err != nil {
					t.Fatal(err)
				}
				return next == due.UnixMilli()
			})
			waitUserIMAP(t, func() bool { _, copy, _ := f.providerSendStatus(t, "alice", id); return copy == "complete" })
			api.mu.Lock()
			accepted := append([]time.Time(nil), api.acceptedAt["alice"]...)
			api.mu.Unlock()
			if len(accepted) != 1 || accepted[0].Before(due) {
				t.Fatalf("background send ignored deadline: %v before %v", accepted, due)
			}
		})
	}
}

func TestUserProviderSendBacklogBeyondOnePass(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderSendFixture(t, provider)
			api.blockOwner = "alice"
			ids := []string{f.queueProviderMessage(t, "alice")}
			awaitIMAP(t, api.entered)
			for i := 1; i < 12; i++ {
				ids = append(ids, f.queueProviderMessage(t, "alice"))
			}
			bob := f.queueProviderMessage(t, "bob")
			waitUserIMAP(t, func() bool { _, copy, _ := f.providerSendStatus(t, "bob", bob); return copy == "complete" })
			if _, err := f.system.Write().Exec(`INSERT INTO gofer_account_poll_schedule(account_id,next_due_ms) SELECT account_id,9999999999999 FROM gofer_account_directory WHERE account_id!=? ON CONFLICT(account_id) DO UPDATE SET next_due_ms=excluded.next_due_ms`, f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			if err := f.imap.Start(mail.UserIMAPBackgroundOptions{PollInterval: 24 * time.Hour, ScanInterval: 20 * time.Millisecond, DisableIDLE: true}); err != nil {
				t.Fatal(err)
			}
			api.unblock()
			waitUserIMAP(t, func() bool {
				for _, id := range ids {
					_, copy, _ := f.providerSendStatus(t, "alice", id)
					if copy != "complete" {
						return false
					}
				}
				return true
			})
			if api.acceptedCount("alice") != 12 || api.acceptedCount("bob") != 1 {
				t.Fatal("bounded passes lost or duplicated outgoing work")
			}
		})
	}
}

func TestUserProviderSendMailboxChangeRejectsLateAcceptance(t *testing.T) {
	for _, provider := range []string{"gmail", "outlook"} {
		t.Run(provider, func(t *testing.T) {
			f, api := newProviderSendFixture(t, provider)
			api.blockOwner = "alice"
			id := f.queueProviderMessage(t, "alice")
			awaitIMAP(t, api.entered)
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				_, err := db.Write().Exec(`UPDATE accounts SET provider_account_id='replacement-subject' WHERE id=?`, f.accounts["alice"].ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			api.unblock()
			waitUserIMAP(t, func() bool { return api.acceptedCount("alice") == 1 })
			if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
				t.Fatal(err)
			}
			if status, _, _ := f.providerSendStatus(t, "alice", id); status != "ambiguous" {
				t.Fatal("late result completed under replacement identity", status)
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				var count int
				if err := db.Read().QueryRow(`SELECT (SELECT count(*) FROM gofer_provider_send_receipts)+(SELECT count(*) FROM messages m JOIN message_folder_state s ON s.message_id=m.id JOIN folders f ON f.id=s.folder_id WHERE f.role='sent')`).Scan(&count); err != nil {
					return err
				}
				if count != 0 {
					t.Fatal("late acceptance leaked into replacement mailbox")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
