package handler

import (
	"database/sql"
	"errors"
	"mime"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (h *Handler) userAttachment(w http.ResponseWriter, r *http.Request, inline, preview bool) {
	ctx := r.Context()
	owner := h.userID(ctx)
	key := "id"
	if inline {
		key = "messageID"
	}
	id, err := strconv.ParseInt(r.PathValue(key), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	var info *storage.AttachmentFetchInfo
	err = h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		if inline {
			info, err = db.GetAttachmentFetchInfoByContentIDForUser(ctx, id, cleanContentID(r.PathValue("contentID")), owner)
		} else {
			info, err = db.GetAttachmentFetchInfoForUser(ctx, id, owner)
		}
		if err == nil && info == nil {
			return sql.ErrNoRows
		}
		return err
	})
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if preview && !isPreviewableImage(info.ContentType, info.Filename) {
		http.NotFound(w, r)
		return
	}
	// Open a copied path under account activity protection, with no database
	// lease. The open file stays readable if cleanup unlinks it during delivery.
	var file *os.File
	open := func() error {
		return h.userStorage.WithAccountActivityForUser(ctx, owner, info.AccountID, func() error {
			if info.StoragePath == "" {
				return sql.ErrNoRows
			}
			var err error
			file, err = os.Open(info.StoragePath)
			return err
		})
	}
	err = open()
	if h.userIMAP != nil && (errors.Is(err, sql.ErrNoRows) || errors.Is(err, os.ErrNotExist)) {
		info, err = h.userIMAP.EnsureAttachment(ctx, owner, info.ID)
		if err == nil {
			if preview && !isPreviewableImage(info.ContentType, info.Filename) {
				http.NotFound(w, r)
				return
			}
			err = open()
		}
	}
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	disposition := "attachment"
	if inline || preview {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": info.Filename}))
	http.ServeContent(w, r, info.Filename, time.Time{}, file)
}
func (h *Handler) handleUserInlineContent(w http.ResponseWriter, r *http.Request) {
	h.userAttachment(w, r, true, false)
}
func (h *Handler) handleUserAttachmentDownload(w http.ResponseWriter, r *http.Request) {
	h.userAttachment(w, r, false, false)
}
func (h *Handler) handleUserAttachmentPreview(w http.ResponseWriter, r *http.Request) {
	h.userAttachment(w, r, false, true)
}
