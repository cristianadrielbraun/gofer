package storage

import (
	"crypto/sha256"
	"os"
	"reflect"
	"strings"
	"testing"
)

// An incompatible file is evidence to preserve, never an invitation for the
// cache opener to upgrade it or replace it with an empty current-schema store.
func TestUserStoresRejectIncompatibleSchemaBeforeOpeningWriter(t *testing.T) {
	for _, mode := range []string{"older", "newer", "missing-version"} {
		t.Run(mode, func(t *testing.T) {
			m := newUserStoreTestManager(t, newUserStoreTestSystem(t), UserStoreOptions{MaxOpen: 1})
			if err := m.WithUser(t.Context(), "alice", func(db *DB) error {
				_, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,email_address) VALUES('kept-account','alice','alice@example.com')`)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			m.mu.Lock()
			evicted := m.evictOldestLocked()
			m.mu.Unlock()
			if !evicted {
				t.Fatal("fixture did not close store")
			}
			waitUserStorePhysicalClose(t, m, "alice")
			path := m.userPath("alice")
			edit, err := openDB(path)
			if err != nil {
				t.Fatal(err)
			}
			edit.SetMaxOpenConns(1)
			switch mode {
			case "older":
				_, err = edit.Exec(`ALTER TABLE calendar_response_requests DROP COLUMN claim_id`)
				if err == nil {
					_, err = edit.Exec(`UPDATE schema_version SET version=106`)
				}
			case "newer":
				_, err = edit.Exec(`UPDATE schema_version SET version=?`, CurrentSchemaVersion+1)
			case "missing-version":
				_, err = edit.Exec(`DROP TABLE schema_version`)
			}
			if err != nil {
				edit.Close()
				t.Fatal(err)
			}
			// New/OpenExisting would change this journal mode before checking the schema.
			var journal string
			if err := edit.QueryRow(`PRAGMA journal_mode=DELETE`).Scan(&journal); err != nil {
				edit.Close()
				t.Fatal(err)
			}
			if err := edit.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(m.directory)
			if err != nil {
				t.Fatal(err)
			}
			names := func(entries []os.DirEntry) []string {
				out := make([]string, len(entries))
				for i, e := range entries {
					out[i] = e.Name()
				}
				return out
			}
			for _, create := range []bool{false, true} {
				lease, err := m.acquire(t.Context(), "alice", create)
				if err == nil {
					lease.Release()
					t.Fatal("incompatible schema opened", mode, create)
				}
				if !strings.Contains(err.Error(), "schema version") {
					t.Fatal("schema preflight not reported", err)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatal("rejected database bytes changed", mode)
			}
			afterEntries, err := os.ReadDir(m.directory)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(names(entries), names(afterEntries)) {
				t.Fatal("rejection created database sidecars", names(entries), names(afterEntries))
			}
			// A rejected open must release its cache slot so a healthy owner can progress.
			if err := m.WithUser(t.Context(), "bob", func(db *DB) error { return db.Read().Ping() }); err != nil {
				t.Fatal("failed open pinned cache slot", err)
			}
		})
	}
}
