package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (h *Handler) handleUserAllowRemoteContent(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid id", 400)
		return
	}
	var request struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&request); err != nil {
		http.Error(w, "invalid body", 400)
		return
	}
	if request.Mode != "once" && request.Mode != "email" && request.Mode != "sender" {
		http.Error(w, "invalid mode", 400)
		return
	}
	ctx, owner := r.Context(), h.userID(r.Context())
	if request.Mode != "once" {
		if err := h.userIMAP.EnsureBody(ctx, owner, id); err != nil {
			userAccountError(w, r, err)
			return
		}
	}
	err = h.userIMAP.RunMessageWork(ctx, owner, id, func(ctx context.Context, account string) error {
		var before *storage.MessageContentSnapshot
		var body []byte
		if err := h.userStorage.WithAccountForUser(ctx, owner, account, func(db *storage.DB) error {
			var err error
			before, err = db.GetMessageContentSnapshotForUser(ctx, owner, id)
			if err != nil || request.Mode == "once" {
				return err
			}
			body, err = db.GetEmailBodyForUser(ctx, strconv.FormatInt(id, 10), owner)
			if err == nil && body == nil {
				return sql.ErrNoRows
			}
			return err
		}); err != nil {
			return err
		}
		if request.Mode == "once" {
			return nil
		}
		path := before.BodyHTMLPath
		// A previously published remote body already references its own assets.
		// A stronger sender approval can reuse it without breaking those links.
		if filepath.Base(path) != "body_remote.html" {
			candidate, err := h.blobStore.NewMessageVersion()
			if err != nil {
				return err
			}
			published := false
			defer func() {
				if !published {
					_ = candidate.DeleteMessage(account, id)
				}
			}()
			urls := message.ExtractRemoteURLs(string(body))
			if len(urls) > 32 {
				urls = urls[:32]
			}
			local := make(map[string]string)
			download := h.remoteResourceDownloader
			if download == nil {
				download = downloadRemoteResource
			}
			for _, url := range urls {
				if err := ctx.Err(); err != nil {
					return err
				}
				data, err := download(ctx, url)
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if err != nil || len(data) == 0 {
					continue
				}
				asset, err := candidate.StoreRemoteAsset(account, id, url, data)
				if err != nil {
					return err
				}
				local[url] = "/api/remote-assets/" + strconv.FormatInt(id, 10) + "/" + filepath.Base(asset)
			}
			path, err = candidate.StoreRemoteBodyHTML(account, id, message.RewriteToLocalAssets(body, local))
			if err != nil {
				return err
			}
			if err := h.publishUserRemoteContent(ctx, before, path, request.Mode); err != nil {
				return err
			}
			published = true
			return nil
		}
		return h.publishUserRemoteContent(ctx, before, path, request.Mode)
	})
	if errors.Is(err, storage.ErrMessageContentChanged) {
		http.Error(w, err.Error(), 409)
		return
	}
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (h *Handler) publishUserRemoteContent(ctx context.Context, snapshot *storage.MessageContentSnapshot, path, mode string) error {
	return h.userStorage.WithAccountForUser(ctx, snapshot.Owner, snapshot.AccountID, func(db *storage.DB) error {
		guard := func(*sql.Tx) error {
			if err := h.userStorage.ValidateUser(ctx, snapshot.Owner); err != nil {
				return err
			}
			state, err := h.userStorage.AccountStateForUser(ctx, snapshot.Owner, snapshot.AccountID)
			if err != nil {
				return err
			}
			if state != storage.AccountActive {
				return storage.ErrAccountRoute
			}
			return ctx.Err()
		}
		return db.PublishUserRemoteContent(ctx, snapshot, path, mode, guard)
	})
}

func (h *Handler) handleUserRemoteAsset(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("messageID"), 10, 64)
	filename := r.PathValue("filename")
	if err != nil || id <= 0 || !validRemoteAssetFilename(filename) {
		http.NotFound(w, r)
		return
	}
	ctx, owner := r.Context(), h.userID(r.Context())
	var file *os.File
	err = h.withUserDB(ctx, owner, func(db *storage.DB) error {
		snapshot, err := db.GetMessageContentSnapshotForUser(ctx, owner, id)
		if err != nil {
			return err
		}
		return h.userStorage.WithAccountActivityForUser(ctx, owner, snapshot.AccountID, func() error {
			// Both migrated legacy bodies and new versions put their assets next
			// to the published remote body. No unpublished version is traversed.
			if filepath.Base(snapshot.BodyHTMLPath) != "body_remote.html" {
				return sql.ErrNoRows
			}
			file, err = os.Open(filepath.Join(filepath.Dir(snapshot.BodyHTMLPath), "remote_assets", filename))
			return err
		})
	})
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	defer file.Close()
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, filename, time.Time{}, file)
}
