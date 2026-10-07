package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type userCalendarResponseFailure struct {
	status             int
	message            string
	uncertain, refresh bool
}

func (e userCalendarResponseFailure) Error() string { return e.message }

func (h *Handler) handleUserCalendarResponse(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if r.URL.RawQuery != "" || r.ParseForm() != nil || len(r.PostForm) != 3 || len(r.PostForm["scope"]) != 1 || len(r.PostForm["response"]) != 1 || len(r.PostForm["version"]) != 1 {
		calendarUpdateFailure(w, http.StatusBadRequest, "Invalid response form. Reopen the invitation.", false, false)
		return
	}
	scope, response, version := r.PostForm.Get("scope"), r.PostForm.Get("response"), r.PostForm.Get("version")
	if (response != "accepted" && response != "tentative" && response != "declined") || strings.TrimSpace(version) == "" || len(version) > 2048 {
		calendarUpdateFailure(w, http.StatusBadRequest, "Choose Accept, Maybe or Decline and reopen the invitation if its version is missing.", false, false)
		return
	}
	// Keep the accepted form operation alive if its browser disconnects, while
	// still joining runtime/account shutdown and bounding all provider work.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), calendarSourceTimeout)
	defer cancel()
	var event storage.CalendarEvent
	var source storage.CalendarSource
	var result calendarResponseResult
	var refreshPending bool
	attempt := &userCalendarResponseAttempt{}
	err := h.userIMAP.RunUserServiceWork(ctx, h.userID(ctx), func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotCalendarEvent(ctx, h.userID(ctx), r.PathValue("id"))
		if err != nil {
			return userCalendarResponseFailure{http.StatusNotFound, "This invitation is no longer available. Refresh the calendar.", false, true}
		}
		event, source = snapshot.Event(), snapshot.Source()
		if !calendarResponseScopeValid(event, scope) {
			return userCalendarResponseFailure{http.StatusBadRequest, "Choose This event or Entire series before responding.", false, false}
		}
		err = h.userIMAP.RunAccountService(ctx, source.UserID, source.AccountID, mail.AccountServiceCalendar, calendarSourceTimeout, func(operation context.Context) error {
			unlock, err := h.lockCalendarCreate(operation, source)
			if err != nil {
				return err
			}
			defer unlock()
			p := &userCalendarRequest{h: h, event: snapshot, responseAttempt: attempt}
			if err := p.responseAccess(operation); err != nil {
				return err
			}
			if source.Provider == storage.CalendarSourceProviderCalDAV {
				err := h.userAccounts.WithAccountForUser(operation, source.UserID, source.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
					_, err := db.UnresolvedCalendarReply(operation, source.UserID, source.ID, calendarReplyResource(event))
					return err
				})
				if !errors.Is(err, sql.ErrNoRows) {
					return userCalendarResponseFailure{http.StatusConflict, "An email reply is already being processed. Reopen this invitation to check delivery before responding again.", true, true}
				}
			}
			target, err := p.readResponseTarget(operation, scope)
			var configuration calendarReplyConfigurationError
			if errors.As(err, &configuration) {
				return userCalendarResponseFailure{http.StatusBadRequest, configuration.Error(), false, false}
			}
			if err != nil || target.Event.ETag != version || target.Event.ResponseStatus == "" || (scope != "series" && event.ETag != version) || (target.OccurrenceVersion != "" && target.OccurrenceVersion != event.ETag) {
				return userCalendarResponseFailure{http.StatusConflict, "This invitation changed or could not be verified. Reopen it before responding; nothing was sent.", false, true}
			}
			// Preflight already bound the write authorization and may have
			// advanced it through its own bounded 401 refresh. Keep that exact
			// authority for dispatch instead of looking up a fresh grant here.
			operation = context.WithValue(operation, userCalendarProviderKey{}, p)
			result = calendarResponseResult{Event: target.Event}
			if target.Event.ResponseStatus != response {
				claim, err := p.reserveResponse(operation, target, response)
				if err != nil {
					return userCalendarResponseFailure{http.StatusConflict, "A response may already have been sent for this version. Check it in your calendar provider, then refresh Gofer; it will not resend automatically.", true, true}
				}
				p.response = claim
				if target.CalDAV != nil {
					if target.CalDAV.ServerScheduling {
						desired, _, prepareErr := prepareCalDAVResponse(target, response)
						if prepareErr != nil {
							err = prepareErr
						} else {
							result, err = putCalDAVResponse(operation, target, desired, response)
						}
					} else {
						result, err = p.queueCalDAVReply(operation, claim, target, response)
					}
				} else {
					result, err = sendCalendarResponse(operation, source, event, target, response)
				}
				if err != nil {
					if !attempt.uncertain {
						_ = p.releaseResponse(operation, claim)
					}
					message := "Could not send your response. Check Calendar write access and try again."
					if attempt.uncertain {
						message = "The response could not be confirmed. Refresh the calendar and reopen the invitation before trying again."
					}
					if errors.As(err, &configuration) {
						message = configuration.Error()
					}
					if errors.Is(err, errCalendarUpdateConflict) && !attempt.uncertain {
						message = "This invitation changed. Reopen it before responding; nothing was sent."
					}
					return userCalendarResponseFailure{http.StatusBadGateway, message, attempt.uncertain, errors.Is(err, errCalendarUpdateConflict)}
				}
			}
			if err := p.validate(operation); err != nil {
				return userCalendarResponseFailure{http.StatusConflict, "Calendar access or settings changed. Refresh and reopen this invitation.", attempt.uncertain || result.Pending, true}
			}
			if !result.Pending && (result.Event.RemoteID != target.Event.RemoteID || result.Event.SeriesRemoteID != target.Event.SeriesRemoteID || result.Event.ResponseStatus != response || result.Event.ETag == "") {
				return userCalendarResponseFailure{http.StatusBadGateway, "The provider's response could not be confirmed. Refresh and reopen the invitation before trying again.", true, false}
			}
			refreshPending = result.Pending
			if !result.Pending && scope != "series" && source.Provider != storage.CalendarSourceProviderCalDAV {
				if err := p.publishEvent(operation, storage.CalendarPublishResponse, calendarStorageEvent(source.UserID, source.ID, result.Event)); err != nil {
					refreshPending = true
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		// These fresh purposes acquire their own profile/gate/flight only after
		// the original provider operation has released its locks.
		if result.Delivery == "email" {
			if err := h.userIMAP.QueueAccount(ctx, source.AccountID); err != nil {
				refreshPending = true
			}
		} else if scope == "series" || refreshPending || source.Provider == storage.CalendarSourceProviderCalDAV {
			refreshPending = !h.refreshUserCalendarEditedSeries(ctx, source, event)
		}
		h.userIMAP.Events().Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: source.UserID, AccountID: source.AccountID, Payload: map[string]any{"source_id": source.ID, "event_id": event.ID}})
		return nil
	})
	if err != nil {
		var failure userCalendarResponseFailure
		if errors.As(err, &failure) {
			calendarUpdateFailure(w, failure.status, failure.message, failure.uncertain, failure.refresh)
			return
		}
		var form userCalendarFormFailure
		if errors.As(err, &form) {
			calendarUpdateFailure(w, form.status, form.message, attempt.uncertain, false)
			return
		}
		calendarUpdateFailure(w, http.StatusServiceUnavailable, "Calendar access or settings changed. Refresh and reopen this invitation.", attempt.uncertain || result.Pending, true)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"responded": !result.Pending, "pending": result.Pending, "response": response, "event_id": event.ID, "scope": scope, "refresh_pending": refreshPending, "delivery": result.Delivery, "delivery_id": result.DeliveryID})
}

func (h *Handler) refreshUserCalendarEditedSeries(parent context.Context, source storage.CalendarSource, event storage.CalendarEvent) bool {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	var settings map[string]string
	if err := h.userAccounts.WithUser(ctx, source.UserID, func(_ *config.AccountStore, db *storage.DB) error {
		settings = db.GetUISettings(ctx, source.UserID)
		return nil
	}); err != nil {
		return false
	}
	location := viewsCalendarLocation(settings)
	anchor := time.Now()
	if event.AllDay {
		anchor, _ = time.ParseInLocation("2006-01-02", event.StartDate, location)
	} else if event.StartAt != nil {
		anchor = *event.StartAt
	}
	start, end := calendarBackgroundWindow(anchor, location)
	nowStart, nowEnd := calendarBackgroundWindow(time.Now(), location)
	windows := [][2]time.Time{{start, end}}
	if !nowStart.After(end) && !start.After(nowEnd) {
		if nowStart.Before(start) {
			windows[0][0] = nowStart
		}
		if nowEnd.After(end) {
			windows[0][1] = nowEnd
		}
	} else {
		windows = append(windows, [2]time.Time{nowStart, nowEnd})
	}
	success := true
	for _, window := range windows {
		if _, err := h.syncUserCalendarSource(ctx, source, window[0], window[1], false, true); err != nil {
			success = false
		}
	}
	return success
}
