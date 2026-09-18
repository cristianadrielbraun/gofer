package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type outlookCalendarListResponse struct {
	Calendars []outlookCalendar `json:"value"`
	NextLink  string            `json:"@odata.nextLink"`
}

type outlookCalendar struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	TimeZone          string `json:"timeZone"`
	Color             string `json:"color"`
	HexColor          string `json:"hexColor"`
	CanEdit           bool   `json:"canEdit"`
	IsDefaultCalendar bool   `json:"isDefaultCalendar"`
}

type outlookCalendarEventsResponse struct {
	Events   []outlookCalendarEvent `json:"value"`
	NextLink string                 `json:"@odata.nextLink"`
}

type outlookCalendarEvent struct {
	ID                    string                   `json:"id"`
	ICalUID               string                   `json:"iCalUId"`
	ChangeKey             string                   `json:"changeKey"`
	Subject               string                   `json:"subject"`
	BodyPreview           string                   `json:"bodyPreview"`
	Body                  outlookCalendarItemBody  `json:"body"`
	Location              outlookCalendarLocation  `json:"location"`
	Start                 outlookCalendarDateTime  `json:"start"`
	End                   outlookCalendarDateTime  `json:"end"`
	IsAllDay              bool                     `json:"isAllDay"`
	IsCancelled           bool                     `json:"isCancelled"`
	WebLink               string                   `json:"webLink"`
	CreatedDateTime       string                   `json:"createdDateTime"`
	LastModifiedDateTime  string                   `json:"lastModifiedDateTime"`
	Organizer             outlookCalendarRecipient `json:"organizer"`
	Attendees             json.RawMessage          `json:"attendees"`
	Recurrence            json.RawMessage          `json:"recurrence"`
	SeriesMasterID        string                   `json:"seriesMasterId"`
	OnlineMeetingURL      string                   `json:"onlineMeetingUrl"`
	OnlineMeetingProvider string                   `json:"onlineMeetingProvider"`
	OnlineMeeting         json.RawMessage          `json:"onlineMeeting"`
}

type outlookCalendarDateTime struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

type outlookCalendarItemBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type outlookCalendarLocation struct {
	DisplayName string `json:"displayName"`
	LocationURI string `json:"locationUri"`
}

type outlookCalendarRecipient struct {
	EmailAddress outlookEmailAddress `json:"emailAddress"`
}

func discoverOutlookCalendars(ctx context.Context, accessToken string) ([]calendar.RemoteCalendar, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, fmt.Errorf("Microsoft Calendar access token is empty")
	}

	endpoint := outlookGraphBaseURL + "/me/calendars?$top=100"
	seenLinks := map[string]bool{}
	var calendars []calendar.RemoteCalendar
	for endpoint != "" {
		var page outlookCalendarListResponse
		if err := (&Handler{}).doOutlookJSON(ctx, http.MethodGet, endpoint, accessToken, nil, &page); err != nil {
			return nil, err
		}
		for _, remote := range page.Calendars {
			remoteID := strings.TrimSpace(remote.ID)
			if remoteID == "" {
				continue
			}
			accessRole := "reader"
			if remote.CanEdit {
				accessRole = "owner"
			}
			calendars = append(calendars, calendar.RemoteCalendar{
				RemoteID:    remoteID,
				Name:        strings.TrimSpace(remote.Name),
				Description: strings.TrimSpace(remote.Description),
				TimeZone:    strings.TrimSpace(remote.TimeZone),
				Color:       outlookCalendarColor(remote.HexColor, remote.Color),
				AccessRole:  accessRole,
				Primary:     remote.IsDefaultCalendar,
			})
		}

		next := strings.TrimSpace(page.NextLink)
		if next == "" {
			break
		}
		if seenLinks[next] {
			return nil, fmt.Errorf("Microsoft Calendar returned a repeated pagination link")
		}
		seenLinks[next] = true
		endpoint = next
	}
	return calendars, nil
}

func outlookCalendarColor(hexColor, namedColor string) string {
	if value := strings.TrimSpace(hexColor); isCalendarHexColor(value) {
		return value
	}
	switch strings.ToLower(strings.TrimSpace(namedColor)) {
	case "lightblue":
		return "#5b9bd5"
	case "lightgreen":
		return "#70ad47"
	case "lightorange":
		return "#ed7d31"
	case "lightgray":
		return "#a5a5a5"
	case "lightyellow":
		return "#ffc000"
	case "lightteal":
		return "#00b0f0"
	case "lightpink":
		return "#e85aad"
	case "lightbrown":
		return "#a67843"
	case "lightred":
		return "#c00000"
	default:
		return "#2563eb"
	}
}

func isCalendarHexColor(value string) bool {
	if len(value) != 7 || value[0] != '#' {
		return false
	}
	for _, char := range value[1:] {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

func listOutlookCalendarEvents(ctx context.Context, accessToken, remoteCalendarID string, query calendar.EventQuery) (calendar.EventPage, error) {
	accessToken = strings.TrimSpace(accessToken)
	remoteCalendarID = strings.TrimSpace(remoteCalendarID)
	if accessToken == "" {
		return calendar.EventPage{}, fmt.Errorf("Microsoft Calendar access token is empty")
	}
	if remoteCalendarID == "" {
		return calendar.EventPage{}, fmt.Errorf("Microsoft Calendar id is empty")
	}
	if query.WindowStart.IsZero() || query.WindowEnd.IsZero() || !query.WindowEnd.After(query.WindowStart) {
		return calendar.EventPage{}, fmt.Errorf("Microsoft Calendar event window is invalid")
	}

	values := url.Values{}
	values.Set("startDateTime", query.WindowStart.Format(time.RFC3339))
	values.Set("endDateTime", query.WindowEnd.Format(time.RFC3339))
	values.Set("$top", "1000")
	values.Set("$orderby", "start/dateTime")
	values.Set("$select", strings.Join([]string{
		"id", "iCalUId", "changeKey", "subject", "bodyPreview", "body", "start", "end",
		"isAllDay", "isCancelled", "webLink", "createdDateTime", "lastModifiedDateTime",
		"organizer", "attendees", "location", "onlineMeetingUrl", "onlineMeetingProvider",
		"onlineMeeting", "seriesMasterId", "recurrence",
	}, ","))

	endpoint := outlookGraphBaseURL + "/me/calendars/" + url.PathEscape(remoteCalendarID) + "/calendarView?" + values.Encode()
	seenLinks := map[string]bool{}
	var events []calendar.RemoteEvent
	for endpoint != "" {
		var page outlookCalendarEventsResponse
		if err := (&Handler{}).doOutlookJSON(ctx, http.MethodGet, endpoint, accessToken, nil, &page); err != nil {
			return calendar.EventPage{}, err
		}
		for _, remote := range page.Events {
			normalized, err := normalizeOutlookCalendarEvent(remote)
			if err != nil {
				return calendar.EventPage{}, err
			}
			if strings.TrimSpace(normalized.RemoteID) != "" {
				events = append(events, normalized)
			}
		}

		next := strings.TrimSpace(page.NextLink)
		if next == "" {
			break
		}
		if seenLinks[next] {
			return calendar.EventPage{}, fmt.Errorf("Microsoft Calendar returned a repeated event pagination link")
		}
		seenLinks[next] = true
		endpoint = next
	}
	return calendar.EventPage{Events: events}, nil
}

func normalizeOutlookCalendarEvent(remote outlookCalendarEvent) (calendar.RemoteEvent, error) {
	status := "confirmed"
	if remote.IsCancelled {
		status = "cancelled"
	}
	event := calendar.RemoteEvent{
		RemoteID:       strings.TrimSpace(remote.ID),
		ICalUID:        strings.TrimSpace(remote.ICalUID),
		SeriesRemoteID: strings.TrimSpace(remote.SeriesMasterID),
		ETag:           strings.TrimSpace(remote.ChangeKey),
		Status:         status,
		Summary:        strings.TrimSpace(remote.Subject),
		Description:    strings.TrimSpace(remote.Body.Content),
		Location:       strings.TrimSpace(remote.Location.DisplayName),
		OrganizerName:  strings.TrimSpace(remote.Organizer.EmailAddress.Name),
		OrganizerEmail: strings.TrimSpace(remote.Organizer.EmailAddress.Address),
		StartTimeZone:  strings.TrimSpace(remote.Start.TimeZone),
		EndTimeZone:    strings.TrimSpace(remote.End.TimeZone),
		HTMLLink:       strings.TrimSpace(remote.WebLink),
		Deleted:        remote.IsCancelled,
	}
	if event.Description == "" {
		event.Description = strings.TrimSpace(remote.BodyPreview)
	}
	event.Recurrence = validOutlookJSON(remote.Recurrence, "{}")
	event.Attendees = validOutlookJSON(remote.Attendees, "[]")
	if len(remote.OnlineMeeting) > 0 && json.Valid(remote.OnlineMeeting) {
		event.OnlineMeeting = append(json.RawMessage(nil), remote.OnlineMeeting...)
	} else if value := strings.TrimSpace(remote.OnlineMeetingURL); value != "" {
		event.OnlineMeeting = json.RawMessage(fmt.Sprintf(`{"url":%q}`, value))
	} else {
		event.OnlineMeeting = json.RawMessage("{}")
	}

	var err error
	event.ProviderCreatedAt, err = parseOutlookCalendarTimestamp(remote.CreatedDateTime)
	if err != nil {
		return calendar.RemoteEvent{}, fmt.Errorf("parse Microsoft Calendar created time for %q: %w", event.RemoteID, err)
	}
	event.ProviderUpdatedAt, err = parseOutlookCalendarTimestamp(remote.LastModifiedDateTime)
	if err != nil {
		return calendar.RemoteEvent{}, fmt.Errorf("parse Microsoft Calendar updated time for %q: %w", event.RemoteID, err)
	}

	if remote.IsAllDay {
		event.AllDay = true
		event.StartDate = outlookCalendarDate(remote.Start.DateTime)
		event.EndDate = outlookCalendarDate(remote.End.DateTime)
		if !event.Deleted && (event.StartDate == "" || event.EndDate == "" || event.StartDate >= event.EndDate) {
			return calendar.RemoteEvent{}, fmt.Errorf("Microsoft Calendar all-day event %q has an invalid date range", event.RemoteID)
		}
		return event, nil
	}

	event.StartAt, err = parseOutlookCalendarDateTime(remote.Start)
	if err != nil {
		return calendar.RemoteEvent{}, fmt.Errorf("parse Microsoft Calendar start for %q: %w", event.RemoteID, err)
	}
	event.EndAt, err = parseOutlookCalendarDateTime(remote.End)
	if err != nil {
		return calendar.RemoteEvent{}, fmt.Errorf("parse Microsoft Calendar end for %q: %w", event.RemoteID, err)
	}
	if !event.Deleted && (event.StartAt == nil || event.EndAt == nil || !event.EndAt.After(*event.StartAt)) {
		return calendar.RemoteEvent{}, fmt.Errorf("Microsoft Calendar timed event %q has an invalid time range", event.RemoteID)
	}
	return event, nil
}

func validOutlookJSON(value json.RawMessage, fallback string) json.RawMessage {
	if len(value) > 0 && json.Valid(value) {
		return append(json.RawMessage(nil), value...)
	}
	return json.RawMessage(fallback)
}

func outlookCalendarDate(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < len("2006-01-02") {
		return ""
	}
	return value[:len("2006-01-02")]
}

func parseOutlookCalendarTimestamp(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, err
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func parseOutlookCalendarDateTime(value outlookCalendarDateTime) (*time.Time, error) {
	raw := strings.TrimSpace(value.DateTime)
	if raw == "" {
		return nil, nil
	}
	location := outlookCalendarTimeLocation(value.TimeZone)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
		parsed, err := time.ParseInLocation(layout, raw, location)
		if err == nil {
			parsed = parsed.UTC()
			return &parsed, nil
		}
	}
	return nil, fmt.Errorf("invalid date-time %q", raw)
}

func outlookCalendarTimeLocation(timeZone string) *time.Location {
	timeZone = strings.TrimSpace(timeZone)
	if timeZone == "" || strings.EqualFold(timeZone, "UTC") || strings.EqualFold(timeZone, "GMT Standard Time") {
		return time.UTC
	}
	if location, err := time.LoadLocation(timeZone); err == nil {
		return location
	}
	locations := map[string]string{
		"W. Europe Standard Time":      "Europe/Berlin",
		"Central Europe Standard Time": "Europe/Budapest",
		"Romance Standard Time":        "Europe/Paris",
		"Eastern Standard Time":        "America/New_York",
		"Central Standard Time":        "America/Chicago",
		"Mountain Standard Time":       "America/Denver",
		"Pacific Standard Time":        "America/Los_Angeles",
		"China Standard Time":          "Asia/Shanghai",
		"Tokyo Standard Time":          "Asia/Tokyo",
	}
	if locationName := locations[timeZone]; locationName != "" {
		if location, err := time.LoadLocation(locationName); err == nil {
			return location
		}
	}
	return time.UTC
}

func (h *Handler) syncOutlookCalendarWindow(ctx context.Context, userID string, windowStart, windowEnd time.Time) (int, error) {
	sources, err := h.db.ListSelectedCalendarSources(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("list selected calendar sources: %w", err)
	}

	tokens := make(map[string]string)
	total := 0
	var firstErr error
	for _, source := range sources {
		if source.Provider != providers.ProviderOutlook {
			continue
		}
		token, ok := tokens[source.AccountID]
		if !ok {
			if h.mailCredentials() == nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("Microsoft OAuth is not configured")
				}
				continue
			}
			var tokenErr error
			token, tokenErr = h.mailCredentials().GetMicrosoftGraphCalendarTokenForAccount(ctx, source.AccountID)
			if tokenErr != nil {
				if firstErr == nil {
					firstErr = tokenErr
				}
				continue
			}
			tokens[source.AccountID] = token
		}

		page, fetchErr := listOutlookCalendarEvents(ctx, token, source.RemoteID, calendar.EventQuery{
			WindowStart: windowStart,
			WindowEnd:   windowEnd,
		})
		if fetchErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("sync Microsoft calendar %q: %w", source.Name, fetchErr)
			}
			continue
		}

		events := make([]storage.CalendarEvent, 0, len(page.Events))
		for _, remote := range page.Events {
			events = append(events, storage.CalendarEvent{
				UserID:            userID,
				SourceID:          source.ID,
				RemoteID:          remote.RemoteID,
				ICalUID:           remote.ICalUID,
				SeriesRemoteID:    remote.SeriesRemoteID,
				ETag:              remote.ETag,
				Status:            remote.Status,
				Summary:           remote.Summary,
				Description:       remote.Description,
				Location:          remote.Location,
				OrganizerName:     remote.OrganizerName,
				OrganizerEmail:    remote.OrganizerEmail,
				AllDay:            remote.AllDay,
				StartDate:         remote.StartDate,
				EndDate:           remote.EndDate,
				StartAt:           remote.StartAt,
				EndAt:             remote.EndAt,
				StartTimeZone:     remote.StartTimeZone,
				EndTimeZone:       remote.EndTimeZone,
				RecurrenceJSON:    string(remote.Recurrence),
				AttendeesJSON:     string(remote.Attendees),
				OnlineMeetingJSON: string(remote.OnlineMeeting),
				HTMLLink:          remote.HTMLLink,
				ProviderCreatedAt: remote.ProviderCreatedAt,
				ProviderUpdatedAt: remote.ProviderUpdatedAt,
				IsDeleted:         remote.Deleted,
			})
		}
		if err := h.db.ReplaceCalendarEvents(ctx, userID, source.ID, events, windowStart, windowEnd); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("store Microsoft calendar %q events: %w", source.Name, err)
			}
			continue
		}
		total += len(events)
	}
	return total, firstErr
}

func (h *Handler) discoverOutlookCalendarSources(ctx context.Context, userID, accountID, accessToken string) (int, error) {
	calendars, err := discoverOutlookCalendars(ctx, accessToken)
	if err != nil {
		return 0, fmt.Errorf("discover Microsoft calendars: %w", err)
	}

	hasPrimary := false
	for _, remote := range calendars {
		if remote.Primary {
			hasPrimary = true
			break
		}
	}
	sources := make([]storage.CalendarSource, 0, len(calendars))
	for index, remote := range calendars {
		selected := remote.Primary
		if !hasPrimary && index == 0 {
			selected = true
		}
		sources = append(sources, storage.CalendarSource{
			UserID:      userID,
			AccountID:   accountID,
			Provider:    providers.ProviderOutlook,
			RemoteID:    remote.RemoteID,
			Name:        remote.Name,
			Description: remote.Description,
			TimeZone:    remote.TimeZone,
			Color:       remote.Color,
			AccessRole:  remote.AccessRole,
			IsPrimary:   remote.Primary,
			IsSelected:  selected,
		})
	}
	if err := h.db.ReplaceCalendarSources(ctx, userID, accountID, providers.ProviderOutlook, sources); err != nil {
		return 0, fmt.Errorf("store discovered Microsoft calendars: %w", err)
	}
	return len(sources), nil
}
