package handler

import (
	"net/http/httptest"
	"testing"
)

func TestSavedSortDefaultsApplyOnlyWhenURLDoesNotOverrideThem(t *testing.T) {
	settings := map[string]string{
		"mail_list_sort_by":        "sender",
		"mail_list_sort_order":     "asc",
		"contacts_list_sort_by":    "name",
		"contacts_list_sort_order": "desc",
	}

	mailDefaultRequest := httptest.NewRequest("GET", "/folder/inbox", nil)
	mailDefaults := applyEmailSortDefaults(parseEmailFilters(mailDefaultRequest), mailDefaultRequest, settings)
	if mailDefaults.SortBy != "sender" || mailDefaults.SortOrder != "asc" {
		t.Fatalf("mail saved sort = %q/%q, want sender/asc", mailDefaults.SortBy, mailDefaults.SortOrder)
	}

	mailOverrideRequest := httptest.NewRequest("GET", "/folder/inbox?sort_by=date&sort_order=desc", nil)
	mailOverride := applyEmailSortDefaults(parseEmailFilters(mailOverrideRequest), mailOverrideRequest, settings)
	if mailOverride.SortBy != "date" || mailOverride.SortOrder != "desc" {
		t.Fatalf("mail URL sort = %q/%q, want date/desc", mailOverride.SortBy, mailOverride.SortOrder)
	}

	h := &Handler{}
	contactDefaultRequest := httptest.NewRequest("GET", "/contacts", nil)
	contactDefaults := applyContactSortDefaults(h.parseContactFilters(contactDefaultRequest), contactDefaultRequest, settings)
	if contactDefaults.SortBy != "name" || contactDefaults.SortOrder != "desc" {
		t.Fatalf("contact saved sort = %q/%q, want name/desc", contactDefaults.SortBy, contactDefaults.SortOrder)
	}

	contactOverrideRequest := httptest.NewRequest("GET", "/contacts?sort_by=last_interaction&sort_order=asc", nil)
	contactOverride := applyContactSortDefaults(h.parseContactFilters(contactOverrideRequest), contactOverrideRequest, settings)
	if contactOverride.SortBy != "last_interaction" || contactOverride.SortOrder != "asc" {
		t.Fatalf("contact URL sort = %q/%q, want last_interaction/asc", contactOverride.SortBy, contactOverride.SortOrder)
	}
}
