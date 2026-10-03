package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

type calendarReplyPayload struct {
	Event                                 storage.CalendarEvent
	Target                                calendar.RemoteEvent
	Scope, SelfEmail, Organizer, Calendar string
}

type calendarReplyConfigurationError string

func (e calendarReplyConfigurationError) Error() string { return string(e) }

func calendarReplyResource(event storage.CalendarEvent) string {
	if event.SeriesRemoteID != "" {
		return event.SeriesRemoteID
	}
	return event.RemoteID
}

func (h *Handler) calendarReplyAccount(ctx context.Context, source storage.CalendarSource) (models.Account, error) {
	accounts, err := h.db.GetAccounts(ctx, source.UserID)
	if err != nil {
		return models.Account{}, err
	}
	for _, account := range accounts {
		if account.ID == source.AccountID && !account.IsDeleting {
			return account, nil
		}
	}
	return models.Account{}, sql.ErrNoRows
}

func (h *Handler) queueCalDAVResponse(ctx context.Context, source storage.CalendarSource, event storage.CalendarEvent, target calendarResponseTarget, response string) (calendarResponseResult, error) {
	fail := func(err error) (calendarResponseResult, error) {
		return calendarResponseResult{}, calendarDeletePreflightError{err}
	}
	account, err := h.calendarReplyAccount(ctx, source)
	if err != nil || !strings.EqualFold(account.Email, target.SelfEmail) {
		return fail(calendarReplyConfigurationError("Email replies require this account's sending address to match the invited attendee."))
	}
	cfg, err := h.accountStore.GetConfig(ctx, source.AccountID)
	if err != nil || cfg.SMTPHost == "" || cfg.SMTPPort <= 0 {
		return fail(calendarReplyConfigurationError("This server needs replies by email. Configure SMTP for this account before responding."))
	}
	desired, reply, err := prepareCalDAVResponse(target, response)
	if err != nil {
		return fail(err)
	}
	body, err := encodeCalendarReply(desired)
	if err != nil {
		return fail(err)
	}
	organizer := calendarReplyAddress(target.CalDAV.Selected.Props.Get("ORGANIZER").Value)
	label := map[string]string{"accepted": "Accepted", "tentative": "Tentative", "declined": "Declined"}[response]
	msg := &message.OutgoingMessage{FromName: account.Name, FromEmail: account.Email, To: []*mail.Address{{Address: organizer}},
		Subject: label + ": " + strings.NewReplacer("\r", " ", "\n", " ").Replace(target.Event.Summary), TextBody: "My response to this invitation: " + response + ".",
		CalendarReply: reply, MessageID: message.NewMessageID(), Date: time.Now().UTC()}
	raw, err := buildOutgoingMIME(storage.OutgoingTransportSMTP, msg)
	if err != nil {
		return fail(err)
	}
	snapshot, err := json.Marshal(snapshotOutgoingMessage(msg))
	if err != nil {
		return fail(err)
	}
	payload, err := json.Marshal(calendarReplyPayload{Event: event, Target: target.Event, Scope: target.Scope, SelfEmail: target.SelfEmail, Organizer: organizer, Calendar: body})
	if err != nil {
		return fail(err)
	}
	id, err := h.db.QueueCalendarReply(ctx, storage.CalendarReplyJob{UserID: source.UserID, SourceID: source.ID, ResourceID: target.Endpoint, RemoteID: target.Event.RemoteID, Version: target.HTTPETag, Response: response, Payload: string(payload)}, storage.QueueOutgoingSendInput{AccountID: source.AccountID, Transport: storage.OutgoingTransportSMTP, EnvelopeFrom: msg.FromEmail, EnvelopeRecipients: []string{organizer}, MIMEData: raw, MessageJSON: snapshot})
	if err != nil {
		return fail(err)
	}
	h.signalOutgoingWorker()
	return calendarResponseResult{Event: target.Event, Pending: true, Delivery: "email", DeliveryID: id}, nil
}

// Run immediately before SMTP, including every safe retry. Canceled, stale or
// reconfigured invitations must not be sent just because they were queued.
func (h *Handler) beforeCalendarReplySend(ctx context.Context, send storage.OutgoingSend, msg *message.OutgoingMessage) (func(), error) {
	noop := func() {}
	if msg.CalendarReply == "" {
		return noop, nil
	}
	job, err := h.db.CalendarReplyForSend(ctx, send.ID)
	if err != nil || job.State != "pending" {
		return noop, fmt.Errorf("calendar reply is no longer active")
	}
	var payload calendarReplyPayload
	if json.Unmarshal([]byte(job.Payload), &payload) != nil {
		return noop, fmt.Errorf("invalid calendar reply")
	}
	unlock, err := h.lockCalendarCreate(ctx, storage.CalendarSource{ID: job.SourceID, UserID: job.UserID})
	if err != nil {
		return noop, markOutgoingSendRetryable(fmt.Errorf("calendar is busy"))
	}
	source, err := h.calendarResponseAccess(ctx, payload.Event)
	if err != nil {
		return unlock, fmt.Errorf("calendar reply access is no longer available")
	}
	account, err := h.calendarReplyAccount(ctx, source)
	if err != nil || source.AccountID != send.AccountID || !strings.EqualFold(account.Email, payload.SelfEmail) || !strings.EqualFold(send.EnvelopeFrom, account.Email) || len(send.EnvelopeRecipients) != 1 || send.EnvelopeRecipients[0] != payload.Organizer {
		return unlock, fmt.Errorf("calendar reply sending identity changed")
	}
	target, err := h.readCalendarResponse(ctx, source, payload.Event, payload.Scope)
	if err != nil || target.Endpoint != job.ResourceID || target.HTTPETag != job.Version || target.SelfEmail != payload.SelfEmail || target.CalDAV == nil || calendarReplyAddress(target.CalDAV.Selected.Props.Get("ORGANIZER").Value) != payload.Organizer {
		return unlock, fmt.Errorf("the invitation changed or could not be verified; refresh it before replying")
	}
	return unlock, nil
}

// Only the calendar write is retried here, never the email. After interruption,
// read back the desired resource before attempting another conditional PUT.
func (h *Handler) runCalendarReplyWorker(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		h.runCalendarReplyFollowups(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *Handler) runCalendarReplyFollowups(ctx context.Context) {
	jobs, err := h.db.ListCalendarReplyFollowups(ctx)
	if err != nil {
		log.Printf("calendar-reply: list follow-ups: %v", err)
		return
	}
	for _, job := range jobs {
		h.finishCalendarReply(ctx, job)
	}
}

func (h *Handler) finishCalendarReply(parent context.Context, job storage.CalendarReplyJob) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	unlock, err := h.lockCalendarCreate(ctx, storage.CalendarSource{ID: job.SourceID, UserID: job.UserID})
	if err != nil {
		return
	}
	defer unlock()
	if err := h.db.AttemptCalendarReply(ctx, job.UserID, job.ID); err != nil {
		return
	}
	var payload calendarReplyPayload
	state := "pending"
	var source storage.CalendarSource
	if json.Unmarshal([]byte(job.Payload), &payload) != nil {
		state = "conflict"
	} else {
		var accessErr error
		source, accessErr = h.calendarResponseAccess(ctx, payload.Event)
		if accessErr != nil {
			state = "conflict"
		} else {
			credentials := h.calendarCredentialsForSource(ctx, job.UserID, source)
			if credentials.err != nil {
				return
			}
			endpoint, endpointErr := calDAVResponseEndpoint(source, job.ResourceID)
			if _, err := resolveCalDAVHref(credentials.baseURL, source.RemoteID); err != nil {
				endpointErr = err
			}
			desired, decodeErr := calendarUpdateDecodeICS([]byte(payload.Calendar))
			if endpointErr != nil || decodeErr != nil {
				state = "conflict"
			} else {
				current, headers, readErr := calendarUpdateCalDAVGet(ctx, calendarDeleteClient(calDAVHTTPTransport), endpoint, credentials.username, credentials.password)
				if readErr != nil {
					if errors.Is(readErr, errCalendarUpdateConflict) || !calendarCreateUncertain(readErr) {
						state = "conflict"
					} else {
						return
					}
				} else if calDAVResponseResourceMatches(desired, current) {
					state = "complete"
				} else if headers.Get("ETag") != job.Version {
					state = "conflict"
				} else {
					target := calendarResponseTarget{Event: payload.Target, Scope: payload.Scope, Endpoint: endpoint, HTTPETag: job.Version, SelfEmail: payload.SelfEmail, CalDAV: &calDAVResponseTarget{Username: credentials.username, Password: credentials.password, ScheduleTag: headers.Get("Schedule-Tag")}}
					_, err := putCalDAVResponse(ctx, target, desired, job.Response)
					if err == nil {
						state = "complete"
					} else if errors.Is(err, errCalendarUpdateConflict) || !calendarCreateUncertain(err) {
						state = "conflict"
					}
				}
			}
		}
	}
	if state == "pending" {
		return
	}
	// Persist the delivery result before the optional cache refresh. A slow
	// REPORT must not turn a confirmed calendar save into endless pending work.
	writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	err = h.db.FinishCalendarReply(writeCtx, job.UserID, job.ID, state)
	writeCancel()
	if err != nil {
		return
	}
	if state == "complete" {
		h.refreshCalendarEditedSeries(ctx, source, payload.Event)
	}
	if h.syncer != nil {
		h.syncer.Events().Publish(mailpkg.Event{Type: mailpkg.EventCalendarChanged, UserID: job.UserID, Payload: map[string]any{"source_id": job.SourceID}})
	}
}

func (h *Handler) renderCalendarReply(w http.ResponseWriter, r *http.Request, job storage.CalendarReplyJob, note string) {
	var payload calendarReplyPayload
	_ = json.Unmarshal([]byte(job.Payload), &payload)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	_ = views.CalendarReplyStatus(views.CalendarReplyData{ID: job.ID, EventID: payload.Event.ID, Scope: payload.Scope, State: job.State, SendStatus: job.SendStatus, Note: note}).Render(r.Context(), w)
}

func (h *Handler) handleCalendarReplyStatus(w http.ResponseWriter, r *http.Request) {
	job, err := h.db.GetCalendarReply(r.Context(), h.userID(r.Context()), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h.renderCalendarReply(w, r, job, "")
}

func (h *Handler) handleCalendarReplyAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	job, err := h.db.GetCalendarReply(ctx, h.userID(ctx), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if r.URL.RawQuery != "" || r.ParseForm() != nil || len(r.PostForm) != 1 || len(r.PostForm["action"]) != 1 || (job.State != "pending" && job.State != "conflict") {
		http.Error(w, "Invalid reply action.", http.StatusBadRequest)
		return
	}
	switch r.PostForm.Get("action") {
	case "retry", "resend-confirmed":
		confirmed := r.PostForm.Get("action") == "resend-confirmed"
		if job.State != "pending" || (!confirmed && job.SendStatus != storage.OutgoingSendFailed) || (confirmed && job.SendStatus != storage.OutgoingSendAmbiguous) {
			http.Error(w, "This reply cannot be retried safely.", http.StatusConflict)
			return
		}
		_, err = h.db.RetryOutgoingSend(ctx, job.UserID, job.ID, confirmed)
		if err == nil {
			h.signalOutgoingWorker()
		}
	case "cancel":
		err = h.db.CancelCalendarReply(ctx, job.UserID, job.ID)
	case "confirm-sent":
		err = h.db.ConfirmCalendarReplySent(ctx, job.UserID, job.ID)
		if err == nil {
			h.signalOutgoingWorker()
		}
	case "dismiss":
		if job.State != "conflict" {
			http.Error(w, "This notice is not ready to dismiss.", http.StatusConflict)
			return
		}
		err = h.db.DismissCalendarReplyConflict(ctx, job.UserID, job.ID)
	default:
		http.Error(w, "Invalid reply action.", http.StatusBadRequest)
		return
	}
	note := ""
	if err != nil {
		note = "The reply status changed. Check its current state before trying again."
	}
	if current, readErr := h.db.GetCalendarReply(ctx, job.UserID, job.ID); readErr == nil {
		job = current
	}
	h.renderCalendarReply(w, r, job, note)
}
