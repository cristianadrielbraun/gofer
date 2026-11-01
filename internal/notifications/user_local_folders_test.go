package notifications

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/handler"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Opening mail gives each user one Local folders account with a route of its
// own, shown in the sidebar and kept when the user tries to delete it.
func TestUserLocalFoldersAreCreatedRoutedAndKept(t *testing.T) {
	cleaned := make(chan string, 4)
	f := newUserStorageFixture(t, handler.UserAccountHooks{
		Created: func(context.Context, string) error { return nil },
		Updated: func(context.Context, string) error { return nil },
		Cleanup: func(_ context.Context, id string) error { cleaned <- id; return nil },
	})
	ids := map[string]string{}
	for _, owner := range []string{"alice", "bob"} {
		for range 2 {
			if rec := f.request(owner, http.MethodGet, "/", ""); rec.Code != http.StatusOK {
				t.Fatalf("%s mailbox: %d %s", owner, rec.Code, rec.Body.String())
			} else if !strings.Contains(rec.Body.String(), storage.LocalFoldersName) {
				t.Fatalf("%s sidebar has no local folders", owner)
			}
		}
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			var err error
			ids[owner], err = db.LocalFoldersAccountIDForUser(t.Context(), owner)
			return err
		}); err != nil || ids[owner] == "" {
			t.Fatalf("%s local folders = %q, %v", owner, ids[owner], err)
		}
		if state, err := f.routing.AccountStateForUser(t.Context(), owner, ids[owner]); err != nil || state != storage.AccountActive {
			t.Fatalf("%s local folders route = %s, %v", owner, state, err)
		}
		accounts, err := f.routing.UserAccountIDs(t.Context(), owner)
		if err != nil || len(accounts) != 2 {
			t.Fatalf("%s accounts = %v, %v", owner, accounts, err)
		}
		if rec := f.request(owner, http.MethodDelete, "/api/accounts/"+ids[owner], ""); rec.Code != http.StatusConflict {
			t.Fatalf("%s deleted local folders: %d", owner, rec.Code)
		}
		if state, err := f.routing.AccountStateForUser(t.Context(), owner, ids[owner]); err != nil || state != storage.AccountActive {
			t.Fatalf("%s local folders after delete = %s, %v", owner, state, err)
		}
	}
	select {
	case id := <-cleaned:
		t.Fatalf("cleanup ran for %s", id)
	default:
	}
	if ids["alice"] == ids["bob"] {
		t.Fatal("users share a local folders account")
	}
}

// With per-user storage, mail in Local folders is moved, marked and deleted
// over HTTP at once, with no server and nothing left queued.
func TestUserLocalFoldersMailChanges(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	if rec := f.request("alice", http.MethodGet, "/", ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Code)
	}
	var account string
	var messageID int64
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		var err error
		if account, err = db.LocalFoldersAccountIDForUser(t.Context(), "alice"); err != nil {
			return err
		}
		result, err := db.Write().Exec(`INSERT INTO messages (account_id, subject, from_email) VALUES (?, 'Seed swap', 'neighbor@example.test')`, account)
		if err != nil {
			return err
		}
		if messageID, err = result.LastInsertId(); err != nil {
			return err
		}
		_, err = db.Write().Exec(`INSERT INTO message_folder_state (message_id, folder_id, is_read) VALUES (?, ?, 0)`, messageID, storage.LocalFolderID(account, "archive"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(messageID, 10)
	trash := storage.LocalFolderID(account, "trash")
	if rec := f.request("alice", http.MethodPost, "/api/messages/"+id+"/move", "folder_id="+trash); rec.Code != http.StatusOK || rec.Header().Get("X-Gofer-Mail-Delivery") != "" {
		t.Fatalf("move: %d %q %s", rec.Code, rec.Header().Get("X-Gofer-Mail-Delivery"), rec.Body.String())
	}
	if rec := f.request("alice", http.MethodPost, "/api/messages/"+id+"/read", ""); rec.Code != http.StatusOK {
		t.Fatalf("read: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.request("alice", http.MethodDelete, "/api/messages/"+id+"?folder_id="+trash, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		var messages, queued int
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM messages WHERE id = ?`, messageID).Scan(&messages); err != nil {
			return err
		}
		if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_mutations`).Scan(&queued); err != nil {
			return err
		}
		if messages != 0 || queued != 0 {
			t.Fatalf("messages=%d queued=%d", messages, queued)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
