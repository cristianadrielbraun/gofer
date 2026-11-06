package storage

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func folderTreeIDs(folders []models.Folder) []string {
	var ids []string
	for _, f := range folders {
		ids = append(ids, f.ID)
	}
	return ids
}

// Folders put in order show in it, special ones too, each among the folders
// next to it; the rest keep the server's order after them.
func TestSetFolderOrderOrdersFoldersNextToEachOther(t *testing.T) {
	db := newFolderThreadTestDB(t)
	ctx := context.Background()
	if _, err := db.Write().Exec(`UPDATE folders SET sort_order = CASE id WHEN 'box-inbox' THEN 0 WHEN 'box-archive' THEN 4 ELSE 6 END;
		INSERT INTO folders(id, account_id, remote_id, name, role, sort_order) VALUES
			('box-garden', 'acct', 'Garden', 'Garden', 'custom', 100),
			('box-books', 'acct', 'Books', 'Books', 'custom', 101);
		INSERT INTO folders(id, account_id, parent_id, remote_id, name, role, sort_order) VALUES
			('box-seeds', 'acct', 'box-garden', 'Garden/Seeds', 'Seeds', 'custom', 102),
			('box-tools', 'acct', 'box-garden', 'Garden/Tools', 'Tools', 'custom', 103)`); err != nil {
		t.Fatal(err)
	}
	tree, err := db.AccountFolderTree(ctx, "acct")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := folderTreeIDs(tree), []string{"box-inbox", "box-archive", "box-trash", "box-garden", "box-books"}; !slices.Equal(got, want) {
		t.Fatalf("folders before ordering = %v, want the server's order %v", got, want)
	}

	if err := db.SetFolderOrder(ctx, "reader", "acct", []string{"box-books", "box-inbox", "box-garden"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFolderOrder(ctx, "reader", "acct", []string{"box-tools", "box-seeds"}); err != nil {
		t.Fatal(err)
	}
	tree, err = db.AccountFolderTree(ctx, "acct")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := folderTreeIDs(tree), []string{"box-books", "box-inbox", "box-garden", "box-archive", "box-trash"}; !slices.Equal(got, want) {
		t.Fatalf("folders = %v, want %v", got, want)
	}
	garden := tree[2]
	if got, want := folderTreeIDs(garden.Children), []string{"box-tools", "box-seeds"}; !slices.Equal(got, want) {
		t.Fatalf("Garden's folders = %v, want %v", got, want)
	}

	// A sync that lists the folders in its own order again doesn't undo it.
	if _, err := db.Write().Exec(`UPDATE folders SET sort_order = 500 - sort_order`); err != nil {
		t.Fatal(err)
	}
	tree, err = db.AccountFolderTree(ctx, "acct")
	if err != nil {
		t.Fatal(err)
	}
	if got := folderTreeIDs(tree); !slices.Equal(got[:3], []string{"box-books", "box-inbox", "box-garden"}) {
		t.Fatalf("folders after a sync = %v, want the user's order first", got)
	}
}

// Only the owner's folders of that account can be put in order.
func TestSetFolderOrderRefusesOtherFolders(t *testing.T) {
	db := newFolderThreadTestDB(t)
	ctx := context.Background()
	if _, err := db.Write().Exec(`INSERT INTO users(id, username, username_normalized) VALUES ('other', 'other', 'other');
		INSERT INTO accounts(id, user_id, email_address) VALUES ('other-acct', 'other', 'other@example.test');
		INSERT INTO folders(id, account_id, remote_id, name, role) VALUES ('other-inbox', 'other-acct', 'INBOX', 'Inbox', 'inbox')`); err != nil {
		t.Fatal(err)
	}
	for name, ids := range map[string][]string{
		"another owner's":   {"box-inbox", "other-inbox"},
		"missing":           {"box-inbox", "box-gone"},
		"twice":             {"box-inbox", "box-inbox"},
		"nothing":           nil,
		"another account's": {"other-inbox"},
	} {
		if err := db.SetFolderOrder(ctx, "reader", "acct", ids); !errors.Is(err, ErrFolderOrder) {
			t.Fatalf("%s folders: err = %v, want ErrFolderOrder", name, err)
		}
	}
	var saved int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM app_settings WHERE key LIKE 'folder_order:%'`).Scan(&saved); err != nil || saved != 0 {
		t.Fatalf("saved orders = %d, %v; want none", saved, err)
	}
}

// Tags put in order show in it, in the sidebar and in the Labels dialog; the
// rest stay by name after them.
func TestSetTagOrderOrdersAnAccountsTags(t *testing.T) {
	db := newFolderThreadTestDB(t)
	ctx := context.Background()
	if _, err := db.Write().Exec(`INSERT INTO labels(id, account_id, name, provider_id, provider_type) VALUES
		('tag-art', 'acct', 'Art', 'l1', 'gmail'), ('tag-bills', 'acct', 'Bills', 'l2', 'gmail'),
		('tag-cats', 'acct', 'Cats', 'l3', 'gmail'), ('tag-dance', 'acct', 'Dance', 'l4', 'gmail')`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetTagOrder(ctx, "reader", "acct", []string{"tag-cats", "tag-art"}); err != nil {
		t.Fatal(err)
	}
	labelIDs := func(labels []models.Label) []string {
		var ids []string
		for _, l := range labels {
			ids = append(ids, l.ID)
		}
		return ids
	}
	want := []string{"tag-cats", "tag-art", "tag-bills", "tag-dance"}
	accounts := []models.Account{{ID: "acct"}}
	if err := db.attachAccountLabels(ctx, "reader", accounts); err != nil {
		t.Fatal(err)
	}
	if got := labelIDs(accounts[0].Labels); !slices.Equal(got, want) {
		t.Fatalf("sidebar tags = %v, want %v", got, want)
	}
	labels, err := db.ProviderLabels(ctx, "acct", "gmail")
	if err != nil {
		t.Fatal(err)
	}
	if got := labelIDs(labels); !slices.Equal(got, want) {
		t.Fatalf("Labels dialog = %v, want %v", got, want)
	}
	if err := db.SetTagOrder(ctx, "reader", "acct", []string{"tag-art", "box-inbox"}); !errors.Is(err, ErrFolderOrder) {
		t.Fatalf("a folder as a tag: err = %v, want ErrFolderOrder", err)
	}
}

// The unified folders and tags keep the user's order with their other
// appearance settings; Scheduled and unknown views can't be put in order.
func TestSetUnifiedOrderReachesTheSidebarSettings(t *testing.T) {
	db := newFolderThreadTestDB(t)
	ctx := context.Background()
	work := models.UnifiedTagID("Work")
	if err := db.SetUnifiedFolderOrder(ctx, "reader", []string{"trash", "inbox"}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetUnifiedTagOrder(ctx, "reader", []string{work}); err != nil {
		t.Fatal(err)
	}
	settings := map[string]string{"virtual_folder_order:inbox": "9"}
	db.addVirtualFolderAppearances(ctx, "reader", settings)
	db.addUnifiedTagAppearances(ctx, "reader", settings)
	if settings["virtual_folder_order:trash"] != "0" || settings["virtual_folder_order:inbox"] != "1" || settings["unified_tag_order:"+work] != "0" {
		t.Fatalf("settings = %v, want the saved places over the browser's", settings)
	}
	for name, save := range map[string]func() error{
		"Scheduled":     func() error { return db.SetUnifiedFolderOrder(ctx, "reader", []string{"scheduled", "inbox"}) },
		"unknown view":  func() error { return db.SetUnifiedFolderOrder(ctx, "reader", []string{"projects"}) },
		"account's tag": func() error { return db.SetUnifiedTagOrder(ctx, "reader", []string{"tag-art"}) },
	} {
		if err := save(); !errors.Is(err, ErrFolderOrder) {
			t.Fatalf("%s: err = %v, want ErrFolderOrder", name, err)
		}
	}
}
