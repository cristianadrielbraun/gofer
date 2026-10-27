package handler

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"

	"github.com/a-h/templ"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// errFolderGone marks a folder page whose folder no longer exists, such as one
// deleted on the server. renderMailboxView sends the browser to the inbox.
var errFolderGone = errors.New("folder no longer exists")

// resolveFolderPage resolves the folder a page shows, reporting a missing
// folder as errFolderGone.
func (h *Handler) resolveFolderPage(ctx context.Context, userID, requested string) (string, error) {
	folderID, err := h.resolveFolderID(ctx, userID, requested)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errFolderGone
	}
	return folderID, err
}

func redirectToInbox(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// renderMailboxView copies repository results into a component while leased,
// then renders to the client after releasing the store. Only the audited local
// read handlers use this adapter. The request-local handler has no provider,
// background-worker or body-fetch state; it must never escape the callback.
func (h *Handler) renderMailboxView(w http.ResponseWriter, r *http.Request, ctx *context.Context, prepare func(*Handler) (templ.Component, error)) {
	var component templ.Component
	err := h.withUserDB(r.Context(), h.userID(r.Context()), func(db *storage.DB) error {
		local := h
		if h.userStorage != nil {
			local = &Handler{db: db, auth: h.auth}
		}
		var err error
		component, err = prepare(local)
		return err
	})
	if err != nil {
		if errors.Is(err, errFolderGone) {
			redirectToInbox(w, r)
			return
		}
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, storage.ErrAccountRoute) {
			http.NotFound(w, r)
			return
		}
		log.Printf("mailbox view unavailable: %v", err)
		http.Error(w, "mailbox unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := component.Render(*ctx, w); err != nil {
		log.Printf("render mailbox view: %v", err)
	}
}
