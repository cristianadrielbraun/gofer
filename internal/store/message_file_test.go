package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMessageFilesAreSavedCompressedAndReadBack(t *testing.T) {
	blobs := NewBlobStore(t.TempDir())
	body := []byte(strings.Repeat("<p>Hello Ana, here are the notes for Thursday.</p>\n", 200))
	path, err := blobs.StoreBodyHTML(context.Background(), "acc", 7, body)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(stored, zstdMagic) || len(stored) >= len(body)/4 {
		t.Fatalf("stored %d bytes for a %d byte body, compressed %t", len(stored), len(body), bytes.HasPrefix(stored, zstdMagic))
	}
	read, err := ReadMessageFile(path)
	if err != nil || !bytes.Equal(read, body) {
		t.Fatalf("read back %d bytes, err %v; want the %d byte body", len(read), err, len(body))
	}
	reader, err := blobs.ReadBodyHTML(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var streamed bytes.Buffer
	if _, err := streamed.ReadFrom(reader); err != nil || !bytes.Equal(streamed.Bytes(), body) {
		t.Fatalf("reader returned %d bytes, err %v", streamed.Len(), err)
	}
}

func TestMessageFilesThatWouldNotShrinkAreSavedAsTheyAre(t *testing.T) {
	blobs := NewBlobStore(t.TempDir())
	small := []byte("Thanks, see you tomorrow.")
	random := make([]byte, 4096)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"small": small, "incompressible": random} {
		path, err := blobs.StoreBodyText(context.Background(), "acc", int64(len(data)), data)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(stored, data) {
			t.Fatalf("%s: stored bytes differ from the body (err %v)", name, err)
		}
		read, err := ReadMessageFile(path)
		if err != nil || !bytes.Equal(read, data) {
			t.Fatalf("%s: read back differs (err %v)", name, err)
		}
	}
}

func TestMessageFilesSavedBeforeCompressionStillRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raw.eml")
	raw := []byte("From: Ben <ben@example.test>\r\nSubject: Lunch\r\n\r\n" + strings.Repeat("Shall we meet at noon?\r\n", 100))
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	read, err := ReadMessageFile(path)
	if err != nil || !bytes.Equal(read, raw) {
		t.Fatalf("plain file read back %d bytes, err %v", len(read), err)
	}
}

func TestDamagedCompressedMessageFileFailsToRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "body.html")
	stored := encodeMessageFile([]byte(strings.Repeat("<p>Quarterly figures</p>", 100)))
	stored = stored[:len(stored)/2]
	if err := os.WriteFile(path, stored, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMessageFile(path); err == nil {
		t.Fatal("a cut-off compressed file was read without an error")
	}
}

func TestRewriteMessageFileKeepsFilesCompressedAndLeavesUnchangedOnes(t *testing.T) {
	blobs := NewBlobStore(t.TempDir())
	body := []byte(strings.Repeat("<p>Lunch on Friday?</p>\n", 100))
	path, err := blobs.StoreRemoteBodyHTML("acc", 7, body)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := RewriteMessageFile(path, func(data []byte) ([]byte, bool) { return data, false })
	if err != nil || changed {
		t.Fatalf("unchanged rewrite = %t, %v", changed, err)
	}
	if after, err := os.Stat(path); err != nil || !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("unchanged file was written again: %v", err)
	}

	changed, err = RewriteMessageFile(path, func(data []byte) ([]byte, bool) {
		return bytes.ReplaceAll(data, []byte("Friday"), []byte("Monday")), true
	})
	if err != nil || !changed {
		t.Fatalf("rewrite = %t, %v", changed, err)
	}
	stored, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(stored, zstdMagic) {
		t.Fatalf("rewritten file is not compressed: %v", err)
	}
	read, err := ReadMessageFile(path)
	if err != nil || !bytes.Equal(read, bytes.ReplaceAll(body, []byte("Friday"), []byte("Monday"))) {
		t.Fatalf("read back %q, %v", read, err)
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), compressTempPrefix+"*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v %v", leftovers, err)
	}

	if _, err := RewriteMessageFile(filepath.Join(filepath.Dir(path), "missing.html"), func(data []byte) ([]byte, bool) { return data, true }); !os.IsNotExist(err) {
		t.Fatalf("missing file error = %v", err)
	}
}
