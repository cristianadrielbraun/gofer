package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// calendarEventActions is what an event allows, for the calendar's
// right-click menu: the same answers its details dialog acts on.
type calendarEventActions struct {
	CanEdit       bool   `json:"can_edit"`
	EditSeries    bool   `json:"edit_series"`
	HasOccurrence bool   `json:"has_occurrence"`
	CanDelete     bool   `json:"can_delete"`
	DeleteSeries  bool   `json:"delete_series"`
	DeleteVersion string `json:"delete_version"`
	JoinURL       string `json:"join_url"`
	// Forward says the invitation can be forwarded from its details dialog.
	Forward  bool                    `json:"forward"`
	Response *calendarEventRSVPState `json:"response"`
}

// calendarEventRSVPState is the user's reply to an invitation and what
// sending a new one needs. Ready is false while the reply needs checking
// with the provider first, which only the details dialog does.
type calendarEventRSVPState struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Scope   string `json:"scope"`
	Ready   bool   `json:"ready"`
}

func calendarEventActionsFrom(details views.CalendarEventDetails) calendarEventActions {
	actions := calendarEventActions{
		CanEdit:       details.CanEdit,
		EditSeries:    details.EditSeries,
		HasOccurrence: details.HasOccurrence,
		CanDelete:     details.CanDelete,
		DeleteSeries:  details.DeleteSeries,
		DeleteVersion: details.DeleteVersion,
		JoinURL:       calendar.SafeMeetingURL(details.JoinURL),
		Forward:       details.Forward != "",
	}
	if response := details.Response; response != nil {
		actions.Response = &calendarEventRSVPState{Status: response.Status, Version: response.Version, Scope: response.Scope, Ready: response.Ready}
	}
	return actions
}

func writeCalendarEventActions(w http.ResponseWriter, details views.CalendarEventDetails) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	json.NewEncoder(w).Encode(calendarEventActionsFrom(details))
}

func (h *Handler) handleCalendarEventActions(w http.ResponseWriter, r *http.Request) {
	eventID := strings.TrimSpace(r.PathValue("id"))
	if eventID == "" {
		http.Error(w, i18n.T(r.Context(), "server.error.event_id_required"), http.StatusBadRequest)
		return
	}
	details, err := h.sharedCalendarEventDetails(r.Context(), h.userID(r.Context()), eventID)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.could_not_load_calendar_event"), http.StatusInternalServerError)
		return
	}
	writeCalendarEventActions(w, details)
}

func (h *Handler) handleUserCalendarEventActions(w http.ResponseWriter, r *http.Request) {
	eventID := strings.TrimSpace(r.PathValue("id"))
	if eventID == "" {
		http.Error(w, i18n.T(r.Context(), "server.error.event_id_required"), http.StatusBadRequest)
		return
	}
	var details views.CalendarEventDetails
	err := h.userIMAP.RunUserServiceWork(r.Context(), h.userID(r.Context()), func(ctx context.Context) error {
		var err error
		details, _, err = h.userCalendarEventDetails(ctx, h.userID(ctx), eventID, false)
		return err
	})
	if err != nil {
		userCalendarEventError(w, r, err)
		return
	}
	writeCalendarEventActions(w, details)
}
