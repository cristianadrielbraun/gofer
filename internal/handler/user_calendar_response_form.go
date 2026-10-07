package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (p *userCalendarRequest) responseAccess(ctx context.Context) error {
	if p == nil || p.event == nil {
		return storage.ErrCalendarEventChanged
	}
	event, source := p.event.Event(), p.event.Source()
	if reason := calendarResponseRestriction(event, source); reason != "" {
		return userCalendarFormFailure{http.StatusForbidden, reason}
	}
	if !p.h.userCalendarWriteAuthorized(ctx, source) {
		return userCalendarFormFailure{http.StatusForbidden, "Calendar response access could not be verified. Check this account's Calendar write access."}
	}
	return p.validate(ctx)
}

// Prepare a response with the real provider read path and the original private
// event/source authority. This is reusable by GET and the future guarded POST;
// callers inside this service gate use readResponseTarget directly.
func (p *userCalendarRequest) readResponse(ctx context.Context, scope string) (target calendarResponseTarget, err error) {
	source := p.event.Source()
	err = p.h.userIMAP.RunAccountService(ctx, source.UserID, source.AccountID, mail.AccountServiceCalendar, 30*time.Second, func(operation context.Context) error {
		unlock, err := p.h.lockCalendarCreate(operation, source)
		if err != nil {
			return err
		}
		defer unlock()
		target, err = p.readResponseTarget(operation, scope)
		return err
	})
	return target, err
}

func (p *userCalendarRequest) readResponseTarget(ctx context.Context, scope string) (calendarResponseTarget, error) {
	if err := p.responseAccess(ctx); err != nil {
		return calendarResponseTarget{}, err
	}
	event, source := p.event.Event(), p.event.Source()
	if !calendarResponseScopeValid(event, scope) {
		return calendarResponseTarget{}, userCalendarFormFailure{http.StatusBadRequest, "Choose This event or Entire series before responding."}
	}
	operation, err := p.actionContext(ctx, true)
	if err != nil {
		return calendarResponseTarget{}, err
	}
	credentials, err := p.actionCredentials()
	if err != nil {
		return calendarResponseTarget{}, err
	}
	var target calendarResponseTarget
	if source.Provider == storage.CalendarSourceProviderCalDAV {
		target, err = readCalDAVResponseTarget(operation, source, event, scope, credentials.username, credentials.password, p.service().Identity().EmailAddress)
		if err == nil && target.CalDAV != nil && !target.CalDAV.ServerScheduling && !p.service().SMTPConfigured() {
			return calendarResponseTarget{}, calendarReplyConfigurationError("This server needs replies by email. Configure SMTP for this account before responding.")
		}
	} else {
		target, err = readCalendarResponseTarget(operation, source, event, scope, credentials.token)
	}
	if err != nil {
		return calendarResponseTarget{}, err
	}
	if err := p.validate(operation); err != nil {
		return calendarResponseTarget{}, err
	}
	return target, nil
}

func (h *Handler) verifyUserMailInvitation(ctx context.Context, p *userCalendarRequest, mailID string) (*mail.RawMessage, error) {
	items, raw, _, err := h.readUserMailCalendarEvents(ctx, mailID)
	if err != nil || raw == nil || raw.AccountID() != p.event.Source().AccountID {
		return nil, userCalendarFormFailure{http.StatusForbidden, "This email does not match this account's invitation."}
	}
	for _, item := range items {
		if item.Method != "REQUEST" || !item.Invited || item.Cancelled || item.Organizer == "" {
			continue
		}
		matched, err := h.matchUserMailCalendarEvent(ctx, raw.AccountID(), item)
		if err != nil {
			return nil, err
		}
		if matched != nil && matched.Event().ID == p.event.Event().ID && matched.Source().ID == p.event.Source().ID {
			if err := p.validate(ctx); err != nil {
				return nil, err
			}
			if err := h.userIMAP.ValidateRawMessage(ctx, raw); err != nil {
				return nil, err
			}
			return raw, nil
		}
	}
	return nil, userCalendarFormFailure{http.StatusForbidden, "This email does not match this account's invitation."}
}

func (h *Handler) handleUserCalendarResponseForm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	var data views.CalendarResponseData
	var reply *views.CalendarReplyData
	var notInvitation bool
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	err := h.userIMAP.RunUserServiceWork(ctx, h.userID(ctx), func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotCalendarEvent(ctx, h.userID(ctx), r.PathValue("id"))
		if err != nil {
			return err
		}
		p := &userCalendarRequest{h: h, event: snapshot}
		event, source := snapshot.Event(), snapshot.Source()
		query, err := url.ParseQuery(r.URL.RawQuery)
		mailID := query.Get("mail_id")
		expected := 1
		if mailID != "" {
			expected++
		}
		editing, hasEditing := query["edit"]
		if hasEditing {
			expected++
		}
		if err != nil || len(query) != expected || len(query["scope"]) != 1 || (mailID != "" && len(query["mail_id"]) != 1) || (hasEditing && (mailID == "" || len(editing) != 1 || editing[0] != "1")) || !calendarResponseScopeValid(event, query.Get("scope")) {
			return userCalendarFormFailure{http.StatusBadRequest, "Choose This event or Entire series before responding."}
		}
		if err := p.responseAccess(ctx); err != nil {
			return err
		}
		var raw *mail.RawMessage
		if mailID != "" {
			raw, err = h.verifyUserMailInvitation(ctx, p, mailID)
			if err != nil {
				return err
			}
		}
		if source.Provider == storage.CalendarSourceProviderCalDAV {
			var job storage.CalendarReplyJob
			err = h.userAccounts.WithAccountForUser(ctx, source.UserID, source.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
				var err error
				job, err = db.UnresolvedCalendarReply(ctx, source.UserID, source.ID, calendarReplyResource(event))
				return err
			})
			if err == nil {
				var payload calendarReplyPayload
				if json.Unmarshal([]byte(job.Payload), &payload) != nil {
					return userCalendarFormFailure{http.StatusServiceUnavailable, "Could not check reply delivery."}
				}
				reply = &views.CalendarReplyData{ID: job.ID, EventID: payload.Event.ID, Scope: payload.Scope, State: job.State, SendStatus: job.SendStatus}
				if err := p.validate(ctx); err != nil {
					return err
				}
				if raw != nil {
					return h.userIMAP.ValidateRawMessage(ctx, raw)
				}
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return userCalendarFormFailure{http.StatusServiceUnavailable, "Could not check reply delivery."}
			}
		}
		target, err := p.readResponse(ctx, query.Get("scope"))
		if errors.Is(err, errCalendarNotInvitation) {
			notInvitation = true
			return nil
		}
		if err != nil {
			var configuration calendarReplyConfigurationError
			if errors.As(err, &configuration) {
				return userCalendarFormFailure{http.StatusConflict, configuration.Error()}
			}
			var failure userCalendarFormFailure
			if errors.As(err, &failure) {
				return err
			}
			if errors.Is(err, storage.ErrCalendarEventChanged) || errors.Is(err, storage.ErrCalendarSourceChanged) || errors.Is(err, config.ErrAccountServicesChanged) {
				return userCalendarFormFailure{http.StatusConflict, "Calendar changed. Refresh and reopen the invitation."}
			}
			return userCalendarFormFailure{http.StatusConflict, "Could not verify this invitation. Refresh the calendar and try again."}
		}
		if (target.OccurrenceVersion != "" && target.OccurrenceVersion != event.ETag) || (query.Get("scope") != "series" && target.Event.ETag != event.ETag) {
			return userCalendarFormFailure{http.StatusConflict, "This invitation changed. Refresh the calendar and reopen it before responding."}
		}
		data = calendarResponseData(event)
		data.MailMessageID = mailID
		data.MailResponseEditing = hasEditing
		data.Status, data.Version, data.Scope, data.Ready = target.Event.ResponseStatus, target.Event.ETag, target.Scope, true
		if target.CalDAV != nil {
			data.Delivery = "server"
			if !target.CalDAV.ServerScheduling {
				data.Delivery = "email"
			}
		}
		if err := p.validate(ctx); err != nil {
			return err
		}
		if raw != nil {
			return h.userIMAP.ValidateRawMessage(ctx, raw)
		}
		return nil
	})
	if err != nil {
		userCalendarFormError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if notInvitation {
		return
	}
	if reply != nil {
		err = views.CalendarReplyStatus(*reply).Render(ctx, w)
	} else if data.MailMessageID != "" {
		err = views.MailCalendarResponseForm(data).Render(ctx, w)
	} else {
		err = views.CalendarResponseForm(data).Render(ctx, w)
	}
	if err != nil {
		http.Error(w, "Could not open invitation response controls.", http.StatusInternalServerError)
	}
}
