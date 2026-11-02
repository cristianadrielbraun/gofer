package storage

import (
	"errors"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestVirtualAppearanceSurvivesStaleSettingsSnapshot(t *testing.T) {
	db := seedManagedFolders(t)
	ctx := t.Context()
	stale := db.GetUISettings(ctx, "default")
	stale[models.VirtualFolderColorKey("inbox")] = "#ef4444"
	stale[models.VirtualFolderStyleKey("inbox")] = "outlined"
	style := "filled"
	if err := db.SetVirtualFolderAppearance(ctx, "default", "inbox", "#3B82F6", &style); err != nil {
		t.Fatal(err)
	}
	if err := db.SetUISettings(ctx, "default", stale); err != nil {
		t.Fatal(err)
	}
	if err := db.MergeUISettings(ctx, "default", map[string]string{"theme_style": "minimal"}); err != nil {
		t.Fatal(err)
	}
	settings := db.GetUISettings(ctx, "default")
	folder, ok := models.VirtualFolderAppearance("inbox", settings)
	if !ok || folder.Color != "#3b82f6" || folder.IconStyle != "filled" || settings["theme_style"] != "minimal" {
		t.Fatalf("stale snapshot overwrote appearance: %+v", folder)
	}
	if err := db.SetVirtualFolderAppearance(ctx, "default", "inbox", "", nil); err != nil {
		t.Fatal(err)
	}
	folder, _ = models.VirtualFolderAppearance("inbox", db.GetUISettings(ctx, "default"))
	if folder.Color != "" || folder.IconStyle != "filled" {
		t.Fatalf("partial reset changed style: %+v", folder)
	}
	if err := db.SetVirtualFolderAppearance(ctx, "default", "acc_inbox", "#3b82f6", &style); !errors.Is(err, ErrFolderNotManaged) {
		t.Fatalf("real ID accepted as virtual: %v", err)
	}
}
