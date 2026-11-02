package storage

import (
	"errors"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestFolderColorSurvivesProviderUpdatesAndNesting(t *testing.T) {
	db := seedManagedFolders(t)
	ctx := t.Context()
	for _, id := range []string{"acc_inbox", "acc_dunes"} {
		if err := db.SetFolderColor(ctx, "default", id, "#8B5CF6"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetFolderColor(ctx, "someone-else", "acc_inbox", "#ef4444"); !errors.Is(err, ErrFolderNotManaged) {
		t.Fatalf("foreign write = %v", err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_dunes", AccountID: "acc", ParentID: "acc_coast", RemoteID: "Trips/Coast/Renamed", Name: "Renamed", Icon: "folder", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatal(err)
	}
	tree, err := db.AccountFolderTree(ctx, "acc")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, f := range tree {
		if f.ID == "acc_inbox" && f.Color == "#8b5cf6" {
			found++
		}
		if f.ID == "acc_trips" {
			dunes := f.Children[0].Children[0]
			if dunes.ID == "acc_dunes" && dunes.Name == "Renamed" && dunes.Color == "#8b5cf6" {
				found++
			}
		}
	}
	if found != 2 {
		t.Fatalf("colors missing from updated tree: %+v", tree)
	}
	if _, err := db.Write().ExecContext(ctx, `UPDATE folders SET discovery_state='missing' WHERE id='acc_dunes'`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFolderColor(ctx, "default", "acc_dunes", "#ef4444"); !errors.Is(err, ErrFolderNotManaged) {
		t.Fatalf("inactive write = %v", err)
	}
}

func TestFolderIconSurvivesSyncAndCannotOverrideNewRole(t *testing.T) {
	db := seedManagedFolders(t)
	ctx := t.Context()
	icon := "briefcase-business"
	if err := db.SetFolderAppearance(ctx, "default", "acc_dunes", "#3b82f6", &icon, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_dunes", AccountID: "acc", ParentID: "acc_coast", RemoteID: "Trips/Coast/Dunes", Name: "Work", Icon: "folder", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatal(err)
	}
	get := func() models.Folder {
		tree, err := db.AccountFolderTree(ctx, "acc")
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range tree {
			if f.ID == "acc_trips" {
				return f.Children[0].Children[0]
			}
		}
		t.Fatal("nested folder missing")
		return models.Folder{}
	}
	f := get()
	if f.Icon != "folder" || models.CustomFolderIcon(f) != icon || f.Color != "#3b82f6" {
		t.Fatalf("sync overwrote appearance: %+v", f)
	}
	if _, err := db.Write().ExecContext(ctx, `UPDATE folders SET role='archive', icon='archive' WHERE id='acc_dunes'`); err != nil {
		t.Fatal(err)
	}
	if f := get(); models.CustomFolderIcon(f) != "" || f.Icon != "archive" {
		t.Fatalf("role icon overridden: %+v", f)
	}
	if err := db.SetFolderAppearance(ctx, "default", "acc_dunes", "#ef4444", &icon, nil); !errors.Is(err, ErrFolderIcon) {
		t.Fatalf("role write = %v", err)
	}
	if f := get(); f.Color != "#3b82f6" {
		t.Fatalf("refused icon write changed color: %+v", f)
	}
}

func TestFolderStyleSurvivesSyncAndPartialUpdates(t *testing.T) {
	db := seedManagedFolders(t)
	ctx := t.Context()
	style, icon := "filled", "camera"
	if err := db.SetFolderAppearance(ctx, "default", "acc_dunes", "#3b82f6", &icon, &style); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFolders(ctx, []UpsertFolderInput{
		{ID: "acc_dunes", AccountID: "acc", ParentID: "acc_coast", RemoteID: "Trips/Coast/Dunes", Name: "Photos", Icon: "folder", Role: "custom", Selectable: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFolderColor(ctx, "default", "acc_dunes", "#ef4444"); err != nil {
		t.Fatal(err)
	}
	tree, err := db.AccountFolderTree(ctx, "acc")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range tree {
		if f.ID == "acc_trips" {
			f = f.Children[0].Children[0]
			if f.IconStyle != "filled" || f.IconOverride != "camera" || f.Color != "#ef4444" {
				t.Fatalf("lost appearance: %+v", f)
			}
			return
		}
	}
	t.Fatal("folder missing")
}
