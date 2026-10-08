package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/store"
)

func migrationFilesSQL(t *testing.T, db *DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Write().Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func migrationFilesSend(t *testing.T, db *DB, id, status, copyStatus, data string) {
	t.Helper()
	migrationFilesSQL(t, db, `INSERT INTO outgoing_sends(id,account_id,transport,envelope_from,send_after,status,sent_copy_status,message_json) VALUES(?,'alice-mail','smtp','alice@example.com',CURRENT_TIMESTAMP,?,?,?)`, id, status, copyStatus, data)
}

func migrationFilesSnapshot(t *testing.T, paths ...string) string {
	t.Helper()
	attachments := make([]map[string]string, 0, len(paths))
	for _, path := range paths {
		attachments = append(attachments, map[string]string{"path": path})
	}
	data, err := json.Marshal(map[string]any{"attachments": attachments})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestUserStorageMigrationFilesPreservesRealBlobStorePathsAndReadOnlySource(t *testing.T) {
	db := sharedMigrationFixture(t)
	base := filepath.Join(filepath.Dir(db.Path()), "accounts")
	blobs := store.NewBlobStore(base)
	version, err := blobs.NewMessageVersion()
	if err != nil {
		t.Fatal(err)
	}
	var message int64
	if err := db.Read().QueryRow(`SELECT id FROM messages WHERE account_id='alice-mail'`).Scan(&message); err != nil {
		t.Fatal(err)
	}
	textPath, err := version.StoreBodyText(t.Context(), "alice-mail", message, []byte("private message"))
	if err != nil {
		t.Fatal(err)
	}
	htmlPath, err := version.StoreBodyHTML(t.Context(), "alice-mail", message, []byte("<p>private message</p>"))
	if err != nil {
		t.Fatal(err)
	}
	originalPath, err := version.StoreBodyOriginalHTML(t.Context(), "alice-mail", message, []byte("original html"))
	if err != nil {
		t.Fatal(err)
	}
	rawPath, err := version.StoreRaw(t.Context(), "alice-mail", message, []byte("original RFC822"))
	if err != nil {
		t.Fatal(err)
	}
	relativeText, err := filepath.Rel(filepath.Dir(db.Path()), textPath)
	if err != nil {
		t.Fatal(err)
	}
	migrationFilesSQL(t, db, `UPDATE messages SET body_text_path=?,body_html_path=?,body_html_original_path=?,raw_path=? WHERE id=?`, relativeText, htmlPath, originalPath, rawPath, message)
	attachmentContent := "binary attachment\x00\xff"
	attachment, err := version.StoreAttachment(t.Context(), "alice-mail", message, 1, "report.txt", strings.NewReader(attachmentContent))
	if err != nil {
		t.Fatal(err)
	}
	checksum := sha256.Sum256([]byte(attachmentContent))
	migrationFilesSQL(t, db, `INSERT INTO attachments(message_id,storage_path,sha256) VALUES(?,?,?)`, message, attachment, strings.ToUpper(hex.EncodeToString(checksum[:])))
	_, compose, err := blobs.StoreComposeAttachment(t.Context(), "alice", "draft.txt", strings.NewReader("draft content"))
	if err != nil {
		t.Fatal(err)
	}
	migrationFilesSQL(t, db, `INSERT INTO accounts(id,user_id,email_address) VALUES('alice-second','alice','other@example.com')`)
	reused, err := blobs.StoreAttachment(t.Context(), "alice-second", 999, 2, "forward.txt", strings.NewReader("reused by same owner"))
	if err != nil {
		t.Fatal(err)
	}
	migrationFilesSend(t, db, "retained", "ambiguous", "pending", migrationFilesSnapshot(t, compose, reused))
	missing := filepath.Join(base, "bob-mail", "messages", "42", "body.txt")
	migrationFilesSQL(t, db, `UPDATE messages SET body_text_path=? WHERE account_id='bob-mail'`, missing)
	avatar, err := blobs.StoreAvatar(strings.Repeat("a", 64), "image/png", []byte("avatar"))
	if err != nil {
		t.Fatal(err)
	}
	migrationFilesSQL(t, db, `INSERT INTO sender_avatars(email_hash,storage_path) VALUES('avatar',?)`, avatar)
	remote, err := version.StoreRemoteAsset("alice-mail", message, "https://fixture.invalid/image.png", []byte("cached remote asset"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := version.StoreRemoteBodyHTML("alice-mail", message, []byte("rendered remote body")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := blobs.StoreComposeAttachment(t.Context(), "bob", "unindexed.txt", strings.NewReader("unindexed upload")); err != nil {
		t.Fatal(err)
	}
	migrationFilesSend(t, db, "malformed", "failed", "not_required", "original invalid JSON")
	migrationFilesSend(t, db, "null", "ambiguous", "not_required", "null")
	// Foreign paths in inert history must not become owned file references.
	foreignHistory := migrationFilesSnapshot(t, filepath.Join(base, "deleted-account", "original.txt"))
	migrationFilesSend(t, db, "canceled", "canceled", "pending", foreignHistory)
	migrationFilesSend(t, db, "complete", "sent", "complete", foreignHistory)
	migrationFilesSend(t, db, "no-copy", "sent", "not_required", foreignHistory)
	path := db.Path()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source, err := openUserStorageMigrationSource(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	proof, err := InspectUserStorageMigrationFiles(t.Context(), source, filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if proof.Version != 1 || proof.References != 9 || proof.MissingReferences != 1 || proof.UnparsedSnapshots != 2 || proof.Files != 11 || proof.Bytes == 0 || len(proof.ReferenceDigest) != 64 || len(proof.TreeDigest) != 64 {
		t.Fatal("incomplete file proof", proof)
	}
	repeated, err := InspectUserStorageMigrationFiles(t.Context(), source, filepath.Dir(path))
	if err != nil || repeated != proof {
		t.Fatal("file proof unstable", repeated, proof, err)
	}
	if err := os.WriteFile(remote, []byte("remote asset changed"), 0644); err != nil {
		t.Fatal(err)
	}
	changed, err := InspectUserStorageMigrationFiles(t.Context(), source, filepath.Dir(path))
	if err != nil || changed.TreeDigest == proof.TreeDigest || changed.ReferenceDigest != proof.ReferenceDigest {
		t.Fatal("unindexed blob change not distinguished", changed, proof, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("file inspection modified source", err)
	}
	metadata, err := json.Marshal(proof)
	if err != nil || strings.Contains(string(metadata), base) || strings.Contains(string(metadata), "private message") {
		t.Fatal("file proof leaked content or paths", err)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inspection repaired preexisting missing file", err)
	}
	if _, err := os.Lstat(path + ".users"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inspection created user stores", err)
	}
}

func TestUserStorageMigrationFilesRejectsUnsafeOwnershipAndPaths(t *testing.T) {
	cases := map[string]func(*testing.T, *DB, string){
		"foreign-mailbox": func(t *testing.T, db *DB, base string) {
			migrationFilesSQL(t, db, `UPDATE messages SET raw_path=? WHERE account_id='alice-mail'`, filepath.Join(base, "bob-mail", "messages", "42", "raw.eml"))
		},
		"foreign-compose": func(t *testing.T, db *DB, base string) {
			key := sha256.Sum256([]byte("bob"))
			migrationFilesSend(t, db, "send", "pending", "pending", migrationFilesSnapshot(t, filepath.Join(base, "_compose", hex.EncodeToString(key[:16]), "draft.txt")))
		},
		"sent-copy-still-retained": func(t *testing.T, db *DB, base string) {
			migrationFilesSend(t, db, "send", "sent", "ambiguous", migrationFilesSnapshot(t, filepath.Join(base, "bob-mail", "messages", "42", "file.txt")))
		},
		"outside-root": func(t *testing.T, db *DB, base string) {
			migrationFilesSQL(t, db, `UPDATE messages SET raw_path=? WHERE account_id='alice-mail'`, filepath.Join(base, "..", "secret.key"))
		},
		"unresolved-account": func(t *testing.T, db *DB, base string) {
			migrationFilesSQL(t, db, `UPDATE messages SET raw_path=? WHERE account_id='alice-mail'`, filepath.Join(base, "gone", "raw.eml"))
		},
		"avatar-in-mailbox": func(t *testing.T, db *DB, _ string) {
			migrationFilesSQL(t, db, `INSERT INTO sender_avatars(email_hash,storage_path) VALUES('avatar','alice-mail/messages/raw.eml')`)
		},
		"avatar-escape": func(t *testing.T, db *DB, _ string) {
			migrationFilesSQL(t, db, `INSERT INTO sender_avatars(email_hash,storage_path) VALUES('avatar','../secret.key')`)
		},
		"root-file": func(t *testing.T, db *DB, base string) {
			migrationFilesSQL(t, db, `UPDATE messages SET raw_path=? WHERE account_id='alice-mail'`, filepath.Join(base, "raw.eml"))
		},
		"namespace-is-file": func(t *testing.T, _ *DB, base string) {
			if err := os.MkdirAll(base, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(base, "alice-mail"), []byte("not directory"), 0644); err != nil {
				t.Fatal(err)
			}
		},
		"checksum-mismatch": func(t *testing.T, db *DB, base string) {
			path, err := store.NewBlobStore(base).StoreAttachment(t.Context(), "alice-mail", 1, 1, "report.txt", strings.NewReader("original bytes"))
			if err != nil {
				t.Fatal(err)
			}
			migrationFilesSQL(t, db, `INSERT INTO attachments(message_id,storage_path,sha256) SELECT id,?,? FROM messages WHERE account_id='alice-mail'`, path, strings.Repeat("0", 64))
		},
		"reference-is-directory": func(t *testing.T, db *DB, base string) {
			path := filepath.Join(base, "alice-mail", "messages")
			if err := os.MkdirAll(path, 0755); err != nil {
				t.Fatal(err)
			}
			migrationFilesSQL(t, db, `UPDATE messages SET raw_path=? WHERE account_id='alice-mail'`, path)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			db := sharedMigrationFixture(t)
			base := filepath.Join(filepath.Dir(db.Path()), "accounts")
			setup(t, db, base)
			if _, err := InspectUserStorageMigrationFiles(t.Context(), db, filepath.Dir(db.Path())); err == nil {
				t.Fatal("unsafe file layout accepted")
			}
		})
	}
	for _, id := range []string{"avatars", "_compose", "../escape", `bad\namespace`, " account "} {
		t.Run("unsafe-account-"+id, func(t *testing.T) {
			db := sharedMigrationFixture(t)
			migrationFilesSQL(t, db, `INSERT INTO accounts(id,user_id,email_address) VALUES(?,'alice','alias@example.com')`, id)
			if _, err := InspectUserStorageMigrationFiles(t.Context(), db, ""); err == nil {
				t.Fatal("unsafe account namespace accepted")
			}
		})
	}
}

func TestUserStorageMigrationFilesRejectsSymlinksIncludingUnindexedFiles(t *testing.T) {
	for _, namespace := range []string{"accounts", "accounts/alice-mail", "accounts/alice-mail/messages/body.txt", "accounts/orphan.txt"} {
		t.Run(namespace, func(t *testing.T) {
			db := sharedMigrationFixture(t)
			link := filepath.Join(filepath.Dir(db.Path()), filepath.FromSlash(namespace))
			if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), link); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			if _, err := InspectUserStorageMigrationFiles(t.Context(), db, ""); err == nil {
				t.Fatal("symlink layout accepted")
			}
		})
	}
}

func TestUserStorageMigrationFilesMissingTreeAndCancellation(t *testing.T) {
	db := sharedMigrationFixture(t)
	proof, err := InspectUserStorageMigrationFiles(t.Context(), db, "")
	if err != nil || proof.Files != 0 || proof.References != 0 || len(proof.TreeDigest) != 64 {
		t.Fatal("empty lazy blob tree", proof, err)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(db.Path()), "accounts")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inspection created empty blob root", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := InspectUserStorageMigrationFiles(ctx, db, ""); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
	if _, err := InspectUserStorageMigrationFiles(t.Context(), nil, ""); err == nil {
		t.Fatal("nil source accepted")
	}
	if _, err := InspectUserStorageMigrationFiles(nil, db, ""); err == nil {
		t.Fatal("nil context accepted")
	}
}
