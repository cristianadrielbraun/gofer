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
	SingleValueExtendedProperties []struct {
		ID    string `json:"id"`
		Value string `json:"value"`
	} `json:"singleValueExtendedProperties"`
	Sensitivity           string                    `json:"sensitivity"`
	ShowAs                string                    `json:"showAs"`
	IsReminderOn          *bool                     `json:"isReminderOn"`
	ID                    string                    `json:"id"`
	ICalUID               string                    `json:"iCalUId"`
	ChangeKey             string                    `json:"changeKey"`
	Subject               string                    `json:"subject"`
	BodyPreview           string                    `json:"bodyPreview"`
	Body                  outlookCalendarItemBody   `json:"body"`
	Location              outlookCalendarLocation   `json:"location"`
	Start                 outlookCalendarDateTime   `json:"start"`
	End                   outlookCalendarDateTime   `json:"end"`
	IsAllDay              bool                      `json:"isAllDay"`
	IsOnlineMeeting       *bool                     `json:"isOnlineMeeting"`
	IsCancelled           bool                      `json:"isCancelled"`
	IsOrganizer           *bool                     `json:"isOrganizer"`
	ResponseStatus        struct{ Response string } `json:"responseStatus"`
	WebLink               string                    `json:"webLink"`
	CreatedDateTime       string                    `json:"createdDateTime"`
	LastModifiedDateTime  string                    `json:"lastModifiedDateTime"`
	Organizer             outlookCalendarRecipient  `json:"organizer"`
	Attendees             json.RawMessage           `json:"attendees"`
	Recurrence            json.RawMessage           `json:"recurrence"`
	SeriesMasterID        string                    `json:"seriesMasterId"`
	OnlineMeetingURL      string                    `json:"onlineMeetingUrl"`
	OnlineMeetingProvider string                    `json:"onlineMeetingProvider"`
	OnlineMeeting         json.RawMessage           `json:"onlineMeeting"`
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

	return discoverOutlookCalendarsWithFetch(ctx, func(endpoint string, out any) error {
		return (&Handler{}).doOutlookJSON(ctx, http.MethodGet, endpoint, accessToken, nil, out)
	})
}

func discoverOutlookCalendarsWithFetch(ctx context.Context, fetch func(string, any) error) ([]calendar.RemoteCalendar, error) {
	endpoint := outlookGraphBaseURL + "/me/calendars?$top=100"
	seenLinks := map[string]bool{}
	var calendars []calendar.RemoteCalendar
	for pageNumber := 0; endpoint != ""; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if pageNumber >= calendarDiscoveryMaxPages {
			return nil, fmt.Errorf("Microsoft Calendar discovery exceeded its page limit")
		}
		var page outlookCalendarListResponse
		if err := fetch(endpoint, &page); err != nil {
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

		if len(calendars) > calendarDiscoveryMaxSources {
			return nil, fmt.Errorf("Microsoft Calendar discovery exceeded its source limit")
		}
		next := strings.TrimSpace(page.NextLink)
		if next == "" {
			break
		}
		if seenLinks[next] {
			return nil, fmt.Errorf("Microsoft Calendar returned a repeated pagination link")
		}
		seenLinks[next] = true
		var err error
		endpoint, err = calendarDiscoveryPageURL(outlookGraphBaseURL+"/me/calendars", next)
		if err != nil {
			return nil, err
		}
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

	return listOutlookCalendarEventsWithFetch(ctx, remoteCalendarID, query, func(endpoint string, out any) error {
		return (&Handler{}).doOutlookJSON(ctx, http.MethodGet, endpoint, accessToken, nil, out)
	})
}

func listOutlookCalendarEventsWithFetch(ctx context.Context, remoteCalendarID string, query calendar.EventQuery, fetch func(string, any) error) (calendar.EventPage, error) {
	if strings.TrimSpace(remoteCalendarID) == "" || query.WindowStart.IsZero() || query.WindowEnd.IsZero() || !query.WindowEnd.After(query.WindowStart) || fetch == nil {
		return calendar.EventPage{}, fmt.Errorf("invalid calendar event request")
	}
	values := url.Values{}
	values.Set("startDateTime", query.WindowStart.Format(time.RFC3339))
	values.Set("endDateTime", query.WindowEnd.Format(time.RFC3339))
	values.Set("$top", "1000")
	values.Set("$expand", calendarTeamsDraftExpand())
	values.Set("$orderby", "start/dateTime")
	values.Set("$select", strings.Join([]string{
		"id", "iCalUId", "changeKey", "subject", "bodyPreview", "body", "start", "end",
		"isAllDay", "isCancelled", "webLink", "createdDateTime", "lastModifiedDateTime",
		"organizer", "attendees", "location", "onlineMeetingUrl", "onlineMeetingProvider",
		"onlineMeeting", "isOnlineMeeting", "seriesMasterId", "recurrence", "isOrganizer", "responseStatus", "sensitivity", "showAs", "isReminderOn",
	}, ","))

	collection := outlookGraphBaseURL + "/me/calendars/" + url.PathEscape(remoteCalendarID) + "/calendarView"
	endpoint := collection + "?" + values.Encode()
	seenLinks := map[string]bool{}
	var events []calendar.RemoteEvent
	for pageNumber := 0; endpoint != ""; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return calendar.EventPage{}, err
		}
		if pageNumber >= calendarSyncMaxPages {
			return calendar.EventPage{}, fmt.Errorf("Microsoft Calendar event traversal exceeded its page limit")
		}
		var page outlookCalendarEventsResponse
		if err := fetch(endpoint, &page); err != nil {
			return calendar.EventPage{}, err
		}
		if err := ctx.Err(); err != nil {
			return calendar.EventPage{}, err
		}
		for _, remote := range page.Events {
			if err := ctx.Err(); err != nil {
				return calendar.EventPage{}, err
			}
			if marker := calendarTeamsDraftMarker(remote); strings.HasPrefix(marker, "draft:") && calendarTeamsDraftPrivate(outlookCalendarUpdateEvent{outlookCalendarEvent: remote}, storage.CalendarTeamsDraft{RemoteID: remote.ID, DraftID: strings.TrimPrefix(marker, "draft:")}) {
				continue
			}
			normalized, err := normalizeOutlookCalendarEvent(remote)
			if err != nil {
				return calendar.EventPage{}, err
			}
			if strings.TrimSpace(normalized.RemoteID) != "" {
				events = append(events, normalized)
				if len(events) > calendarSyncMaxEvents {
					return calendar.EventPage{}, fmt.Errorf("calendar event traversal exceeded its event limit")
				}
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
		var err error
		endpoint, err = calendarDiscoveryPageURL(collection, next)
		if err != nil {
			return calendar.EventPage{}, err
		}
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
	if event.Description == "" && remote.Body.ContentType == "" {
		event.Description = strings.TrimSpace(remote.BodyPreview)
	}
	event.Recurrence = validOutlookJSON(remote.Recurrence, "{}")
	if remote.IsOrganizer != nil && !*remote.IsOrganizer && !remote.IsCancelled && remote.Organizer.EmailAddress.Address != "" {
		event.ResponseStatus = calendarResponseStatus(remote.ResponseStatus.Response)
	} else if remote.IsOrganizer != nil && *remote.IsOrganizer {
		event.ResponseStatus = "organizer"
	}
	event.Attendees = validOutlookJSON(remote.Attendees, "[]")
	event.OnlineMeeting = calendarOutlookMeetingJSON(remote)

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
	return h.syncCalendarProviderWindow(ctx, userID, windowStart, windowEnd, providers.ProviderOutlook)
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
