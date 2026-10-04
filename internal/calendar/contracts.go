// Package calendar defines the provider-neutral Calendar boundary.
//
// Provider adapters translate Google Calendar, Microsoft Graph, and CalDAV responses
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
	ProviderCalDAV  Provider = "caldav"
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
	ResponseStatus string // This calendar owner's RSVP, "organizer", or empty when unknown.

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

// RecurrenceDraft describes a simple series anchored to the event's local start.
// Until is an inclusive local date; Count includes the first occurrence. With
// neither set the series has no end. Attendees and invitations remain separate.
type RecurrenceDraft struct {
	Frequency string
	Interval  int
	Until     string
	Count     int
}

// EventDraft creates either a single appointment or a simple recurring series.
type EventDraft struct {
	RequestID       string
	Summary         string
	Description     string
	DescriptionHTML *string `json:",omitempty"` // Nil keeps legacy plain-text requests and hashes unchanged.
	TeamsMeeting    bool    `json:",omitempty"`
	TeamsMeetingSet bool    `json:"-"` // Distinguish an explicit disable from an omitted field.
	Location        string
	TimeZone        string
	AllDay          bool
	StartDate       string // inclusive
	EndDate         string // exclusive
	StartAt         *time.Time
	EndAt           *time.Time
	Recurrence      *RecurrenceDraft `json:",omitempty"` // Preserve existing single-event idempotency hashes.
	Guests          []GuestDraft     `json:",omitempty"`
	GuestsSet       bool             `json:"-"`
	OrganizerEmail  string           `json:"-"` // Trusted account identity, never a form field.
	OrganizerName   string           `json:"-"`
	ScheduleAgent   string           `json:"-"`
}

type GuestDraft struct {
	Email    string
	Name     string `json:",omitempty"`
	Optional bool   `json:",omitempty"`
}

// CreateProvider is separate from reading: it requires both a writable source
// and an explicitly authorized credential. RequestID remains stable on retry.
type CreateProvider interface {
	CreateEvent(ctx context.Context, remoteCalendarID string, draft EventDraft) (RemoteEvent, error)
}

// ReadProvider never creates or changes provider events.
type ReadProvider interface {
	ListCalendars(ctx context.Context) ([]RemoteCalendar, error)
	ListEvents(ctx context.Context, remoteCalendarID string, query EventQuery) (EventPage, error)
}
