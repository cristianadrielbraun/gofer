package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (h *Handler) handleUserSyncContactNow(w http.ResponseWriter, r *http.Request) {
	ctx, owner := r.Context(), h.userID(r.Context())
	id := strings.TrimSpace(r.PathValue("id"))
	var contact *models.Contact
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		var err error
		contact, err = db.GetContact(ctx, owner, id)
		return err
	})
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if contact == nil {
		http.NotFound(w, r)
		return
	}
	if !contact.GoferSyncEnabled {
		http.Error(w, "Gofer Sync is disabled for this contact", http.StatusConflict)
		return
	}
	if _, err := h.userAccounts.EnqueueContactSyncForUser(ctx, owner, id); err != nil {
		if errors.Is(err, storage.ErrContactSyncUnavailable) {
			http.Error(w, "No enabled sync locations are available", http.StatusConflict)
		} else {
			userAccountError(w, r, err)
		}
		return
	}
	if err := h.WakeUserContactQueue(ctx, owner); err != nil && ctx.Err() == nil {
		log.Printf("owned contacts: wake manually queued sync: %v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "contact_id": id, "contact_sync_queued": true, "status": "pending"})
}
