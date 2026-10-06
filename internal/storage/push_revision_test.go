package storage

import (
	"path/filepath"
	"testing"
)

func TestPushDeliveryCannotMutateRenewedRegistration(t *testing.T) {
	db := newUserStoreTestSystem(t)
	sub := WebPushSubscription{Endpoint: "https://push.example/endpoint", UserID: "alice", P256DH: "public", Auth: "private"}
	if err := db.SaveWebPushSubscription(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	before, err := db.ListWebPushSubscriptions(t.Context(), "alice")
	if err != nil || len(before) != 1 {
		t.Fatalf("before: %v %v", before, err)
	}
	if err := db.SaveWebPushSubscription(t.Context(), sub); err != nil {
		t.Fatal(err)
	}
	if err := db.SetWebPushSubscriptionErrorIfCurrent(t.Context(), before[0], "stale failure"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteWebPushSubscriptionIfCurrent(t.Context(), before[0]); err != nil {
		t.Fatal(err)
	}
	after, err := db.ListWebPushSubscriptions(t.Context(), "alice")
	if err != nil || len(after) != 1 || after[0].LastError != "" || after[0].Revision == before[0].Revision {
		t.Fatalf("stale result changed renewal: %v %v", after, err)
	}
	if err := db.SetWebPushSubscriptionErrorIfCurrent(t.Context(), after[0], "current failure"); err != nil {
		t.Fatal(err)
	}
	current, err := db.ListWebPushSubscriptions(t.Context(), "alice")
	if err != nil || len(current) != 1 || current[0].LastError != "current failure" {
		t.Fatalf("current result dropped: %v %v", current, err)
	}
	if err := db.DeleteWebPushSubscriptionIfCurrent(t.Context(), current[0]); err != nil {
		t.Fatal(err)
	}
	remaining, err := db.ListWebPushSubscriptions(t.Context(), "alice")
	if err != nil || len(remaining) != 0 {
		t.Fatalf("current removal failed: %v %v", remaining, err)
	}
}

func TestPushRevisionMigrationPreservesExistingRegistration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "system.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO users(id, username, username_normalized) VALUES ('alice', 'alice', 'alice');
		ALTER TABLE web_push_subscriptions DROP COLUMN revision;
		DELETE FROM schema_version WHERE version > 105;
		INSERT OR REPLACE INTO schema_version(version) VALUES(105);
		INSERT INTO web_push_subscriptions(endpoint, user_id, p256dh, auth, user_agent, last_error)
		VALUES ('https://push.example/old', 'alice', 'old-public', 'old-private', 'old-agent', 'old-error')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	subs, err := db.ListWebPushSubscriptions(t.Context(), "alice")
	if err != nil || len(subs) != 1 {
		t.Fatalf("migrated subscriptions: %v %v", subs, err)
	}
	sub := subs[0]
	if sub.Endpoint != "https://push.example/old" || sub.P256DH != "old-public" || sub.Auth != "old-private" || sub.UserAgent != "old-agent" || sub.LastError != "old-error" || sub.Revision == "" {
		t.Fatal("existing registration changed or revision missing")
	}
	if err := db.requireCurrentSchema(); err != nil {
		t.Fatal(err)
	}
}
