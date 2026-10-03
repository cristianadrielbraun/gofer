package handler

import (
	"fmt"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	ical "github.com/emersion/go-ical"
)

// Include a real VTIMEZONE, not a frozen UTC offset or an unadvertised
// timezone-by-reference extension. Enumerate the installed IANA transitions
// through the last possible occurrence; open-ended rules cover all four-digit
// iCalendar years. Grouping observances into RDATEs keeps the payload bounded
// without guessing future DST rules or silently stopping them after a few years.
func calendarRecurrenceTimezone(draft calendar.EventDraft, zone *time.Location) (*ical.Component, error) {
	start := draft.StartAt.In(zone)
	end := time.Date(9999, 12, 31, 23, 59, 59, 0, zone)
	if r := draft.Recurrence; r.Until != "" {
		until, _ := time.ParseInLocation("2006-01-02", r.Until, zone)
		end = until.AddDate(0, 0, 2).Add(draft.EndAt.Sub(*draft.StartAt))
	} else if r.Count > 0 {
		end = calendarRecurrenceDate(start, *r, r.Count-1).Add(draft.EndAt.Sub(*draft.StartAt)).AddDate(0, 0, 2)
	}
	if end.Year() > 9999 {
		end = time.Date(9999, 12, 31, 23, 59, 59, 0, zone)
	}
	tz := ical.NewComponent("VTIMEZONE")
	tz.Props.SetText("TZID", draft.TimeZone)
	type observance struct {
		component *ical.Component
		dates     []string
	}
	groups := map[string]*observance{}
	var ordered []*observance
	offsetText := func(offset int) string {
		sign := "+"
		if offset < 0 {
			sign, offset = "-", -offset
		}
		value := fmt.Sprintf("%s%02d%02d", sign, offset/3600, offset%3600/60)
		if offset%60 != 0 {
			value += fmt.Sprintf("%02d", offset%60)
		}
		return value
	}
	add := func(at time.Time, from, to int, name string) {
		kind := "STANDARD"
		if at.In(zone).IsDST() {
			kind = "DAYLIGHT"
		}
		key := fmt.Sprintf("%s/%d/%d/%s", kind, from, to, name)
		// Observance DTSTART/RDATE is the local wall time BEFORE the transition.
		wall := at.UTC().Add(time.Duration(from) * time.Second).Format("20060102T150405")
		if group := groups[key]; group != nil {
			group.dates = append(group.dates, wall)
			return
		}
		component := ical.NewComponent(kind)
		component.Props.Set(&ical.Prop{Name: "DTSTART", Value: wall})
		component.Props.Set(&ical.Prop{Name: "TZOFFSETFROM", Value: offsetText(from)})
		component.Props.Set(&ical.Prop{Name: "TZOFFSETTO", Value: offsetText(to)})
		component.Props.SetText("TZNAME", name)
		group := &observance{component: component}
		groups[key], ordered = group, append(ordered, group)
	}
	name, offset := start.Zone()
	add(start, offset, offset, name) // Establish the offset even in fixed zones.
	for at, steps := start, 0; at.Before(end); steps++ {
		if steps > 50000 {
			return nil, fmt.Errorf("calendar timezone has too many transitions")
		}
		_, next := at.ZoneBounds()
		if next.IsZero() || next.After(end) {
			break
		}
		if !next.After(at) {
			// Go's POSIX extension may end at a synthetic year boundary in leap
			// years. Move to the next year rather than looping on that boundary.
			next = time.Date(at.Year()+1, 1, 1, 0, 0, 0, 0, zone)
		}
		if next.After(end) {
			break
		}
		next = next.In(zone)
		newName, newOffset := next.Zone()
		if newOffset != offset || newName != name || next.IsDST() != at.IsDST() {
			add(next, offset, newOffset, newName)
		}
		at, offset, name = next, newOffset, newName
	}
	for _, group := range ordered {
		if len(group.dates) > 0 {
			group.component.Props.Set(&ical.Prop{Name: "RDATE", Value: strings.Join(group.dates, ",")})
		}
		tz.Children = append(tz.Children, group.component)
	}
	return tz, nil
}
