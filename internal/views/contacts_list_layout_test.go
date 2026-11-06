package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// A contact card has each field the card layout can place, and the table
// row a cell for each column the column menu can show.
func TestContactListRowsHaveEveryLayoutField(t *testing.T) {
	contact := models.Contact{ID: "c1", Name: "Nora Vale", Email: "nora@work.example", Phone: "+1 555 0100", Organization: "Vale Studio", Title: "Editor", Notes: "Prefers\nmornings", StoredIn: "local", MessageCount: 3, LastSeenAt: "Mar 3", CreatedAt: "Jan 5", UpdatedAt: "Feb 9", GoferSyncEnabled: true, SyncStatus: "error", SyncError: "book is read-only"}
	card := renderForTest(t, func(out *bytes.Buffer) error {
		return ContactListCard(contact, models.ContactFilters{}, nil, nil, 0).Render(context.Background(), out)
	})
	if !strings.Contains(card, "data-contact-card-layout-scope") || !strings.Contains(card, `data-mail-card-zone="hidden"`) {
		t.Error("the card can't be laid out")
	}
	for _, field := range []string{"avatar", "name", "organization", "title", "email", "phone", "home", "date", "created", "edited", "notes", "messages", "sync"} {
		if !strings.Contains(card, `data-mail-card-field="`+field+`"`) {
			t.Errorf("card has no %s field", field)
		}
	}
	row := renderForTest(t, func(out *bytes.Buffer) error {
		return ContactListTableRow(contact, models.ContactFilters{}, nil, nil, 0).Render(context.Background(), out)
	})
	header := renderForTest(t, func(out *bytes.Buffer) error {
		return ContactListTableHeader().Render(context.Background(), out)
	})
	for _, column := range contactTableColumns {
		if !strings.Contains(row, `data-mail-table-cell="`+column+`"`) {
			t.Errorf("row has no %s cell", column)
		}
		if !strings.Contains(header, `data-mail-table-column-id="`+column+`"`) {
			t.Errorf("header has no %s heading", column)
		}
	}
	// Cards say which date each is; the table leaves that to its headings.
	for _, want := range []string{">Last mail on Mar 3<", ">Created on Jan 5<", ">Edited on Feb 9<"} {
		if !strings.Contains(card, want) {
			t.Errorf("card has no %q", want)
		}
	}
	for _, want := range []string{`data-mail-table-cell="date">Mar 3<`, `data-mail-table-cell="created">Jan 5<`, `data-mail-table-cell="edited">Feb 9<`} {
		if !strings.Contains(row, want) {
			t.Errorf("row has no %q", want)
		}
	}
	// Notes on one line; a failed sync says why.
	if !strings.Contains(card, ">Prefers mornings<") || !strings.Contains(row, "Gofer Sync: Sync failed: book is read-only") {
		t.Errorf("notes or sync state not shown as expected:\n%s", row)
	}
	off := renderForTest(t, func(out *bytes.Buffer) error {
		return ContactListCard(models.Contact{ID: "c3", Name: "Una Ross", StoredIn: "local"}, models.ContactFilters{}, nil, nil, 0).Render(context.Background(), out)
	})
	if !strings.Contains(off, `data-mail-card-field="sync"`) || !strings.Contains(off, "Gofer Sync: Off") {
		t.Error("a contact without Gofer Sync doesn't say it is off")
	}
	if strings.Contains(row, "--mail-list-table-columns") {
		t.Error("a row sets its own column widths instead of the list's")
	}
}

// A contact without an address shows its phone in the address's place.
func TestContactWithoutEmailFallsBackToPhone(t *testing.T) {
	contact := models.Contact{ID: "c2", Name: "Ivo Lind", Phone: "+1 555 0101", StoredIn: "local"}
	card := renderForTest(t, func(out *bytes.Buffer) error {
		return ContactListCard(contact, models.ContactFilters{}, nil, nil, 0).Render(context.Background(), out)
	})
	if !strings.Contains(card, `<span data-contact-phone-fallback>+1 555 0101</span>`) {
		t.Errorf("no phone fallback in %s", card)
	}
}

// The contacts list opens its own card layout editor and column menu.
func TestContactsListHasLayoutControls(t *testing.T) {
	html := renderForTest(t, func(out *bytes.Buffer) error {
		return ContactsListPane(nil, nil, models.ContactFilters{}, 0, "", nil).Render(context.Background(), out)
	})
	for _, want := range []string{
		`id="contact-card-layout-dialog"`,
		"data-contact-card-layout-dialog-open",
		`data-table-columns-list="contacts"`,
		`data-mail-table-column-item="organization"`,
		"data-contact-table-header-template",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("contacts list missing %q", want)
		}
	}
	mail := renderForTest(t, func(out *bytes.Buffer) error {
		return MailListColumnSelector().Render(context.Background(), out)
	})
	if !strings.Contains(mail, "data-mail-card-layout-dialog-open") || !strings.Contains(mail, `data-table-columns-list="mail"`) || !strings.Contains(mail, `data-mail-table-column-item="subject"`) {
		t.Error("the mail list lost its layout controls")
	}
}
