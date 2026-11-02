package handler

import (
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleTagAppearance(w http.ResponseWriter, r *http.Request) {
	h.renderTagAppearance(w, r, false)
}
func (h *Handler) handleUnifiedTagAppearance(w http.ResponseWriter, r *http.Request) {
	h.renderTagAppearance(w, r, true)
}
func (h *Handler) handleSaveTagAppearance(w http.ResponseWriter, r *http.Request) {
	h.saveTagAppearance(w, r, false)
}
func (h *Handler) handleSaveUnifiedTagAppearance(w http.ResponseWriter, r *http.Request) {
	h.saveTagAppearance(w, r, true)
}

func (h *Handler) renderTagAppearance(w http.ResponseWriter, r *http.Request, unified bool) {
	ctx := r.Context()
	var label models.Label
	err := h.withUserDB(ctx, h.userID(ctx), func(db *storage.DB) error {
		var err error
		label, err = db.TagAppearanceForUser(ctx, h.userID(ctx), r.PathValue("id"), unified)
		return err
	})
	if err != nil {
		writeFolderError(w, r, "load tag appearance", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	views.FolderAppearancePanel(models.TagAppearanceFolder(label), label.AccountID).Render(ctx, w)
}

func (h *Handler) saveTagAppearance(w http.ResponseWriter, r *http.Request, unified bool) {
	input, ok := readFolderAppearance(w, r)
	if !ok {
		return
	}
	if input.Icon != nil {
		http.Error(w, "Tag symbols cannot be changed.", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	var label models.Label
	err := h.withUserDB(ctx, h.userID(ctx), func(db *storage.DB) error {
		var err error
		label, err = db.SetTagAppearance(ctx, h.userID(ctx), r.PathValue("id"), input.Color, input.Style, unified)
		return err
	})
	if err != nil {
		writeFolderError(w, r, "save tag appearance", err)
		return
	}
	result := map[string]any{"id": label.ID, "account_id": label.AccountID, "color": input.Color, "tag": true, "unified_tag": unified}
	if input.Style != nil {
		result["style"] = *input.Style
	}
	writeFolderJSON(w, result)
}
