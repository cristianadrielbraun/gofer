package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	stdmail "net/mail"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
	"github.com/google/uuid"
)

func calendarSourceWritable(source storage.CalendarSource) bool {
	role := strings.ToLower(strings.TrimSpace(source.AccessRole))
	if source.Provider == storage.CalendarSourceProviderCalDAV && (role == "" || role == "unknown") {
		return true // Older CalDAV discoveries omitted privileges; PUT still enforces server access.
	}
	return role == "owner" || role == "writer"
}

func (h *Handler) calendarWriteAuthorized(ctx context.Context, source storage.CalendarSource) bool {
	if source.Provider == storage.CalendarSourceProviderCalDAV {
		return h.accountStore != nil || h.calendarCreateEvent != nil
	}
	if h.calendarCreateEvent != nil {
		return true
	} // Isolated provider fixtures only.
	return h.mailCredentials() != nil && h.mailCredentials().CalendarWriteAuthorized(ctx, source.AccountID, source.Provider)
}

func (h *Handler) handleNewCalendarEvent(w http.ResponseWriter, r *http.Request) {
	ctx, userID := r.Context(), h.userID(r.Context())
	sources, err := h.db.ListSelectedCalendarSources(ctx, userID)
	if err != nil {
		http.Error(w, "could not load configured calendars", 500)
		return
	}
	accounts, err := h.db.GetAccounts(ctx, userID)
	if err != nil {
		http.Error(w, "could not load calendar accounts", 500)
		return
	}
	accountNames := make(map[string]string)
	for _, account := range accounts {
		accountNames[account.ID] = account.Name
		if account.Name == "" {
			accountNames[account.ID] = account.Email
		}
	}
	location := viewsCalendarLocation(h.db.GetUISettings(ctx, userID))
	now := time.Now().In(location)
	date := now
	if value := r.URL.Query().Get("date"); value != "" {
		date, err = time.ParseInLocation("2006-01-02", value, location)
		if err != nil {
			http.Error(w, "invalid selected date", 400)
			return
		}
	}
	start := time.Date(date.Year(), date.Month(), date.Day(), 9, 0, 0, 0, location)
	if date.Format("2006-01-02") == now.Format("2006-01-02") {
		start = now.Truncate(30 * time.Minute).Add(30 * time.Minute)
	}
	end := start.Add(time.Hour)
	zone := location.String()
	if zone == "Local" {
		zone = "UTC"
	}
	data := views.CalendarCreateData{RequestID: uuid.NewString(), Date: start.Format("2006-01-02"), StartTime: start.Format("15:04"), EndDate: end.Format("2006-01-02"), EndTime: end.Format("15:04"), TimeZone: zone}
	for _, source := range sources {
		choice := views.CalendarCreateSource{ID: source.ID, Name: source.Name, AccountName: accountNames[source.AccountID], Writable: calendarSourceWritable(source), Authorized: h.calendarWriteAuthorized(ctx, source)}
		if choice.Name == "" {
			choice.Name = "Calendar"
		}
		data.Sources = append(data.Sources, choice)
		if choice.Writable && (data.SourceID == "" || (source.IsPrimary && choice.Authorized)) {
			data.SourceID = choice.ID
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := views.CalendarCreateDialog(data).Render(ctx, w); err != nil {
		http.Error(w, "could not open new event", 500)
	}
}

func parseCalendarEventDraft(r *http.Request) (calendar.EventDraft, error) {
	if err := r.ParseForm(); err != nil {
		return calendar.EventDraft{}, fmt.Errorf("invalid event form")
	}
	allowed := map[string]bool{"request_id": true, "source_id": true, "summary": true, "description": true, "location": true, "timezone": true, "all_day": true, "start_date": true, "end_date": true, "start_time": true, "end_time": true,
		"repeat_frequency": true, "repeat_interval": true, "repeat_end": true, "repeat_until": true, "repeat_count": true, "guests": true}
	for name, values := range r.PostForm {
		if !allowed[name] || len(values) != 1 {
			return calendar.EventDraft{}, fmt.Errorf("this form contains unsupported or duplicate event fields")
		}
	}
	draft := calendar.EventDraft{RequestID: strings.TrimSpace(r.FormValue("request_id")), Summary: strings.TrimSpace(r.FormValue("summary")),
		Description: strings.TrimSpace(r.FormValue("description")), Location: strings.TrimSpace(r.FormValue("location")), TimeZone: strings.TrimSpace(r.FormValue("timezone"))}
	id, err := uuid.Parse(draft.RequestID)
	if err != nil || id == uuid.Nil {
		return draft, fmt.Errorf("invalid event request; reopen New event")
	}
	draft.RequestID = id.String()
	draft.GuestsSet = r.PostForm.Has("guests")
	if raw := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(r.PostForm.Get("guests")), ",")); raw != "" {
		if len(raw) > 16384 || !utf8.ValidString(raw) || strings.ContainsAny(raw, "\r\n\x00") {
			return draft, fmt.Errorf("enter guest email addresses separated by commas")
		}
		addresses, err := stdmail.ParseAddressList(raw)
		if err != nil || len(addresses) > 50 {
			return draft, fmt.Errorf("enter up to 50 valid guest email addresses, separated by commas")
		}
		seen := map[string]bool{}
		for _, address := range addresses {
			email := strings.ToLower(address.Address)
			if calendarReplyAddress("mailto:"+email) == "" || len(address.Name) > 255 {
				return draft, fmt.Errorf("enter valid guest email addresses")
			}
			if !seen[email] {
				draft.Guests = append(draft.Guests, calendar.GuestDraft{Email: email, Name: address.Name})
				seen[email] = true
			}
		}
		sort.Slice(draft.Guests, func(i, j int) bool { return draft.Guests[i].Email < draft.Guests[j].Email })
		if frequency := r.PostForm.Get("repeat_frequency"); frequency != "" && frequency != "none" {
			return draft, fmt.Errorf("guest invitations currently support events that do not repeat")
		}
	}
	if draft.Summary == "" || utf8.RuneCountInString(draft.Summary) > 255 {
		return draft, fmt.Errorf("enter an event title of at most 255 characters")
	}
	if !utf8.ValidString(draft.Summary+draft.Description+draft.Location) || len(draft.Description) > 65536 || utf8.RuneCountInString(draft.Description) > 16384 || utf8.RuneCountInString(draft.Location) > 1024 || strings.ContainsRune(draft.Summary+draft.Description+draft.Location, 0) {
		return draft, fmt.Errorf("event details are invalid or too long")
	}
	if draft.TimeZone == "" || draft.TimeZone == "Local" || len(draft.TimeZone) > 100 {
		return draft, fmt.Errorf("choose a valid time zone, for example Europe/Prague")
	}
	location, err := time.LoadLocation(draft.TimeZone)
	if err != nil {
		return draft, fmt.Errorf("choose a valid time zone, for example Europe/Prague")
	}
	allDay := r.FormValue("all_day")
	if allDay != "" && allDay != "false" && allDay != "true" {
		return draft, fmt.Errorf("invalid all-day setting")
	}
	draft.AllDay = allDay == "true"
	startDate, endDate := r.FormValue("start_date"), r.FormValue("end_date")
	start, err := time.Parse("2006-01-02", startDate)
	if err != nil || start.Year() < 1 {
		return draft, fmt.Errorf("enter a valid start date")
	}
	end, err := time.Parse("2006-01-02", endDate)
	if err != nil || end.Year() < 1 {
		return draft, fmt.Errorf("enter a valid end date")
	}
	if draft.AllDay {
		if end.Before(start) || end.Year() >= 9999 {
			return draft, fmt.Errorf("the last day must be on or after the first day")
		}
		draft.StartDate, draft.EndDate = startDate, end.AddDate(0, 0, 1).Format("2006-01-02") // UI end is inclusive.
		return parseCalendarRecurrenceDraft(r, draft)
	}
	parseWallTime := func(date, clock string) (time.Time, error) {
		value := date + "T" + clock
		at, err := time.ParseInLocation("2006-01-02T15:04", value, location)
		if err != nil || at.Format("2006-01-02T15:04") != value {
			return at, fmt.Errorf("that time does not exist in the selected time zone; choose another time")
		}
		civil, _ := time.Parse("2006-01-02T15:04", value)
		for _, probe := range []time.Time{at.Add(-12 * time.Hour), at.Add(12 * time.Hour)} {
			_, offset := probe.Zone()
			candidate := civil.Add(-time.Duration(offset) * time.Second).In(location)
			if !candidate.Equal(at) && candidate.Format("2006-01-02T15:04") == value {
				return at, fmt.Errorf("that time occurs twice during a daylight-saving change; choose an unambiguous time")
			}
		}
		return at, nil
	}
	start, err = parseWallTime(startDate, r.FormValue("start_time"))
	if err != nil {
		return draft, err
	}
	end, err = parseWallTime(endDate, r.FormValue("end_time"))
	if err != nil {
		return draft, err
	}
	if !end.After(start) {
		return draft, fmt.Errorf("the end must be after the start")
	}
	draft.StartAt, draft.EndAt = &start, &end
	return parseCalendarRecurrenceDraft(r, draft)
}

// Share the source gate with reads so an older snapshot cannot reconcile over
// a just-created event. Different sources remain independent.
func (h *Handler) lockCalendarCreate(ctx context.Context, source storage.CalendarSource) (func(), error) {
	key := source.UserID + "\x00" + source.ID
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h.calendarSyncMu.Lock()
		if h.calendarSyncRunning == nil {
			h.calendarSyncRunning = make(map[string]*calendarSyncRun)
		}
		if active := h.calendarSyncRunning[key]; active != nil {
			h.calendarSyncMu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-active.done:
				continue
			}
		}
		run := &calendarSyncRun{done: make(chan struct{})}
		h.calendarSyncRunning[key] = run
		h.calendarSyncMu.Unlock()
		return func() {
			h.calendarSyncMu.Lock()
			delete(h.calendarSyncRunning, key)
			close(run.done)
			h.calendarSyncMu.Unlock()
		}, nil
	}
}

func (h *Handler) createCalendarProviderEvent(ctx context.Context, source storage.CalendarSource, draft calendar.EventDraft) (calendar.RemoteEvent, error) {
	if h.calendarCreateEvent != nil {
		return h.calendarCreateEvent(ctx, source, draft)
	}
	credentials := calendarCredentials{}
	switch source.Provider {
	case providers.ProviderGmail:
		credentials.token, credentials.err = h.mailCredentials().GetGoogleCalendarWriteTokenForAccount(ctx, source.AccountID)
	case providers.ProviderOutlook:
		credentials.token, credentials.err = h.mailCredentials().GetMicrosoftGraphCalendarWriteTokenForAccount(ctx, source.AccountID)
	case storage.CalendarSourceProviderCalDAV:
		credentials = h.calendarCredentialsForSource(ctx, source.UserID, source)
	}
	if credentials.err != nil {
		return calendar.RemoteEvent{}, calendarCreateAuthError{credentials.err}
	}
	switch source.Provider {
	case providers.ProviderGmail:
		return createGoogleCalendarEvent(ctx, credentials.token, source.RemoteID, draft)
	case providers.ProviderOutlook:
		return createOutlookCalendarEvent(ctx, credentials.token, source.RemoteID, draft)
	case storage.CalendarSourceProviderCalDAV:
		if _, err := resolveCalDAVHref(credentials.baseURL, source.RemoteID); err != nil {
			return calendar.RemoteEvent{}, err
		}
		if len(draft.Guests) > 0 {
			return h.createCalDAVMeeting(ctx, source, credentials, draft)
		}
		return createCalDAVCalendarEvent(ctx, source, credentials.username, credentials.password, draft)
	}
	return calendar.RemoteEvent{}, calendarCreateProviderError{http.StatusBadRequest}
}

func calendarCreateFailure(w http.ResponseWriter, status int, message string, uncertain bool) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	response := map[string]any{"error": message, "uncertain": uncertain}
	if !uncertain {
		response["request_id"] = uuid.NewString()
	}
	_ = json.NewEncoder(w).Encode(response)
}

func (h *Handler) handleCreateCalendarEvent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 100<<10)
	if err := r.ParseForm(); err != nil {
		calendarCreateFailure(w, 400, "Event details could not be read.", false)
		return
	}
	draft, err := parseCalendarEventDraft(r)
	if err != nil {
		calendarCreateFailure(w, 400, err.Error(), false)
		return
	}
	userID := h.userID(r.Context())
	sources, err := h.db.ListSelectedCalendarSources(r.Context(), userID)
	if err != nil {
		calendarCreateFailure(w, 500, "Could not load configured calendars.", false)
		return
	}
	var source storage.CalendarSource
	for _, candidate := range sources {
		if candidate.ID == r.FormValue("source_id") {
			source = candidate
			break
		}
	}
	if source.ID == "" {
		calendarCreateFailure(w, 404, "This calendar is no longer configured.", false)
		return
	}
	if !calendarSourceWritable(source) {
		calendarCreateFailure(w, 403, "This calendar is read-only. Choose a writable calendar.", false)
		return
	}
	if !h.calendarWriteAuthorized(r.Context(), source) {
		calendarCreateFailure(w, 403, "This account has read-only Calendar access. Reconnect it from Accounts to grant event creation permission.", false)
		return
	}
	if len(draft.Guests) > 0 {
		account, err := h.calendarReplyAccount(r.Context(), source)
		if err != nil || calendarReplyAddress("mailto:"+account.Email) == "" {
			calendarCreateFailure(w, 400, "A valid account sending address is required to invite guests.", false)
			return
		}
		draft.OrganizerEmail, draft.OrganizerName = strings.ToLower(account.Email), account.Name
		for _, guest := range draft.Guests {
			if strings.EqualFold(guest.Email, account.Email) {
				calendarCreateFailure(w, 400, "You are already the organizer; add other people as guests.", false)
				return
			}
		}
	}
	// Once accepted, finish even if the browser disconnects. A timed-out client
	// can retry the same durable ID instead of generating a second appointment.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), calendarSourceTimeout)
	defer cancel()
	unlock, err := h.lockCalendarCreate(ctx, source)
	if err != nil {
		calendarCreateFailure(w, 503, "Calendar is still refreshing. Try again.", false)
		return
	}
	defer unlock()
	// Recheck access after waiting behind a refresh or another creation.
	current, err := h.db.ListSelectedCalendarSources(ctx, userID)
	if err != nil {
		calendarCreateFailure(w, 500, "Could not verify the calendar.", false)
		return
	}
	found := false
	for _, candidate := range current {
		if candidate.ID == source.ID {
			source = candidate
			found = true
			break
		}
	}
	if !found || !calendarSourceWritable(source) {
		calendarCreateFailure(w, 409, "Calendar configuration changed. Reopen New event.", false)
		return
	}
	hash := calendarDraftHash(draft)
	result, err := h.db.BeginCalendarCreate(ctx, userID, source.ID, draft.RequestID, hash)
	if err != nil {
		calendarCreateFailure(w, 409, "This event request is no longer valid. Reopen New event.", false)
		return
	}
	replayed := result.RemoteID != ""
	if !replayed {
		remote, createErr := h.createCalendarProviderEvent(ctx, source, draft)
		if createErr != nil {
			message := "Could not create the event. Check the calendar permissions and try again."
			uncertain := calendarCreateUncertain(createErr)
			var provider calendarCreateProviderError
			var configuration calendarReplyConfigurationError
			if errors.As(createErr, &configuration) {
				message = configuration.Error()
			}
			if errors.Is(createErr, errCalendarUpdateUnsupported) {
				message = createErr.Error()
			}
			if errors.As(createErr, &provider) && (provider.Status == 401 || provider.Status == 403) {
				message = "Calendar write access was denied. Reconnect OAuth accounts from Accounts, or check your CalDAV permissions."
			}
			if uncertain {
				message = "The provider's response could not be confirmed. Retry this same event to check without creating a duplicate."
			}
			calendarCreateFailure(w, 502, message, uncertain)
			return
		}
		if draft.Recurrence != nil {
			if remote.Deleted || remote.SeriesRemoteID != "" || !calendarUpdateHasDetails(remote.Recurrence) {
				calendarCreateFailure(w, 502, "The provider did not confirm the series. Retry this same event to verify it safely.", true)
				return
			}
			result, err = h.db.CompleteCalendarSeriesCreate(ctx, userID, source.ID, draft.RequestID, hash, remote.RemoteID)
		} else {
			result, err = h.db.CompleteCalendarCreate(ctx, userID, source.ID, draft.RequestID, hash, calendarStorageEvent(userID, source.ID, remote))
		}
		if err != nil {
			calendarCreateFailure(w, 503, "The provider created the event, but the local update could not finish. Retry this same event to recover it without a duplicate.", true)
			return
		}
		// This is an event change, not a fabricated successful full sync.
		if h.syncer != nil && draft.Recurrence == nil {
			h.syncer.Events().Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: userID, Payload: map[string]any{"source_id": source.ID, "event_id": result.EventID}})
		}
	}
	response := map[string]any{"event_id": result.EventID, "source_id": source.ID, "replayed": replayed, "hidden": source.IsHidden, "notify_guests": len(draft.Guests) > 0}
	if draft.Recurrence != nil {
		response["series_id"] = result.RemoteID
		// We already own the source gate. Read real instances, never generate a
		// guessed local series. A failed read is a refresh failure, not a failed
		// creation; the durable request above prevents another provider write.
		response["refresh_pending"] = !h.refreshCalendarSeries(ctx, source, draft)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(response)
}
