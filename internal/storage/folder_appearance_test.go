package storage

import (
	"errors"
	"testing"
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
