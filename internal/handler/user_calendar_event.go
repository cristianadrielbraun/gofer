package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// Checking button access is local: no token refresh or provider request belongs
// in a cached event read. The owned credential facade opens its own short lease
// after the view data's lease is released.
func (h *Handler) userCalendarWriteAuthorized(ctx context.Context, source storage.CalendarSource) bool {
	if source.Provider == storage.CalendarSourceProviderCalDAV {
		return h.userAccounts != nil
	}
	return h.userCredentials != nil && h.userCredentials.CalendarWriteAuthorizedForUser(ctx, source.UserID, source.AccountID, source.Provider)
}

func userCalendarDetailAccess(details *views.CalendarEventDetails, event storage.CalendarEvent, source storage.CalendarSource, authorized bool) {
	series := calendarEventIsSeries(event)
	reason := calendarEventEditRestriction(event)
	if reason == "" {
		reason = calendarEventMutationRestriction(event, source, series)
	}
	if reason == "" && !authorized {
		reason = "Reconnect this account from Accounts to grant Calendar write access."
	}
	details.CanEdit, details.EditUnavailableReason = reason == "", reason
	details.EditSeries = details.CanEdit && series
	deleteReason := "Online meetings cannot be deleted in Gofer yet."
	if !calendarStoredEditableOnline(event) {
		deleteReason = calendarEventMutationRestriction(event, source, series)
		if deleteReason == "" && !authorized {
			deleteReason = "Reconnect this account from Accounts to grant Calendar write access."
		}
	}
	details.CanDelete = deleteReason == ""
	calendarEventDetailActions(details, event)
	if authorized && calendarResponseRestriction(event, source) == "" && event.ResponseStatus != "organizer" && (event.ResponseStatus != "" || len(details.Attendees) > 0) {
		data := calendarResponseData(event)
		if source.Provider == storage.CalendarSourceProviderCalDAV {
			data.Ready = false
		}
		details.Response = &data
	}
}

func userCalendarEventError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, storage.ErrAccountRoute):
		http.NotFound(w, r)
	case errors.Is(err, storage.ErrCalendarEventChanged), errors.Is(err, config.ErrAccountServicesChanged):
		http.Error(w, "Calendar changed. Refresh and reopen the event.", http.StatusConflict)
	default:
		http.Error(w, "Calendar event is temporarily unavailable.", http.StatusServiceUnavailable)
	}
}

func (h *Handler) userCalendarEventDetails(ctx context.Context, owner, id string, deliveryOnly bool) (details views.CalendarEventDetails, location *time.Location, err error) {
	snapshot, err := h.userAccounts.SnapshotCalendarEvent(ctx, owner, id)
	if err != nil {
		return details, nil, err
	}
	event, source := snapshot.Event(), snapshot.Source()
	if deliveryOnly && !calendarDeliveryAvailable(event) {
		return details, nil, sql.ErrNoRows
	}
	details = calendarEventDetails(event)
	err = h.userAccounts.WithAccountForUser(ctx, owner, source.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
		location = viewsCalendarLocation(db.GetUISettings(ctx, owner))
		if calendarDeliveryAvailable(event) {
			// This helper only reads owned local delivery records. Its temporary
			// handler cannot reach providers, queues, or shared runtime state.
			local := &Handler{db: db}
			data, deliveryErr := local.calendarDeliveryData(ctx, event)
			if deliveryErr != nil {
				if deliveryOnly {
					return deliveryErr
				}
				data.Error = "Delivery status is temporarily unavailable."
			}
			details.Delivery = &data
		}
		return nil
	})
	if err != nil {
		return details, nil, err
	}
	if !deliveryOnly {
		userCalendarDetailAccess(&details, event, source, h.userCalendarWriteAuthorized(ctx, source))
	}
	// Reacquire and validate after any intervening cache/configuration wait.
	err = h.userAccounts.ValidateCalendarEvent(ctx, snapshot)
	return details, location, err
}

func (h *Handler) handleUserCalendarEvent(w http.ResponseWriter, r *http.Request) {
	h.renderUserCalendarEvent(w, r, false)
}

func (h *Handler) handleUserCalendarDeliveryStatus(w http.ResponseWriter, r *http.Request) {
	h.renderUserCalendarEvent(w, r, true)
}

func (h *Handler) renderUserCalendarEvent(w http.ResponseWriter, r *http.Request, deliveryOnly bool) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.Error(w, "event id required", http.StatusBadRequest)
		return
	}
	var details views.CalendarEventDetails
	var location *time.Location
	err := h.userIMAP.RunUserServiceWork(r.Context(), h.userID(r.Context()), func(ctx context.Context) error {
		var err error
		details, location, err = h.userCalendarEventDetails(ctx, h.userID(ctx), id, deliveryOnly)
		return err
	})
	if err != nil {
		userCalendarEventError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if deliveryOnly {
		err = views.CalendarEventGuests(details).Render(r.Context(), w)
	} else {
		err = views.CalendarEventDialog(details, location).Render(r.Context(), w)
	}
	if err != nil {
		http.Error(w, "could not render calendar event", http.StatusInternalServerError)
	}
}

func (h *Handler) handleUserNewCalendarEvent(w http.ResponseWriter, r *http.Request) {
	var data views.CalendarCreateData
	var formErr error
	err := h.userIMAP.RunUserServiceWork(r.Context(), h.userID(r.Context()), func(ctx context.Context) error {
		owner := h.userID(ctx)
		var sources []storage.CalendarSource
		var location *time.Location
		names := make(map[string]string)
		if err := h.userAccounts.WithUser(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
			selected, err := db.ListSelectedCalendarSources(ctx, owner)
			if err != nil {
				return err
			}
			accounts, err := db.GetAccounts(ctx, owner)
			if err != nil {
				return err
			}
			for _, account := range accounts {
				names[account.ID] = account.Name
				if account.Name == "" {
					names[account.ID] = account.Email
				}
			}
			for _, source := range selected {
				state, err := h.userStorage.AccountStateForUser(ctx, owner, source.AccountID)
				if errors.Is(err, storage.ErrAccountRoute) {
					continue
				}
				if err != nil {
					return err
				}
				if state == storage.AccountActive {
					sources = append(sources, source)
				}
			}
			location = viewsCalendarLocation(db.GetUISettings(ctx, owner))
			return nil
		}); err != nil {
			return err
		}
		// Grant checks reacquire independently; never nest a credential lease
		// inside the local view lease when the cache may have only one slot.
		grants := make(map[string]bool)
		data, formErr = calendarNewEventData(r.URL.Query().Get("date"), time.Now(), location, sources, names, func(source storage.CalendarSource) bool {
			grant, found := grants[source.AccountID]
			if !found {
				grant = h.userCalendarWriteAuthorized(ctx, source)
				grants[source.AccountID] = grant
			}
			return grant
		})
		return ctx.Err()
	})
	if err != nil {
		userCalendarEventError(w, r, err)
		return
	}
	if formErr != nil {
		http.Error(w, formErr.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := views.CalendarCreateDialog(data).Render(r.Context(), w); err != nil {
		http.Error(w, "could not open new event", http.StatusInternalServerError)
	}
}
