package mail

import (
	"context"
	"errors"
	"fmt"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"testing"
	"time"
)

func activeGmailJob(t *testing.T, f *userGmailFixture, owner string) (userActivePollJob, func()) {
	t.Helper()
	release, err := f.worker.BeginActiveUserSession(t.Context(), owner)
	if err != nil {
		t.Fatal(err)
	}
	f.worker.mu.Lock()
	session := f.worker.activePollUsers[owner]
	f.worker.mu.Unlock()
	return userActivePollJob{owner: owner, account: f.ids[owner], session: session}, release
}
func TestUserGmailActivePollChecksAndQueuesOnlyOwnedChanges(t *testing.T) {
	f := newUserGmailFixture(t)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	job, release := activeGmailJob(t, f, "alice")
	defer release()
	before := f.api.count("alice", "/users/me/profile", "")
	labels := f.api.count("alice", "/users/me/labels", "")
	if err := f.worker.pollActiveGmailAccount(job); err != nil {
		t.Fatal(err)
	}
	if got := f.api.count("alice", "/users/me/profile", ""); got != before+1 {
		t.Fatalf("profile checks: %d", got)
	}
	if got := f.api.count("alice", "/users/me/labels", ""); got != labels {
		t.Fatalf("unchanged profile triggered full receive: %d", got)
	}
	if err := f.worker.pollActiveGmailAccount(job); err != nil {
		t.Fatal(err)
	}
	if f.api.count("alice", "/users/me/profile", "") != before+1 {
		t.Fatal("duplicate profile check bypassed deadline")
	}
	f.local(t, "alice", func(db *storage.DB) error {
		state, err := db.GetGmailPollState(t.Context(), f.ids["alice"])
		if err == nil && (!state.LastCheckedAt.Valid || state.LastChangedAt.Valid || state.ProfileHistoryID != "100") {
			t.Errorf("unchanged poll state: %+v", state)
		}
		return err
	})
	f.api.change("alice", func(state *routedGmailState) { state.phase = 1 })
	if err := f.routing.ResetActivePolling(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	if err := f.worker.pollActiveGmailAccount(job); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for f.api.count("alice", "/users/me/messages/m2", "metadata") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.api.count("alice", "/users/me/messages/m2", "metadata") == 0 {
		t.Fatal("changed profile did not reconcile metadata")
	}
	if f.api.count("bob", "/users/me/profile", "") != 0 {
		t.Fatal("inactive owner's provider was polled")
	}
	f.local(t, "alice", func(db *storage.DB) error {
		state, err := db.GetGmailPollState(t.Context(), f.ids["alice"])
		if err == nil && (!state.LastChangedAt.Valid || state.ProfileHistoryID != "200") {
			t.Errorf("changed poll state: %+v", state)
		}
		return err
	})
}
func TestUserGmailActivePollLateProfileIsRejectedOrCanceled(t *testing.T) {
	for _, change := range []string{"last-session", "identity", "disabled", "deletion"} {
		t.Run(change, func(t *testing.T) {
			f := newUserGmailFixture(t)
			if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
				t.Fatal(err)
			}
			job, release := activeGmailJob(t, f, "alice")
			defer release()
			other, err := f.worker.BeginActiveUserSession(t.Context(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			defer other()
			block := f.api.stopAt("alice", "/users/me/profile", "")
			defer block.unblock()
			result := gmailAsync(func() error { return f.worker.pollActiveGmailAccount(job) })
			gmailStarted(t, block)
			// One user-store cache slot: Bob still receives while Alice is on HTTP.
			if err := f.worker.Sync(t.Context(), "bob", f.ids["bob"]); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "last-session":
				release()
				release()
				if job.session.ctx.Err() != nil {
					t.Fatal("first tab canceled another tab's poll")
				}
				other()
			case "identity":
				f.local(t, "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE accounts SET provider_account_id='changed' WHERE id=?`, f.ids["alice"])
					return err
				})
				block.unblock()
			case "disabled":
				f.local(t, "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE accounts SET email_sync_enabled=0 WHERE id=?`, f.ids["alice"])
					return err
				})
				block.unblock()
			case "deletion":
				if err := f.routing.RequestAccountDeletion(t.Context(), "alice", f.ids["alice"]); err != nil {
					t.Fatal(err)
				}
				if err := f.accounts.DeleteAccount(t.Context(), "alice", f.ids["alice"], f.worker.Cleanup); err != nil {
					t.Fatal(err)
				}
			}
			if err := gmailAwait(t, result); err == nil {
				t.Fatal("superseded or canceled profile was accepted")
			}
			if change != "deletion" {
				f.local(t, "alice", func(db *storage.DB) error {
					state, err := db.GetGmailPollState(t.Context(), f.ids["alice"])
					if err == nil && state.LastCheckedAt.Valid {
						t.Errorf("late profile committed: %+v", state)
					}
					return err
				})
			}
		})
	}
}
func TestUserGmailActivePollRefreshAndCooldown(t *testing.T) {
	f := newUserGmailFixture(t)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	job, release := activeGmailJob(t, f, "alice")
	defer release()
	f.api.change("alice", func(state *routedGmailState) { state.rejectOld = true })
	if err := f.worker.pollActiveGmailAccount(job); err != nil {
		t.Fatal(err)
	}
	if f.api.count("alice", "/token", "") != 1 {
		t.Fatal("unauthorized profile did not refresh once")
	}
	f.api.change("alice", func(state *routedGmailState) { state.profileStatus = 429 })
	if err := f.routing.ResetActivePolling(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	if err := f.worker.pollActiveGmailAccount(job); err == nil {
		t.Fatal("throttle was accepted")
	}
	until, err := f.routing.ProviderRetryUntil(t.Context(), "alice", f.ids["alice"])
	if err != nil || !until.After(time.Now()) {
		t.Fatal(until, err)
	}
	calls := f.api.count("alice", "/users/me/profile", "")
	release()
	next, releaseNext := activeGmailJob(t, f, "alice")
	defer releaseNext()
	if err := f.worker.pollActiveGmailAccount(next); err != nil {
		t.Fatal(err)
	}
	if f.api.count("alice", "/users/me/profile", "") != calls {
		t.Fatal("reconnect bypassed provider cooldown")
	}
	if _, err := f.system.Write().Exec(`UPDATE gofer_account_provider_retry SET retry_until_ms=0 WHERE account_id=?`, f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	if err := f.routing.ResetActivePolling(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	f.api.change("alice", func(state *routedGmailState) { state.profileStatus = 0 })
	if err := f.worker.pollActiveGmailAccount(next); err != nil {
		t.Fatal(err)
	}
	f.local(t, "alice", func(db *storage.DB) error {
		state, err := db.GetGmailPollState(t.Context(), f.ids["alice"])
		if err == nil && (state.ConsecutiveErrors != 0 || state.LastError != "") {
			t.Errorf("poll error did not clear: %+v", state)
		}
		return err
	})
}
func TestUserGmailActivePollBackgroundAndDisabledConfiguration(t *testing.T) {
	f := newUserGmailFixture(t)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.worker.BeginActiveUserSession(t.Context(), "unknown"); !errors.Is(err, storage.ErrUserStoreOwner) {
		t.Fatalf("unknown active owner: %v", err)
	}
	t.Setenv("GOFER_GMAIL_API_POLL", "off")
	release, err := f.worker.BeginActiveUserSession(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	release()
	f.worker.mu.Lock()
	count := len(f.worker.activePollUsers)
	f.worker.mu.Unlock()
	if count != 0 {
		t.Fatal("disabled polling retained a session")
	}
	t.Setenv("GOFER_GMAIL_API_POLL", "on")
	job, release := activeGmailJob(t, f, "alice")
	defer release()
	f.worker.mu.Lock()
	f.worker.startActiveGmailPollingLocked()
	f.worker.mu.Unlock()
	deadline := time.Now().Add(15 * time.Second)
	checked := false
	for !checked && time.Now().Before(deadline) {
		f.local(t, "alice", func(db *storage.DB) error {
			state, err := db.GetGmailPollState(t.Context(), f.ids["alice"])
			checked = state.LastCheckedAt.Valid
			return err
		})
		if !checked {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !checked {
		t.Fatal("active session was not discovered by background polling")
	}
	if f.api.count("bob", "/users/me/profile", "") != 0 {
		t.Fatal("idle owner was polled")
	}
	release()
	if !errors.Is(job.session.ctx.Err(), context.Canceled) {
		t.Fatal("last session release did not cancel poll lifecycle")
	}
}

func TestUserGmailActivePollDueMetadataAndInvalidProfile(t *testing.T) {
	f := newUserGmailFixture(t)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	job, release := activeGmailJob(t, f, "alice")
	defer release()
	f.api.change("alice", func(state *routedGmailState) { state.emptyProfile = true })
	if err := f.worker.pollActiveGmailAccount(job); err == nil {
		t.Fatal("empty profile was accepted")
	}
	f.local(t, "alice", func(db *storage.DB) error {
		state, err := db.GetGmailPollState(t.Context(), f.ids["alice"])
		if err != nil {
			return err
		}
		if state.LastError == "" || state.LastChangedAt.Valid {
			t.Errorf("invalid profile state: %+v", state)
		}
		if err := db.EnqueueGmailMessageFetch(t.Context(), f.ids["alice"], "m1", "100", errors.New("temporary metadata error")); err != nil {
			return err
		}
		_, err = db.Write().Exec(`UPDATE gmail_message_fetch_queue SET next_attempt_at='2000-01-01 00:00:00' WHERE account_id=?`, f.ids["alice"])
		return err
	})
	f.api.change("alice", func(state *routedGmailState) { state.emptyProfile = false })
	if err := f.routing.ResetActivePolling(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	before := f.api.count("alice", "/users/me/messages/m1", "metadata")
	if err := f.worker.pollActiveGmailAccount(job); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for f.api.count("alice", "/users/me/messages/m1", "metadata") == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.api.count("alice", "/users/me/messages/m1", "metadata") == before {
		t.Fatal("unchanged history hid due metadata recovery")
	}
}

func TestUserGmailActivePollingShutdownCancelsBoundedProfileWorkers(t *testing.T) {
	f := newUserGmailFixture(t)
	if err := f.worker.Sync(t.Context(), "alice", f.ids["alice"]); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		subject := fmt.Sprintf("extra-subject-%d", i)
		account, err := f.accounts.CreateAccount(t.Context(), "alice", providers.GmailAccountRequest(fmt.Sprintf("extra-%d@mail.test", i), "Extra", subject))
		if err != nil {
			t.Fatal(err)
		}
		expiry := time.Now().Add(time.Hour)
		if err := f.credentials.UpsertForUser(t.Context(), "alice", account.ID, "google", subject, "alice-access", "alice-refresh", "Bearer", &expiry, "https://mail.google.com/"); err != nil {
			t.Fatal(err)
		}
	}
	before := f.api.count("alice", "/users/me/profile", "")
	block := f.api.stopAt("alice", "/users/me/profile", "")
	defer block.unblock()
	_, release := activeGmailJob(t, f, "alice")
	defer release()
	f.worker.mu.Lock()
	f.worker.startActiveGmailPollingLocked()
	f.worker.mu.Unlock()
	deadline := time.Now().Add(15 * time.Second)
	for f.api.count("alice", "/users/me/profile", "") < before+4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.api.count("alice", "/users/me/profile", "") != before+4 {
		t.Fatal("fixed profile workers did not reach four bounded sessions")
	}
	// User-store reads still proceed while every protocol slot is occupied.
	f.local(t, "bob", func(db *storage.DB) error { _, err := db.GetGmailPollState(t.Context(), f.ids["bob"]); return err })
	f.cancel()
	if err := gmailAwait(t, gmailAsync(func() error { f.worker.Wait(); return nil })); err != nil {
		t.Fatal(err)
	}
	f.worker.mu.Lock()
	gates := len(f.worker.gates)
	slots := len(f.worker.slots)
	f.worker.mu.Unlock()
	if gates != 0 || slots != 0 {
		t.Fatalf("shutdown retained profile activity: gates=%d slots=%d", gates, slots)
	}
	f.local(t, "alice", func(db *storage.DB) error {
		state, err := db.GetGmailPollState(t.Context(), f.ids["alice"])
		if state.LastCheckedAt.Valid {
			t.Error("shutdown published late profile")
		}
		return err
	})
}
