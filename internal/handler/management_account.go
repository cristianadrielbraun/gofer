package handler

import (
	"net/http"
)

func (h *Handler) handleManagementAccountSecurity(w http.ResponseWriter, r *http.Request) {
	h.renderPasswordSecurityTab(w, r, http.StatusOK, nil)
}
