package jmap

import (
	"context"
	"encoding/json"
	"fmt"
)

// JMAP for Calendars (draft-ietf-jmap-calendars). Parse turns uploaded
// iCalendar data into events; servers offer it separately.
const (
	CapabilityCalendars      = "urn:ietf:params:jmap:calendars"
	CapabilityCalendarsParse = "urn:ietf:params:jmap:calendars:parse"
)

// Calendar is a calendar the account can see.
type Calendar struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Description  string         `json:"description"`
	Color        string         `json:"color"`
	SortOrder    int            `json:"sortOrder"`
	IsSubscribed bool           `json:"isSubscribed"`
	IsVisible    bool           `json:"isVisible"`
	IsDefault    bool           `json:"isDefault"`
	TimeZone     string         `json:"timeZone"`
	MyRights     CalendarRights `json:"myRights"`
}

// CalendarRights are what the user may do with a calendar's events.
type CalendarRights struct {
	MayReadFreeBusy  bool `json:"mayReadFreeBusy"`
	MayReadItems     bool `json:"mayReadItems"`
	MayWriteAll      bool `json:"mayWriteAll"`
	MayWriteOwn      bool `json:"mayWriteOwn"`
	MayUpdatePrivate bool `json:"mayUpdatePrivate"`
	MayRSVP          bool `json:"mayRSVP"`
	MayDelete        bool `json:"mayDelete"`
}

// CalendarEvent is a JSCalendar event as the server sends it. JSCalendar
// has far more properties than Gofer uses, and servers differ in which
// version of it they speak, so events stay generic JSON objects.
type CalendarEvent map[string]any

// ID is the event's JMAP id.
func (e CalendarEvent) ID() string {
	id, _ := e["id"].(string)
	return id
}

// CalendarEventFilter limits CalendarEvent/query to events in a calendar
// and overlapping a time range (UTC, RFC 3339); empty fields don't limit.
type CalendarEventFilter struct {
	InCalendar string `json:"inCalendar,omitempty"`
	After      string `json:"after,omitempty"`
	Before     string `json:"before,omitempty"`
}

// GetCalendars runs Calendar/get for every calendar.
func (c *Client) GetCalendars(ctx context.Context, session *Session, accountID string) (*GetResponse[Calendar], error) {
	args := struct {
		AccountID string   `json:"accountId"`
		IDs       []string `json:"ids"`
	}{accountID, nil}
	responses, err := c.Call(ctx, session, []string{CapabilityCalendars},
		Invocation{Name: "Calendar/get", Args: args, CallID: "0"})
	if err != nil {
		return nil, err
	}
	var result GetResponse[Calendar]
	if err := responses.Get("0", &result); err != nil {
		return nil, fmt.Errorf("Calendar/get: %w", err)
	}
	return &result, nil
}

// GetCalendarEvents runs CalendarEvent/get. An empty ids still answers with
// the current state.
func (c *Client) GetCalendarEvents(ctx context.Context, session *Session, accountID string, ids []string) (*GetResponse[CalendarEvent], error) {
	args := struct {
		AccountID string   `json:"accountId"`
		IDs       []string `json:"ids"`
	}{accountID, ids}
	responses, err := c.Call(ctx, session, []string{CapabilityCalendars},
		Invocation{Name: "CalendarEvent/get", Args: args, CallID: "0"})
	if err != nil {
		return nil, err
	}
	var result GetResponse[CalendarEvent]
	if err := responses.Get("0", &result); err != nil {
		return nil, fmt.Errorf("CalendarEvent/get: %w", err)
	}
	return &result, nil
}

// QueryCalendarEvents runs CalendarEvent/query for the ids of the events the
// filter matches, from position on, at most limit of them (0 leaves it to
// the server). Recurring events come back once, unexpanded.
func (c *Client) QueryCalendarEvents(ctx context.Context, session *Session, accountID string, filter CalendarEventFilter, position, limit int) (*QueryResponse, error) {
	args := struct {
		AccountID      string              `json:"accountId"`
		Filter         CalendarEventFilter `json:"filter"`
		Position       int                 `json:"position,omitempty"`
		Limit          int                 `json:"limit,omitempty"`
		CalculateTotal bool                `json:"calculateTotal"`
	}{accountID, filter, position, limit, true}
	responses, err := c.Call(ctx, session, []string{CapabilityCalendars},
		Invocation{Name: "CalendarEvent/query", Args: args, CallID: "0"})
	if err != nil {
		return nil, err
	}
	var result QueryResponse
	if err := responses.Get("0", &result); err != nil {
		return nil, fmt.Errorf("CalendarEvent/query: %w", err)
	}
	return &result, nil
}

// GetCalendarEventChanges runs CalendarEvent/changes. Keep calling with
// NewState while HasMoreChanges is true.
func (c *Client) GetCalendarEventChanges(ctx context.Context, session *Session, accountID, sinceState string, maxChanges int) (*ChangesResponse, error) {
	var result ChangesResponse
	if err := c.getChanges(ctx, session, CapabilityCalendars, "CalendarEvent/changes", changesArgs{accountID, sinceState, maxChanges}, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// CalendarEventSet are the CalendarEvent/set arguments. SendSchedulingMessages
// asks the server to send invitations, updates and replies itself.
type CalendarEventSet struct {
	AccountID              string                   `json:"accountId"`
	Create                 map[string]CalendarEvent `json:"create,omitempty"`
	Update                 map[string]PatchObject   `json:"update,omitempty"`
	Destroy                []string                 `json:"destroy,omitempty"`
	SendSchedulingMessages bool                     `json:"sendSchedulingMessages,omitempty"`
}

// CalendarEventSetResponse adds the created events, which hold the ids the
// server gave them, to the standard /set result.
type CalendarEventSetResponse struct {
	SetResponse
	Created    map[string]CalendarEvent `json:"created"`
	NotCreated map[string]SetError      `json:"notCreated"`
}

// SetCalendarEvents runs CalendarEvent/set.
func (c *Client) SetCalendarEvents(ctx context.Context, session *Session, args CalendarEventSet) (*CalendarEventSetResponse, error) {
	responses, err := c.Call(ctx, session, []string{CapabilityCalendars},
		Invocation{Name: "CalendarEvent/set", Args: args, CallID: "0"})
	if err != nil {
		return nil, err
	}
	var result CalendarEventSetResponse
	if err := responses.Get("0", &result); err != nil {
		return nil, fmt.Errorf("CalendarEvent/set: %w", err)
	}
	return &result, nil
}

// ParseCalendarEvents runs CalendarEvent/parse on uploaded iCalendar blobs
// and gives the events found in each, by blob id.
func (c *Client) ParseCalendarEvents(ctx context.Context, session *Session, accountID string, blobIDs []string) (map[string][]CalendarEvent, error) {
	args := struct {
		AccountID string   `json:"accountId"`
		BlobIDs   []string `json:"blobIds"`
	}{accountID, blobIDs}
	responses, err := c.Call(ctx, session, []string{CapabilityCalendars, CapabilityCalendarsParse},
		Invocation{Name: "CalendarEvent/parse", Args: args, CallID: "0"})
	if err != nil {
		return nil, err
	}
	var result struct {
		Parsed      map[string]json.RawMessage `json:"parsed"`
		NotParsable []string                   `json:"notParsable"`
		NotFound    []string                   `json:"notFound"`
	}
	if err := responses.Get("0", &result); err != nil {
		return nil, fmt.Errorf("CalendarEvent/parse: %w", err)
	}
	if len(result.NotParsable) > 0 || len(result.NotFound) > 0 {
		return nil, fmt.Errorf("CalendarEvent/parse: the server could not read %d of %d calendars", len(result.NotParsable)+len(result.NotFound), len(blobIDs))
	}
	parsed := make(map[string][]CalendarEvent, len(result.Parsed))
	for blobID, raw := range result.Parsed {
		// A blob holds one event or a list of them.
		var many []CalendarEvent
		if err := json.Unmarshal(raw, &many); err != nil {
			var one CalendarEvent
			if err := json.Unmarshal(raw, &one); err != nil {
				return nil, fmt.Errorf("CalendarEvent/parse: %w", err)
			}
			many = []CalendarEvent{one}
		}
		parsed[blobID] = many
	}
	return parsed, nil
}
