package notifications

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func ageUserFiles(t *testing.T, root string) {
	t.Helper()
	old := time.Now().Add(-48 * time.Hour)
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, old, old)
	}); err != nil {
		t.Fatal(err)
	}
}

func userDraftPaths(t *testing.T, f *userStorageFixture, owner, draft string) (int64, string, string) {
	t.Helper()
	var id int64
	var raw, attachment string
	err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
		var err error
		id, err = db.GetMessageLocalIDByInternetIDInternal(t.Context(), f.accounts[owner].ID, draft)
		if err != nil {
			return err
		}
		if err = db.Read().QueryRow(`SELECT raw_path FROM messages WHERE id=?`, id).Scan(&raw); err != nil {
			return err
		}
		return db.Read().QueryRow(`SELECT storage_path FROM attachments WHERE message_id=? LIMIT 1`, id).Scan(&attachment)
	})
	if err != nil {
		t.Fatal(err)
	}
	return id, raw, attachment
}

func TestUserFileCleanupRetainsDraftsAndUncertainSendSnapshots(t *testing.T) {
	f, server, smtp := newUserComposeFixture(t)
	uploadID, upload, err := f.blobs.StoreComposeAttachment(t.Context(), "alice", "report.txt", strings.NewReader("queued attachment"))
	if err != nil {
		t.Fatal(err)
	}
	_, foreignUpload, err := f.blobs.StoreComposeAttachment(t.Context(), "bob", "report.txt", strings.NewReader("bob attachment"))
	if err != nil {
		t.Fatal(err)
	}
	key := "<gc-draft@example.com>"
	form := url.Values{"draft_id": {key}, "to": {"recipient@example.com"}, "body": {"version one"}, "attachment_id": {uploadID}, "attachment_filename": {"report.txt"}, "attachment_content_type": {"text/plain"}}
	at := time.Now().UTC().Add(time.Hour).Truncate(5 * time.Minute)
	form.Set("schedule_timezone", "UTC")
	form.Set("schedule_date", at.Format("2006-01-02"))
	form.Set("schedule_hour", at.Format("15"))
	form.Set("schedule_minute", at.Format("04"))
	f.compose(t, "alice", "/compose/schedule", form)
	id, firstRaw, firstAttachment := userDraftPaths(t, f, "alice", key)
	// Store an immutable SMTP snapshot, then edit the visible draft. An
	// uncertain result still needs the first attachment after that edit.
	var sendID string
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		return db.Read().QueryRow(`SELECT id FROM outgoing_sends WHERE message_id=?`, id).Scan(&sendID)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE outgoing_sends SET status='ambiguous' WHERE id=?`, sendID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	form.Set("body", "version two")
	f.compose(t, "alice", "/compose/draft", form)
	_, latestRaw, latestAttachment := userDraftPaths(t, f, "alice", key)
	orphanStore, _ := f.blobs.NewMessageVersion()
	orphan, err := orphanStore.StoreRaw(t.Context(), f.accounts["alice"].ID, id, []byte("unpublished"))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := orphanStore.StoreRaw(t.Context(), f.accounts["bob"].ID, id, []byte("foreign orphan"))
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{filepath.Dir(upload), filepath.Dir(foreignUpload), filepath.Dir(filepath.Dir(firstRaw)), filepath.Dir(foreign)} {
		ageUserFiles(t, root)
	}
	result, err := f.imap.CleanupUserFiles(t.Context(), "alice")
	if err != nil || result.Skipped || result.Uploads != 1 || result.Versions != 1 {
		t.Fatalf("cleanup: %+v %v", result, err)
	}
	for _, path := range []string{upload, orphan} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("obsolete path remains: %s %v", path, err)
		}
	}
	for _, path := range []string{firstRaw, firstAttachment, latestRaw, latestAttachment, foreignUpload, foreign} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("live/foreign path lost: %s %v", path, err)
		}
	}
	if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	if smtp.count("alice") != 0 || len(server.rawCopies("alice", "Drafts")) != 1 {
		t.Fatal("cleanup changed delivery uncertainty or draft replay")
	}
	if rec := f.request("alice", http.MethodPost, "/api/outgoing-sends/"+sendID+"/cancel", ""); rec.Code != 200 {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body.String())
	}
	result, err = f.imap.CleanupUserFiles(t.Context(), "alice")
	if err != nil || result.Versions != 1 {
		t.Fatalf("obsolete snapshot cleanup: %+v %v", result, err)
	}
	if _, err := os.Stat(firstRaw); !os.IsNotExist(err) {
		t.Fatalf("canceled snapshot retained: %v", err)
	}
	f.compose(t, "alice", "/compose/draft/discard", url.Values{"draft_id": {key}})
	result, err = f.imap.CleanupUserFiles(t.Context(), "alice")
	if err != nil || result.Versions != 1 {
		t.Fatalf("discard cleanup: %+v %v", result, err)
	}
	if _, err := os.Stat(latestRaw); !os.IsNotExist(err) {
		t.Fatalf("discarded version retained: %v", err)
	}
	if err := f.imap.Sync(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	if len(server.rawCopies("alice", "Drafts")) != 0 {
		t.Fatal("remote draft deletion lost after local file cleanup")
	}
}

func TestUserFileCleanupSkipsActiveFilesAndRejectsIncompleteReferences(t *testing.T) {
	f, _, _ := newUserComposeFixture(t)
	v, _ := f.blobs.NewMessageVersion()
	path, err := v.StoreRaw(t.Context(), f.accounts["alice"].ID, 123456, []byte("candidate"))
	if err != nil {
		t.Fatal(err)
	}
	ageUserFiles(t, filepath.Dir(path))
	release, err := f.blobs.PinUserFiles(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.imap.CleanupUserFiles(t.Context(), "alice")
	if err != nil || !result.Skipped {
		t.Fatalf("busy cleanup: %+v %v", result, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if rec := f.request("bob", http.MethodGet, "/api/accounts", ""); rec.Code != 200 {
		t.Fatalf("other user blocked: %d", rec.Code)
	}
	release()
	// A query failure must not turn a partial reference set into permission
	// to unlink files. The test schema damage is confined to its temp DB.
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error { _, err := db.Write().Exec(`DROP TABLE attachments`); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.imap.CleanupUserFiles(t.Context(), "alice"); err == nil {
		t.Fatal("cleanup accepted incomplete references")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("incomplete snapshot removed candidate")
	}
	if _, err := f.imap.CleanupUserFiles(t.Context(), "unknown-owner"); !errors.Is(err, storage.ErrUserStoreOwner) {
		t.Fatalf("unknown owner: %v", err)
	}
}

func TestUserFileCleanupSnapshotBlocksPublicationAndRejectsBadPayload(t *testing.T) {
	f, _, _ := newUserComposeFixture(t)
	at := time.Now().UTC().Add(time.Hour).Truncate(5 * time.Minute)
	f.compose(t, "alice", "/compose/schedule", url.Values{"draft_id": {"<gc-queue@example.com>"}, "body": {"queued"}, "to": {"recipient@example.com"}, "schedule_timezone": {"UTC"}, "schedule_date": {at.Format("2006-01-02")}, "schedule_hour": {at.Format("15")}, "schedule_minute": {at.Format("04")}})
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		entered, resume := make(chan struct{}), make(chan struct{})
		cleanupDone := make(chan error, 1)
		go func() {
			cleanupDone <- db.WithUserBlobReferences(t.Context(), "alice", func(ids []string, keep map[string]bool) error { close(entered); <-resume; return nil })
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			return fmt.Errorf("snapshot did not start")
		}
		written := make(chan error, 1)
		go func() {
			_, err := db.Write().ExecContext(t.Context(), `UPDATE messages SET subject='after cleanup'`)
			written <- err
		}()
		select {
		case err := <-written:
			close(resume)
			<-cleanupDone
			return fmt.Errorf("publication overlapped snapshot: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		close(resume)
		if err := <-cleanupDone; err != nil {
			return err
		}
		if err := <-written; err != nil {
			return err
		}
		for _, payload := range []string{`not-json`, `null`, `{"attachments":"bad"}`} {
			if _, err := db.Write().Exec(`UPDATE outgoing_sends SET message_json=?`, payload); err != nil {
				return err
			}
			called := false
			err := db.WithUserBlobReferences(t.Context(), "alice", func([]string, map[string]bool) error { called = true; return nil })
			if err == nil || called {
				return fmt.Errorf("bad snapshot accepted: %q %v", payload, err)
			}
		}
		called := false
		if err := db.WithUserBlobReferences(t.Context(), "bob", func([]string, map[string]bool) error { called = true; return nil }); !errors.Is(err, storage.ErrUserStoreIdentity) || called {
			return fmt.Errorf("wrong owner snapshot accepted: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserFileCleanupSweepsUploadsWithoutCreatingUnusedStores(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	// This authenticated owner never opened a user DB or created an account.
	if _, err := f.system.Write().Exec(`INSERT INTO users(id,username,username_normalized,name) VALUES('idle-owner','idle-owner','idle-owner','Idle')`); err != nil {
		t.Fatal(err)
	}
	_, path, err := f.blobs.StoreComposeAttachment(t.Context(), "idle-owner", "old.txt", strings.NewReader("abandoned"))
	if err != nil {
		t.Fatal(err)
	}
	ageUserFiles(t, filepath.Dir(path))
	if err := f.imap.Start(mail.UserIMAPBackgroundOptions{PollInterval: time.Hour, ScanInterval: time.Hour, FileCleanupInterval: 20 * time.Millisecond, DisableIDLE: true}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background cleanup did not reach idle owner")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := f.routing.WithExistingUser(t.Context(), "idle-owner", func(*storage.DB) error { return nil }); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sweep created unused DB: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.imap.CleanupUserFiles(ctx, "alice"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup: %v", err)
	}
}

func TestUserFileCleanupMissingMailboxStorePreservesUploads(t *testing.T) {
	f := newUserStorageFixtureMode(t, true)
	_, upload, err := f.blobs.StoreComposeAttachment(t.Context(), "alice", "old.txt", strings.NewReader("possibly referenced"))
	if err != nil {
		t.Fatal(err)
	}
	ageUserFiles(t, filepath.Dir(upload))
	var path string
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error { path = db.Path(); return nil }); err != nil {
		t.Fatal(err)
	}
	// The one-slot manager must close Alice's file before this simulated loss.
	if err := f.routing.WithUser(t.Context(), "bob", func(*storage.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := f.imap.CleanupUserFiles(t.Context(), "alice"); !errors.Is(err, storage.ErrAccountRoute) {
		t.Fatalf("lost store authorized cleanup: %v", err)
	}
	if _, err := os.Stat(upload); err != nil {
		t.Fatalf("lost store removed upload: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleanup recreated lost store: %v", err)
	}
}
