package views

import (
	"context"
	"io"

	"github.com/a-h/templ"
	"github.com/cristianadrielbraun/gofer/components/icon"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// Both artworks are present so preview, save and theme switching can change
// styles without fetching or redrawing icons in the browser.
func folderAppearanceSymbol(name string) templ.Component {
	normalized := name
	switch name {
	case "starred":
		normalized = "star"
	case "trash":
		normalized = "trash-2"
	case "alert-circle":
		normalized = "circle-alert"
	case "inbox", "star", "send", "file", "calendar-clock", "archive":
	default:
		if name == "" || !models.ValidFolderIcon(name) {
			normalized = "folder"
		}
	}
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if _, err := io.WriteString(w, `<span class="inline-flex" data-folder-icon-symbol><span class="inline-flex" data-folder-symbol-outline>`); err != nil {
			return err
		}
		if err := icon.Icon(normalized)(icon.Props{Class: "size-4"}).Render(ctx, w); err != nil {
			return err
		}
		if _, err := io.WriteString(w, `</span><span class="inline-flex" data-folder-symbol-filled>`); err != nil {
			return err
		}
		if err := icon.FolderFilled(normalized).Render(ctx, w); err != nil {
			return err
		}
		_, err := io.WriteString(w, `</span></span>`)
		return err
	})
}
