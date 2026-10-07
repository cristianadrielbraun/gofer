package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserCalendarReplyRestoresWithFreshRepositoryAndDisposableDatabaseCopy(t *testing.T) {
	f := newOwnedReplyFixture(t)
	id := f.queue(t, "alice")
	var source string
	if err := f.accounts.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error { source = db.Path(); return nil }); err != nil {
		t.Fatal(err)
	}
	// MaxOpen1: close/checkpoint Alice before copying its database file.
	if err := f.accounts.WithUser(t.Context(), "bob", func(_ *AccountStore, _ *storage.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		info, err := os.Stat(source + suffix)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal("could not inspect source SQLite sidecar", suffix, err)
		}
		if err == nil && info.Size() != 0 {
			t.Fatal("source still has live SQLite sidecar", suffix)
		}
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	destination := filepath.Join(directory, filepath.Base(source))
	if err := os.WriteFile(destination, data, 0600); err != nil {
		t.Fatal(err)
	}
	stores, err := storage.NewUserStores(f.system, storage.UserStoreOptions{Directory: directory, MaxOpen: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := stores.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	routing, err := storage.NewAccountRouting(stores)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewUserAccountStore(routing, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := repository.RestoreCalendarReply(t.Context(), "alice", id, true)
	if err != nil {
		t.Fatal("saved authority could not be restored", err)
	}
	if snapshot.Job().ID != id || snapshot.Source().Source().AccountID != f.claims["alice"].event.service.id {
		t.Fatal("copied identity changed")
	}
	if err := repository.ValidateCalendarReply(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err := repository.WithUser(t.Context(), "alice", func(_ *AccountStore, db *storage.DB) error {
		if db.Path() != destination {
			t.Fatal("fresh repository borrowed original store")
		}
		_, err := db.Write().Exec(`UPDATE outgoing_sends SET status='sent',mime_data=NULL,message_json='' WHERE id=?`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RestoreCalendarReply(t.Context(), "alice", id, true); err == nil {
		t.Fatal("fresh repository allowed sent email preparation")
	}
	if _, err := repository.RestoreCalendarReply(t.Context(), "alice", id, false); err != nil {
		t.Fatal("fresh repository lost calendar-only followup", err)
	}
	// The acceptance state was written only to the disposable copy.
	original, err := f.accounts.RestoreCalendarReply(t.Context(), "alice", id, true)
	if err != nil || original.Job().SendStatus != storage.OutgoingSendPending {
		t.Fatal("source database changed", err)
	}
}
