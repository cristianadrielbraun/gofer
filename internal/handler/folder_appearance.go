package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleFolderAppearance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := h.userID(ctx)
	folder, _, err := h.managedFolder(ctx, owner, r.PathValue("id"))
	var appearance models.Folder
	if err == nil {
		err = h.withProfileAccount(ctx, owner, folder.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
			folders, err := db.AccountFolderTree(ctx, folder.AccountID)
			if err != nil {
				return err
			}
			var find func([]models.Folder) bool
			find = func(folders []models.Folder) bool {
				for _, f := range folders {
					if f.ID == folder.ID {
						appearance = f
						return true
					}
					if find(f.Children) {
						return true
					}
				}
				return false
			}
			if !find(folders) {
				return storage.ErrFolderNotManaged
			}
			return nil
		})
	}
	if err != nil {
		writeFolderError(w, r, "load folder appearance", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	views.FolderAppearancePanel(appearance, folder.AccountID).Render(ctx, w)
}

func (h *Handler) handleSaveFolderAppearance(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid appearance.", http.StatusBadRequest)
		return
	}
	// This slice only accepts color. Symbols remain controlled by folder roles.
	for key := range r.PostForm {
		if key != "color" {
			http.Error(w, "Only folder color can be changed.", http.StatusBadRequest)
			return
		}
	}
	colors, ok := r.PostForm["color"]
	if !ok || len(colors) != 1 || !models.ValidFolderColor(colors[0]) {
		http.Error(w, "Choose a valid folder color.", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	owner := h.userID(ctx)
	folder, _, err := h.managedFolder(ctx, owner, r.PathValue("id"))
	if err == nil {
		err = h.withProfileAccount(ctx, owner, folder.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
			return db.SetFolderColor(ctx, owner, folder.ID, colors[0])
		})
	}
	if errors.Is(err, storage.ErrFolderColor) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		writeFolderError(w, r, "save folder appearance", err)
		return
	}
	writeFolderJSON(w, map[string]string{"id": folder.ID, "account_id": folder.AccountID, "color": strings.ToLower(colors[0])})
}
