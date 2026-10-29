package views

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestMailSearchControlsShowScopeAndNestedChoices(t *testing.T) {
	accounts := []models.Account{{ID: "work", Email: "team@example.com", Folders: []models.Folder{
		{ID: "projects", Name: "Projects", Children: []models.Folder{{ID: "invoices", Name: "Invoices"}}},
	}}}
	var buf bytes.Buffer
	filters := models.EmailFilters{SearchScope: &models.MailSearchScope{Kind: models.MailSearchFolders, FolderIDs: []string{"projects"}, IncludeDescendants: true}}
	if err := MailListHeader(accounts, "search", 2, filters).Render(t.Context(), &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{`aria-label="Search in: 1 folder"`, `Edit folders (1 folder + subfolders)`, `value="invoices"`, `data-search-text="team@example.com projects / invoices"`, `aria-label="Choose folders to search"`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	scope := strings.Index(html, "data-mail-search-controls")
	search := strings.Index(html, "data-mail-search-input")
	pills := strings.Index(html, "data-mail-active-filter-pills")
	if !(scope < search && search < pills) {
		t.Fatal("the scope must start the search bar, before the input and the active filters")
	}
	email := models.Email{AccountID: "work", FolderID: "invoices"}
	if got := MailSearchLocation(accounts, email); got != "team@example.com · Projects / Invoices" {
		t.Fatal(got)
	}
}
