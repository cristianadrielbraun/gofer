package handler

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleSaveAccountCalendarSources(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountID := strings.TrimSpace(r.PathValue("id"))
	if accountID == "" {
		http.Error(w, "account id required", http.StatusBadRequest)
		return
	}
	if !h.requireOwnedAccount(w, r, accountID) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid calendar source selection", http.StatusBadRequest)
		return
	}

	selectedSourceIDs := r.Form["source_id"]
	if err := h.db.SetCalendarSourceSelection(ctx, h.userID(ctx), accountID, selectedSourceIDs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(err.Error(), "does not belong to account") {
			http.Error(w, "invalid calendar source selection", http.StatusBadRequest)
			return
		}
		http.Error(w, "Could not save calendar source selection.", http.StatusInternalServerError)
		return
	}

	data, err := h.accountStore.GetEditData(ctx, accountID)
	if err != nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.CalendarSyncSettingsResult(*data, "Calendar sources saved.", false).Render(ctx, w); err != nil {
		http.Error(w, "failed to render calendar source selection", http.StatusInternalServerError)
	}
}
