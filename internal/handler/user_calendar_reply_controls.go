package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (h *Handler) handleUserCalendarReplyStatus(w http.ResponseWriter, r *http.Request) {
	var job storage.CalendarReplyJob
	err := h.userIMAP.RunUserServiceWork(r.Context(), h.userID(r.Context()), func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotCalendarReplyControl(ctx, h.userID(ctx), r.PathValue("id"))
		if err != nil {
			return err
		}
		job = snapshot.Job()
		return nil
	})
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Render copied local history after releasing the user-store lease. Provider
	// configuration and selection are intentionally unnecessary for recovery.
	h.renderCalendarReply(w, r, job, "")
}

func (h *Handler) retryUserCalendarReply(ctx context.Context, control *config.UserCalendarReplyControlSnapshot, confirmed bool) error {
	job := control.Job()
	return h.userIMAP.RunAccountService(ctx, job.UserID, control.AccountID(), mail.AccountServiceCalendar, 30*time.Second, func(operation context.Context) error {
		unlock, err := h.lockCalendarCreate(operation, storage.CalendarSource{UserID: job.UserID, ID: job.SourceID})
		if err != nil {
			return err
		}
		defer unlock()
		authority, err := h.userAccounts.PrepareCalendarReplyRetry(operation, control, confirmed)
		if err != nil {
			return err
		}
		var payload calendarReplyPayload
		if json.Unmarshal([]byte(job.Payload), &payload) != nil {
			return storage.ErrCalendarEventChanged
		}
		p := &userCalendarRequest{h: h, event: authority.Event(), reply: authority}
		target, err := p.readResponseTarget(operation, payload.Scope)
		if err != nil {
			return err
		}
		send := authority.Send()
		if target.Endpoint != job.ResourceID || target.HTTPETag != job.Version || target.Scope != payload.Scope || target.SelfEmail != payload.SelfEmail || target.CalDAV == nil ||
			calendarReplyAddress(target.CalDAV.Selected.Props.Get("ORGANIZER").Value) != payload.Organizer || !strings.EqualFold(send.EnvelopeFrom, payload.SelfEmail) || len(send.EnvelopeRecipients) != 1 || send.EnvelopeRecipients[0] != payload.Organizer {
			return storage.ErrCalendarEventChanged
		}
		cfg, err := p.service().SMTPConfig(operation)
		if err != nil {
			return err
		}
		if cfg.AuthMethod != "plain" {
			return calendarReplyConfigurationError("Queued Calendar replies require this account's SMTP credentials.")
		}
		if _, err := p.service().SMTPPassword(); err != nil {
			return err
		}
		if err := p.validate(operation); err != nil {
			return err
		}
		return h.userAccounts.RequeueCalendarReply(operation, control, authority, confirmed)
	})
}

func (h *Handler) handleUserCalendarReplyAction(w http.ResponseWriter, r *http.Request) {
	var job storage.CalendarReplyJob
	var note string
	err := h.userIMAP.RunUserServiceWork(r.Context(), h.userID(r.Context()), func(ctx context.Context) error {
		control, err := h.userAccounts.SnapshotCalendarReplyControl(ctx, h.userID(ctx), r.PathValue("id"))
		if err != nil {
			return err
		}
		job = control.Job()
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		if r.URL.RawQuery != "" || r.ParseForm() != nil || len(r.PostForm) != 1 || len(r.PostForm["action"]) != 1 || (job.State != "pending" && job.State != "conflict") {
			return userCalendarFormFailure{http.StatusBadRequest, "Invalid reply action."}
		}
		action := r.PostForm.Get("action")
		switch action {
		case "retry", "resend-confirmed":
			confirmed := action == "resend-confirmed"
			if job.State != "pending" || (!confirmed && job.SendStatus != storage.OutgoingSendFailed) || (confirmed && job.SendStatus != storage.OutgoingSendAmbiguous) {
				return userCalendarFormFailure{http.StatusConflict, "This reply cannot be retried safely."}
			}
			err = h.retryUserCalendarReply(ctx, control, confirmed)
			if err == nil {
				err = h.userIMAP.QueueAccount(ctx, control.AccountID())
			}
		case "cancel", "confirm-sent", "dismiss":
			if action == "dismiss" && job.State != "conflict" {
				return userCalendarFormFailure{http.StatusConflict, "This notice is not ready to dismiss."}
			}
			err = h.userAccounts.ApplyCalendarReplyControl(ctx, control, action)
			if err == nil && action == "confirm-sent" {
				err = h.WakeUserCalendarAccount(ctx, job.UserID, control.AccountID())
			}
		default:
			return userCalendarFormFailure{http.StatusBadRequest, "Invalid reply action."}
		}
		if err != nil {
			note = "The reply status changed. Check its current state before trying again."
		}
		if current, readErr := h.userAccounts.SnapshotCalendarReplyControl(ctx, job.UserID, job.ID); readErr == nil {
			job = current.Job()
		}
		return nil
	})
	if err != nil {
		var failure userCalendarFormFailure
		if errors.As(err, &failure) {
			http.Error(w, failure.message, failure.status)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	h.renderCalendarReply(w, r, job, note)
}
