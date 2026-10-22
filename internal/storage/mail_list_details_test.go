package storage

import "testing"

func TestBatchCountAttachmentsLeavesOutInlineImages(t *testing.T) {
	db, _ := seedUserLabelTest(t, "imap")
	if _, err := db.Write().Exec(`INSERT INTO messages(id,account_id,internet_message_id,subject) VALUES(2,'acc','<inline@test.invalid>','Inline only')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().Exec(`INSERT INTO attachments(message_id,filename,content_type,size_bytes,inline,storage_path) VALUES
		(1,'agenda.pdf','application/pdf',10,0,'a'),
		(1,'budget.xlsx','application/vnd.ms-excel',10,0,'b'),
		(1,'logo.png','image/png',10,1,'c'),
		(2,'banner.png','image/png',10,1,'d')`); err != nil {
		t.Fatal(err)
	}

	counts, err := db.batchCountAttachments(t.Context(), []int64{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if counts[1] != 2 {
		t.Fatalf("message 1 count = %d, want 2 (inline image left out)", counts[1])
	}
	if counts[2] != 0 {
		t.Fatalf("message 2 count = %d, want 0 (only an inline image)", counts[2])
	}
}
