package mail

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserMailConnectionProviderWaitReleasesCacheAndRejectsChanges(t *testing.T) {
	for _, change := range []string{"subject", "disable", "shutdown"} {
		t.Run(change, func(t *testing.T) {
			f := newUserGmailFixture(t)
			block := f.api.stopAt("alice", "/users/me/profile", "")
			t.Cleanup(block.unblock)
			result := gmailAsync(func() error { _, err := f.worker.TestAccount(t.Context(), "alice", f.ids["alice"]); return err })
			select {
			case <-block.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("probe did not start")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			results, err := f.worker.TestAccount(ctx, "bob", f.ids["bob"])
			if err != nil || len(results) != 1 || !results[0].Success {
				t.Fatal("provider wait pinned store or crossed owner", results, err)
			}
			switch change {
			case "subject":
				f.local(t, "alice", func(db *storage.DB) error {
					_, err := db.Write().Exec(`UPDATE accounts SET provider_account_id='changed' WHERE id=?`, f.ids["alice"])
					return err
				})
				block.unblock()
			case "disable":
				if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='alice'`); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				f.cancel()
			}
			err = gmailAwait(t, result)
			if err == nil || (change == "subject" && !errors.Is(err, ErrUserMailConnectionChanged)) {
				t.Fatal("changed/cancelled probe returned success", err)
			}
		})
	}
}

func TestUserGmailRepairRestoresImportedMetadataWithoutCrossingOwners(t *testing.T) {
	f := newUserGmailFixture(t)
	for _, owner := range []string{"alice", "bob"} {
		if err := f.worker.Sync(t.Context(), owner, f.ids[owner]); err != nil {
			t.Fatal(err)
		}
		f.local(t, owner, func(db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE messages SET subject='damaged metadata' WHERE account_id=?`, f.ids[owner])
			return err
		})
	}
	if _, _, err := f.worker.StartGmailRepair(t.Context(), "bob", f.ids["alice"]); !errors.Is(err, storage.ErrAccountRoute) {
		t.Fatal("foreign repair accepted", err)
	}
	block := f.api.stopAt("alice", "/users/me/messages/m1", "metadata")
	t.Cleanup(block.unblock)
	events := f.worker.Events().Subscribe()
	defer f.worker.Events().Unsubscribe(events)
	id, started, err := f.worker.StartGmailRepair(t.Context(), "alice", f.ids["alice"])
	if err != nil || !started {
		t.Fatal("repair not admitted", id, started, err)
	}
	select {
	case <-block.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("repair skipped already imported metadata")
	}
	if repeated, started, err := f.worker.StartManualSync(t.Context(), "alice", []string{f.ids["alice"]}); err != nil || started || repeated != id {
		t.Fatal("overlapping manual run admitted", repeated, started, err)
	}
	snapshot, err := f.worker.ManualSyncSnapshot(t.Context(), "alice")
	if err != nil || len(snapshot) != 2 || snapshot[0].Payload["mode"] != "repair" {
		t.Fatal("repair snapshot mode", snapshot, err)
	}
	block.unblock()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
wait:
	for {
		select {
		case event := <-events:
			if event.Type == EventManualSyncComplete {
				if event.UserID != "alice" || event.Payload["mode"] != "repair" || event.Payload["failures"] != 0 {
					t.Fatal("repair failed", event)
				}
				break wait
			}
		case <-timer.C:
			t.Fatal("repair did not complete")
		}
	}
	for _, owner := range []string{"alice", "bob"} {
		f.local(t, owner, func(db *storage.DB) error {
			var subject string
			if err := db.Read().QueryRow(`SELECT subject FROM messages WHERE account_id=? AND remote_message_id='m1'`, f.ids[owner]).Scan(&subject); err != nil {
				return err
			}
			if (subject == "damaged metadata") == (owner == "alice") {
				t.Fatal("repair failed or crossed owners", owner, subject)
			}
			return nil
		})
	}
}
