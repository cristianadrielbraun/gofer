package handler

import (
	"net/http/httptest"
	"testing"
)

func TestParseEmailFiltersNormalizesAdvancedValues(t *testing.T) {
	req := httptest.NewRequest("GET", "/?has_tags=1&no_tags=1&threads_only=1&no_threads=1&participant=Person%40Example.com&recipient_type=CC&recipient_domain=Example.com&attachment_type=custom&attachment_extension=.EML&min_size_mb=10&max_size_mb=2.5", nil)
	filters := parseEmailFilters(req)

	if !filters.HasTags || filters.NoTags {
		t.Fatalf("tag presence filters = has:%t no:%t", filters.HasTags, filters.NoTags)
	}
	if !filters.ThreadsOnly || filters.NoThreads {
		t.Fatalf("thread presence filters = has:%t no:%t", filters.ThreadsOnly, filters.NoThreads)
	}
	if filters.RecipientType != "cc" || filters.RecipientDomain != "Example.com" {
		t.Fatalf("recipient filters = %q, %q", filters.RecipientType, filters.RecipientDomain)
	}
	if filters.Participant != "Person@Example.com" {
		t.Fatalf("participant filter = %q", filters.Participant)
	}
	if filters.AttachmentType != "custom" || filters.AttachmentExt != "eml" {
		t.Fatalf("attachment filters = %q, %q", filters.AttachmentType, filters.AttachmentExt)
	}
	if filters.MinSizeBytes != 2621440 || filters.MaxSizeBytes != 10485760 {
		t.Fatalf("size range = %d..%d", filters.MinSizeBytes, filters.MaxSizeBytes)
	}
}

func TestParseEmailFiltersRejectsUnsupportedAdvancedValues(t *testing.T) {
	req := httptest.NewRequest("GET", "/?recipient_type=reply-to&attachment_type=executable&attachment_extension=tar.gz&min_size_mb=-1", nil)
	filters := parseEmailFilters(req)

	if filters.RecipientType != "" || filters.AttachmentType != "" || filters.AttachmentExt != "" || filters.MinSizeBytes != 0 {
		t.Fatalf("unsupported values were not cleared: %#v", filters)
	}
}

func TestParseEmailFiltersRequiresAnExtensionForCustomAttachmentType(t *testing.T) {
	req := httptest.NewRequest("GET", "/?attachment_type=custom", nil)
	filters := parseEmailFilters(req)
	if filters.AttachmentType != "" {
		t.Fatalf("custom attachment type without an extension = %q", filters.AttachmentType)
	}
}
