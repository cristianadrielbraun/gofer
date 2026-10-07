package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (h *Handler) handleUserConfirmContactSyncSetup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid conflict choices", http.StatusBadRequest)
		return
	}
	selected := contactSetupPreferredFields(r)
	owner, id := h.userID(r.Context()), strings.TrimSpace(r.PathValue("id"))
	var result storage.ContactEditResult
	err := h.userIMAP.RunUserServiceWork(r.Context(), owner, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotContactSetup(ctx, owner, id)
		if err != nil {
			return err
		}
		result, err = h.userAccounts.ConfirmContactSetup(ctx, snapshot, selected)
		if err != nil {
			return err
		}
		if result.OperationID != "" {
			if err := h.WakeUserContactQueue(ctx, owner); err != nil && ctx.Err() == nil {
				log.Printf("owned contacts: wake committed setup sync: %v", err)
			}
		}
		return nil
	})
	if err != nil && result.Contact.ID == "" {
		userContactEditError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "contact_id": result.Contact.ID,
		"location": "/contacts?contact=" + result.Contact.ID, "contact_sync_queued": result.OperationID != ""})
}

func contactSetupPreferredFields(r *http.Request) map[string]string {
	selected := map[string]string{}
	for name, values := range r.Form {
		if strings.HasPrefix(name, "preferred_") && len(values) > 0 && strings.TrimSpace(values[0]) != "" {
			selected[strings.TrimPrefix(name, "preferred_")] = strings.TrimSpace(values[0])
		}
	}
	return selected
}
