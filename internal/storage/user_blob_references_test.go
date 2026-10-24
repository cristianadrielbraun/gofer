package storage

import (
	"errors"
	"os"
	"testing"
)

func TestUserBlobReferencesRetainRetryableDeliveryAndSentCopies(t *testing.T) {
	system := newUserStoreTestSystem(t)
	m := newUserStoreTestManager(t, system, UserStoreOptions{MaxOpen: 1})
	db := acquireUserStore(t, m, "alice").DB()
	if _, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,email_address) VALUES('account','alice','alice@example.com')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO outgoing_sends(id,account_id,transport,envelope_from,send_after,message_json) VALUES('send','account','smtp','alice@example.com',CURRENT_TIMESTAMP,'{"attachments":[{"path":"/retained/version/attachment.txt"}]}')`); err != nil {
		t.Fatal(err)
	}
	for _, state := range []struct {
		status, copy string
		keep         bool
	}{
		{"pending", "not_required", true}, {"sending", "not_required", true},
		{"failed", "not_required", true}, {"ambiguous", "not_required", true},
		{"sent", "pending", true}, {"sent", "copying", true},
		{"sent", "failed", true}, {"sent", "ambiguous", true},
		{"canceled", "not_required", false}, {"sent", "complete", false},
		{"sent", "not_required", false},
	} {
		t.Run(state.status+"/"+state.copy, func(t *testing.T) {
			if _, err := db.Write().Exec(`UPDATE outgoing_sends SET status=?,sent_copy_status=?`, state.status, state.copy); err != nil {
				t.Fatal(err)
			}
			called := false
			if err := db.WithUserBlobReferences(t.Context(), "alice", func(refs BlobReferences) error {
				called = true
				if len(refs.Accounts) != 1 || refs.Accounts[0] != "account" {
					t.Fatalf("account snapshot: %v", refs.Accounts)
				}
				if refs.Paths["/retained/version/attachment.txt"] != state.keep {
					t.Fatalf("retained files: %v, want keep=%v", refs.Paths, state.keep)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("snapshot callback missing")
			}
		})
	}
	// The shared central DB cannot be used with an incomplete owner keep-set.
	called := false
	if err := system.WithUserBlobReferences(t.Context(), "alice", func(BlobReferences) error { called = true; return nil }); err == nil || called {
		t.Fatalf("shared DB cleanup accepted: %v", err)
	}
}

func TestWithExistingUserMissingFileDoesNotEvictCachedStore(t *testing.T) {
	system := newUserStoreTestSystem(t)
	m := newUserStoreTestManager(t, system, UserStoreOptions{MaxOpen: 1})
	routing, err := NewAccountRouting(m)
	if err != nil {
		t.Fatal(err)
	}
	lease := acquireUserStore(t, m, "alice")
	cached := lease.DB()
	lease.Release()
	if err := routing.WithExistingUser(t.Context(), "bob", func(*DB) error { t.Fatal("missing store callback ran"); return nil }); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing store: %v", err)
	}
	m.mu.Lock()
	entry := m.entries["alice"]
	m.mu.Unlock()
	if entry == nil || entry.db != cached {
		t.Fatal("missing-store maintenance evicted the useful handle")
	}
	if err := routing.WithExistingUser(t.Context(), "alice", func(db *DB) error {
		if db != cached {
			t.Fatal("existing handle replaced")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBlobReferencesCoverEverySharedAccount(t *testing.T) {
	db := newContactsTestDB(t)
	if _, err := db.Write().Exec(`INSERT INTO accounts(id,user_id,email_address,is_deleting) VALUES
		('ana','default','ana@example.com',0), ('rosa','default','rosa@example.com',0), ('leaving','default','old@example.com',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO messages(id,account_id,body_text_path) VALUES
		(1,'ana','/blobs/ana/messages/1/body.txt'), (2,'rosa',NULL), (3,'leaving','/blobs/leaving/messages/3/body.txt')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO attachments(message_id,storage_path) VALUES(2,'/blobs/rosa/messages/2/attachments/1-menu.txt')`); err != nil {
		t.Fatal(err)
	}
	var refs BlobReferences
	if err := db.WithBlobReferences(t.Context(), func(r BlobReferences) error { refs = r; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(refs.Accounts) != 2 || refs.Accounts[0] != "ana" || refs.Accounts[1] != "rosa" {
		t.Fatalf("accounts = %v", refs.Accounts)
	}
	if !refs.Messages["ana"][1] || !refs.Messages["rosa"][2] || len(refs.Messages["ana"]) != 1 || refs.Messages["leaving"] != nil {
		t.Fatalf("messages = %v", refs.Messages)
	}
	if !refs.Paths["/blobs/ana/messages/1/body.txt"] || !refs.Paths["/blobs/rosa/messages/2/attachments/1-menu.txt"] {
		t.Fatalf("paths = %v", refs.Paths)
	}
}
