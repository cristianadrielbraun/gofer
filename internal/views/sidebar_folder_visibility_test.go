package views

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func sidebarVisibilityIDs(folders []models.Folder) []string {
	var ids []string
	for _, folder := range folders {
		ids = append(ids, folder.ID)
		ids = append(ids, sidebarVisibilityIDs(folder.Children)...)
	}
	return ids
}

func TestSidebarFolderVisibilityPreservesSystemDescendants(t *testing.T) {
	account := models.Account{
		Provider: "imap",
		Folders: []models.Folder{
			{
				ID: "projects", Role: "custom",
				Children: []models.Folder{
					{
						ID: "hidden", Role: "custom", Hidden: true,
						Children: []models.Folder{
							{ID: "ordinary", Role: "custom"},
							{
								ID: "inbox", Role: "inbox",
								Children: []models.Folder{
									{ID: "ordinary-under-inbox", Role: "custom"},
									{ID: "trash", Role: "trash"},
								},
							},
							{ID: "archive", Role: "archive"},
						},
					},
					{ID: "visible", Role: "custom"},
				},
			},
		},
	}

	originalCount := len(account.Folders[0].Children[0].Children)
	result := sidebarFoldersForAccount(account)

	want := []string{
		"projects", "inbox", "trash", "archive", "visible",
	}
	if got := sidebarVisibilityIDs(result); !slices.Equal(got, want) {
		t.Fatalf("visible folders = %v, want %v", got, want)
	}

	// Hidden custom ancestors are removed from the presentation tree.
	if len(result) != 1 || len(result[0].Children) != 3 {
		t.Fatalf("unexpected visible hierarchy: %+v", result)
	}

	// Filtering must not mutate the source tree.
	if got := len(account.Folders[0].Children[0].Children); got != originalCount {
		t.Fatalf("source tree changed: %d children, want %d", got, originalCount)
	}
}

func TestSidebarFolderVisibilityHiddenRootAndSystemProtection(t *testing.T) {
	account := models.Account{
		Provider: "imap",
		Folders: []models.Folder{
			{
				ID: "hidden-root", Role: "custom", Hidden: true,
				Children: []models.Folder{
					{ID: "custom-child", Role: "custom"},
					{ID: "sent", Role: "sent"},
					{ID: "outbox", Role: "outbox"},
				},
			},
			{ID: "inbox", Role: "inbox", Hidden: true},
		},
	}

	result := sidebarFoldersForAccount(account)
	want := []string{"sent", "outbox", "inbox"}

	if got := sidebarVisibilityIDs(result); !slices.Equal(got, want) {
		t.Fatalf("visible folders = %v, want %v", got, want)
	}
}

func TestSidebarFolderVisibilityOnEveryAccountType(t *testing.T) {
	for _, provider := range []string{"imap", "outlook", "jmap", "local"} {
		t.Run(provider, func(t *testing.T) {
			account := models.Account{
				Provider: provider,
				Folders: []models.Folder{
					{ID: "inbox", Role: "inbox"},
					{ID: "receipts", Role: "custom", Hidden: true},
				},
			}
			if got := sidebarVisibilityIDs(sidebarFoldersForAccount(account)); !slices.Equal(got, []string{"inbox"}) {
				t.Fatalf("sidebar folders = %v, want [inbox]", got)
			}
		})
	}
}

// Any account's tags can be hidden, and the tags of all accounts apart from
// them: hiding Work's Travel leaves the unified Travel, and the other way
// round.
func TestSidebarHidesHiddenTags(t *testing.T) {
	gmail := models.Account{ID: "mail", Provider: "gmail", EmailSyncEnabled: true, Labels: []models.Label{
		{ID: "receipts", Name: "Receipts", Hidden: true},
		{ID: "family", Name: "Family"},
	}}
	work := models.Account{ID: "work", Provider: "imap", EmailSyncEnabled: true, Labels: []models.Label{
		{ID: "travel", Name: "Travel", Hidden: true},
	}}
	settings := map[string]string{models.TagVisibleKey(models.UnifiedTagID("Family"), true): "false"}

	names := func(labels []models.Label) []string {
		var got []string
		for _, label := range labels {
			got = append(got, label.Name)
		}
		return got
	}
	if got := names(sidebarLabels(gmail.Labels)); !slices.Equal(got, []string{"Family"}) {
		t.Fatalf("Gmail's tags = %v, want [Family]", got)
	}
	if got := names(sidebarLabels(work.Labels)); len(got) != 0 {
		t.Fatalf("Work's tags = %v, want none", got)
	}
	all := unifiedSidebarLabels([]models.Account{gmail, work}, settings)
	if got := names(all); !slices.Equal(got, []string{"Family", "Receipts", "Travel"}) {
		t.Fatalf("unified tags = %v, want all three for Settings", got)
	}
	if got := names(sidebarLabels(all)); !slices.Equal(got, []string{"Receipts", "Travel"}) {
		t.Fatalf("unified tags in the sidebar = %v, want [Receipts Travel]", got)
	}
}

// Settings → Sync lists the unified tags hidden from the sidebar too, with
// their switch off, as that's where they are shown again. While unified
// folders are off, the switches are too.
func TestUnifiedTagSettingsShowsHiddenTags(t *testing.T) {
	accounts := []models.Account{{ID: "work", Name: "Work", Provider: "imap", EmailSyncEnabled: true, Labels: []models.Label{
		{ID: "travel", AccountID: "work", Name: "Travel", Hidden: true},
		{ID: "clients", AccountID: "work", Name: "Clients"},
	}}}
	switches := func(settings map[string]string) map[string]string {
		t.Helper()
		var out strings.Builder
		if err := UnifiedTagSettings(unifiedSidebarLabels(accounts, settings), settings).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, m := range regexp.MustCompile(`<input[^>]*data-tag-visibility="([^"]*)"[^>]*>`).FindAllStringSubmatch(out.String(), -1) {
			state := "off"
			if strings.Contains(m[0], " checked") {
				state = "on"
			}
			if strings.Contains(m[0], " disabled") {
				state += ", disabled"
			}
			got[m[1]] = state
		}
		return got
	}
	travel, clients := models.UnifiedTagID("Travel"), models.UnifiedTagID("Clients")
	settings := map[string]string{models.TagVisibleKey(clients, true): "false"}
	// Work's own Travel is hidden, which leaves the unified one.
	want := map[string]string{travel: "on", clients: "off"}
	if got := switches(settings); !maps.Equal(got, want) {
		t.Fatalf("switches = %v, want %v", got, want)
	}
	settings["unified_folders_enabled"] = "false"
	want = map[string]string{travel: "on, disabled", clients: "off, disabled"}
	if got := switches(settings); !maps.Equal(got, want) {
		t.Fatalf("with unified folders off, switches = %v, want %v", got, want)
	}
}

func TestFolderRowActionsHideAndShow(t *testing.T) {
	account := models.Account{ID: "acc", Provider: "jmap"}
	for _, tc := range []struct {
		name       string
		folder     models.Folder
		inHidden   bool
		hide, show bool
	}{
		{"a shown folder", models.Folder{Role: "custom"}, false, true, false},
		{"a hidden folder", models.Folder{Role: "custom", Hidden: true}, false, false, true},
		{"a folder inside a hidden one", models.Folder{Role: "custom"}, true, false, false},
		{"a hidden folder inside a hidden one", models.Folder{Role: "custom", Hidden: true}, true, false, true},
		{"Inbox", models.Folder{Role: "inbox"}, false, false, false},
		{"Inbox with an old hidden setting", models.Folder{Role: "inbox", Hidden: true}, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := folderRowActionsFor(account, tc.folder, false, tc.inHidden)
			if got.Hide != tc.hide || got.Show != tc.show {
				t.Fatalf("Hide=%v Show=%v, want Hide=%v Show=%v", got.Hide, got.Show, tc.hide, tc.show)
			}
		})
	}
}
