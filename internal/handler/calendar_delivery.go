package handler

import (
	"context"
	"database/sql"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func calendarDeliveryAvailable(e storage.CalendarEvent) bool {
	return e.AccountEmail != "" && strings.EqualFold(e.OrganizerEmail, e.AccountEmail) && (storage.CalendarSourceMailServer(e.SourceProvider) || len(calendarEventParticipants(e.AttendeesJSON)) > 0)
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
	data := views.CalendarDeliveryData{EventID: e.ID, EmptyNote: i18n.T(ctx, "server.msg.no_email_delivery_record_your_calendar_server")}
	if !storage.CalendarSourceMailServer(e.SourceProvider) {
		provider := "your calendar provider"
		if e.SourceProvider == "gmail" {
			provider = "Google Calendar"
		}
		if e.SourceProvider == "outlook" {
			provider = "Microsoft Calendar"
		}
		data.EmptyNote = i18n.T(ctx, "server.pattern.delivery_managed", "provider", provider)
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
		kind := "invitation"
		if d.Method == "CANCEL" {
			kind = "cancellation"
		}
		switch d.Status {
		case storage.OutgoingSendPending:
			row.Label = i18n.T(ctx, "server.delivery."+kind+".queued")
			row.Note = i18n.T(ctx, "server.msg.waiting_to_send_temporary_failures_retry_automatically")
		case storage.OutgoingSendSending:
			row.Label = i18n.T(ctx, "server.delivery."+kind+".sending")
			row.Note = i18n.T(ctx, "server.msg.delivery_is_in_progress")
		case storage.OutgoingSendSent:
			row.Label = i18n.T(ctx, "server.delivery."+kind+".sent")
			row.Note = i18n.T(ctx, "server.msg.accepted_by_the_sending_mail_server")
		case storage.OutgoingSendFailed:
			row.Label = i18n.T(ctx, "server.delivery."+kind+".failed")
			row.Note = i18n.T(ctx, "server.msg.not_sent_check_this_account_s_sending")
		case storage.OutgoingSendAmbiguous:
			row.Label = i18n.T(ctx, "server.delivery."+kind+".uncertain")
			row.Note = i18n.T(ctx, "server.msg.it_may_already_have_been_sent_check")
		case storage.OutgoingSendCanceled:
			row.Label = i18n.T(ctx, "server.delivery."+kind+".canceled")
			row.Note = i18n.T(ctx, "server.msg.this_queued_delivery_was_canceled")
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
			row.Label = i18n.T(ctx, "server.msg.processing_guest_reply")
			row.Note = i18n.T(ctx, "server.msg.verifying_the_sender_and_current_meeting_before")
		case "verification_pending":
			row.Label = i18n.T(ctx, "server.msg.reply_verification_waiting_to_retry")
			row.Note = i18n.T(ctx, "server.msg.sender_verification_is_temporarily_unavailable_the_guest")
		case "unverified":
			row.Label = i18n.T(ctx, "server.msg.guest_reply_could_not_be_verified")
			row.Note = i18n.T(ctx, "server.msg.the_email_s_sender_could_not_be")
		case "retry":
			row.Label = i18n.T(ctx, "server.msg.guest_reply_waiting_to_retry")
			row.Note = i18n.T(ctx, "server.msg.the_calendar_update_could_not_be_confirmed")
		case "not_applied":
			row.Label = i18n.T(ctx, "server.msg.guest_reply_not_applied")
			row.Note = i18n.T(ctx, "server.msg.the_reply_is_outdated_the_meeting_changed")
		case "applied":
			row.Label = i18n.T(ctx, "server.msg.guest_reply_processed")
			row.Note = i18n.T(ctx, "server.msg.the_response_was_confirmed_on_the_calendar")
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
		http.Error(w, i18n.T(r.Context(), "server.error.could_not_load_event_delivery"), http.StatusInternalServerError)
		return e, false
	}
	return e, true
}

func (h *Handler) renderCalendarDelivery(w http.ResponseWriter, r *http.Request, e storage.CalendarEvent, note string) {
	data, err := h.calendarDeliveryData(r.Context(), e)
	if err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.could_not_load_event_delivery"), http.StatusInternalServerError)
		return
	}
	data.Error = serverText(r.Context(), note)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	details := views.CalendarEventDetails{Event: calendarViewEvent(r.Context(), e), Organizer: views.CalendarEventParticipant{Name: e.OrganizerName, Email: e.OrganizerEmail}, Attendees: calendarEventParticipants(e.AttendeesJSON), Delivery: &data}
	if err := views.CalendarEventGuests(details).Render(r.Context(), w); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.could_not_render_event_delivery"), http.StatusInternalServerError)
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
		http.Error(w, i18n.T(r.Context(), "server.error.could_not_load_invitation_delivery"), http.StatusInternalServerError)
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
