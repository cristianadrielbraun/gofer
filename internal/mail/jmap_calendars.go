package mail

import (
	"context"
	"errors"
	"fmt"

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
