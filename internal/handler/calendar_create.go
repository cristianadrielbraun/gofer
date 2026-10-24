package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	stdmail "net/mail"
	"sort"
	"strconv"
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
	if storage.CalendarSourceMailServer(source.Provider) {
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
	data, err := calendarNewEventData(r.URL.Query().Get("date"), time.Now(), location, sources, accountNames, func(source storage.CalendarSource) bool { return h.calendarWriteAuthorized(ctx, source) })
	if err == nil {
		err = calendarNewEventRange(&data, r.URL.Query().Get("start"), r.URL.Query().Get("end"), location)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-store")
	if err := views.CalendarCreateDialog(data).Render(ctx, w); err != nil {
		http.Error(w, "could not open new event", 500)
	}
}

func calendarNewEventData(selectedDate string, currentTime time.Time, location *time.Location, sources []storage.CalendarSource, accountNames map[string]string, authorized func(storage.CalendarSource) bool) (views.CalendarCreateData, error) {
	now := currentTime.In(location)
	date := now
	if selectedDate != "" {
		var err error
		date, err = time.ParseInLocation("2006-01-02", selectedDate, location)
		if err != nil {
			return views.CalendarCreateData{}, errors.New("invalid selected date")
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
		choice := views.CalendarCreateSource{ID: source.ID, Name: source.Name, AccountName: accountNames[source.AccountID], Writable: calendarSourceWritable(source), Authorized: authorized(source), Provider: source.Provider}
		if choice.Name == "" {
			choice.Name = "Calendar"
		}
		data.Sources = append(data.Sources, choice)
		if choice.Writable && (data.SourceID == "" || (source.IsPrimary && choice.Authorized)) {
			data.SourceID = choice.ID
		}
	}
	if data.SourceID == "" && len(data.Sources) > 0 {
		data.SourceID = data.Sources[0].ID
	}
	return data, nil
}

// calendarNewEventRange fills a new event's times from a range picked in the
// week view: local date-times, such as 2026-11-03T09:00.
func calendarNewEventRange(data *views.CalendarCreateData, start, end string, location *time.Location) error {
	if start == "" && end == "" {
		return nil
	}
	from, err := time.ParseInLocation("2006-01-02T15:04", start, location)
	if err != nil {
		return errors.New("invalid start time")
	}
	to, err := time.ParseInLocation("2006-01-02T15:04", end, location)
	if err != nil || !to.After(from) || to.Sub(from) > 31*24*time.Hour {
		return errors.New("invalid end time")
	}
	data.Date, data.StartTime = from.Format("2006-01-02"), from.Format("15:04")
	data.EndDate, data.EndTime = to.Format("2006-01-02"), to.Format("15:04")
	return nil
}

func parseCalendarEventDraft(r *http.Request) (calendar.EventDraft, error) {
	if err := r.ParseForm(); err != nil {
		return calendar.EventDraft{}, fmt.Errorf("invalid event form")
	}
	allowed := map[string]bool{"request_id": true, "source_id": true, "summary": true, "description": true, "location": true, "timezone": true, "all_day": true, "start_date": true, "end_date": true, "start_time": true, "end_time": true,
		"repeat_frequency": true, "repeat_interval": true, "repeat_end": true, "repeat_until": true, "repeat_count": true, "guests": true, "description_html": true, "teams_meeting": true, "teams_draft_id": true, "google_meet_meeting": true, "google_meet_draft_id": true, "reset_series_dates": true, "remove_teams_details": true, "reminder": true, "show_as": true, "private": true}
	for name, values := range r.PostForm {
		if !allowed[name] || len(values) != 1 {
			return calendar.EventDraft{}, fmt.Errorf("this form contains unsupported or duplicate event fields")
		}
	}
	draft := calendar.EventDraft{RequestID: strings.TrimSpace(r.FormValue("request_id")), Summary: strings.TrimSpace(r.FormValue("summary")),
		Description: strings.TrimSpace(r.FormValue("description")), Location: strings.TrimSpace(r.FormValue("location")), TimeZone: strings.TrimSpace(r.FormValue("timezone"))}
	draft.TeamsMeetingSet = r.PostForm.Has("teams_meeting")
	if value := r.PostForm.Get("teams_meeting"); value != "" && value != "true" && value != "false" {
		return draft, fmt.Errorf("invalid Teams meeting setting")
	}
	draft.TeamsMeeting = r.PostForm.Get("teams_meeting") == "true"
	draft.TeamsDraftID = strings.TrimSpace(r.PostForm.Get("teams_draft_id"))
	if draft.TeamsDraftID != "" {
		id, err := uuid.Parse(draft.TeamsDraftID)
		if err != nil || id == uuid.Nil || !draft.TeamsMeeting {
			return draft, fmt.Errorf("invalid Teams draft")
		}
		draft.TeamsDraftID = id.String()
	}
	if r.PostForm.Has("reminder") {
		// "default" leaves the calendar's own reminder; "none" removes it.
		switch value := r.PostForm.Get("reminder"); value {
		case "default":
		case "none":
			draft.ReminderSet = true
		default:
			minutes, err := strconv.Atoi(value)
			if err != nil || minutes < 0 || minutes > calendarReminderMaxMinutes {
				return draft, fmt.Errorf("choose a valid reminder")
			}
			draft.ReminderSet, draft.ReminderMinutes = true, &minutes
		}
	}
	if r.PostForm.Has("show_as") {
		value := r.PostForm.Get("show_as")
		if storage.CalendarShowAsValue(value) != value || value == "" {
			return draft, fmt.Errorf("choose whether the event shows you as busy or free")
		}
		draft.ShowAs = value
	}
	if r.PostForm.Has("private") {
		switch r.PostForm.Get("private") {
		case "true":
			draft.PrivateSet, draft.Private = true, true
		case "false":
			draft.PrivateSet = true
		default:
			return draft, fmt.Errorf("choose whether the event is private")
		}
	}
	if value := r.PostForm.Get("reset_series_dates"); value != "" && value != "true" {
		return draft, fmt.Errorf("invalid series confirmation")
	}
	draft.ResetSeriesDates = r.PostForm.Get("reset_series_dates") == "true"
	draft.GoogleMeetMeetingSet = r.PostForm.Has("google_meet_meeting")
	if value := r.PostForm.Get("google_meet_meeting"); value != "" && value != "true" && value != "false" {
		return draft, fmt.Errorf("invalid Google Meet meeting setting")
	}
	draft.GoogleMeetMeeting = r.PostForm.Get("google_meet_meeting") == "true"
	if draft.GoogleMeetMeeting && draft.TeamsMeeting {
		return draft, fmt.Errorf("choose one meeting provider")
	}
	draft.GoogleMeetDraftID = strings.TrimSpace(r.PostForm.Get("google_meet_draft_id"))
	if draft.GoogleMeetDraftID != "" {
		id, err := uuid.Parse(draft.GoogleMeetDraftID)
		if err != nil || id == uuid.Nil || !draft.GoogleMeetMeeting {
			return draft, fmt.Errorf("invalid Google Meet draft")
		}
		draft.GoogleMeetDraftID = id.String()
	}
	if r.PostForm.Has("description_html") {
		raw := r.PostForm.Get("description_html")
		if len(raw) > 65536 || !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
			return draft, fmt.Errorf("event description is invalid or too long")
		}
		safe := calendar.SanitizeDescriptionHTML(raw)
		draft.DescriptionHTML = &safe
		draft.Description = calendar.DescriptionPlainText(safe)
	}
	// A copy replacing a Teams meeting leaves out Microsoft's joining details,
	// which would point to the deleted meeting.
	if value := r.PostForm.Get("remove_teams_details"); value != "" && value != "true" {
		return draft, fmt.Errorf("invalid Teams details setting")
	} else if value == "true" {
		draft.Description = strings.TrimSpace(calendar.RemoveTeamsJoinBlock(draft.Description))
		if draft.DescriptionHTML != nil {
			safe := calendar.SanitizeDescriptionHTML(calendar.RemoveTeamsJoinBlock(*draft.DescriptionHTML))
			draft.DescriptionHTML = &safe
			draft.Description = calendar.DescriptionPlainText(safe)
		}
	}
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
		// Repeating meetings depend on the calendar: Google and Outlook support
		// them, and the CalDAV paths refuse them before writing.
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
	if _, owned := ctx.Value(userCalendarProviderKey{}).(*userCalendarRequest); !owned && h.calendarCreateEvent != nil {
		return h.calendarCreateEvent(ctx, source, draft)
	}
	credentials := h.calendarUpdateCredentials(ctx, source)
	if credentials.err != nil {
		if draft.TeamsRemoteID != "" {
			return calendar.RemoteEvent{}, fmt.Errorf("Could not confirm the prepared Teams meeting credentials: %v", credentials.err)
		}
		return calendar.RemoteEvent{}, calendarCreateAuthError{credentials.err}
	}
	switch source.Provider {
	case providers.ProviderGmail:
		return createGoogleCalendarEvent(ctx, credentials.token, source.RemoteID, draft)
	case providers.ProviderOutlook:
		if draft.TeamsRemoteID != "" {
			remote, err := finalizeCalendarTeamsDraft(ctx, source, credentials.token, draft)
			if err != nil {
				return remote, fmt.Errorf("Could not confirm the prepared Teams event: %v", err)
			}
			return remote, nil
		}
		return createOutlookCalendarEvent(ctx, credentials.token, source.RemoteID, draft)
	case storage.CalendarSourceProviderCalDAV, storage.CalendarSourceProviderJMAP:
		if _, err := resolveCalDAVHref(credentials.baseURL, source.RemoteID); err != nil {
			return calendar.RemoteEvent{}, err
		}
		ctx = withJMAPCalendarDAV(ctx, source, credentials)
		var remote calendar.RemoteEvent
		var err error
		if len(draft.Guests) > 0 {
			remote, err = h.createCalDAVMeeting(ctx, source, credentials, draft)
		} else {
			remote, err = createCalDAVCalendarEvent(ctx, source, credentials.username, credentials.password, draft)
		}
		return jmapCalendarDAVCanonical(ctx, remote), err
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
	r.Body = http.MaxBytesReader(w, r.Body, 384<<10) // Bounded HTML + plain fallback, including URL-encoding overhead.
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
	if draft.GoogleMeetMeeting && source.Provider != providers.ProviderGmail {
		calendarCreateFailure(w, 400, "Google Meet meetings require a supported Google calendar.", false)
		return
	}
	if draft.TeamsMeeting && source.Provider != providers.ProviderOutlook {
		calendarCreateFailure(w, 400, "Teams meetings require a supported Outlook calendar.", false)
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
		if err := h.attachCalendarTeamsDraft(ctx, source, &draft); err != nil {
			calendarCreateFailure(w, 409, "The Teams link could not be loaded. Toggle Teams off and on to prepare it again.", false)
			return
		}
		if err := h.attachCalendarMeetDraft(ctx, source, &draft, "create:"+draft.RequestID); err != nil {
			calendarCreateFailure(w, 409, "The Google Meet link could not be loaded. Toggle Google Meet off and on to prepare it again.", false)
			return
		}
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
	if draft.TeamsDraftID != "" && source.Provider == providers.ProviderOutlook {
		if d, err := h.db.GetCalendarTeamsDraft(ctx, userID, source.ID, draft.TeamsDraftID); err == nil && d.UsedBy == "create:"+draft.RequestID && d.RemoteID == result.RemoteID {
			_ = h.db.FinishCalendarTeamsDraft(ctx, d, "saved")
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
	// Derive this from the durable result on both create and replay. Microsoft
	// may save an ordinary appointment without enabling the requested meeting;
	// cache that confirmed event instead of inviting a duplicate POST retry.
	// A series is read from one of its refreshed dates.
	savedMeeting := func() (storage.CalendarEvent, error) {
		if draft.Recurrence != nil {
			return h.db.GetCalendarSeriesOccurrence(ctx, userID, source.ID, result.RemoteID)
		}
		return h.db.GetCalendarEvent(ctx, userID, result.EventID)
	}
	if draft.TeamsMeeting && source.Provider == providers.ProviderOutlook {
		saved, readErr := savedMeeting()
		response["teams_unconfirmed"] = readErr != nil || !calendarTeamsLinkConfirmed(calendarOutlookCachedMeetingJSON(saved))
	}
	if draft.GoogleMeetMeeting && source.Provider == providers.ProviderGmail {
		saved, readErr := savedMeeting()
		response["google_meet_unconfirmed"] = readErr != nil || !calendarGoogleMeetConfirmed(json.RawMessage(saved.OnlineMeetingJSON))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(response)
}

// calendarReminderMaxMinutes is four weeks, the longest reminder providers offer.
const calendarReminderMaxMinutes = 40320
