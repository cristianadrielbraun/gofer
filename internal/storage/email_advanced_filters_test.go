package storage

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestEmailFilterSQLSupportsRecipientTagsAndSizeRange(t *testing.T) {
	parts := emailFilterSQL(models.EmailFilters{
		NoTags:          true,
		To:              "person",
		RecipientType:   "cc",
		RecipientDomain: "example.com",
		MinSizeBytes:    1024,
		MaxSizeBytes:    4096,
	})

	for _, want := range []string{"NOT EXISTS", "mr.kind = ?", "lower(mr.email) LIKE ?", "m.size_bytes >= ?", "m.size_bytes <= ?"} {
		if !strings.Contains(parts.cteClause, want) {
			t.Errorf("filter SQL missing %q: %s", want, parts.cteClause)
		}
	}
	wantArgs := []any{"cc", "%person%", "%person%", "cc", "%@example.com", int64(1024), int64(4096)}
	if !reflect.DeepEqual(parts.args, wantArgs) {
		t.Fatalf("filter args = %#v, want %#v", parts.args, wantArgs)
	}
}

func TestAttachmentTypeFilterSQLSupportsPresetsAndCustomExtensions(t *testing.T) {
	imageClause, imageArgs := attachmentTypeFilterSQL("image", "")
	if !strings.Contains(imageClause, "content_type") || !reflect.DeepEqual(imageArgs, []any{"image/%"}) {
		t.Fatalf("image filter = %q, %#v", imageClause, imageArgs)
	}

	customClause, customArgs := attachmentTypeFilterSQL("custom", ".eml")
	if !strings.Contains(customClause, "filename") || !reflect.DeepEqual(customArgs, []any{"%.eml"}) {
		t.Fatalf("custom filter = %q, %#v", customClause, customArgs)
	}
}

func TestAttachmentTypeMatchLeavesOutInlineAndReadsFileNames(t *testing.T) {
	db, _ := seedUserLabelTest(t, "imap")
	if _, err := db.Write().Exec(`
		INSERT INTO messages(id,account_id,internet_message_id,subject) VALUES
			(2,'acc','<photo@test.invalid>','Photo as a generic file'),
			(3,'acc','<chart@test.invalid>','Chart'),
			(4,'acc','<song@test.invalid>','Song');
		INSERT INTO attachments(message_id,filename,content_type,size_bytes,inline,storage_path) VALUES
			(1,'logo.png','image/png',10,1,'a'),
			(2,'photo.JPG','application/octet-stream',10,0,'b'),
			(3,'chart','image/png',10,0,'c'),
			(4,'song.mp3','application/octet-stream',10,0,'d')`); err != nil {
		t.Fatal(err)
	}
	matching := func(filterType string) []int64 {
		t.Helper()
		match, args := attachmentTypeMatchSQL(filterType, "")
		rows, err := db.Read().Query("SELECT m.id FROM messages m WHERE "+match+" ORDER BY m.id", args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return ids
	}
	// Message 1 has only an inline logo; 2 is a photo sent as a generic file.
	if got := matching("image"); !reflect.DeepEqual(got, []int64{2, 3}) {
		t.Fatalf("image matches %v, want [2 3]", got)
	}
	if got := matching("audio"); !reflect.DeepEqual(got, []int64{4}) {
		t.Fatalf("audio matches %v, want [4]", got)
	}
}

func TestAttachmentNameFilterLeavesOutInlineImages(t *testing.T) {
	db, _ := seedUserLabelTest(t, "imap")
	if _, err := db.Write().Exec(`
		INSERT INTO messages(id,account_id,internet_message_id,subject) VALUES (2,'acc','<attached@test.invalid>','Attached logo');
		INSERT INTO attachments(message_id,filename,content_type,size_bytes,inline,storage_path) VALUES
			(1,'logo.png','image/png',10,1,'a'),
			(2,'logo-final.png','image/png',10,0,'b')`); err != nil {
		t.Fatal(err)
	}
	parts := emailFilterSQL(models.EmailFilters{Attachment: "logo"})
	var ids []int64
	rows, err := db.Read().Query("SELECT m.id FROM messages m WHERE 1=1"+parts.cteClause+" ORDER BY m.id", parts.args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// Message 1's logo is inline, in its body.
	if !reflect.DeepEqual(ids, []int64{2}) {
		t.Fatalf("attachment:logo matches %v, want [2]", ids)
	}
}

func TestEmailFilterSQLSupportsSingleMessagesOnly(t *testing.T) {
	parts := emailFilterSQL(models.EmailFilters{NoThreads: true})
	if !strings.Contains(parts.outerClause, "thread_count = 1") {
		t.Fatalf("single-message filter SQL = %q", parts.outerClause)
	}
}

func TestEmailFilterSQLSupportsExactParticipant(t *testing.T) {
	parts := emailFilterSQL(models.EmailFilters{Participant: " Person@Example.com "})
	for _, want := range []string{"lower(trim(m.from_email)) = ?", "lower(trim(participant_recipient.email)) = ?"} {
		if !strings.Contains(parts.cteClause, want) {
			t.Fatalf("participant filter SQL missing %q: %s", want, parts.cteClause)
		}
	}
	wantArgs := []any{"person@example.com", "person@example.com"}
	if !reflect.DeepEqual(parts.args, wantArgs) {
		t.Fatalf("participant filter args = %#v, want %#v", parts.args, wantArgs)
	}
}
