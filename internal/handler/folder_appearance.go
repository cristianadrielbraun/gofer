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

type folderAppearanceInput struct {
	Color       string
	Icon, Style *string
}

func readFolderAppearance(w http.ResponseWriter, r *http.Request) (folderAppearanceInput, bool) {
	var input folderAppearanceInput
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid appearance.", http.StatusBadRequest)
		return input, false
	}
	for key := range r.PostForm {
		if key != "color" && key != "icon" && key != "style" {
			http.Error(w, "Invalid folder appearance.", http.StatusBadRequest)
			return input, false
		}
	}
	colors, ok := r.PostForm["color"]
	if !ok || len(colors) != 1 || !models.ValidFolderColor(colors[0]) {
		http.Error(w, "Choose a valid folder color.", http.StatusBadRequest)
		return input, false
	}
	input.Color = strings.ToLower(colors[0])
	if icons, present := r.PostForm["icon"]; present {
		if len(icons) != 1 || !models.ValidFolderIcon(icons[0]) {
			http.Error(w, "Choose an available folder icon.", http.StatusBadRequest)
			return input, false
		}
		input.Icon = &icons[0]
	}
	if styles, present := r.PostForm["style"]; present {
		if len(styles) != 1 || !models.ValidFolderIconStyle(styles[0]) {
			http.Error(w, "Choose an outlined or filled icon style.", http.StatusBadRequest)
			return input, false
		}
		input.Style = &styles[0]
	}
	return input, true
}

func (h *Handler) handleSaveFolderAppearance(w http.ResponseWriter, r *http.Request) {
	input, ok := readFolderAppearance(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	owner := h.userID(ctx)
	folder, _, err := h.managedFolder(ctx, owner, r.PathValue("id"))
	if err == nil {
		err = h.withProfileAccount(ctx, owner, folder.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
			return db.SetFolderAppearance(ctx, owner, folder.ID, input.Color, input.Icon, input.Style)
		})
	}
	if errors.Is(err, storage.ErrFolderColor) || errors.Is(err, storage.ErrFolderIcon) || errors.Is(err, storage.ErrFolderIconStyle) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		writeFolderError(w, r, "save folder appearance", err)
		return
	}
	result := map[string]string{"id": folder.ID, "account_id": folder.AccountID, "color": input.Color}
	if input.Icon != nil {
		result["icon"] = *input.Icon
	}
	if input.Style != nil {
		result["style"] = *input.Style
	}
	writeFolderJSON(w, result)
}

func (h *Handler) handleVirtualFolderAppearance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := models.VirtualFolderAppearance(id, nil); !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	var folder models.Folder
	err := h.withUserDB(ctx, h.userID(ctx), func(db *storage.DB) error {
		folder, _ = models.VirtualFolderAppearance(id, db.GetUISettings(ctx, h.userID(ctx)))
		return nil
	})
	if err != nil {
		writeFolderError(w, r, "load view appearance", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	views.FolderAppearancePanel(folder, "").Render(ctx, w)
}

func (h *Handler) handleSaveVirtualFolderAppearance(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := models.VirtualFolderAppearance(id, nil); !ok {
		http.NotFound(w, r)
		return
	}
	input, ok := readFolderAppearance(w, r)
	if !ok {
		return
	}
	if input.Icon != nil {
		http.Error(w, "This view's symbol cannot be changed.", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	err := h.withUserDB(ctx, h.userID(ctx), func(db *storage.DB) error {
		return db.SetVirtualFolderAppearance(ctx, h.userID(ctx), id, input.Color, input.Style)
	})
	if err != nil {
		writeFolderError(w, r, "save view appearance", err)
		return
	}
	result := map[string]any{"id": id, "color": input.Color, "virtual": true}
	if input.Style != nil {
		result["style"] = *input.Style
	}
	writeFolderJSON(w, result)
}
