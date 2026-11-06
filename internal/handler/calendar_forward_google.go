package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	ical "github.com/emersion/go-ical"
)

// Google's API lets a guest change only their own copy of a meeting, so a
// Google invitation is forwarded by email, like a CalDAV one. Google gives
// the event as JSON; this makes the invitation other calendars read. For a
// series, exceptions are its dates that were changed or deleted on their own.
func googleCalendarForwardCalendar(remote googleCalendarEvent, exceptions []googleCalendarUpdateEvent) (*ical.Calendar, error) {
	if strings.TrimSpace(remote.ICalUID) == "" || calendarReplyAddress("mailto:"+remote.Organizer.Email) == "" {
		return nil, errors.New("Google did not return this invitation's identity")
	}
	cal := ical.NewCalendar()
	cal.Props.SetText("PRODID", "-//Gofer//Calendar//EN")
	cal.Props.SetText("VERSION", "2.0")
	series, err := googleCalendarForwardEvent(cal, remote)
	if err != nil {
		return nil, err
	}
	cal.Children = append(cal.Children, series)
	for _, exception := range exceptions {
		if exception.OriginalStartTime == nil || exception.ICalUID != remote.ICalUID {
			continue
		}
		date, err := googleCalendarForwardRecurrenceID(remote, *exception.OriginalStartTime)
		if err != nil {
			return nil, err
		}
		if exception.Status == "cancelled" {
			date.Name = "EXDATE"
			series.Props.Add(date)
			continue
		}
		exception.Recurrence = nil
		changed, err := googleCalendarForwardEvent(cal, exception.googleCalendarEvent)
		if err != nil {
			return nil, err
		}
		changed.Props.Set(date)
		cal.Children = append(cal.Children, changed)
	}
	return cal, nil
}

// googleCalendarForwardEvent is one of Google's events as a VEVENT; its
// time zone goes in cal.
func googleCalendarForwardEvent(cal *ical.Calendar, remote googleCalendarEvent) (*ical.Component, error) {
	event := ical.NewEvent()
	event.Props.SetText("UID", remote.ICalUID)
	event.Props.SetDateTime("DTSTAMP", time.Now().UTC())
	text := func(value string) string {
		return strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
	}
	event.Props.SetText("SUMMARY", text(remote.Summary))
	description := calendarDescriptionText(remote.Description)
	if link := calendar.SafeMeetingURL(remote.HangoutLink); link != "" && !strings.Contains(description, link) {
		description = strings.TrimSpace(description + "\n\nJoin with Google Meet: " + link)
	}
	if description != "" {
		event.Props.SetText("DESCRIPTION", text(description))
	}
	if html := calendar.DescriptionHTML(remote.Description); html != "" && calendarDescriptionText(remote.Description) != "" {
		event.Props.SetText("X-ALT-DESC", html)
		event.Props.Get("X-ALT-DESC").Params.Set("FMTTYPE", "text/html")
	}
	if remote.Location != "" {
		event.Props.SetText("LOCATION", text(remote.Location))
	}
	event.Props.SetText("STATUS", "CONFIRMED")
	if remote.Transparency == "transparent" {
		event.Props.SetText("TRANSP", "TRANSPARENT")
	}
	organizer := &ical.Prop{Name: "ORGANIZER", Value: "mailto:" + remote.Organizer.Email, Params: ical.Params{}}
	if remote.Organizer.DisplayName != "" {
		organizer.Params.Set("CN", remote.Organizer.DisplayName)
	}
	event.Props.Set(organizer)
	var attendees []struct {
		Email, DisplayName, ResponseStatus string
		Optional, Resource                 bool
	}
	if len(remote.Attendees) > 0 && json.Unmarshal(remote.Attendees, &attendees) != nil {
		return nil, errors.New("Google returned unreadable guests")
	}
	for _, person := range attendees {
		if calendarReplyAddress("mailto:"+person.Email) == "" {
			continue
		}
		attendee := ical.Prop{Name: "ATTENDEE", Value: "mailto:" + person.Email, Params: ical.Params{}}
		if person.DisplayName != "" {
			attendee.Params.Set("CN", person.DisplayName)
		}
		status := map[string]string{"accepted": "ACCEPTED", "declined": "DECLINED", "tentative": "TENTATIVE"}[person.ResponseStatus]
		if status == "" {
			status = "NEEDS-ACTION"
		}
		attendee.Params.Set("PARTSTAT", status)
		role := "REQ-PARTICIPANT"
		if person.Optional {
			role = "OPT-PARTICIPANT"
		}
		attendee.Params.Set("ROLE", role)
		if person.Resource {
			attendee.Params.Set("CUTYPE", "RESOURCE")
		}
		event.Props["ATTENDEE"] = append(event.Props["ATTENDEE"], attendee)
	}
	if err := googleCalendarForwardTimes(cal, event.Component, remote); err != nil {
		return nil, err
	}
	for _, line := range remote.Recurrence {
		prop, err := googleCalendarRecurrenceProp(line)
		if err != nil {
			return nil, err
		}
		event.Props.Add(prop)
	}
	return event.Component, nil
}

// googleCalendarForwardZone is the zone an event was made in, or UTC.
func googleCalendarForwardZone(remote googleCalendarEvent) *time.Location {
	zone, err := time.LoadLocation(remote.Start.TimeZone)
	if remote.Start.TimeZone == "" || err != nil || calendarZoneIsUTC(zone) {
		return time.UTC
	}
	return zone
}

// googleCalendarForwardRecurrenceID names a date of the series as its start
// does: a date, or a time in the series' zone.
func googleCalendarForwardRecurrenceID(series googleCalendarEvent, original googleCalendarEventDateTime) (*ical.Prop, error) {
	prop := ical.NewProp("RECURRENCE-ID")
	if original.Date != "" {
		date, err := time.Parse("2006-01-02", original.Date)
		if err != nil {
			return nil, err
		}
		prop.SetDate(date)
		return prop, nil
	}
	at, err := time.Parse(time.RFC3339, original.DateTime)
	if err != nil {
		return nil, fmt.Errorf("invalid date of the series: %w", err)
	}
	prop.SetDateTime(at.In(googleCalendarForwardZone(series)))
	return prop, nil
}

// Start and end in the zone the event was made in, with that zone's rules,
// so a repeating meeting keeps its time across daylight-saving changes.
func googleCalendarForwardTimes(cal *ical.Calendar, event *ical.Component, remote googleCalendarEvent) error {
	if remote.Start.Date != "" {
		start, err := time.Parse("2006-01-02", remote.Start.Date)
		if err != nil {
			return err
		}
		end, err := time.Parse("2006-01-02", remote.End.Date)
		if err != nil {
			return err
		}
		event.Props.SetDate("DTSTART", start)
		event.Props.SetDate("DTEND", end)
		return nil
	}
	start, err := time.Parse(time.RFC3339, remote.Start.DateTime)
	if err != nil {
		return fmt.Errorf("invalid start: %w", err)
	}
	end, err := time.Parse(time.RFC3339, remote.End.DateTime)
	if err != nil {
		return fmt.Errorf("invalid end: %w", err)
	}
	zone := googleCalendarForwardZone(remote)
	if zone == time.UTC {
		event.Props.SetDateTime("DTSTART", start.UTC())
		calendarSetICSEnd(event.Props, start, end.UTC())
		return nil
	}
	event.Props.SetDateTime("DTSTART", start.In(zone))
	calendarSetICSEnd(event.Props, start, end.In(zone))
	// The zone's rules for the next twenty years, or as long as the event.
	until := end.In(zone)
	if len(remote.Recurrence) > 0 {
		until = until.AddDate(20, 0, 0)
	}
	for _, child := range cal.Children {
		if name, _ := child.Props.Text("TZID"); child.Name == ical.CompTimezone && name == zone.String() {
			return nil // A changed date in the series' zone shares its rules.
		}
	}
	timezone, err := calendarRecurrenceTimezone(calendar.EventDraft{TimeZone: zone.String(), StartAt: &start, EndAt: &end, Recurrence: &calendar.RecurrenceDraft{Until: until.Format("2006-01-02")}}, zone)
	if err != nil {
		return err
	}
	cal.Children = append(cal.Children, timezone)
	return nil
}

// Google gives a series' rules as iCalendar lines, such as
// "RRULE:FREQ=WEEKLY;BYDAY=MO" or "EXDATE;TZID=Europe/Prague:20261110T100000".
func googleCalendarRecurrenceProp(line string) (*ical.Prop, error) {
	line = strings.TrimSpace(line)
	name := strings.ToUpper(strings.SplitN(strings.SplitN(line, ":", 2)[0], ";", 2)[0])
	if strings.ContainsAny(line, "\r\n") || (name != "RRULE" && name != "EXDATE" && name != "RDATE" && name != "EXRULE") {
		return nil, fmt.Errorf("unsupported repeat rule %q", line)
	}
	cal, err := calendarUpdateDecodeICS([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Gofer//Calendar//EN\r\nBEGIN:VEVENT\r\nUID:rule\r\nDTSTAMP:20260101T000000Z\r\n" + line + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	if err != nil || len(cal.Events()) != 1 || len(cal.Events()[0].Props[name]) != 1 {
		return nil, fmt.Errorf("unreadable repeat rule %q", line)
	}
	prop := cal.Events()[0].Props[name][0]
	return &prop, nil
}
