package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
)

func (h *Handler) handleUserPrefetchBody(w http.ResponseWriter, r *http.Request) {
	h.handleUserFetchBody(w, r, false)
}

func (h *Handler) handleUserRefetchBody(w http.ResponseWriter, r *http.Request) {
	h.handleUserFetchBody(w, r, true)
}

func (h *Handler) handleUserFetchBody(w http.ResponseWriter, r *http.Request, force bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}
	if force {
		err = h.userIMAP.RefetchBody(r.Context(), h.userID(r.Context()), id)
	} else {
		err = h.userIMAP.EnsureBody(r.Context(), h.userID(r.Context()), id)
	}
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	if !force {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "refetched"})
}
