package storage

import (
	"context"
	"testing"
)

func searchMatches(t *testing.T, db *DB, query string) int {
	t.Helper()
	var count int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM message_search WHERE message_search MATCH ?`, query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// Inline images are part of a message's body, so their file names aren't
// searchable as attachment names; entries indexed before that are corrected.
func TestSearchIndexLeavesOutInlineImageNames(t *testing.T) {
	db, _ := seedUserLabelTest(t, "imap")
	ctx := context.Background()
	if _, err := db.Write().Exec(`INSERT INTO attachments(message_id,filename,content_type,size_bytes,inline,storage_path) VALUES
		(1,'signaturelogo.png','image/png',10,1,'a'),
		(1,'quarterlyreport.pdf','application/pdf',10,0,'b')`); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.reindexMessagesSearchTx(ctx, tx, []int64{1}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if searchMatches(t, db, "signaturelogo") != 0 || searchMatches(t, db, "quarterlyreport") != 1 {
		t.Fatal("indexing a message must list only its real attachments")
	}

	// An entry indexed before inline images were left out.
	if _, err := db.Write().Exec(`UPDATE message_search SET attachment_names = 'signaturelogo.png quarterlyreport.pdf' WHERE rowid = 1`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := db.EnsureSearchAttachmentNames(ctx); err != nil {
			t.Fatal(err)
		}
		var names string
		if err := db.Read().QueryRow(`SELECT attachment_names FROM message_search WHERE rowid = 1`).Scan(&names); err != nil {
			t.Fatal(err)
		}
		if names != "quarterlyreport.pdf" || searchMatches(t, db, "signaturelogo") != 0 {
			t.Fatalf("attachment names = %q", names)
		}
	}
}
