package storage

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

type contactChangeRecorder struct {
	mu     sync.Mutex
	owners []string
}

func (r *contactChangeRecorder) record(owner string) {
	r.mu.Lock()
	r.owners = append(r.owners, owner)
	r.mu.Unlock()
}

// told waits out the gathering delay and returns who was told since the
// last call.
func (r *contactChangeRecorder) told() []string {
	time.Sleep(contactChangeDelay + 300*time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()
	owners := r.owners
	r.owners = nil
	return owners
}

func TestContactChangesTellTheOwnerOnceCommitted(t *testing.T) {
	db, err := New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('owner-a','owner-a','owner-a'),('owner-b','owner-b','owner-b')`); err != nil {
		t.Fatal(err)
	}
	var recorder contactChangeRecorder
	db.SetContactChangeHook(recorder.record)
	ctx := context.Background()

	// A burst of saves is one notice.
	for _, contact := range []models.Contact{{Name: "Ana Moreno", Email: "ana@example.test"}, {Name: "Bruno Salas", Email: "bruno@example.test"}} {
		if _, err := db.SaveContact(ctx, "owner-a", contact); err != nil {
			t.Fatal(err)
		}
	}
	if owners := recorder.told(); len(owners) != 1 || owners[0] != "owner-a" {
		t.Fatalf("after saving, told %v; want [owner-a]", owners)
	}

	// A change that is rolled back tells no one.
	tx, err := db.Write().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE contact_profiles SET display_name = 'Someone else' WHERE user_id = 'owner-a'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if owners := recorder.told(); len(owners) != 0 {
		t.Fatalf("after a rollback, told %v; want no one", owners)
	}

	// Mail seen from an address only updates its observation.
	if _, err := db.Write().Exec(`INSERT INTO contact_observations (user_id, normalized_email, email) VALUES ('owner-b', 'carla@example.test', 'carla@example.test')`); err != nil {
		t.Fatal(err)
	}
	if owners := recorder.told(); len(owners) != 0 {
		t.Fatalf("after an observation, told %v; want no one", owners)
	}

	// Each owner is told about their own contacts only.
	if _, err := db.SaveContact(ctx, "owner-b", models.Contact{Name: "Diego Vidal", Email: "diego@example.test"}); err != nil {
		t.Fatal(err)
	}
	if owners := recorder.told(); len(owners) != 1 || owners[0] != "owner-b" {
		t.Fatalf("after owner-b saved, told %v; want [owner-b]", owners)
	}
}

func TestClosedDatabaseStopsWatchingContacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gofer.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := contactWatches.Load(path); !ok {
		t.Fatal("an open database is not watched")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := contactWatches.Load(path); ok {
		t.Fatal("a closed database is still watched")
	}
}

// A person's own store tells the system database's hook, as their contact
// activity does, since only the system database is wired to the pages.
func TestOwnedStoreTellsTheSystemHook(t *testing.T) {
	system, err := New(filepath.Join(t.TempDir(), "system.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer system.Close()
	owned, err := New(filepath.Join(t.TempDir(), "owned.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	if _, err := owned.Write().Exec(`INSERT OR IGNORE INTO users(id,username,username_normalized) VALUES('default','default','default')`); err != nil {
		t.Fatal(err)
	}
	owned.contactHookSource = system
	var recorder contactChangeRecorder
	system.SetContactChangeHook(recorder.record)
	if _, err := owned.SaveContact(context.Background(), "default", models.Contact{Name: "Elena Quiroga", Email: "elena@example.test"}); err != nil {
		t.Fatal(err)
	}
	if owners := recorder.told(); len(owners) != 1 || owners[0] != "default" {
		t.Fatalf("told %v; want [default]", owners)
	}
}
