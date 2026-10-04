package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func calendarDeliveryAvailable(e storage.CalendarEvent) bool {
	return e.AccountEmail != "" && strings.EqualFold(e.OrganizerEmail, e.AccountEmail) && (e.SourceProvider == storage.CalendarSourceProviderCalDAV || len(calendarEventParticipants(e.AttendeesJSON)) > 0)
}

func calendarNotificationBelongsToEvent(d storage.CalendarNotificationDelivery, e storage.CalendarEvent) bool {
	cal, err := calendarUpdateDecodeICS([]byte(d.Calendar))
	if err != nil || len(cal.Events()) != 1 || cal.Events()[0].Props.Get("UID") == nil || cal.Events()[0].Props.Get("ORGANIZER") == nil {
		return false
	}
	uid, err := cal.Events()[0].Props.Text("UID")
	return err == nil && uid == e.ICalUID && calendarReplyAddress(cal.Events()[0].Props.Get("ORGANIZER").Value) == strings.ToLower(e.AccountEmail) && (d.Method == "REQUEST" || d.Method == "CANCEL")
}

func (h *Handler) calendarDeliveryData(ctx context.Context, e storage.CalendarEvent) (views.CalendarDeliveryData, error) {
	data := views.CalendarDeliveryData{EventID: e.ID, EmptyNote: "No email-delivery record. Your calendar server may handle invitations directly."}
	if e.SourceProvider != storage.CalendarSourceProviderCalDAV {
		provider := "your calendar provider"
		if e.SourceProvider == "gmail" {
			provider = "Google Calendar"
		}
		if e.SourceProvider == "outlook" {
			provider = "Microsoft Calendar"
		}
		data.EmptyNote = "Invitation delivery is managed by " + provider + ". Guest responses reflect the last calendar sync."
		return data, nil
	}
	deliveries, err := h.db.ListCalendarNotificationDeliveries(ctx, e.UserID, e.ID)
	if err != nil {
		return data, err
	}
	seen := map[string]bool{}
	for _, d := range deliveries {
		if !calendarNotificationBelongsToEvent(d, e) {
			continue
		}
		// Show the latest delivery for each recipient, not every historical update.
		var recipients []string
		for _, recipient := range d.Recipients {
			key := strings.ToLower(recipient)
			if !seen[key] {
				recipients = append(recipients, recipient)
				seen[key] = true
			}
		}
		if len(recipients) == 0 {
			continue
		}
		row := views.CalendarDeliveryRow{ID: d.ID, Recipient: strings.Join(recipients, ", "), State: d.Status, CanRetry: d.Status == storage.OutgoingSendFailed}
		kind := "Invitation/update"
		if d.Method == "CANCEL" {
			kind = "Guest cancellation"
		}
		switch d.Status {
		case storage.OutgoingSendPending:
			row.Label = kind + " queued"
			row.Note = "Waiting to send; temporary failures retry automatically."
		case storage.OutgoingSendSending:
			row.Label = "Sending " + strings.ToLower(kind) + "…"
			row.Note = "Delivery is in progress."
		case storage.OutgoingSendSent:
			row.Label = kind + " sent"
			row.Note = "Accepted by the sending mail server."
		case storage.OutgoingSendFailed:
			row.Label = kind + " failed"
			row.Note = "Not sent. Check this account’s sending settings and refresh the calendar before retrying."
		case storage.OutgoingSendAmbiguous:
			row.Label = kind + " delivery uncertain"
			row.Note = "It may already have been sent. Check Sent before taking further action; resending could duplicate it."
		case storage.OutgoingSendCanceled:
			row.Label = kind + " canceled"
			row.Note = "This queued delivery was canceled."
		default:
			continue
		}
		data.Rows = append(data.Rows, row)
		data.HasEmailDelivery = true
	}
	replies, err := h.db.ListCalendarIncomingDeliveries(ctx, e.UserID, e.ID)
	if err != nil {
		return data, err
	}
	for _, d := range replies {
		row := views.CalendarDeliveryRow{Recipient: d.Attendee, State: d.State}
		switch d.Reason {
		case "processing":
			row.State = "processing"
			row.Label = "Processing guest reply…"
			row.Note = "Verifying the sender and current meeting before applying the response."
		case "verification_pending":
			row.Label = "Reply verification waiting to retry"
			row.Note = "Sender verification is temporarily unavailable. The guest’s RSVP is unchanged."
		case "unverified":
			row.Label = "Guest reply could not be verified"
			row.Note = "The email’s sender could not be authenticated. It remains in Mail; the guest’s RSVP is unchanged."
		case "retry":
			row.Label = "Guest reply waiting to retry"
			row.Note = "The calendar update could not be confirmed. Gofer will retry automatically."
		case "not_applied":
			row.Label = "Guest reply not applied"
			row.Note = "The reply is outdated, the meeting changed, or this reply is unsupported. The email remains in Mail."
		case "applied":
			row.Label = "Guest reply processed"
			row.Note = "The response was confirmed on the calendar server."
		default:
			continue
		}
		data.Rows = append(data.Rows, row)
	}
	return data, nil
}

func (h *Handler) calendarDeliveryEvent(w http.ResponseWriter, r *http.Request) (storage.CalendarEvent, bool) {
	e, err := h.db.GetCalendarEvent(r.Context(), h.userID(r.Context()), strings.TrimSpace(r.PathValue("id")))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !calendarDeliveryAvailable(e)) {
		http.NotFound(w, r)
		return e, false
	}
	if err != nil {
		http.Error(w, "could not load event delivery", http.StatusInternalServerError)
		return e, false
	}
	return e, true
}

func (h *Handler) renderCalendarDelivery(w http.ResponseWriter, r *http.Request, e storage.CalendarEvent, note string) {
	data, err := h.calendarDeliveryData(r.Context(), e)
	if err != nil {
		http.Error(w, "could not load event delivery", http.StatusInternalServerError)
		return
	}
	data.Error = note
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	details := views.CalendarEventDetails{Event: calendarViewEvent(e), Organizer: views.CalendarEventParticipant{Name: e.OrganizerName, Email: e.OrganizerEmail}, Attendees: calendarEventParticipants(e.AttendeesJSON), Delivery: &data}
	if err := views.CalendarEventGuests(details).Render(r.Context(), w); err != nil {
		http.Error(w, "could not render event delivery", http.StatusInternalServerError)
	}
}

func (h *Handler) handleCalendarDeliveryStatus(w http.ResponseWriter, r *http.Request) {
	e, ok := h.calendarDeliveryEvent(w, r)
	if !ok {
		return
	}
	h.renderCalendarDelivery(w, r, e, "")
}

func (h *Handler) handleCalendarDeliveryRetry(w http.ResponseWriter, r *http.Request) {
	e, ok := h.calendarDeliveryEvent(w, r)
	if !ok {
		return
	}
	deliveries, err := h.db.ListCalendarNotificationDeliveries(r.Context(), e.UserID, e.ID)
	if err != nil {
		http.Error(w, "could not load invitation delivery", http.StatusInternalServerError)
		return
	}
	var target *storage.CalendarNotificationDelivery
	for i := range deliveries {
		if deliveries[i].ID == r.PathValue("sendID") && calendarNotificationBelongsToEvent(deliveries[i], e) {
			target = &deliveries[i]
			break
		}
	}
	if target == nil {
		http.NotFound(w, r)
		return
	}
	if target.Status != storage.OutgoingSendFailed {
		h.renderCalendarDelivery(w, r, e, "This delivery cannot be retried safely in its current state. No new email was queued.")
		return
	}
	// The existing worker performs a fresh DAV/version/recipient preflight on
	// every attempt. Never bypass it or confirm ambiguous SMTP deliveries here.
	_, err = h.db.RetryOutgoingSend(r.Context(), e.UserID, target.ID, false)
	if err != nil {
		h.renderCalendarDelivery(w, r, e, "Delivery changed or retry could not be queued. No additional email was queued by this request.")
		return
	}
	h.signalOutgoingWorker()
	h.renderCalendarDelivery(w, r, e, "")
}
