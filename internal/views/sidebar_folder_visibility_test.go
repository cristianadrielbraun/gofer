package views

import (
	"slices"
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

func TestSidebarFolderVisibilityOnlyIMAP(t *testing.T) {
	for _, provider := range []string{"gmail", "local", "jmap"} {
		t.Run(provider, func(t *testing.T) {
			account := models.Account{
				Provider: provider,
				Folders: []models.Folder{
					{ID: "unaffected", Role: "custom", Hidden: true},
				},
			}

			result := sidebarFoldersForAccount(account)
			if len(result) != 1 || result[0].ID != "unaffected" {
				t.Fatalf("non-IMAP folders were filtered: %+v", result)
			}
		})
	}
}
