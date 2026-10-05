package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
)

func calendarEventIsSeries(event storage.CalendarEvent) bool {
	return event.SeriesRemoteID != "" || calendarUpdateHasDetails(json.RawMessage(event.RecurrenceJSON))
}

func calendarSeriesID(event storage.CalendarEvent) string {
	if event.SeriesRemoteID != "" {
		return event.SeriesRemoteID
	}
	return event.RemoteID
}

const calendarSeriesUnsupported = "This series uses repeat settings that Gofer cannot edit yet. Edit it in your calendar provider."

// Never substitute the UI timezone for a series' timezone: that would change
// its wall-clock schedule across daylight-saving transitions.
func calendarSeriesLocation(zone string) (*time.Location, error) {
	if zone != "" && zone != "Local" {
		if location, err := time.LoadLocation(zone); err == nil {
			return location, nil
		}
		if zone == "GMT Standard Time" {
			return time.LoadLocation("Europe/London")
		}
		if location := outlookCalendarTimeLocation(zone); location != time.UTC {
			return location, nil
		}
	}
	return nil, calendarUpdateUnsupported("This series' timezone is not supported. Edit it in your calendar provider.")
}

// Decode only patterns that round-trip through our controls without losing any
// rule fields. Multiple rules, exceptions and relative patterns stay read-only.
func calendarSeriesDraft(event calendar.RemoteEvent) (calendar.EventDraft, error) {
	draft := calendar.EventDraft{Summary: event.Summary, Description: calendarDescriptionText(event.Description), Location: event.Location,
		AllDay: event.AllDay, StartDate: event.StartDate, EndDate: event.EndDate, StartAt: event.StartAt, EndAt: event.EndAt, TimeZone: event.StartTimeZone}
	if draft.AllDay && draft.TimeZone == "" {
		draft.TimeZone = "UTC"
	}
	zone, err := calendarSeriesLocation(draft.TimeZone)
	if err != nil {
		return draft, err
	}
	draft.TimeZone = zone.String()
	if _, err := calendarUpdateDraftLocation(draft); err != nil || event.SeriesRemoteID != "" {
		return draft, calendarUpdateUnsupported(calendarSeriesUnsupported)
	}
	form := url.Values{"repeat_interval": {"1"}, "repeat_end": {"never"}}
	var lines []string
	if json.Unmarshal(event.Recurrence, &lines) == nil {
		if len(lines) != 1 || !strings.HasPrefix(lines[0], "RRULE:") {
			return draft, calendarUpdateUnsupported(calendarSeriesUnsupported)
		}
		for _, part := range strings.Split(lines[0][6:], ";") {
			key, value, _ := strings.Cut(part, "=")
			switch strings.ToUpper(key) {
			case "FREQ":
				form.Set("repeat_frequency", strings.ToLower(value))
			case "INTERVAL":
				form.Set("repeat_interval", value)
			case "COUNT":
				form.Set("repeat_end", "count")
				form.Set("repeat_count", value)
			case "UNTIL":
				until, err := calendarSeriesUntil(value, draft)
				if err != nil {
					return draft, calendarUpdateUnsupported(calendarSeriesUnsupported)
				}
				form.Set("repeat_end", "until")
				form.Set("repeat_until", until)
			}
		}
	} else {
		var graph struct {
			Pattern struct {
				Type     string
				Interval int
			}
			Range struct {
				Type, EndDate       string
				NumberOfOccurrences int
			}
		}
		if json.Unmarshal(event.Recurrence, &graph) != nil {
			return draft, calendarUpdateUnsupported(calendarSeriesUnsupported)
		}
		frequency := map[string]string{"daily": "daily", "weekly": "weekly", "absoluteMonthly": "monthly", "absoluteYearly": "yearly"}[graph.Pattern.Type]
		form.Set("repeat_frequency", frequency)
		form.Set("repeat_interval", strconv.Itoa(graph.Pattern.Interval))
		switch graph.Range.Type {
		case "numbered":
			form.Set("repeat_end", "count")
			form.Set("repeat_count", strconv.Itoa(graph.Range.NumberOfOccurrences))
		case "endDate":
			form.Set("repeat_end", "until")
			form.Set("repeat_until", graph.Range.EndDate)
		}
	}
	draft, err = parseCalendarRecurrenceDraft(&http.Request{PostForm: form}, draft)
	if err != nil || draft.Recurrence == nil || !calendarRecurrenceMatchesDraft(event.Recurrence, draft) {
		return draft, calendarUpdateUnsupported(calendarSeriesUnsupported)
	}
	return draft, nil
}

func calendarSeriesUntil(value string, draft calendar.EventDraft) (string, error) {
	if draft.AllDay {
		until, err := time.Parse("20060102", value)
		return until.Format("2006-01-02"), err
	}
	until, err := time.Parse("20060102T150405Z", value)
	if err != nil {
		return "", err
	}
	start := calendarDraftStart(draft)
	until = until.In(start.Location())
	// UNTIL is an instant, while the picker uses the last included local day.
	if until.Before(time.Date(until.Year(), until.Month(), until.Day(), start.Hour(), start.Minute(), start.Second(), start.Nanosecond(), start.Location())) {
		until = until.AddDate(0, 0, -1)
	}
	return until.Format("2006-01-02"), nil
}

func calendarGoogleSeriesEvent(remote googleCalendarUpdateEvent) (calendar.RemoteEvent, error) {
	if calendarUpdateHasDetails(remote.Attendees) {
		return calendar.RemoteEvent{}, calendarUpdateUnsupported("Recurring meetings with guests are not supported yet.")
	}
	event, err := normalizeGoogleCalendarEvent(remote.googleCalendarEvent)
	if err != nil {
		return event, err
	}
	draft, err := calendarSeriesDraft(event)
	if err != nil {
		return event, err
	}
	if reason := remote.savedRestriction(draft); reason != "" || !calendarUpdateValidETag(event.ETag, false) {
		return event, calendarUpdateUnsupported("This series cannot be safely edited in Gofer.")
	}
	return event, nil
}

func calendarOutlookSeriesEvent(remote outlookCalendarUpdateEvent) (calendar.RemoteEvent, error) {
	if calendarUpdateHasDetails(remote.Attendees) {
		return calendar.RemoteEvent{}, calendarUpdateUnsupported("Recurring meetings with guests are not supported yet.")
	}
	// Validate wire zones before the normal reader's permissive fallback.
	for _, endpoint := range []*outlookCalendarDateTime{&remote.Start, &remote.End} {
		location, err := calendarSeriesLocation(endpoint.TimeZone)
		if err != nil {
			return calendar.RemoteEvent{}, err
		}
		endpoint.TimeZone = location.String()
	}
	event, err := normalizeOutlookCalendarEvent(remote.outlookCalendarEvent)
	if err != nil {
		return event, err
	}
	var recurrence struct {
		Range struct{ RecurrenceTimeZone string }
	}
	if err := json.Unmarshal(remote.Recurrence, &recurrence); err != nil {
		return event, err
	}
	zone := recurrence.Range.RecurrenceTimeZone
	if zone == "" {
		zone = remote.OriginalStartTimeZone
	}
	if zone == "" {
		return event, calendarUpdateUnsupported("Microsoft did not return the series' original timezone.")
	}
	location, err := calendarSeriesLocation(zone)
	if err != nil {
		return event, err
	}
	event.StartTimeZone, event.EndTimeZone = location.String(), location.String()
	draft, err := calendarSeriesDraft(event)
	if err != nil {
		return event, err
	}
	if reason := remote.savedRestriction(draft); reason != "" || event.ETag == "" || !calendarUpdateValidETag(remote.ODataETag, true) {
		return event, calendarUpdateUnsupported("This series cannot be safely edited in Gofer.")
	}
	return event, nil
}

func calendarCalDAVSeriesEvent(cal *ical.Calendar, headers http.Header, endpoint string, location *time.Location) (calendar.RemoteEvent, error) {
	if len(cal.Events()) != 1 || len(cal.Events()[0].Props["RRULE"]) != 1 {
		return calendar.RemoteEvent{}, calendarUpdateUnsupported(calendarSeriesUnsupported)
	}
	props := cal.Events()[0].Props
	if start := props.Get("DTSTART"); start != nil && start.ValueType() != ical.ValueDate && start.Params.Get("TZID") == "" && !strings.HasSuffix(start.Value, "Z") {
		return calendar.RemoteEvent{}, calendarUpdateUnsupported("This series uses floating times. Edit it in your calendar provider to preserve its timezone behavior.")
	}
	rules := props["RRULE"]
	delete(props, "RRULE")
	defer func() { props["RRULE"] = rules }()
	if reason := calendarUpdateCalDAVRestriction(cal, headers); reason != "" {
		return calendar.RemoteEvent{}, calendarUpdateUnsupported("Series with exceptions, invitations or online meetings must be edited in your calendar provider.")
	}
	etag := headers.Get("ETag")
	if !calendarUpdateValidETag(etag, false) {
		return calendar.RemoteEvent{}, calendarUpdateUnsupported("The provider did not supply a safe series version.")
	}
	event, err := normalizeCalDAVEvent(cal.Events()[0], endpoint, etag, location)
	if err != nil {
		return event, err
	}
	event.Recurrence, _ = json.Marshal([]string{"RRULE:" + rules[0].Value})
	_, err = calendarSeriesDraft(event)
	return event, err
}

func (h *Handler) readCalendarProviderSeries(ctx context.Context, source storage.CalendarSource, occurrence storage.CalendarEvent, deletingScope ...bool) (storage.CalendarEvent, error) {
	id := calendarSeriesID(occurrence)
	var event calendar.RemoteEvent
	var err error
	if h.calendarReadSeries != nil {
		event, err = h.calendarReadSeries(ctx, source, id)
	} else {
		credentials := h.calendarUpdateCredentials(ctx, source)
		if credentials.err != nil {
			return storage.CalendarEvent{}, calendarCreateAuthError{credentials.err}
		}
		switch source.Provider {
		case providers.ProviderGmail:
			var remote googleCalendarUpdateEvent
			err = calendarCreateJSON(ctx, http.MethodGet, googleCalendarAPIBaseURL+"/calendars/"+url.PathEscape(source.RemoteID)+"/events/"+url.PathEscape(id), credentials.token, nil, &remote)
			if err == nil {
				event, err = calendarGoogleSeriesEvent(remote)
			}
		case providers.ProviderOutlook:
			var remote outlookCalendarUpdateEvent
			err = calendarCreateJSON(ctx, http.MethodGet, outlookGraphBaseURL+"/me/calendars/"+url.PathEscape(source.RemoteID)+"/events/"+url.PathEscape(id), credentials.token, nil, &remote)
			if err == nil {
				event, err = calendarOutlookSeriesEvent(remote)
			}
		case storage.CalendarSourceProviderCalDAV:
			endpoint, endpointErr := calendarUpdateCalDAVEndpoint(source, storage.CalendarEvent{RemoteID: id})
			if endpointErr != nil {
				return storage.CalendarEvent{}, endpointErr
			}
			location := time.UTC
			if source.TimeZone != "" {
				location, err = calendarSeriesLocation(source.TimeZone)
				if err != nil {
					return storage.CalendarEvent{}, err
				}
			}
			client := &http.Client{Transport: calDAVHTTPTransport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			cal, headers, getErr := calendarUpdateCalDAVGet(ctx, client, endpoint, credentials.username, credentials.password)
			err = getErr
			if err == nil {
				if calendarOccurrenceScope(deletingScope) {
					event, err = calendarCalDAVDeleteSeriesEvent(cal, headers, endpoint, location)
				} else {
					event, err = calendarCalDAVSeriesEvent(cal, headers, endpoint, location)
				}
			}
		default:
			err = errCalendarUpdateUnsupported
		}
	}
	if err != nil {
		return storage.CalendarEvent{}, err
	}
	if event.RemoteID != id || strings.TrimSpace(event.ETag) == "" || event.SeriesRemoteID != "" {
		return storage.CalendarEvent{}, fmt.Errorf("provider did not return the requested series master and version")
	}
	draft, err := calendarSeriesDraft(event)
	if err != nil {
		return storage.CalendarEvent{}, err
	}
	if reason := calendarUpdateSavedRestriction(event, draft); reason != "" {
		return storage.CalendarEvent{}, calendarUpdateUnsupported(reason)
	}
	stored := calendarStorageEvent(source.UserID, source.ID, event)
	stored.ID = occurrence.ID // UI route remains the owned, cached occurrence.
	return stored, nil
}

func calendarUpdateExistingScopeRestriction(existing storage.CalendarEvent, series bool, occurrenceScope ...bool) error {
	if calendarStoredGoogleMeet(existing) && (series || calendarOccurrenceScope(occurrenceScope)) {
		return calendarUpdateUnsupported("Recurring online meetings are not supported yet.")
	}
	if calendarOccurrenceScope(occurrenceScope) {
		if series {
			return errCalendarUpdateUnsupported
		}
		return calendarOccurrenceExistingRestriction(existing)
	}
	if series {
		_, err := calendarSeriesDraft(calendar.RemoteEvent{AllDay: existing.AllDay, StartDate: existing.StartDate, EndDate: existing.EndDate,
			StartAt: existing.StartAt, EndAt: existing.EndAt, StartTimeZone: existing.StartTimeZone, SeriesRemoteID: existing.SeriesRemoteID, Recurrence: json.RawMessage(existing.RecurrenceJSON)})
		if err != nil {
			return err
		}
		existing.RecurrenceJSON = "[]"
	}
	return calendarUpdateExistingRestriction(existing)
}

// Refresh the selected occurrence's window and today's Upcoming window, not
// the possibly years-old master start date. Disjoint windows remain bounded.
func (h *Handler) refreshCalendarEditedSeries(ctx context.Context, source storage.CalendarSource, occurrence storage.CalendarEvent) bool {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	location := viewsCalendarLocation(h.db.GetUISettings(ctx, source.UserID))
	anchor := time.Now()
	if occurrence.AllDay {
		anchor, _ = time.ParseInLocation("2006-01-02", occurrence.StartDate, location)
	} else if occurrence.StartAt != nil {
		anchor = *occurrence.StartAt
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
	credentials := make(map[string]calendarCredentials)
	for _, window := range windows {
		if _, err := h.performCalendarSourceSync(ctx, source, window[0], window[1], credentials); err != nil {
			success = false
		}
	}
	return success
}
