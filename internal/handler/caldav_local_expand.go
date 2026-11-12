package handler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	ical "github.com/emersion/go-ical"
)

// Gofer asks CalDAV servers to expand recurring events into their dates
// (RFC 4791, section 9.6.5). Some servers fail at that while serving the same
// events unexpanded without trouble. Baïkal's admin pages, for one, store a
// calendar's timezone as a bare name such as "Europe/Zurich" instead of the
// VCALENDAR the property should hold, and its CalDAV library answers every
// expanding request on that calendar with a 500. Other servers accept the
// request and answer it with the events as stored, recurrence rules and all.
// For such a calendar Gofer reads its events as stored and expands them
// itself, as it already does for JMAP calendars.

// calDAVExpandingReport sends a REPORT whose body asks the server to expand
// recurring events between start and end, and reports whether the answer
// came expanded. When the server fails at expanding, or answers with series
// it left unexpanded, the same REPORT is sent asking for the events as
// stored instead, and the calendar is remembered so later reads skip the
// expanding request. body gets the calendar-data element to ask for.
func calDAVExpandingReport(ctx context.Context, collection, username, password string, body func(calendarData string) string, start, end time.Time) (davMultiStatus, bool, error) {
	// Allow local expansion when a server returns apparently expanded but incomplete results.
	if os.Getenv("GOFER_CALDAV_FORCE_LOCAL_EXPAND") != "true" && !calDAVExpandRefusedFor(collection) {
		expand := fmt.Sprintf(`<c:calendar-data><c:expand start="%s" end="%s"/></c:calendar-data>`, start.UTC().Format("20060102T150405Z"), end.UTC().Format("20060102T150405Z"))
		multi, err := calDAVRequest(ctx, "REPORT", collection, username, password, "1", body(expand), 30*time.Second)
		if err == nil && !calDAVResponseNotExpanded(multi) {
			return multi, true, nil
		}
		if err != nil && !calDAVServerCannotExpand(err) {
			return davMultiStatus{}, false, err
		}
	}
	multi, err := calDAVRequest(ctx, "REPORT", collection, username, password, "1", body(`<c:calendar-data/>`), 30*time.Second)
	if err != nil {
		return davMultiStatus{}, false, err
	}
	rememberCalDAVExpandRefused(collection)
	return multi, false, nil
}

// calDAVResponseNotExpanded reports whether an answer to an expanding
// request still holds an event with recurrence rules or dates, which an
// expanded answer never does (RFC 4791, section 9.6.5). Data that can't be
// read is left to the caller, which rejects it.
func calDAVResponseNotExpanded(multi davMultiStatus) bool {
	for _, response := range multi.Responses {
		for _, propStat := range response.PropStats {
			if !strings.Contains(propStat.Status, " 200 ") || strings.TrimSpace(propStat.Prop.CalendarData) == "" {
				continue
			}
			cal, err := calendarUpdateDecodeICS([]byte(propStat.Prop.CalendarData))
			if err != nil {
				continue
			}
			for _, event := range cal.Events() {
				for _, name := range []string{ical.PropRecurrenceRule, "EXRULE", ical.PropRecurrenceDates, ical.PropExceptionDates} {
					if event.Props.Get(name) != nil {
						return true
					}
				}
			}
		}
	}
	return false
}

// calDAVServerCannotExpand reports whether a request that asked the server
// to expand recurring events failed on the server's side in a way an
// unexpanded request might not. A busy or unreachable server (502, 503, 504)
// is left to the usual retry instead.
func calDAVServerCannotExpand(err error) bool {
	var failure calDAVDiscoveryError
	return errors.As(err, &failure) && (failure.status == 500 || failure.status == 501)
}

// calDAVExpandRefused remembers the calendars whose server failed to expand
// them or left them unexpanded, so later reads go straight to the unexpanded
// request instead of asking the server first each time. It is kept in memory
// only: after a restart one more expanding request finds out again.
var calDAVExpandRefused sync.Map // calendar collection URL → struct{}

func calDAVExpandRefusedFor(collection string) bool {
	_, refused := calDAVExpandRefused.Load(collection)
	return refused
}

func rememberCalDAVExpandRefused(collection string) {
	calDAVExpandRefused.Store(collection, struct{}{})
}

// expandCalDAVResource reads one stored event resource as the occurrences
// that overlap the window, as the server would have expanded them. A
// resource with no series, only dates changed on their own (an invitation to
// some dates of someone else's series), is read date by date.
func expandCalDAVResource(data, href, etag string, start, end time.Time, location *time.Location) ([]jmapOccurrence, error) {
	cal, err := calendarUpdateDecodeICS([]byte(data))
	if err != nil {
		return nil, err
	}
	var master *ical.Event
	var changed []ical.Event
	for _, component := range cal.Events() {
		if component.Props.Get(ical.PropRecurrenceID) != nil {
			changed = append(changed, component)
			continue
		}
		if master != nil {
			return nil, fmt.Errorf("the event has two series")
		}
		master = &component
	}
	if master != nil {
		return expandCalendarSeries(append([]ical.Event{*master}, changed...), href, etag, start, end, location)
	}
	if len(changed) == 0 {
		return nil, errCalDAVNoEvents
	}
	var out []jmapOccurrence
	for _, component := range changed {
		occurrences, err := expandCalendarSeries([]ical.Event{component}, href, etag, start, end, location)
		if err != nil {
			return nil, err
		}
		out = append(out, occurrences...)
	}
	return out, nil
}
