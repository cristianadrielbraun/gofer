package handler

import (
	"bytes"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Body reads use the local cache, optionally populated by the routed IMAP worker.
func (h *Handler) handleUserEmailBody(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner, id := h.userID(ctx), r.PathValue("id")
	number, err := strconv.ParseInt(id, 10, 64)
	if err != nil || number <= 0 {
		http.NotFound(w, r)
		return
	}
	original := r.URL.Query().Get("mode") == "original"
	requestedRemote := r.URL.Query().Get("remote") == "true"
	remote := requestedRemote
	var body, cached []byte
	var rawPath, accountID string
	attempted := false
	var cidURLs map[string]string
load:
	body, cached, rawPath = nil, nil, ""
	remote = requestedRemote
	cidURLs = make(map[string]string)
	err = h.withUserDB(ctx, owner, func(db *storage.DB) error {
		info, err := db.GetMessageStorageInfoForUser(ctx, number, owner)
		if err != nil {
			return err
		}
		if info == nil {
			return sql.ErrNoRows
		}
		accountID = info.AccountID
		// Resolve the copied account before using its message paths, and keep its
		// scope pinned until the local reads finish. No provider network call occurs.
		return h.userStorage.WithAccountForUser(ctx, owner, info.AccountID, func(*storage.DB) error {
			cached, err = db.GetEmailBodyForUser(ctx, id, owner)
			if err != nil {
				return err
			}
			if original {
				body, err = db.GetEmailOriginalHTMLBodyForUser(ctx, id, owner)
				if err != nil {
					return err
				}
				rawPath = info.RawPath
				attachments, err := db.GetAttachmentsInternal(ctx, number)
				if err != nil {
					return err
				}
				for _, attachment := range attachments {
					if attachment.Inline && attachment.ContentID != "" {
						cidURLs[attachment.ContentID] = inlineContentURL(number, attachment.ContentID)
					}
				}
			}
			if !remote {
				remote = db.IsRemoteContentAllowedForMessageForUser(ctx, number, owner)
				if !remote {
					sender, err := db.GetMessageSenderEmailForUser(ctx, number, owner)
					if err != nil {
						return err
					}
					if sender != "" {
						remote = db.IsRemoteContentAllowedForSenderForUser(ctx, sender, owner)
					}
				}
			}
			return nil
		})
	})
	if err != nil {
		h.writeEmailBodyError(w, ctx, owner, id, accountID, err)
		return
	}
	if original && body == nil && rawPath != "" {
		// Copy/parse saved MIME without pinning a database or writing a cached path.
		if file, err := os.Open(rawPath); err == nil {
			raw, readErr := io.ReadAll(io.LimitReader(file, 35<<20+1))
			_ = file.Close()
			if readErr == nil && len(raw) <= 35<<20 {
				body, _ = message.ExtractHTMLBody(bytes.NewReader(raw))
			}
		}
	}
	if original && body != nil {
		body = message.RewriteCIDReferences(message.SanitizeOriginalHTML(body), cidURLs)
	}
	if body == nil {
		body = cached
	}
	if body == nil && h.userIMAP != nil && !attempted {
		attempted = true
		if err := h.userIMAP.EnsureBody(ctx, owner, number); err != nil {
			h.writeEmailBodyError(w, ctx, owner, id, accountID, err)
			return
		}
		goto load
	}
	if body == nil {
		h.writeEmailBodyError(w, ctx, owner, id, accountID, errors.New("message body is not cached yet"))
		return
	}
	if remote {
		body = message.RestoreRemoteImages(body)
	}
	doc := buildBodyDocument(body, emailResizeScript(id), r.URL.Query().Get("theme"), r.URL.Query().Get("bg"), r.URL.Query().Get("fg"), r.URL.Query().Get("link"), original, emailBodyTextSize(r))
	if !remote {
		doc = append(doc, remoteImagesDetectScript(id)...)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(doc)
}
