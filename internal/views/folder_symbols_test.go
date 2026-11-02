package views

import (
	"bytes"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestFolderSymbolStylesCoverPickerAndRoles(t *testing.T) {
	cases := map[string]string{"folder": "folder", "inbox": "inbox", "starred": "star", "send": "send", "file": "file", "calendar-clock": "calendar-clock", "archive": "archive", "alert-circle": "circle-alert", "trash": "trash-2"}
	for _, choice := range models.FolderIconChoices {
		cases[choice.Icon] = choice.Icon
	}
	for name, expected := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := folderAppearanceSymbol(name).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), `data-lucide="`+expected+`"`) || !strings.Contains(out.String(), `data-folder-filled-symbol="`+expected+`"`) {
				t.Fatalf("missing matching artwork: %s", out.String())
			}
		})
	}
}
