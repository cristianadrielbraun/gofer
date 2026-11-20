package handler

import (
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// handleSaveFolderVisibility updates only Gofer's local presentation setting.
// It never changes IMAP subscriptions or mail synchronization.
func (h *Handler) handleSaveFolderVisibility(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid visibility request", http.StatusBadRequest)
		return
	}

	values := r.PostForm["visible"]
	if len(r.PostForm) != 1 || len(values) != 1 ||
		(values[0] != "true" && values[0] != "false") {
		http.Error(w, "Expected visible=true or visible=false", http.StatusBadRequest)
		return
	}

	visible := values[0] == "true"
	ctx := r.Context()
	owner := h.userID(ctx)

	folder, _, err := h.managedFolder(ctx, owner, r.PathValue("id"))
	if err == nil {
		err = h.withProfileAccount(ctx, owner, folder.AccountID,
			func(_ *config.AccountStore, db *storage.DB) error {
				return db.SetFolderVisibility(ctx, owner, folder.ID, visible)
			})
	}
	if err != nil {
		writeFolderError(w, r, "save folder visibility", err)
		return
	}

	writeFolderJSON(w, map[string]string{
		"id":         folder.ID,
		"account_id": folder.AccountID,
		"visible":    values[0],
	})
}
