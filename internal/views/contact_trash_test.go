package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func renderForTest(t *testing.T, render func(*bytes.Buffer) error) string {
	t.Helper()
	var out bytes.Buffer
	if err := render(&out); err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	return out.String()
}

func TestContactsTrashListOffersRestoreAndDeleteForever(t *testing.T) {
	contacts := []models.Contact{{ID: "c1", Name: "Pia Strand", TrashedAt: "Oct 1, 2026", TrashPurgeAt: "Oct 31, 2026"}}
	html := renderForTest(t, func(out *bytes.Buffer) error {
		return ContactsListPane(contacts, nil, models.ContactFilters{Trash: true}, 1, "", nil).Render(context.Background(), out)
	})
	for _, want := range []string{`data-contacts-title>`, "Trash", "data-contacts-empty-trash", `data-contact-selection-action="restore"`, `data-contact-selection-action="delete-forever"`, `data-contacts-context-action="delete-forever"`, `data-trash="1"`, "trash=1", "Oct 1, 2026"} {
		if !strings.Contains(html, want) {
			t.Fatalf("trash list missing %q", want)
		}
	}
	for _, unwanted := range []string{`data-contact-selection-action="compose"`, `data-contacts-context-action="edit"`} {
		if strings.Contains(html, unwanted) {
			t.Fatalf("trash list offers %q", unwanted)
		}
	}

	html = renderForTest(t, func(out *bytes.Buffer) error {
		return ContactsListPane(nil, nil, models.ContactFilters{}, 0, "", nil).Render(context.Background(), out)
	})
	if strings.Contains(html, "data-contacts-empty-trash") || strings.Contains(html, `data-trash="1"`) || !strings.Contains(html, `data-contact-selection-action="delete"`) {
		t.Fatal("the contacts list shows the Trash's controls")
	}
}

func TestATrashedContactsPageIsReadOnly(t *testing.T) {
	contact := models.Contact{ID: "c1", Name: "Pia Strand", Email: "pia@example.com", TrashedAt: "Oct 1, 2026", TrashPurgeAt: "Oct 31, 2026"}
	html := renderForTest(t, func(out *bytes.Buffer) error {
		return ContactsDetail(&contact, nil, false, false, nil).Render(context.Background(), out)
	})
	for _, want := range []string{"deleted for good on Oct 31, 2026", `data-contact-restore="c1"`, `data-contact-delete-forever="c1"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("trashed contact page missing %q", want)
		}
	}
	for _, unwanted := range []string{"data-contact-edit-trigger", "data-contact-edit-form", "data-contact-delete-form"} {
		if strings.Contains(html, unwanted) {
			t.Fatalf("trashed contact page offers %q", unwanted)
		}
	}
}

func TestContactsSidebarTrashItem(t *testing.T) {
	if got := contactSidebarURL(models.ContactFilters{Query: "pia", SaveTarget: "local"}, "trash"); got != "/contacts?q=pia&trash=1" {
		t.Fatalf("trash URL = %q", got)
	}
	if got := contactSidebarActiveTarget(models.ContactFilters{Trash: true, SaveTarget: "local"}); got != "trash" {
		t.Fatalf("active target in the Trash = %q", got)
	}
	html := renderForTest(t, func(out *bytes.Buffer) error {
		return ContactsSidebarBody(nil, "contacts", nil, models.ContactFilters{Trash: true}).Render(context.Background(), out)
	})
	if !strings.Contains(html, `data-contact-sidebar-target="trash"`) || !strings.Contains(html, `hx-get="/contacts/trash-count"`) {
		t.Fatal("sidebar has no Trash with its count")
	}
	// The count sits in the Trash link, whose target htmx would otherwise
	// pass down: it would replace the whole page with the count.
	if !strings.Contains(html, `hx-get="/contacts/trash-count" hx-trigger="load, contacts-trash-changed from:body" hx-target="this"`) {
		t.Fatal("the Trash count loads into the link's target")
	}
	if html := renderForTest(t, func(out *bytes.Buffer) error { return ContactsTrashCount(0).Render(context.Background(), out) }); strings.TrimSpace(html) != "" {
		t.Fatalf("empty Trash count = %q, want nothing", html)
	}
}
