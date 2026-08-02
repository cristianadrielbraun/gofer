package store

import (
	"bytes"
	"os"
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
