package storage

import (
	"database/sql"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// Folders nest at any depth, and one whose parent isn't listed sits at the top.
func TestBuildFolderTreeNestsAtAnyDepth(t *testing.T) {
	row := func(id, parent string) folderRow {
		return folderRow{folder: models.Folder{ID: id, Name: id}, parentID: sql.NullString{String: parent, Valid: parent != ""}}
	}
	tree := buildFolderTree([]folderRow{
		row("root", ""), row("finance", "root"), row("bank", "finance"), row("mybank", "bank"), row("orphan", "gone"),
	})
	if len(tree) != 2 || tree[0].ID != "root" || tree[1].ID != "orphan" {
		t.Fatalf("roots = %+v", tree)
	}
	finance := tree[0].Children
	if len(finance) != 1 || finance[0].ID != "finance" {
		t.Fatalf("root's children = %+v", finance)
	}
	bank := finance[0].Children
	if len(bank) != 1 || bank[0].ID != "bank" {
		t.Fatalf("finance's children = %+v", bank)
	}
	if mybank := bank[0].Children; len(mybank) != 1 || mybank[0].ID != "mybank" || len(mybank[0].Children) != 0 {
		t.Fatalf("bank's children = %+v", mybank)
	}
}
