package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestSettingsSyncTabIncludesUnifiedFoldersPanel(t *testing.T) {
	var out bytes.Buffer
	if err := SettingsSyncTab(models.SyncSettings{SyncIntervalMinutes: 5}, nil).Render(context.Background(), &out); err != nil {
		t.Fatalf("SettingsSyncTab.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		"Sync settings",
		"Unified folders",
		`name="unified_folders_enabled"`,
		`name="unified_folder_inbox_enabled"`,
		`name="unified_folder_starred_enabled"`,
		`name="unified_folder_spam_enabled"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered sync tab missing %q: %s", want, html)
		}
	}
	if strings.Contains(html, "Spam and junk folders across accounts.") {
		t.Fatalf("unified folder rows should not render descriptions: %s", html)
	}
	if strings.Contains(html, `name="unified_folder_scheduled_enabled"`) {
		t.Fatalf("scheduled should not render as a unified folder setting: %s", html)
	}
}
