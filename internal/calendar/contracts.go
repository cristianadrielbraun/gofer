// Package calendar defines the provider-neutral Calendar boundary.
//
// Provider adapters translate Google Calendar and Microsoft Graph responses
// into these types. Storage and HTTP layers should not depend on either
// provider's wire representation.
package calendar

import (
	"context"
	"encoding/json"
	"time"
)

type Provider string

const (
	ProviderGmail   Provider = "gmail"
	ProviderOutlook Provider = "outlook"
)

type SyncCursor struct {
	Kind        string
	Value       string
	WindowStart time.Time
	WindowEnd   time.Time
}

// RemoteCalendar is the provider-facing representation of one calendar that
// belongs to a connected mailbox account. RemoteID is scoped to that account.
type RemoteCalendar struct {
	RemoteID    string
	Name        string
	Description string
	TimeZone    string
	Color       string
	AccessRole  string
	Primary     bool
}

// RemoteEvent is normalized enough for the local cache and UI while keeping
// provider-specific recurrence, attendee, and online-meeting details as JSON.
// All-day events use the inclusive start date and exclusive end date fields;
// timed events use StartAt/EndAt and preserve the provider timezone names.
type RemoteEvent struct {
	RemoteID       string
	ICalUID        string
	SeriesRemoteID string
	ETag           string
	Status         string
	Summary        string
	Description    string
	Location       string
	OrganizerName  string
	OrganizerEmail string

	AllDay        bool
	StartDate     string
	EndDate       string
	StartAt       *time.Time
	EndAt         *time.Time
	StartTimeZone string
	EndTimeZone   string

	Recurrence    json.RawMessage
	Attendees     json.RawMessage
	OnlineMeeting json.RawMessage
	HTMLLink      string
	Deleted       bool

	ProviderCreatedAt *time.Time
	ProviderUpdatedAt *time.Time
}

type EventQuery struct {
	WindowStart    time.Time
	WindowEnd      time.Time
	Cursor         SyncCursor
	IncludeDeleted bool
}

type EventPage struct {
	Events           []RemoteEvent
	NextPageToken    string
	NextSyncCursor   SyncCursor
	FullSyncRequired bool
}

// ReadProvider is intentionally read-only for the first Calendar vertical
// slice. Create/update/delete operations will add a separate write boundary
// after conflict and invitation semantics are defined.
type ReadProvider interface {
	ListCalendars(ctx context.Context) ([]RemoteCalendar, error)
	ListEvents(ctx context.Context, remoteCalendarID string, query EventQuery) (EventPage, error)
}
