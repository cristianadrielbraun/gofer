package views

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestTagAppearanceSettingsAndUnifiedIdentity(t *testing.T) {
	accounts := []models.Account{
		{ID: "imap", Name: "IMAP", EmailSyncEnabled: true, Labels: []models.Label{{ID: "imap-tag", AccountID: "imap", Name: "Work", AppearanceColor: "#ef4444", IconStyle: "filled"}}},
		{ID: "gmail", Name: "Gmail", EmailSyncEnabled: true, Labels: []models.Label{{ID: "gmail-tag", AccountID: "gmail", Name: "work", AppearanceColor: "#3b82f6"}}},
		{ID: "local", Name: "Local", Labels: []models.Label{{ID: "local-tag", AccountID: "local", Name: "Local only"}}},
	}
	id := models.UnifiedTagID("work")
	settings := map[string]string{models.TagColorKey(id, true): "#8b5cf6", models.TagStyleKey(id, true): "outlined"}
	unified := unifiedSidebarLabels(accounts, settings)
	if len(unified) != 1 || unified[0].ID != id || unified[0].AppearanceColor != "#8b5cf6" || unified[0].IconStyle != "outlined" || !unified[0].IsUnified || unified[0].AccountID != "" {
		t.Fatalf("combined tags = %+v", unified)
	}
	accounts[0], accounts[1] = accounts[1], accounts[0]
	if unifiedSidebarLabels(accounts, settings)[0].ID != id {
		t.Fatal("combined tag identity changed with account order")
	}
	var out bytes.Buffer
	if err := TagAppearanceSettings(accounts, settings).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), `data-folder-action="folder-appearance"`) != 4 || !strings.Contains(out.String(), `data-folder-id="local-tag"`) {
		t.Fatalf("settings omitted tags: %s", out.String())
	}
	out.Reset()
	if err := SidebarTagFilterLink(unified[0], "inbox", "inbox", models.EmailFilters{}, "").Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `data-tag-appearance-unified`) || !strings.Contains(out.String(), `color:#8b5cf6`) || strings.Contains(out.String(), `tag_provider_id`) {
		t.Fatalf("unified filter changed semantics: %s", out.String())
	}
}
