package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Reserve/replay uses a local grant snapshot. Refreshing an expired token is
// necessary only if this durable request still needs native provider work.
func (p *userCalendarRequest) createContext(ctx context.Context) (context.Context, error) {
	if p == nil || p.source == nil {
		return nil, storage.ErrCalendarSourceChanged
	}
	p.write = true
	if err := p.validate(ctx); err != nil {
		return nil, err
	}
	if p.provider() != storage.CalendarSourceProviderCalDAV {
		if p.h.userCredentials == nil {
			return nil, mailauth.ErrMailboxAuthorizationChanged
		}
		service := p.service()
		p.credentials = p.h.userCredentials.CalendarAccount(service.OwnerID(), service.AccountID(), true)
		var err error
		p.authorization, err = p.h.userCredentials.SnapshotCalendarAuthorization(ctx, service.OwnerID(), service.AccountID(), p.provider(), true)
		if err != nil {
			return nil, err
		}
	}
	if err := p.validate(ctx); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, userCalendarProviderKey{}, p), nil
}
func (p *userCalendarRequest) prepareCreateDispatch(ctx context.Context) error {
	if err := p.ready(ctx); err != nil {
		return err
	}
	if p.provider() != storage.CalendarSourceProviderCalDAV {
		authorization, err := p.h.userCredentials.EnsureServiceAuthorization(ctx, p.authorization)
		if err != nil {
			return p.recordRetry(ctx, err)
		}
		p.authorization, p.token = authorization, authorization.Token()
	}
	return p.ready(ctx)
}

func (h *Handler) handleUserCreateCalendarEvent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 384<<10)
	if r.URL.RawQuery != "" || r.ParseForm() != nil {
		calendarCreateFailure(w, 400, "Event details could not be read.", false)
		return
	}
	r.Form = r.PostForm
	draft, err := parseCalendarEventDraft(r)
	if err != nil {
		calendarCreateFailure(w, 400, err.Error(), false)
		return
	}
	sourceID := r.PostForm.Get("source_id")
	if sourceID == "" || len(sourceID) > 4096 {
		calendarCreateFailure(w, 400, "Choose a configured calendar.", false)
		return
	}
	owner := h.userID(r.Context())
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), calendarSourceTimeout)
	defer cancel()
	attempt := &userCalendarResponseAttempt{}
	var response map[string]any
	err = h.userIMAP.RunUserServiceWork(ctx, owner, func(ctx context.Context) error {
		selected, err := h.userAccounts.SnapshotCalendarSource(ctx, owner, sourceID)
		if err != nil {
			return err
		}
		source := selected.Source()
		if draft.GoogleMeetMeeting && source.Provider != "gmail" {
			return userCalendarFormFailure{400, "Google Meet meetings require a supported Google calendar."}
		}
		if draft.TeamsMeeting && source.Provider != "outlook" {
			return userCalendarFormFailure{400, "Teams meetings require a supported Outlook calendar."}
		}
		if !calendarSourceWritable(source) {
			return userCalendarFormFailure{403, "This calendar is read-only. Choose a writable calendar."}
		}
		if !h.userCalendarWriteAuthorized(ctx, source) {
			return userCalendarFormFailure{403, "This account has read-only Calendar access. Reconnect it from Accounts to grant event creation permission."}
		}
		p := &userCalendarRequest{h: h, source: selected, writeAttempt: attempt}
		if len(draft.Guests) > 0 {
			identity := selected.Service().Identity()
			if calendarReplyAddress("mailto:"+identity.EmailAddress) == "" {
				return userCalendarFormFailure{400, "A valid account sending address is required to invite guests."}
			}
			draft.OrganizerEmail, draft.OrganizerName = strings.ToLower(identity.EmailAddress), identity.DisplayName
			for _, guest := range draft.Guests {
				if strings.EqualFold(guest.Email, identity.EmailAddress) {
					return userCalendarFormFailure{400, "You are already the organizer; add other people as guests."}
				}
			}
		}
		hash := calendarDraftHash(draft)
		var result storage.CalendarCreateRequest
		var replayed bool
		err = h.userIMAP.RunAccountService(ctx, owner, source.AccountID, mail.AccountServiceCalendar, calendarSourceTimeout, func(operation context.Context) error {
			unlock, err := h.lockCalendarCreate(operation, source)
			if err != nil {
				return err
			}
			defer unlock()
			operation, err = p.createContext(operation)
			if err != nil {
				return err
			}
			claim, err := p.beginCreate(operation, draft.RequestID, hash)
			if err != nil {
				return err
			}
			result = claim.Result()
			replayed = result.RemoteID != ""
			if !replayed {
				if err := p.prepareCreateDispatch(operation); err != nil {
					return err
				}
				if err := h.attachCalendarTeamsDraft(operation, source, &draft); err != nil {
					return userCalendarFormFailure{409, "The Teams link could not be loaded. Toggle Teams off and on to prepare it again."}
				}
				if err := h.attachCalendarMeetDraft(operation, source, &draft, "create:"+draft.RequestID); err != nil {
					return userCalendarFormFailure{409, "The Google Meet link could not be loaded. Toggle Google Meet off and on to prepare it again."}
				}
				remote, err := h.createCalendarProviderEvent(operation, source, draft)
				if err != nil {
					message := "Could not create the event. Check the calendar permissions and try again."
					var provider calendarCreateProviderError
					var configuration calendarReplyConfigurationError
					if errors.As(err, &configuration) || errors.Is(err, errCalendarUpdateUnsupported) {
						message = err.Error()
					}
					if errors.As(err, &provider) && (provider.Status == 401 || provider.Status == 403) {
						message = "Calendar write access was denied. Reconnect OAuth accounts from Accounts, or check your CalDAV permissions."
					}
					if attempt.uncertain {
						message = "The provider's response could not be confirmed. Retry this same event to check without creating a duplicate."
					}
					return userCalendarMutationFailure{status: 502, message: message, uncertain: attempt.uncertain}
				}
				if draft.Recurrence != nil && (remote.Deleted || remote.SeriesRemoteID != "" || !calendarUpdateHasDetails(remote.Recurrence)) {
					return userCalendarMutationFailure{status: 502, message: "The provider did not confirm the series. Retry this same event to verify it safely.", uncertain: true}
				}
				result, err = p.publishCreate(operation, calendarStorageEvent(owner, source.ID, remote), draft.Recurrence != nil)
				if err != nil {
					return userCalendarMutationFailure{status: 503, message: "The provider created the event, but the local update could not finish. Retry this same event to recover it without a duplicate.", uncertain: true}
				}
			}
			// Publication consumed the old pending claim. A completed private claim
			// verifies the accepted target before best-effort Teams state finalization.
			p.create = nil
			if draft.TeamsDraftID != "" && source.Provider == "outlook" {
				if completed, err := p.beginCreate(operation, draft.RequestID, hash); err == nil {
					_ = p.finishTeamsCreate(operation, draft.TeamsDraftID, completed)
				}
			}
			response = map[string]any{"event_id": result.EventID, "source_id": source.ID, "replayed": replayed, "hidden": source.IsHidden, "notify_guests": len(draft.Guests) > 0}
			if draft.TeamsMeeting || draft.GoogleMeetMeeting {
				var saved storage.CalendarEvent
				readErr := h.userAccounts.WithAccountForUser(operation, owner, source.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
					var err error
					saved, err = db.GetCalendarEvent(operation, owner, result.EventID)
					return err
				})
				if draft.TeamsMeeting {
					response["teams_unconfirmed"] = readErr != nil || !calendarTeamsLinkConfirmed(calendarOutlookCachedMeetingJSON(saved))
				}
				if draft.GoogleMeetMeeting {
					response["google_meet_unconfirmed"] = readErr != nil || !calendarGoogleMeetConfirmed(json.RawMessage(saved.OnlineMeetingJSON))
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if draft.Recurrence != nil {
			response["series_id"] = result.RemoteID
			response["refresh_pending"] = !h.refreshUserCalendarCreatedSeries(ctx, source, draft)
		} else if !replayed {
			h.userIMAP.Events().Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: owner, AccountID: source.AccountID, Payload: map[string]any{"source_id": source.ID, "event_id": result.EventID}})
		}
		return nil
	})
	if err != nil {
		var failure userCalendarMutationFailure
		var form userCalendarFormFailure
		switch {
		case errors.As(err, &failure):
			calendarCreateFailure(w, failure.status, failure.message, failure.uncertain)
		case errors.As(err, &form):
			calendarCreateFailure(w, form.status, form.message, attempt.uncertain)
		case errors.Is(err, sql.ErrNoRows):
			calendarCreateFailure(w, 404, "This calendar is no longer configured.", attempt.uncertain)
		case errors.Is(err, storage.ErrCalendarCreateConflict), errors.Is(err, storage.ErrCalendarSourceChanged), errors.Is(err, config.ErrAccountServicesChanged), errors.Is(err, mailauth.ErrMailboxAuthorizationChanged):
			calendarCreateFailure(w, 409, "Calendar configuration or this event request changed. Reopen New event.", attempt.uncertain)
		default:
			calendarCreateFailure(w, 503, "Calendar is unavailable. Retry this same event when access is restored.", attempt.uncertain)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(response)
}
func (h *Handler) refreshUserCalendarCreatedSeries(ctx context.Context, source storage.CalendarSource, draft calendar.EventDraft) bool {
	event := storage.CalendarEvent{AllDay: draft.AllDay, StartDate: draft.StartDate, StartAt: draft.StartAt}
	return h.refreshUserCalendarEditedSeries(ctx, source, event)
}
