package storage

import (
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestTagAppearanceFollowsRenameAndKeepsNamespacesSeparate(t *testing.T) {
	db := seedManagedFolders(t)
	ctx := t.Context()
	label, err := db.EnsureLabel(ctx, LabelInput{ID: "acc_inbox", AccountID: "acc", Name: "Work", ProviderID: "work", ProviderType: LabelProviderGmail, Color: "provider-color"})
	if err != nil {
		t.Fatal(err)
	}
	style := "filled"
	if err := db.SetFolderColor(ctx, "default", "acc_inbox", "#ef4444"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetTagAppearance(ctx, "default", label.ID, "#3b82f6", &style, false); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertLabels(ctx, []LabelInput{{AccountID: "acc", Name: "Renamed", ProviderID: "work", ProviderType: LabelProviderGmail, Color: "new-provider-color"}}); err != nil {
		t.Fatal(err)
	}
	got, err := db.TagAppearanceForUser(ctx, "default", label.ID, false)
	if err != nil || got.Name != "Renamed" || got.AppearanceColor != "#3b82f6" || got.IconStyle != "filled" || got.Color != "new-provider-color" {
		t.Fatalf("renamed tag = %+v, %v", got, err)
	}
	if color, err := db.GetSetting(ctx, "default", "folder_color:acc_inbox"); err != nil || color != "#ef4444" {
		t.Fatalf("folder preference changed = %q, %v", color, err)
	}
	id := models.UnifiedTagID("Renamed")
	stale := db.GetUISettings(ctx, "default")
	stale[models.TagColorKey(id, true)] = "#ef4444"
	if _, err := db.SetTagAppearance(ctx, "default", id, "#8b5cf6", &style, true); err != nil {
		t.Fatal(err)
	}
	if err := db.SetUISettings(ctx, "default", stale); err != nil {
		t.Fatal(err)
	}
	unified := models.UnifiedTagAppearance(got, db.GetUISettings(ctx, "default"))
	if unified.AppearanceColor != "#8b5cf6" || unified.IconStyle != "filled" {
		t.Fatalf("stale snapshot changed unified tag: %+v", unified)
	}
	if models.UnifiedTagID(" WORK ") != models.UnifiedTagID("work") {
		t.Fatal("unified tag identity depends on casing or whitespace")
	}
}
