package handler

import (
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/mail"
)

func (h *Handler) handleUserTestAccount(w http.ResponseWriter, r *http.Request) {
	results, err := h.userIMAP.TestAccount(r.Context(), h.userID(r.Context()), r.PathValue("id"))
	if errors.Is(err, mail.ErrUserMailConnectionChanged) {
		http.Error(w, i18n.T(r.Context(), "server.error.account_settings_changed_retry_the_connection_test_alt"), http.StatusConflict)
		return
	}
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	h.writeConnectionTestResults(w, r, results, r.PathValue("id"))
}

func (h *Handler) handleUserRepairMailAccount(w http.ResponseWriter, r *http.Request) {
	id, started, err := h.userIMAP.StartGmailRepair(r.Context(), h.userID(r.Context()), strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		if errors.Is(err, mail.ErrUserGmailRepairUnsupported) {
			htmlStatus(w, r, http.StatusBadRequest, "Full mail repair is only available for Gmail accounts.")
		} else if errors.Is(err, mail.ErrUserIMAPManualCapacity) {
			w.Header().Set("Retry-After", "2")
			htmlStatus(w, r, 503, "Mail sync is busy; retry shortly.")
		} else {
			userAccountError(w, r, err)
		}
		return
	}
	w.Header().Set("X-Gofer-Mail-Sync-Run-ID", id)
	if !started {
		w.Header().Set("X-Gofer-Mail-Sync-Running", "true")
		htmlStatus(w, r, 200, "Mail sync is already running.")
		return
	}
	w.Header().Set("X-Gofer-Mail-Sync-Accounts", "1")
	w.Header().Set("X-Gofer-Mail-Sync-Mode", "repair")
	htmlStatus(w, r, 200, "Gmail repair and full resync started for 1 account.")
}
