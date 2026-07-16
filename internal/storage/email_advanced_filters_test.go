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
