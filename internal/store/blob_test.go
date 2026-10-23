package store

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestComposeAttachmentsAreScopedByUser(t *testing.T) {
	store := NewBlobStore(t.TempDir())
	id, path, err := store.StoreComposeAttachment(t.Context(), "owner", "private.txt", bytes.NewBufferString("private"))
	if err != nil {
		t.Fatalf("StoreComposeAttachment() error = %v", err)
	}
	if _, err := store.ComposeAttachmentPath("owner", id); err != nil {
		t.Fatalf("owner ComposeAttachmentPath() error = %v", err)
	}
	if _, err := store.ComposeAttachmentPath("attacker", id); !os.IsNotExist(err) {
		t.Fatalf("foreign ComposeAttachmentPath() error = %v, want not exist", err)
	}
	if err := store.DeleteComposeAttachment("attacker", id); err != nil {
		t.Fatalf("foreign DeleteComposeAttachment() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("foreign delete removed owner file: %v", err)
	}
	if err := store.DeleteComposeAttachment("owner", id); err != nil {
		t.Fatalf("owner DeleteComposeAttachment() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("owner file still exists: %v", err)
	}
}

func TestCleanupComposeAttachmentsVisitsUserDirectories(t *testing.T) {
	store := NewBlobStore(t.TempDir())
	_, path, err := store.StoreComposeAttachment(t.Context(), "owner", "old.txt", bytes.NewBufferString("old"))
	if err != nil {
		t.Fatalf("StoreComposeAttachment() error = %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}
	removed, err := store.CleanupComposeAttachments(time.Hour, nil)
	if err != nil {
		t.Fatalf("CleanupComposeAttachments() error = %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("old compose attachment still exists: %v", err)
	}
}

func TestDeleteAccountRejectsPathsOutsideAnAccountDirectory(t *testing.T) {
	base := t.TempDir()
	store := NewBlobStore(base)
	marker := filepath.Join(base, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, accountID := range []string{"", ".", "..", "../outside", "nested/account", `nested\account`} {
		if err := store.DeleteAccount(accountID); err == nil {
			t.Fatalf("DeleteAccount(%q) error = nil", accountID)
		}
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("account path rejection removed base marker: %v", err)
	}
}

func TestMessageFileSizesAddUpEachMessage(t *testing.T) {
	store := NewBlobStore(t.TempDir())
	ctx := t.Context()
	if _, err := store.StoreBodyHTML(ctx, "acc", 7, []byte("<p>Lunch on Friday?</p>")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StoreAttachment(ctx, "acc", 7, 1, "menu.txt", bytes.NewBufferString("soup, bread")); err != nil {
		t.Fatal(err)
	}
	version, err := store.NewMessageVersion()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := version.StoreBodyText(ctx, "acc", 9, []byte("Site photos attached")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StoreBodyText(ctx, "other", 7, []byte("not this account")); err != nil {
		t.Fatal(err)
	}

	sizes, err := store.MessageFileSizes("acc")
	if err != nil {
		t.Fatal(err)
	}
	size := func(id int64) int64 {
		t.Helper()
		var total int64
		err := filepath.WalkDir(filepath.Join(store.basePath, "acc", "messages", fmt.Sprint(id)), func(_ string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := d.Info()
			total += info.Size()
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return total
	}
	if len(sizes) != 2 || sizes[7] != size(7) || sizes[9] != size(9) || sizes[7] == 0 || sizes[9] == 0 {
		t.Fatalf("sizes = %v, want 7: %d, 9: %d", sizes, size(7), size(9))
	}

	if sizes, err := store.MessageFileSizes("nobody"); err != nil || len(sizes) != 0 {
		t.Fatalf("account without files = %v, %v", sizes, err)
	}
	if _, err := store.MessageFileSizes(".."); err == nil {
		t.Fatal("MessageFileSizes(\"..\") read outside the blob directory")
	}
}
