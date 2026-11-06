package handler

import (
	"errors"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// handleFolderOrder puts an account's folders that are next to each other in
// the order given (folder_id, repeated), from the sidebar or the Folders
// dialog. A folder that isn't the account's any more answers 409, and the
// page loads its folders again.
func (h *Handler) handleFolderOrder(w http.ResponseWriter, r *http.Request) {
	h.saveOrder(w, r, "folder_id", func(db *storage.DB, owner, accountID string, ids []string) error {
		return db.SetFolderOrder(r.Context(), owner, accountID, ids)
	})
}

// handleTagOrder puts an account's tags in the order given (tag_id,
// repeated).
func (h *Handler) handleTagOrder(w http.ResponseWriter, r *http.Request) {
	h.saveOrder(w, r, "tag_id", func(db *storage.DB, owner, accountID string, ids []string) error {
		return db.SetTagOrder(r.Context(), owner, accountID, ids)
	})
}

// handleUnifiedFolderOrder puts the unified folders in the order given
// (virtual_folder_id, repeated).
func (h *Handler) handleUnifiedFolderOrder(w http.ResponseWriter, r *http.Request) {
	h.saveUnifiedOrder(w, r, "virtual_folder_id", func(db *storage.DB, owner string, ids []string) error {
		return db.SetUnifiedFolderOrder(r.Context(), owner, ids)
	})
}

// handleUnifiedTagOrder puts the unified tags in the order given (tag_id,
// repeated).
func (h *Handler) handleUnifiedTagOrder(w http.ResponseWriter, r *http.Request) {
	h.saveUnifiedOrder(w, r, "tag_id", func(db *storage.DB, owner string, ids []string) error {
		return db.SetUnifiedTagOrder(r.Context(), owner, ids)
	})
}

func (h *Handler) saveUnifiedOrder(w http.ResponseWriter, r *http.Request, field string, save func(db *storage.DB, owner string, ids []string) error) {
	r.Body = http.MaxBytesReader(w, r.Body, 512<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	owner := h.userID(ctx)
	ids := r.PostForm[field]
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		return save(db, owner, ids)
	})
	if errors.Is(err, storage.ErrFolderOrder) {
		http.Error(w, http.StatusText(http.StatusConflict), http.StatusConflict)
		return
	}
	if err != nil {
		writeFolderError(w, r, "save the order", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) saveOrder(w http.ResponseWriter, r *http.Request, field string, save func(db *storage.DB, owner, accountID string, ids []string) error) {
	r.Body = http.MaxBytesReader(w, r.Body, 512<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	owner := h.userID(ctx)
	accountID := r.PathValue("id")
	ids := r.PostForm[field]
	err := h.withProfileAccount(ctx, owner, accountID, func(_ *config.AccountStore, db *storage.DB) error {
		return save(db, owner, accountID, ids)
	})
	if errors.Is(err, storage.ErrFolderOrder) {
		http.Error(w, http.StatusText(http.StatusConflict), http.StatusConflict)
		return
	}
	if err != nil {
		writeFolderError(w, r, "save the order", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
