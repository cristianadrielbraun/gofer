package handler

import (
	"errors"
	"fmt"
	"sync"
	"time"

	ical "github.com/emersion/go-ical"
)

// Gofer asks CalDAV servers to expand recurring events into their dates
// (RFC 4791, section 9.6.5). Some servers fail at that while serving the same
// events unexpanded without trouble. Baïkal's admin pages, for one, store a
// calendar's timezone as a bare name such as "Europe/Zurich" instead of the
// VCALENDAR the property should hold, and its CalDAV library answers every
// expanding request on that calendar with a 500. For such a calendar Gofer
// reads its events as stored and expands them itself, as it already does for
// JMAP calendars.

// calDAVServerCannotExpand reports whether a request that asked the server
// to expand recurring events failed on the server's side in a way an
// unexpanded request might not. A busy or unreachable server (502, 503, 504)
// is left to the usual retry instead.
func calDAVServerCannotExpand(err error) bool {
	var failure calDAVDiscoveryError
	return errors.As(err, &failure) && (failure.status == 500 || failure.status == 501)
}

// calDAVExpandRefused remembers the calendars whose server failed to expand
// them, so later reads go straight to the unexpanded request instead of
// failing on the server first each time. It is kept in memory only: after a
// restart one more expanding request finds out again.
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
		return nil, fmt.Errorf("CalDAV calendar data contains no events")
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
