package storage

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/store"
)

func TestMigrateV151ToV152RestoresImagesSavedRemoteContentHid(t *testing.T) {
	db := newContactsTestDB(t)
	blobs := store.NewBlobStore(t.TempDir())
	padding := strings.Repeat("<p>Lunch on Friday? The usual place at noon.</p>\n", 20)
	hidden := []byte(padding + `<img src="/api/remote-assets/1/0123456789abcdef.png"><img src="" data-removed-src="https://cdn.example/signature.png" alt="Alex">`)
	savedPath, err := blobs.StoreRemoteBodyHTML("acc", 1, hidden)
	if err != nil {
		t.Fatal(err)
	}
	// Only saved remote content was ever marked this way; other bodies keep
	// whatever they hold.
	plain := []byte(`<p>Notes for Thursday</p><p>data-removed-src="kept as written"</p>`)
	plainPath, err := blobs.StoreBodyHTML(t.Context(), "acc", 2, plain)
	if err != nil {
		t.Fatal(err)
	}
	missingPath := filepath.Join(filepath.Dir(savedPath), "..", "3", "body_remote.html")
	for _, statement := range []string{
		`DELETE FROM schema_version`,
		`INSERT INTO schema_version (version) VALUES (151)`,
		`INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('acc', 'default', 'imap', 'alex@example.com')`,
	} {
		if _, err := db.Write().Exec(statement); err != nil {
			t.Fatalf("set up schema 151: %v", err)
		}
	}
	for id, path := range map[int64]string{1: savedPath, 2: plainPath, 3: missingPath} {
		if _, err := db.Write().Exec(`INSERT INTO messages (id, account_id, internet_message_id, subject, body_html_path) VALUES (?, 'acc', ?, 'Lunch on Friday?', ?)`, id, "<lunch-"+strconv.FormatInt(id, 10)+"@example.com>", path); err != nil {
			t.Fatalf("insert message %d: %v", id, err)
		}
	}
	for range 2 {
		if err := migrateV151ToV152(db.Write()); err != nil {
			t.Fatalf("migrateV151ToV152() error = %v", err)
		}
	}
	got, err := store.ReadMessageFile(savedPath)
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.ReplaceAll(hidden, []byte(`data-removed-src=`), []byte(`data-remote-src=`))
	if !bytes.Equal(got, want) {
		t.Fatalf("saved remote content after migration:\n%s\nwant:\n%s", got, want)
	}
	if got, err := store.ReadMessageFile(plainPath); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("other body changed: %q, %v", got, err)
	}
	if n := countRows(t, db, `SELECT MAX(version) FROM schema_version`); n != 152 {
		t.Fatalf("schema version = %d, want 152", n)
	}
}

func TestMigrateV151ToV152WithoutMessages(t *testing.T) {
	raw, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(`CREATE TABLE schema_version (version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO schema_version (version) VALUES (151)`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := migrateV151ToV152(raw); err != nil {
			t.Fatalf("migrateV151ToV152() error = %v", err)
		}
	}
	var version int
	if err := raw.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil || version != 152 {
		t.Fatalf("version = %d, %v", version, err)
	}
}
