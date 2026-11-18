package handler

import (
	"database/sql"
	"errors"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

// handleSetSharedIdentityActive turns the identity Gofer made for a shared
// mailbox on or off (form value active, "true" or "false"). Turned off, it
// is not offered in the From menu, and the folder sync leaves it off.
func (h *Handler) handleSetSharedIdentityActive(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	data, ok := h.loadAccountIdentitiesData(w, r)
	if !ok {
		return
	}
	err := h.db.SetSharedIdentityActive(r.Context(), h.userID(r.Context()), data.Account.ID, r.PathValue("identity"), r.FormValue("active") == "true")
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("shared identity account=%s: %v", data.Account.ID, err)
		data.Error = i18n.T(r.Context(), "identities.shared.change_failed")
	} else if data, ok = h.loadAccountIdentitiesData(w, r); !ok {
		return
	}
	h.renderAccountIdentitiesPanel(w, r, data)
}
