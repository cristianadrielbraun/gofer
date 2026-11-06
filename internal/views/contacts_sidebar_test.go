package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// An address book or account in the sidebar lists the contacts whose main
// location it is, not the ones that came from it.
func TestContactsSidebarPlaceItemsFilterByMainLocation(t *testing.T) {
	if got := contactSidebarURL(models.ContactFilters{Query: "ana"}, "stored_in:book:b1"); got != "/contacts?q=ana&stored_in=book%3Ab1" {
		t.Fatalf("book URL = %q", got)
	}
	if got := contactSidebarURL(models.ContactFilters{}, "stored_in:local"); got != "/contacts?stored_in=local" {
		t.Fatalf("local URL = %q", got)
	}
	if got := contactSidebarURL(models.ContactFilters{View: "table"}, "stored_in:account:g1"); got != "/contacts?stored_in=account%3Ag1&view=table" {
		t.Fatalf("account URL = %q", got)
	}
	for _, tc := range []struct {
		filters models.ContactFilters
		want    string
	}{
		{models.ContactFilters{StoredIn: "book:b1"}, "stored_in:book:b1"},
		{models.ContactFilters{StoredIn: "account:g1"}, "stored_in:account:g1"},
		{models.ContactFilters{StoredIn: "local"}, "stored_in:local"},
		{models.ContactFilters{}, ""},
		// Nothing in the sidebar stands for an origin, a sync location or
		// several places.
		{models.ContactFilters{SaveTarget: "local"}, "-"},
		{models.ContactFilters{Source: "book:b1"}, "-"},
		{models.ContactFilters{StoredIn: "book:b1,book:b2"}, "-"},
		{models.ContactFilters{StoredIn: "book:b1", SaveTarget: "local"}, "-"},
		{models.ContactFilters{StoredIn: "book:b1", Source: "manual"}, "-"},
	} {
		if got := contactSidebarActiveTarget(tc.filters); got != tc.want {
			t.Errorf("active target for %+v = %q, want %q", tc.filters, got, tc.want)
		}
	}

	accounts := []models.Account{
		{ID: "d1", Name: "Dana DAV", ContactSyncEnabled: true, ContactSyncProvider: "carddav", ContactAddressBooks: []models.ContactAddressBook{{ID: "b1", AccountID: "d1", Name: "Friends"}}},
		{ID: "g1", Name: "Gil Google", ContactSyncEnabled: true, ContactSyncProvider: "gmail"},
	}
	filters := models.ContactFilters{StoredIn: "book:b1"}
	html := renderForTest(t, func(out *bytes.Buffer) error {
		return ContactsSidebarBody(accounts, "contacts", nil, filters).Render(context.Background(), out)
	})
	for _, want := range []string{
		`data-contact-sidebar-target="stored_in:book:b1"`,
		`data-contact-sidebar-target="stored_in:account:g1"`,
		`hx-get="/contacts?stored_in=book%3Ab1"`,
		`data-contact-sidebar-target="stored_in:local"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("sidebar missing %q", want)
		}
	}
	if strings.Contains(html, "source=") || strings.Contains(html, "save_target=") {
		t.Error("a sidebar item still filters by origin or sync location")
	}
	if !contactSidebarAccountActive(accounts[0], filters) || contactSidebarAccountActive(accounts[1], filters) {
		t.Error("only the account holding the chosen book is active")
	}
}

// The main location says whether the contact's copy there is still being
// written or was deleted there and waits to be resolved.
func TestContactStoredInLabelStates(t *testing.T) {
	accounts := []models.Account{{ID: "d1", Name: "Dana DAV", ContactAddressBooks: []models.ContactAddressBook{{ID: "b1", AccountID: "d1", Name: "Friends"}}}}
	card := models.ContactCard{Kind: "provider", AccountID: "d1", AddressBookID: "b1"}
	for _, tc := range []struct {
		name        string
		cards       []models.ContactCard
		deletedFrom []string
		want        string
	}{
		{"copy there", []models.ContactCard{card}, nil, "Dana DAV / Friends"},
		{"not written yet", nil, nil, "Dana DAV / Friends (writing…)"},
		{"deleted there", nil, []string{"book:b1"}, "Dana DAV / Friends (deleted here)"},
	} {
		profile := &models.ContactProfile{Home: "book:b1", Cards: tc.cards}
		if got := contactStoredInLabels(context.Background(), profile, tc.deletedFrom, accounts); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: labels = %q, want %q", tc.name, got, tc.want)
		}
	}
}
