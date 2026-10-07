package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// This endpoint is cache/presentation-only: it never contacts a provider.
func (h *Handler) handleCalendarVisibility(w http.ResponseWriter, r *http.Request) {
	sourceID := strings.TrimSpace(r.PathValue("id"))
	var request struct {
		Visible *bool `json:"visible"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if sourceID == "" || decoder.Decode(&request) != nil || request.Visible == nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid calendar visibility", http.StatusBadRequest)
		return
	}
	var err error
	if h.userStorage == nil {
		err = h.db.SetCalendarSourceVisibility(r.Context(), h.userID(r.Context()), sourceID, *request.Visible)
	} else {
		err = h.userIMAP.RunUserServiceWork(r.Context(), h.userID(r.Context()), func(ctx context.Context) error {
			return h.userAccounts.SetCalendarSourceVisible(ctx, h.userID(r.Context()), sourceID, *request.Visible)
		})
		if err != nil {
			userAccountError(w, r, err)
			return
		}
	}
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not save calendar visibility", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"source_id": sourceID, "visible": *request.Visible})
}
