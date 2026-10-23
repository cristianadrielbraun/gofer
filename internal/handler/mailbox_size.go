package handler

import (
	"net/http"
	"strings"

	"github.com/a-h/templ"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// handleMailboxSize shows how much room an account takes on the mail server
// and on this device: the line on its settings card with view=summary,
// otherwise the dialog with each folder.
func (h *Handler) handleMailboxSize(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	if accountID == "" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	blobs := h.blobStore
	h.renderMailboxView(w, r, &ctx, func(h *Handler) (templ.Component, error) {
		size, err := h.db.GetMailboxSize(ctx, h.userID(ctx), accountID)
		if err != nil {
			return nil, err
		}
		// Files are read only for an account the owner has.
		if blobs != nil {
			files, err := blobs.MessageFileSizes(accountID)
			if err != nil {
				return nil, err
			}
			if err := h.db.AddLocalMailboxSizes(ctx, &size, files); err != nil {
				return nil, err
			}
		}
		if r.URL.Query().Get("view") == "summary" {
			return views.AccountMailboxSizeSummary(size), nil
		}
		return views.MailboxSizeDetails(size), nil
	})
}
