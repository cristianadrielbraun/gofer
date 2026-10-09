package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestAbsolutizeStoredPathsResolvesRetainedRelativePaths(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "store.db")
	db, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	blobs := filepath.Join(root, "data", "accounts")
	absolute := filepath.Join(blobs, "a1", "messages", "2", "raw.eml")
	if _, err := db.Write().Exec(`INSERT INTO users(id,username,username_normalized) VALUES('alice','alice','alice');
 INSERT INTO accounts(id,user_id,provider,email_address) VALUES('a1','alice','imap','alice@example.com');
 INSERT INTO messages(id,account_id,raw_path,body_text_path,body_html_path) VALUES
  (1,'a1','data/accounts/a1/messages/1/raw.eml','data/accounts/a1/messages/1/body.txt',NULL),
  (2,'a1',?,'',NULL);
 INSERT INTO attachments(id,message_id,storage_path) VALUES(1,1,'data/accounts/a1/messages/1/file.pdf'),(2,2,'');
 INSERT INTO outgoing_sends(id,account_id,transport,envelope_from,send_after,message_json) VALUES
  ('s/1','a1','smtp','alice@example.com',CURRENT_TIMESTAMP,'{"subject":"Hi","attachments":[{"path":"data/accounts/_compose/k/1-a.txt","size":3},{"path":"/already/absolute"}]}'),
  ('s2','a1','smtp','alice@example.com',CURRENT_TIMESTAMP,'not json')`, absolute); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	layout := UserStorageLayout{WorkingDirectory: root, BlobDirectory: blobs}
	for range 2 { // The second pass finds nothing left to rewrite.
		if err := absolutizeStoredPaths(t.Context(), path, layout); err != nil {
			t.Fatal(err)
		}
	}
	check, err := openReadOnlyDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var raw, text string
	var html sql.NullString
	if err := check.QueryRow(`SELECT raw_path,body_text_path,body_html_path FROM messages WHERE id=1`).Scan(&raw, &text, &html); err != nil {
		t.Fatal(err)
	}
	if raw != filepath.Join(blobs, "a1", "messages", "1", "raw.eml") || text != filepath.Join(blobs, "a1", "messages", "1", "body.txt") || html.Valid {
		t.Fatal("message paths", raw, text, html)
	}
	if err := check.QueryRow(`SELECT raw_path,body_text_path FROM messages WHERE id=2`).Scan(&raw, &text); err != nil || raw != absolute || text != "" {
		t.Fatal("absolute or empty path changed", raw, text, err)
	}
	var attachment, empty string
	if err := check.QueryRow(`SELECT (SELECT storage_path FROM attachments WHERE id=1),(SELECT storage_path FROM attachments WHERE id=2)`).Scan(&attachment, &empty); err != nil || attachment != filepath.Join(blobs, "a1", "messages", "1", "file.pdf") || empty != "" {
		t.Fatal("attachment paths", attachment, empty, err)
	}
	var compose, untouched, size, invalid string
	if err := check.QueryRow(`SELECT json_extract(message_json,'$.attachments[0].path'),json_extract(message_json,'$.attachments[1].path'),json_extract(message_json,'$.attachments[0].size')||json_extract(message_json,'$.subject'),(SELECT message_json FROM outgoing_sends WHERE id='s2') FROM outgoing_sends WHERE id='s/1'`).Scan(&compose, &untouched, &size, &invalid); err != nil {
		t.Fatal(err)
	}
	if compose != filepath.Join(blobs, "_compose", "k", "1-a.txt") || untouched != "/already/absolute" || size != "3Hi" || invalid != "not json" {
		t.Fatal("send snapshot paths", compose, untouched, size, invalid)
	}

	// A relative path that escapes the blob directory is never rewritten.
	db, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`UPDATE attachments SET storage_path='elsewhere/file.pdf' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := absolutizeStoredPaths(t.Context(), path, layout); err == nil {
		t.Fatal("escaping path accepted")
	}
}
