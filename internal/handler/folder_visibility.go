package handler

import (
	"net/http"
	"strconv"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// handleSaveFolderVisibility hides a folder from the sidebar, or shows it
// again. It changes nothing on the mail server: the folder still syncs, and
// search, rules and the Move dialog still offer it.
func (h *Handler) handleSaveFolderVisibility(w http.ResponseWriter, r *http.Request) {
	visible, ok := readSidebarVisibility(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	owner := h.userID(ctx)
	folder, _, err := h.managedFolder(ctx, owner, r.PathValue("id"))
	if err == nil {
		err = h.withProfileAccount(ctx, owner, folder.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
			return db.SetFolderVisibility(ctx, owner, folder.ID, visible)
		})
	}
	if err != nil {
		writeFolderError(w, r, "save whether the folder shows in the sidebar", err)
		return
	}
	writeFolderJSON(w, map[string]string{"id": folder.ID, "account_id": folder.AccountID, "visible": strconv.FormatBool(visible)})
}

// handleSaveTagVisibility hides one of an account's tags from the sidebar,
// or shows it again, as handleSaveFolderVisibility does for a folder. It
// changes nothing on the mail server.
func (h *Handler) handleSaveTagVisibility(w http.ResponseWriter, r *http.Request) {
	h.saveTagVisibility(w, r, false)
}

// handleSaveUnifiedTagVisibility hides a tag from the tags of all accounts,
// or shows it again. Each account's own copy keeps its place.
func (h *Handler) handleSaveUnifiedTagVisibility(w http.ResponseWriter, r *http.Request) {
	h.saveTagVisibility(w, r, true)
}

func (h *Handler) saveTagVisibility(w http.ResponseWriter, r *http.Request, unified bool) {
	visible, ok := readSidebarVisibility(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	owner := h.userID(ctx)
	id := r.PathValue("id")
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		if unified {
			return db.SetUnifiedTagVisibility(ctx, owner, id, visible)
		}
		return db.SetTagVisibility(ctx, owner, id, visible)
	})
	if err != nil {
		writeFolderError(w, r, "save whether the tag shows in the sidebar", err)
		return
	}
	writeFolderJSON(w, map[string]string{"id": id, "visible": strconv.FormatBool(visible)})
}

// readSidebarVisibility reads the request's one field, visible=true or
// visible=false. Only app.js sends it, so anything else is refused without
// a message, and the app says to try again.
func readSidebarVisibility(w http.ResponseWriter, r *http.Request) (bool, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return false, false
	}
	values := r.PostForm["visible"]
	if len(r.PostForm) != 1 || len(values) != 1 || (values[0] != "true" && values[0] != "false") {
		w.WriteHeader(http.StatusBadRequest)
		return false, false
	}
	return values[0] == "true", true
}
