package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ageCleanupTree(t *testing.T, root string) {
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

func TestUserFileCleanupScopesAndRetainsWholeVersions(t *testing.T) {
	base := t.TempDir()
	s := NewBlobStore(base)
	_, upload, err := s.StoreComposeAttachment(t.Context(), "alice", "old.txt", strings.NewReader("old"))
	if err != nil {
		t.Fatal(err)
	}
	_, retainedUpload, err := s.StoreComposeAttachment(t.Context(), "alice", "keep.txt", strings.NewReader("keep"))
	if err != nil {
		t.Fatal(err)
	}
	_, foreignUpload, err := s.StoreComposeAttachment(t.Context(), "bob", "old.txt", strings.NewReader("foreign"))
	if err != nil {
		t.Fatal(err)
	}
	version, _ := s.NewMessageVersion()
	retained, err := version.StoreBodyText(t.Context(), "alice-account", 1, []byte("keep"))
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := version.StoreRaw(t.Context(), "alice-account", 1, []byte("keep sibling"))
	if err != nil {
		t.Fatal(err)
	}
	orphanVersion, _ := s.NewMessageVersion()
	orphan, err := orphanVersion.StoreRaw(t.Context(), "alice-account", 1, []byte("orphan"))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := orphanVersion.StoreRaw(t.Context(), "bob-account", 1, []byte("foreign"))
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := s.StoreRaw(t.Context(), "alice-account", 1, []byte("legacy"))
	if err != nil {
		t.Fatal(err)
	}
	asset, err := s.StoreRemoteAsset("alice-account", 1, "image.png", []byte("asset"))
	if err != nil {
		t.Fatal(err)
	}
	ageCleanupTree(t, base)
	result, err := s.CleanupUserFiles(t.Context(), "alice", []string{"alice-account"}, map[string]bool{retained: true, retainedUpload: true}, 24*time.Hour)
	if err != nil || result.Uploads != 1 || result.Versions != 1 {
		t.Fatalf("cleanup: %+v %v", result, err)
	}
	for _, path := range []string{upload, orphan} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("obsolete path remains: %s %v", path, err)
		}
	}
	for _, path := range []string{retainedUpload, foreignUpload, retained, sibling, foreign, legacy, asset} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retained path lost: %s %v", path, err)
		}
	}
}

func TestUserFileCleanupKeepsFreshDescendantsAndSymlinks(t *testing.T) {
	s := NewBlobStore(t.TempDir())
	v, _ := s.NewMessageVersion()
	oldPath, err := v.StoreRaw(t.Context(), "account", 1, []byte("old"))
	if err != nil {
		t.Fatal(err)
	}
	ageCleanupTree(t, s.basePath)
	fresh, err := v.StoreAttachment(t.Context(), "account", 1, 1, "new.txt", strings.NewReader("new"))
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	marker := filepath.Join(outside, "private")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.basePath, "symlink-account")); err != nil {
		t.Fatal(err)
	}
	linkedVersion, _ := s.NewMessageVersion()
	linkedRaw, err := linkedVersion.StoreRaw(t.Context(), "account", 2, []byte("linked"))
	if err != nil {
		t.Fatal(err)
	}
	ageCleanupTree(t, filepath.Dir(linkedRaw))
	if err := os.Symlink(marker, filepath.Join(filepath.Dir(linkedRaw), "link")); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Dir(linkedRaw), old, old); err != nil {
		t.Fatal(err)
	}
	result, err := s.CleanupUserFiles(t.Context(), "alice", []string{"account", "symlink-account"}, nil, 24*time.Hour)
	if err != nil || result.Versions != 0 {
		t.Fatalf("cleanup: %+v %v", result, err)
	}
	for _, path := range []string{oldPath, fresh, marker, linkedRaw} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.CleanupUserFiles(ctx, "alice", []string{"account"}, nil, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup: %v", err)
	}
}

func TestUserFilePinsSkipBusyOwnersAndBoundWaits(t *testing.T) {
	s := NewBlobStore(t.TempDir())
	release, err := s.PinUserFiles(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	v, _ := s.NewMessageVersion()
	nested, err := v.PinUserFiles(t.Context(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if end, ok := s.TryUserFileCleanup("alice"); ok {
		end()
		t.Fatal("cleanup acquired a busy owner")
	}
	release()
	release()
	if end, ok := s.TryUserFileCleanup("alice"); ok {
		end()
		t.Fatal("version pin did not share activity")
	}
	nested()
	end, ok := s.TryUserFileCleanup("alice")
	if !ok {
		t.Fatal("cleanup did not acquire idle owner")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if release, err := s.PinUserFiles(ctx, "alice"); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("pin during cleanup: %v", err)
	}
	bob, err := s.PinUserFiles(t.Context(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	bob()
	entered := make(chan func(), 1)
	go func() { release, _ := s.PinUserFiles(t.Context(), "alice"); entered <- release }()
	end()
	end()
	select {
	case release := <-entered:
		if release == nil {
			t.Fatal("waiting reader failed")
		}
		release()
	case <-time.After(time.Second):
		t.Fatal("reader did not resume")
	}
	s.activity.mu.Lock()
	defer s.activity.mu.Unlock()
	if len(s.activity.owners) != 0 {
		t.Fatalf("activity entries leaked: %d", len(s.activity.owners))
	}
}
