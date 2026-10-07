package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleUserCalendarDeliveryRetry(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if r.URL.RawQuery != "" || err != nil || len(body) != 0 {
		http.Error(w, "Invalid invitation retry request.", http.StatusBadRequest)
		return
	}
	owner, id, sendID := h.userID(r.Context()), strings.TrimSpace(r.PathValue("id")), strings.TrimSpace(r.PathValue("sendID"))
	var details views.CalendarEventDetails
	var note string
	err = h.userIMAP.RunUserServiceWork(r.Context(), owner, func(ctx context.Context) error {
		event, err := h.userAccounts.SnapshotCalendarEvent(ctx, owner, id)
		if err != nil {
			return err
		}
		if !calendarDeliveryAvailable(event.Event()) || event.Source().Provider != storage.CalendarSourceProviderCalDAV {
			return sql.ErrNoRows
		}
		err = h.userIMAP.RunAccountService(ctx, owner, event.Service().AccountID(), mail.AccountServiceCalendar, 30*time.Second, func(operation context.Context) error {
			unlock, err := h.lockCalendarCreate(operation, event.Source())
			if err != nil {
				return err
			}
			defer unlock()
			if err := h.userAccounts.ValidateCalendarEvent(operation, event); err != nil {
				return err
			}
			// Check membership from the exact immutable control snapshot, not a
			// separate list read that could be replaced before the writer wait.
			control, err := h.userAccounts.SnapshotCalendarNotificationControl(operation, owner, sendID)
			if err != nil {
				return sql.ErrNoRows
			}
			send := control.Send()
			var snapshot outgoingMessageSnapshot
			if json.Unmarshal(send.MessageJSON, &snapshot) != nil || snapshot.CalendarNotification == nil {
				return sql.ErrNoRows
			}
			notification := snapshot.CalendarNotification
			if send.AccountID != event.Service().AccountID() || notification.UserID != owner || notification.SourceID != event.Source().ID || notification.ResourceID != event.Event().RemoteID || !calendarNotificationBelongsToEvent(storage.CalendarNotificationDelivery{ID: send.ID, Method: notification.Method, Calendar: notification.Calendar}, event.Event()) {
				return sql.ErrNoRows
			}
			if send.Status != storage.OutgoingSendFailed {
				note = "This delivery cannot be retried safely in its current state. No new email was queued."
				return nil
			}
			if _, err := h.userAccounts.RetryCalendarNotificationForEvent(operation, control, event); err != nil {
				note = "Delivery changed or retry could not be queued. No additional email was queued by this request."
				return nil
			}
			return nil
		})
		if err != nil {
			return err
		}
		// The SMTP worker performs the native DAV preflight on each attempt.
		// Queue it only after releasing the Calendar/source gates.
		if note == "" {
			if err := h.userIMAP.QueueAccount(ctx, event.Service().AccountID()); err != nil {
				note = "Retry was queued. Delivery will resume when this account's worker is available."
			}
		}
		details, _, err = h.userCalendarEventDetails(ctx, owner, id, true)
		if err == nil && details.Delivery != nil {
			details.Delivery.Error = note
		}
		return err
	})
	if err != nil {
		userCalendarEventError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := views.CalendarEventGuests(details).Render(r.Context(), w); err != nil {
		http.Error(w, "could not render event delivery", http.StatusInternalServerError)
	}
}
