package mail

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

var (
	// ErrJMAPNoCalendars means the login reaches the JMAP server but is not
	// offered calendars, as with a server without JMAP for Calendars or an
	// API token made without the calendars permission.
	ErrJMAPNoCalendars = errors.New("the JMAP server doesn't offer calendars for this login; if you signed in with an API token, create one that also has the calendars permission")
	// ErrJMAPCalendarsCannotParse means the server can't turn iCalendar data
	// into its events, which Gofer needs to save events there.
	ErrJMAPCalendarsCannotParse = errors.New("the JMAP server can't read iCalendar data, so Gofer can't save events there; use CalDAV for this account's calendars instead")
)

// JMAPCalendars lists the calendars of a JMAP account.
func JMAPCalendars(ctx context.Context, cfg *models.AccountConfig, password string) ([]jmap.Calendar, error) {
	client, session, account, err := jmapCalendarsSession(ctx, cfg, password)
	if err != nil {
		return nil, err
	}
	calendars, err := client.GetCalendars(ctx, session, account)
	if err != nil {
		return nil, err
	}
	return calendars.List, nil
}

// CreateJMAPCalendar makes a calendar named name on the account's JMAP server
// and returns its id.
func CreateJMAPCalendar(ctx context.Context, cfg *models.AccountConfig, password, name string) (string, error) {
	client, session, account, err := jmapCalendarsSession(ctx, cfg, password)
	if err != nil {
		return "", err
	}
	return client.CreateCalendar(ctx, session, account, name)
}

// JMAPEventsFromICS has the server read an iCalendar file into JMAP events,
// one for each UID in it, in the form that server stores them.
func JMAPEventsFromICS(ctx context.Context, cfg *models.AccountConfig, password string, data []byte) ([]jmap.CalendarEvent, error) {
	client, session, account, err := jmapCalendarsSession(ctx, cfg, password)
	if err != nil {
		return nil, err
	}
	return jmapParseICS(ctx, client, session, account, data)
}

func jmapParseICS(ctx context.Context, client *jmap.Client, session *jmap.Session, account string, data []byte) ([]jmap.CalendarEvent, error) {
	if _, ok := session.Capabilities[jmap.CapabilityCalendarsParse]; !ok {
		return nil, ErrJMAPCalendarsCannotParse
	}
	blob, err := client.UploadBlob(ctx, session, account, "text/calendar", data)
	if err != nil {
		return nil, err
	}
	parsed, err := client.ParseCalendarEvents(ctx, session, account, []string{blob})
	if err != nil {
		return nil, err
	}
	events := parsed[blob]
	if len(events) == 0 {
		return nil, fmt.Errorf("CalendarEvent/parse: the server found no event in the iCalendar data")
	}
	return events, nil
}

func jmapCalendarsSession(ctx context.Context, cfg *models.AccountConfig, password string) (*jmap.Client, *jmap.Session, string, error) {
	client, err := newJMAPAccountClient(cfg, password)
	if err != nil {
		return nil, nil, "", err
	}
	session, err := client.Session(ctx, cfg.JMAPSessionURL)
	if err != nil {
		return nil, nil, "", fmt.Errorf("JMAP session: %w", jmapLoginError(err))
	}
	account := session.PrimaryAccount(jmap.CapabilityCalendars)
	if _, ok := session.Capabilities[jmap.CapabilityCalendars]; !ok || account == "" {
		return nil, nil, "", ErrJMAPNoCalendars
	}
	return client, session, account, nil
}

// jmapCalendarPageSize is how many events are fetched per request when the
// server sets no lower limit.
const jmapCalendarPageSize = 500

// JMAPCalendarEvents are the events of one calendar that take place in a
// time range: each series once, unexpanded, as the server stores it. State
// is the account's event state from before they were read, so a change made
// while they are read is seen by the next check.
type JMAPCalendarEvents struct {
	Events []jmap.CalendarEvent
	State  string
}

// PullJMAPCalendarEvents fetches the events of a calendar that overlap
// [start, end), series included when an occurrence does.
func PullJMAPCalendarEvents(ctx context.Context, cfg *models.AccountConfig, password, calendarID string, start, end time.Time) (*JMAPCalendarEvents, error) {
	client, session, account, err := jmapCalendarsSession(ctx, cfg, password)
	if err != nil {
		return nil, err
	}
	current, err := client.GetCalendarEvents(ctx, session, account, []string{})
	if err != nil {
		return nil, err
	}
	out := &JMAPCalendarEvents{State: current.State}
	limit := jmapCalendarPageSize
	if most := session.Core.MaxObjectsInGet; most > 0 && most < limit {
		limit = most
	}
	filter := jmap.CalendarEventFilter{InCalendar: calendarID, After: start.UTC().Format(time.RFC3339), Before: end.UTC().Format(time.RFC3339)}
	for position := 0; ; {
		query, err := client.QueryCalendarEvents(ctx, session, account, filter, position, limit)
		if err != nil {
			return nil, err
		}
		if len(query.IDs) > 0 {
			events, err := client.GetCalendarEvents(ctx, session, account, query.IDs)
			if err != nil {
				return nil, err
			}
			out.Events = append(out.Events, events.List...)
		}
		position += len(query.IDs)
		// A server that leaves out the total is read until a page is empty.
		if len(query.IDs) == 0 || (query.Total > 0 && position >= query.Total) {
			return out, nil
		}
	}
}

// JMAPCalendarState is the account's calendar event state, which changes
// whenever any of its events does.
func JMAPCalendarState(ctx context.Context, cfg *models.AccountConfig, password string) (string, error) {
	client, session, account, err := jmapCalendarsSession(ctx, cfg, password)
	if err != nil {
		return "", err
	}
	current, err := client.GetCalendarEvents(ctx, session, account, []string{})
	if err != nil {
		return "", err
	}
	return current.State, nil
}

// CreateJMAPCalendarEvent saves a new event in a calendar and gives its id.
func CreateJMAPCalendarEvent(ctx context.Context, cfg *models.AccountConfig, password, calendarID string, event jmap.CalendarEvent) (string, error) {
	client, session, account, err := jmapCalendarsSession(ctx, cfg, password)
	if err != nil {
		return "", err
	}
	event["calendarIds"] = map[string]any{calendarID: true}
	delete(event, "id")
	result, err := client.SetCalendarEvents(ctx, session, jmap.CalendarEventSet{AccountID: account, Create: map[string]jmap.CalendarEvent{"new": event}})
	if err != nil {
		return "", err
	}
	if failure, ok := result.NotCreated["new"]; ok {
		return "", &failure
	}
	id := result.Created["new"].ID()
	if id == "" {
		return "", errors.New("CalendarEvent/set: the server gave the new event no id")
	}
	return id, nil
}

// DeleteJMAPCalendarEvent destroys an event. One already gone counts as
// deleted.
func DeleteJMAPCalendarEvent(ctx context.Context, cfg *models.AccountConfig, password, id string) error {
	client, session, account, err := jmapCalendarsSession(ctx, cfg, password)
	if err != nil {
		return err
	}
	result, err := client.SetCalendarEvents(ctx, session, jmap.CalendarEventSet{AccountID: account, Destroy: []string{id}})
	if err != nil {
		return err
	}
	if failure, ok := result.NotDestroyed[id]; ok && failure.Type != "notFound" {
		return &failure
	}
	return nil
}

// JMAPCalendarConnection is one JMAP session for a series of calendar
// writes, so each step doesn't open the session again.
type JMAPCalendarConnection struct {
	client  *jmap.Client
	session *jmap.Session
	account string
}

// OpenJMAPCalendars opens a JMAP session for calendar writes.
func OpenJMAPCalendars(ctx context.Context, cfg *models.AccountConfig, password string) (*JMAPCalendarConnection, error) {
	client, session, account, err := jmapCalendarsSession(ctx, cfg, password)
	if err != nil {
		return nil, err
	}
	return &JMAPCalendarConnection{client: client, session: session, account: account}, nil
}

// Event fetches one event, or nil when the server has no such event.
func (c *JMAPCalendarConnection) Event(ctx context.Context, id string) (jmap.CalendarEvent, error) {
	events, err := c.client.GetCalendarEvents(ctx, c.session, c.account, []string{id})
	if err != nil {
		return nil, err
	}
	for _, event := range events.List {
		if event.ID() == id {
			return event, nil
		}
	}
	return nil, nil
}

// ByUID finds the event with a uid in a calendar, or nil. A server may
// index a new event for search a moment after saving it.
func (c *JMAPCalendarConnection) ByUID(ctx context.Context, calendarID, uid string) (jmap.CalendarEvent, error) {
	query, err := c.client.QueryCalendarEvents(ctx, c.session, c.account, jmap.CalendarEventFilter{InCalendar: calendarID, UID: uid}, 0, 2)
	if err != nil || len(query.IDs) == 0 {
		return nil, err
	}
	return c.Event(ctx, query.IDs[0])
}

// FromICS has the server read iCalendar data into events.
func (c *JMAPCalendarConnection) FromICS(ctx context.Context, data []byte) ([]jmap.CalendarEvent, error) {
	return jmapParseICS(ctx, c.client, c.session, c.account, data)
}

// Create saves a new event in a calendar and gives its id.
func (c *JMAPCalendarConnection) Create(ctx context.Context, calendarID string, event jmap.CalendarEvent) (string, error) {
	event["calendarIds"] = map[string]any{calendarID: true}
	delete(event, "id")
	result, err := c.client.SetCalendarEvents(ctx, c.session, jmap.CalendarEventSet{AccountID: c.account, Create: map[string]jmap.CalendarEvent{"new": event}})
	if err != nil {
		return "", err
	}
	if failure, ok := result.NotCreated["new"]; ok {
		return "", &failure
	}
	id := result.Created["new"].ID()
	if id == "" {
		return "", errors.New("CalendarEvent/set: the server gave the new event no id")
	}
	return id, nil
}

// Update applies a patch to an event. It reports false when the event is
// gone.
func (c *JMAPCalendarConnection) Update(ctx context.Context, id string, patch jmap.PatchObject) (bool, error) {
	result, err := c.client.SetCalendarEvents(ctx, c.session, jmap.CalendarEventSet{AccountID: c.account, Update: map[string]jmap.PatchObject{id: patch}})
	if err != nil {
		return false, err
	}
	if failure, ok := result.NotUpdated[id]; ok {
		if failure.Type == "notFound" {
			return false, nil
		}
		return false, &failure
	}
	return true, nil
}

// Delete destroys an event. It reports false when the event was already
// gone.
func (c *JMAPCalendarConnection) Delete(ctx context.Context, id string) (bool, error) {
	result, err := c.client.SetCalendarEvents(ctx, c.session, jmap.CalendarEventSet{AccountID: c.account, Destroy: []string{id}})
	if err != nil {
		return false, err
	}
	if failure, ok := result.NotDestroyed[id]; ok {
		if failure.Type == "notFound" {
			return false, nil
		}
		return false, &failure
	}
	return true, nil
}

// jmapEventServerProperties are set by the server or name where an event
// is kept, never sent back in a patch.
var jmapEventServerProperties = map[string]bool{
	"id": true, "@type": true, "uid": true, "calendarIds": true,
	"isDraft": true, "isOrigin": true, "baseEventId": true, "utcStart": true, "utcEnd": true,
	"blobId": true, "isRecurrenceExpanded": true,
}

// JMAPEventPatch is what to change on the server to turn an event read as
// before into after, both as the server read them from iCalendar. Only the
// properties that differ are replaced, so ones Gofer's iCalendar doesn't
// carry stay on the server as they are. That includes iCalendar, the
// details the server keeps aside, such as Gofer's own X- properties.
func JMAPEventPatch(before, after jmap.CalendarEvent) jmap.PatchObject {
	patch := jmap.PatchObject{}
	for key, value := range after {
		if !jmapEventServerProperties[key] && !reflect.DeepEqual(before[key], value) {
			patch[key] = value
		}
	}
	for key := range before {
		if _, kept := after[key]; !kept && !jmapEventServerProperties[key] {
			patch[key] = nil
		}
	}
	return patch
}
