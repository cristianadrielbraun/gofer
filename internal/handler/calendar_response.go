package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func calendarResponseData(event storage.CalendarEvent) views.CalendarResponseData {
	data := views.CalendarResponseData{EventID: event.ID, Status: event.ResponseStatus, Version: event.ETag, Scope: "event", Ready: event.ResponseStatus != "" && event.ETag != ""}
	if event.SeriesRemoteID != "" {
		data.Scope, data.HasOccurrence = "occurrence", true
	} else if calendarEventIsSeries(event) {
		data.Scope, data.Ready = "series", false
	}
	return data
}

func calendarResponseScopeValid(event storage.CalendarEvent, scope string) bool {
	if !calendarEventIsSeries(event) {
		return scope == "event"
	}
	return scope == "series" || (scope == "occurrence" && event.SeriesRemoteID != "")
}

func (h *Handler) calendarResponseAccess(ctx context.Context, event storage.CalendarEvent) (storage.CalendarSource, error) {
	if event.IsDeleted || event.Status == "cancelled" {
		return storage.CalendarSource{}, fmt.Errorf("this invitation is no longer active")
	}
	sources, err := h.db.ListSelectedCalendarSources(ctx, event.UserID)
	if err != nil {
		return storage.CalendarSource{}, err
	}
	for _, source := range sources {
		if source.ID != event.SourceID {
			continue
		}
		if source.Provider != providers.ProviderGmail && source.Provider != providers.ProviderOutlook && source.Provider != storage.CalendarSourceProviderCalDAV {
			return source, fmt.Errorf("this calendar provider does not support invitation responses")
		}
		if !calendarSourceWritable(source) {
			return source, fmt.Errorf("this calendar is read-only")
		}
		if !h.calendarWriteAuthorized(ctx, source) {
			return source, fmt.Errorf("reconnect this account from Accounts to grant Calendar write access")
		}
		return source, nil
	}
	return storage.CalendarSource{}, sql.ErrNoRows
}

func (h *Handler) readCalendarResponse(ctx context.Context, source storage.CalendarSource, event storage.CalendarEvent, scope string) (calendarResponseTarget, error) {
	if h.calendarReadResponse != nil {
		return h.calendarReadResponse(ctx, source, event, scope)
	}
	if source.Provider == storage.CalendarSourceProviderCalDAV {
		credentials := h.calendarCredentialsForSource(ctx, source.UserID, source)
		if credentials.err != nil {
			return calendarResponseTarget{}, credentials.err
		}
		if _, err := resolveCalDAVHref(credentials.baseURL, source.RemoteID); err != nil {
			return calendarResponseTarget{}, err
		}
		account, err := h.calendarReplyAccount(ctx, source)
		if err != nil {
			return calendarResponseTarget{}, err
		}
		target, err := readCalDAVResponseTarget(ctx, source, event, scope, credentials.username, credentials.password, account.Email)
		if err == nil && !target.CalDAV.ServerScheduling {
			cfg, cfgErr := h.accountStore.GetConfig(ctx, source.AccountID)
			if cfgErr != nil || cfg.SMTPHost == "" || cfg.SMTPPort <= 0 {
				return calendarResponseTarget{}, calendarReplyConfigurationError("This server needs replies by email. Configure SMTP for this account before responding.")
			}
		}
		return target, err
	}
	var token string
	var err error
	if source.Provider == providers.ProviderGmail {
		token, err = h.mailCredentials().GetGoogleCalendarWriteTokenForAccount(ctx, source.AccountID)
	} else {
		token, err = h.mailCredentials().GetMicrosoftGraphCalendarWriteTokenForAccount(ctx, source.AccountID)
	}
	if err != nil {
		return calendarResponseTarget{}, calendarCreateAuthError{err}
	}
	return readCalendarResponseTarget(ctx, source, event, scope, token)
}

func (h *Handler) handleCalendarResponseForm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	event, err := h.db.GetCalendarEvent(ctx, h.userID(ctx), r.PathValue("id"))
	if err != nil {
		http.Error(w, "This event is unavailable. Refresh the calendar.", http.StatusNotFound)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	mailID := query.Get("mail_id")
	expectedFields := 1
	if mailID != "" {
		expectedFields = 2
	}
	editing, hasEditing := query["edit"]
	if hasEditing {
		expectedFields++
	}
	if err != nil || len(query) != expectedFields || len(query["scope"]) != 1 || (mailID != "" && len(query["mail_id"]) != 1) || (hasEditing && (mailID == "" || len(editing) != 1 || editing[0] != "1")) || !calendarResponseScopeValid(event, query.Get("scope")) {
		http.Error(w, "Choose This event or Entire series before responding.", http.StatusBadRequest)
		return
	}
	source, err := h.calendarResponseAccess(ctx, event)
	if err != nil {
		http.Error(w, "Calendar response access could not be verified. Check this account's Calendar write access.", http.StatusForbidden)
		return
	}
	if mailID != "" {
		items, accountID, err := h.readMailCalendarEvents(ctx, mailID)
		verified := false
		if err == nil && accountID == source.AccountID {
			for _, item := range items {
				if item.Method != "REQUEST" || !item.Invited || item.Cancelled || item.Organizer == "" {
					continue
				}
				if matched, ok := h.matchMailCalendarEvent(ctx, accountID, item); ok && matched.ID == event.ID {
					verified = true
					break
				}
			}
		}
		if !verified {
			http.Error(w, "This email does not match this account's invitation.", http.StatusForbidden)
			return
		}
	}
	if source.Provider == storage.CalendarSourceProviderCalDAV {
		job, err := h.db.UnresolvedCalendarReply(ctx, event.UserID, source.ID, calendarReplyResource(event))
		if err == nil {
			h.renderCalendarReply(w, r, job, "")
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "Could not check reply delivery.", http.StatusServiceUnavailable)
			return
		}
	}
	target, err := h.readCalendarResponse(ctx, source, event, query.Get("scope"))
	if errors.Is(err, errCalendarNotInvitation) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		return // An older cache may not yet distinguish an organizer's own event.
	}
	if err != nil {
		message := "Could not verify this invitation. Refresh the calendar and try again."
		var configuration calendarReplyConfigurationError
		if errors.As(err, &configuration) {
			message = configuration.Error()
		}
		http.Error(w, message, http.StatusConflict)
		return
	}
	if (target.OccurrenceVersion != "" && target.OccurrenceVersion != event.ETag) || (query.Get("scope") != "series" && target.Event.ETag != event.ETag) {
		http.Error(w, "This invitation changed. Refresh the calendar and reopen it before responding.", http.StatusConflict)
		return
	}
	data := calendarResponseData(event)
	data.MailMessageID = mailID
	data.MailResponseEditing = hasEditing
	data.Status, data.Version, data.Scope, data.Ready = target.Event.ResponseStatus, target.Event.ETag, target.Scope, true
	if target.CalDAV != nil {
		data.Delivery = "server"
		if !target.CalDAV.ServerScheduling {
			data.Delivery = "email"
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if mailID != "" {
		if err := views.MailCalendarResponseForm(data).Render(ctx, w); err != nil {
			http.Error(w, "Could not open invitation response controls.", http.StatusInternalServerError)
		}
		return
	}
	if err := views.CalendarResponseForm(data).Render(ctx, w); err != nil {
		http.Error(w, "Could not open invitation response controls.", http.StatusInternalServerError)
	}
}

func (h *Handler) handleCalendarResponse(w http.ResponseWriter, r *http.Request) {
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
	userID := h.userID(r.Context())
	event, err := h.db.GetCalendarEvent(r.Context(), userID, r.PathValue("id"))
	if err != nil {
		calendarUpdateFailure(w, http.StatusNotFound, "This invitation is no longer available. Refresh the calendar.", false, true)
		return
	}
	if !calendarResponseScopeValid(event, scope) {
		calendarUpdateFailure(w, http.StatusBadRequest, "Choose This event or Entire series before responding.", false, false)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), calendarSourceTimeout)
	defer cancel()
	unlock, err := h.lockCalendarCreate(ctx, storage.CalendarSource{ID: event.SourceID, UserID: userID})
	if err != nil {
		calendarUpdateFailure(w, http.StatusServiceUnavailable, "Calendar is still refreshing. Try again.", false, false)
		return
	}
	defer unlock()
	event, err = h.db.GetCalendarEvent(ctx, userID, event.ID)
	if err != nil || !calendarResponseScopeValid(event, scope) {
		calendarUpdateFailure(w, http.StatusConflict, "This invitation changed. Refresh and reopen it.", false, true)
		return
	}
	source, err := h.calendarResponseAccess(ctx, event)
	if err != nil {
		calendarUpdateFailure(w, http.StatusForbidden, "Responding is unavailable. Check this account's Calendar write access.", false, false)
		return
	}
	if source.Provider == storage.CalendarSourceProviderCalDAV {
		_, pendingErr := h.db.UnresolvedCalendarReply(ctx, event.UserID, source.ID, calendarReplyResource(event))
		if !errors.Is(pendingErr, sql.ErrNoRows) {
			calendarUpdateFailure(w, http.StatusConflict, "An email reply is already being processed. Reopen this invitation to check delivery before responding again.", true, true)
			return
		}
	}
	target, err := h.readCalendarResponse(ctx, source, event, scope)
	var configuration calendarReplyConfigurationError
	if errors.As(err, &configuration) {
		calendarUpdateFailure(w, http.StatusBadRequest, configuration.Error(), false, false)
		return
	}
	if err != nil || target.Event.ETag != version || target.Event.ResponseStatus == "" || (scope != "series" && event.ETag != version) || (target.OccurrenceVersion != "" && target.OccurrenceVersion != event.ETag) {
		calendarUpdateFailure(w, http.StatusConflict, "This invitation changed or could not be verified. Reopen it before responding; nothing was sent.", false, true)
		return
	}
	result := calendarResponseResult{Event: target.Event}
	if target.Event.ResponseStatus != response {
		if err := h.db.BeginCalendarResponse(ctx, userID, source.ID, target.Event.RemoteID, version, response); err != nil {
			calendarUpdateFailure(w, http.StatusConflict, "A response may already have been sent for this version. Check it in your calendar provider, then refresh Gofer; it will not resend automatically.", true, true)
			return
		}
		if h.calendarSendResponse != nil {
			result, err = h.calendarSendResponse(ctx, source, event, target, response)
		} else if target.CalDAV != nil {
			if target.CalDAV.ServerScheduling {
				var desiredErr error
				desired, _, desiredErr := prepareCalDAVResponse(target, response)
				if desiredErr != nil {
					err = calendarDeletePreflightError{desiredErr}
				} else {
					result, err = putCalDAVResponse(ctx, target, desired, response)
				}
			} else {
				result, err = h.queueCalDAVResponse(ctx, source, event, target, response)
			}
		} else {
			result, err = sendCalendarResponse(ctx, source, event, target, response)
		}
		if err != nil {
			uncertain := !errors.Is(err, errCalendarUpdateConflict) && calendarCreateUncertain(err)
			var preflight calendarDeletePreflightError
			if errors.As(err, &preflight) {
				uncertain = false
			}
			if !uncertain {
				_ = h.db.ReleaseCalendarResponse(ctx, userID, source.ID, target.Event.RemoteID, version)
			}
			message := "Could not send your response. Check Calendar write access and try again."
			if errors.As(err, &configuration) {
				message = configuration.Error()
			}
			if uncertain {
				message = "The response could not be confirmed. Refresh the calendar and reopen the invitation before trying again."
			}
			if errors.Is(err, errCalendarUpdateConflict) {
				message = "This invitation changed. Reopen it before responding; nothing was sent."
			}
			calendarUpdateFailure(w, http.StatusBadGateway, message, uncertain, errors.Is(err, errCalendarUpdateConflict))
			return
		}
	}
	if !result.Pending && (result.Event.RemoteID != target.Event.RemoteID || result.Event.SeriesRemoteID != target.Event.SeriesRemoteID || result.Event.ResponseStatus != response || result.Event.ETag == "") {
		calendarUpdateFailure(w, http.StatusBadGateway, "The provider's response could not be confirmed. Refresh and reopen the invitation before trying again.", true, false)
		return
	}
	refreshPending := result.Pending
	if !result.Pending && scope != "series" && source.Provider != storage.CalendarSourceProviderCalDAV {
		if err := h.db.CompleteCalendarResponse(ctx, event, calendarStorageEvent(userID, source.ID, result.Event)); err != nil {
			refreshPending = true
		}
	}
	if result.Delivery != "email" && (scope == "series" || refreshPending || source.Provider == storage.CalendarSourceProviderCalDAV) {
		refreshPending = !h.refreshCalendarEditedSeries(ctx, source, event)
	}
	if h.syncer != nil {
		h.syncer.Events().Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: userID, Payload: map[string]any{"source_id": source.ID, "event_id": event.ID}})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"responded": !result.Pending, "pending": result.Pending, "response": response, "event_id": event.ID, "scope": scope, "refresh_pending": refreshPending, "delivery": result.Delivery, "delivery_id": result.DeliveryID})
}
