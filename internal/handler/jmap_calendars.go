package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
)

// listJMAPCalendarEvents reads a JMAP calendar's events in the query window
// as a CalDAV calendar's are read: each converted to iCalendar and a series
// expanded into its occurrences. Gofer expands series itself, since servers
// that do it over JMAP are not reliable about changed occurrences.
func listJMAPCalendarEvents(ctx context.Context, source storage.CalendarSource, cfg *models.AccountConfig, password string, query calendar.EventQuery) (calendar.EventPage, error) {
	if query.WindowStart.IsZero() || query.WindowEnd.IsZero() || !query.WindowEnd.After(query.WindowStart) {
		return calendar.EventPage{}, fmt.Errorf("JMAP event window is invalid")
	}
	calendarID, name, ok := jmapCalendarDAVParse(source.RemoteID)
	if !ok || name != "" {
		return calendar.EventPage{}, fmt.Errorf("JMAP calendar has no valid id")
	}
	pulled, err := mail.PullJMAPCalendarEvents(ctx, cfg, password, calendarID, query.WindowStart, query.WindowEnd)
	if err != nil {
		return calendar.EventPage{}, err
	}
	location := query.WindowStart.Location()
	if source.TimeZone != "" {
		if location, err = time.LoadLocation(source.TimeZone); err != nil {
			return calendar.EventPage{}, fmt.Errorf("load calendar timezone: %w", err)
		}
	}
	return jmapCalendarEventPage(pulled.Events, source.RemoteID, query.WindowStart, query.WindowEnd, location)
}

// jmapCalendarChangeTag is the change tag of a JMAP calendar: the account's
// event state, which changes when any of its calendars does.
func jmapCalendarChangeTag(ctx context.Context, cfg *models.AccountConfig, password string) (string, error) {
	state, err := mail.JMAPCalendarState(ctx, cfg, password)
	if err != nil || state == "" {
		return "", err
	}
	return "jmap:" + state, nil
}

// jmapCalendarEventPage turns JMAP events into the window's events and
// occurrences, with the ids CalDAV ones get: the event's resource URL in the
// collection, and for an occurrence that with "#recurrence=" and the
// original start.
func jmapCalendarEventPage(events []jmap.CalendarEvent, collection string, start, end time.Time, location *time.Location) (calendar.EventPage, error) {
	page := calendar.EventPage{}
	seen := make(map[string]bool)
	for _, event := range events {
		id := event.ID()
		if id == "" {
			return calendar.EventPage{}, fmt.Errorf("JMAP returned an event without an id")
		}
		remote, err := jmapCalendarOccurrences(event, jmapCalendarDAVResource(collection, id), start, end, location)
		if err != nil {
			return calendar.EventPage{}, fmt.Errorf("read JMAP event %s: %w", id, err)
		}
		for _, occurrence := range remote {
			if seen[occurrence.RemoteID] {
				return calendar.EventPage{}, fmt.Errorf("JMAP returned a duplicate event instance")
			}
			seen[occurrence.RemoteID] = true
			page.Events = append(page.Events, occurrence)
			if len(page.Events) > calendarSyncMaxEvents {
				return calendar.EventPage{}, fmt.Errorf("JMAP event traversal exceeded its event limit")
			}
		}
	}
	return page, nil
}

// jmapCalendarOccurrences reads one JMAP event: the event itself, or the
// occurrences of a series that overlap the window. A changed occurrence is
// read from its own VEVENT, wherever it was moved to.
func jmapCalendarOccurrences(event jmap.CalendarEvent, id string, start, end time.Time, location *time.Location) ([]calendar.RemoteEvent, error) {
	expanded, err := jmapCalendarExpand(event, id, start, end, location)
	if err != nil {
		return nil, err
	}
	out := make([]calendar.RemoteEvent, 0, len(expanded))
	for _, occurrence := range expanded {
		out = append(out, occurrence.remote)
	}
	return out, nil
}

// jmapOccurrence is an event or occurrence as an iCalendar VEVENT, and as
// Gofer reads it.
type jmapOccurrence struct {
	component *ical.Component
	remote    calendar.RemoteEvent
}

// jmapCalendarExpand is jmapCalendarOccurrences with each one's VEVENT, as
// a CalDAV server's expanded calendar data has them.
func jmapCalendarExpand(event jmap.CalendarEvent, id string, start, end time.Time, location *time.Location) ([]jmapOccurrence, error) {
	etag := jmapCalendarEventETag(event)
	cal, err := mail.JMAPEventCalendar(event)
	if err != nil {
		return nil, err
	}
	components := cal.Events()
	master := components[0]
	overlaps := func(remote calendar.RemoteEvent) bool {
		if remote.AllDay {
			first, last := start.In(location).Format("2006-01-02"), end.In(location).Format("2006-01-02")
			return remote.StartDate <= last && remote.EndDate > first
		}
		return remote.StartAt != nil && remote.EndAt != nil && remote.StartAt.Before(end) && (remote.EndAt.After(start) || remote.EndAt.Equal(*remote.StartAt) && !remote.StartAt.Before(start))
	}
	if master.Props.Get(ical.PropRecurrenceRule) == nil && master.Props.Get(ical.PropRecurrenceDates) == nil {
		remote, err := normalizeCalDAVEvent(master, id, etag, location)
		if err != nil || !overlaps(remote) {
			return nil, err
		}
		return []jmapOccurrence{{master.Component, remote}}, nil
	}

	startProp := master.Props.Get(ical.PropDateTimeStart)
	allDay := startProp.ValueType() == ical.ValueDate
	zone := location
	switch {
	case allDay:
		zone = time.UTC
	case startProp.Params.Get(ical.ParamTimezoneID) != "":
		if zone, err = time.LoadLocation(startProp.Params.Get(ical.ParamTimezoneID)); err != nil {
			return nil, err
		}
	case len(startProp.Value) > 0 && startProp.Value[len(startProp.Value)-1] == 'Z':
		zone = time.UTC
	}
	first, err := master.DateTimeStart(zone)
	if err != nil {
		return nil, err
	}
	last, err := master.DateTimeEnd(zone)
	if err != nil {
		return nil, err
	}
	days := 0
	span := last.Sub(first)
	if allDay {
		days = int(last.Sub(first).Hours()/24 + 0.5)
	}
	set, err := master.RecurrenceSet(zone)
	if err != nil {
		return nil, err
	}

	var out []jmapOccurrence
	changed := make(map[string]bool)
	for _, component := range components[1:] {
		key, err := calendarRecurrenceKey(component.Props.Get(ical.PropRecurrenceID), zone)
		if err != nil {
			return nil, err
		}
		changed[key] = true
		remote, err := normalizeCalDAVEvent(component, id, etag, location)
		if err != nil {
			return nil, err
		}
		if overlaps(remote) || (remote.Deleted && remote.StartAt == nil && remote.StartDate == "") {
			out = append(out, jmapOccurrence{component.Component, remote})
		}
	}
	// An occurrence that began up to its length before the window still
	// overlaps it.
	for _, at := range set.Between(start.Add(-span-24*time.Hour), end, true) {
		occurrence := cloneCalendarComponent(master.Component)
		for _, name := range []string{ical.PropRecurrenceRule, ical.PropRecurrenceDates, ical.PropExceptionDates, "EXRULE", ical.PropDateTimeStart, ical.PropDateTimeEnd, ical.PropDuration} {
			occurrence.Props.Del(name)
		}
		occurrenceEnd := at.Add(span)
		if allDay {
			occurrenceEnd = at.AddDate(0, 0, days)
		}
		occurrence.Props.Set(jmapOccurrenceDate(ical.PropRecurrenceID, startProp, at))
		occurrence.Props.Set(jmapOccurrenceDate(ical.PropDateTimeStart, startProp, at))
		occurrence.Props.Set(jmapOccurrenceDate(ical.PropDateTimeEnd, startProp, occurrenceEnd))
		key, err := calendarRecurrenceKey(occurrence.Props.Get(ical.PropRecurrenceID), zone)
		if err != nil {
			return nil, err
		}
		if changed[key] {
			continue
		}
		remote, err := normalizeCalDAVEvent(ical.Event{Component: occurrence}, id, etag, location)
		if err != nil {
			return nil, err
		}
		if overlaps(remote) {
			out = append(out, jmapOccurrence{occurrence, remote})
		}
	}
	return out, nil
}

// jmapOccurrenceDate writes an occurrence's date in the form of the series
// start: a DATE, a UTC time, a time in the start's zone, or a floating time.
func jmapOccurrenceDate(name string, start *ical.Prop, at time.Time) *ical.Prop {
	prop := ical.NewProp(name)
	switch {
	case start.ValueType() == ical.ValueDate:
		prop.Params.Set(ical.ParamValue, string(ical.ValueDate))
		prop.Value = at.Format("20060102")
	case start.Params.Get(ical.ParamTimezoneID) != "":
		prop.Params.Set(ical.ParamTimezoneID, start.Params.Get(ical.ParamTimezoneID))
		prop.Value = at.Format("20060102T150405")
	case len(start.Value) > 0 && start.Value[len(start.Value)-1] == 'Z':
		prop.Value = at.UTC().Format("20060102T150405Z")
	default:
		prop.Value = at.Format("20060102T150405")
	}
	return prop
}

// jmapCalendarEventETag stands in for an ETag, which JMAP events don't
// have: it changes whenever anything in the event does.
func jmapCalendarEventETag(event jmap.CalendarEvent) string {
	data, _ := json.Marshal(event)
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// jmapCalendarSources are a JMAP account's calendars as Gofer's calendar
// sources. The default calendar starts selected, as the first does for
// CalDAV.
func jmapCalendarSources(userID, accountID string, calendars []jmap.Calendar) []storage.CalendarSource {
	sources := make([]storage.CalendarSource, 0, len(calendars))
	hasDefault := false
	for _, c := range calendars {
		hasDefault = hasDefault || c.IsDefault
	}
	for i, c := range calendars {
		if !c.MyRights.MayReadItems && c.MyRights != (jmap.CalendarRights{}) {
			continue
		}
		role := "reader"
		if c.MyRights.MayWriteAll || c.MyRights.MayWriteOwn {
			role = "writer"
		}
		primary := c.IsDefault || (!hasDefault && i == 0)
		sources = append(sources, storage.CalendarSource{
			UserID: userID, AccountID: accountID, Provider: storage.CalendarSourceProviderJMAP,
			RemoteID: jmapCalendarDAVCollection(c.ID), Name: c.Name, Description: c.Description, TimeZone: jmapCalendarZone(c.TimeZone),
			Color: c.Color, AccessRole: role, IsPrimary: primary, IsSelected: primary,
		})
	}
	return sources
}

// jmapCalendarZone keeps a calendar's zone only when Gofer can load it.
func jmapCalendarZone(zone string) string {
	if zone == "" {
		return ""
	}
	if _, err := time.LoadLocation(zone); err != nil {
		return ""
	}
	return zone
}

// calendarProviderUsesOAuth reports whether a calendar provider signs in
// with the account's OAuth authorization: Google and Outlook. CalDAV and JMAP
// calendars use a password.
func calendarProviderUsesOAuth(provider string) bool {
	return !storage.CalendarSourceMailServer(provider)
}

// jmapConnection is the JMAP connection and password of the request's
// account, for its JMAP calendars.
func (p *userCalendarRequest) jmapConnection(ctx context.Context) (*models.AccountConfig, string, error) {
	service := p.service()
	if service == nil {
		return nil, "", fmt.Errorf("calendar account is not available")
	}
	cfg, err := service.JMAPConfig(ctx)
	if err != nil {
		return nil, "", err
	}
	password, err := service.MailboxPassword()
	return cfg, password, err
}

// calendarDiscoveryProtocol is the provider a discovery request asks for: a
// mail-server account's calendars come from CalDAV unless the form chose
// JMAP; other accounts keep their provider.
func calendarDiscoveryProtocol(r *http.Request, current string) string {
	if current != storage.CalendarSourceProviderCalDAV && current != storage.CalendarSourceProviderJMAP {
		return current
	}
	if strings.TrimSpace(r.FormValue("calendar_protocol")) == storage.CalendarSourceProviderJMAP {
		return storage.CalendarSourceProviderJMAP
	}
	return storage.CalendarSourceProviderCalDAV
}

// discoverJMAP lists the request account's JMAP calendars, for the per-user
// stores.
func (p *userCalendarRequest) discoverJMAP(ctx context.Context) ([]storage.CalendarSource, error) {
	cfg, password, err := p.jmapConnection(ctx)
	if err != nil {
		return nil, err
	}
	if err := p.ready(ctx); err != nil {
		return nil, err
	}
	calendars, err := mail.JMAPCalendars(ctx, cfg, password)
	if err != nil {
		return nil, p.recordRetry(ctx, err)
	}
	if len(calendars) > calendarDiscoveryMaxSources {
		return nil, errors.New("JMAP discovery exceeded its source limit")
	}
	service := p.service()
	return jmapCalendarSources(service.OwnerID(), service.AccountID(), calendars), nil
}

// discoverJMAPCalendarSources lists a shared-store JMAP account's calendars
// and saves them as its calendar sources, in place of any CalDAV ones.
func (h *Handler) discoverJMAPCalendarSources(ctx context.Context, userID, accountID string) (int, error) {
	cfg, password, err := h.jmapAccountConnection(ctx, accountID)
	if err != nil {
		return 0, err
	}
	calendars, err := mail.JMAPCalendars(ctx, cfg, password)
	if err != nil {
		return 0, err
	}
	sources := jmapCalendarSources(userID, accountID, calendars)
	if err := h.db.ReplaceCalendarSources(ctx, userID, accountID, storage.CalendarSourceProviderJMAP, sources); err != nil {
		return 0, fmt.Errorf("store discovered JMAP calendars: %w", err)
	}
	return len(sources), nil
}

// readJMAPLogin reads the JMAP sign-in of a JMAP calendar's account for an
// action on it, which the CalDAV code then uses through jmapCalendarDAV.
func (p *userCalendarRequest) readJMAPLogin(ctx context.Context) error {
	if p.provider() != storage.CalendarSourceProviderJMAP {
		return nil
	}
	cfg, password, err := p.jmapConnection(ctx)
	if err != nil {
		return err
	}
	p.jmapLogin = calendarCredentials{jmap: cfg, password: password, baseURL: jmapCalendarDAVOrigin, username: "jmap"}
	return nil
}
