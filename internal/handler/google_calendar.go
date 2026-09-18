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
	"github.com/cristianadrielbraun/gofer/internal/views"
)

var googleCalendarAPIBaseURL = "https://www.googleapis.com/calendar/v3"

type googleCalendarListResponse struct {
	Items         []googleCalendarListEntry `json:"items"`
	NextPageToken string                    `json:"nextPageToken"`
}

type googleCalendarListEntry struct {
	ID              string `json:"id"`
	Summary         string `json:"summary"`
	SummaryOverride string `json:"summaryOverride"`
	Description     string `json:"description"`
	Location        string `json:"location"`
	TimeZone        string `json:"timeZone"`
	BackgroundColor string `json:"backgroundColor"`
	AccessRole      string `json:"accessRole"`
	Primary         bool   `json:"primary"`
	Deleted         bool   `json:"deleted"`
}

type googleCalendarEventsResponse struct {
	Items         []googleCalendarEvent `json:"items"`
	NextPageToken string                `json:"nextPageToken"`
	NextSyncToken string                `json:"nextSyncToken"`
}

type googleCalendarEvent struct {
	ID               string                      `json:"id"`
	ICalUID          string                      `json:"iCalUID"`
	ETag             string                      `json:"etag"`
	Status           string                      `json:"status"`
	Summary          string                      `json:"summary"`
	Description      string                      `json:"description"`
	Location         string                      `json:"location"`
	HTMLLink         string                      `json:"htmlLink"`
	Created          string                      `json:"created"`
	Updated          string                      `json:"updated"`
	RecurringEventID string                      `json:"recurringEventId"`
	Recurrence       []string                    `json:"recurrence"`
	Attendees        json.RawMessage             `json:"attendees"`
	Organizer        googleCalendarPerson        `json:"organizer"`
	Start            googleCalendarEventDateTime `json:"start"`
	End              googleCalendarEventDateTime `json:"end"`
	ConferenceData   json.RawMessage             `json:"conferenceData"`
	HangoutLink      string                      `json:"hangoutLink"`
}

type googleCalendarPerson struct {
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
}

type googleCalendarEventDateTime struct {
	Date     string `json:"date"`
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

func discoverGoogleCalendars(ctx context.Context, accessToken string) ([]calendar.RemoteCalendar, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, fmt.Errorf("Google Calendar access token is empty")
	}

	var calendars []calendar.RemoteCalendar
	pageToken := ""
	seenPageTokens := map[string]bool{}
	for {
		values := url.Values{}
		values.Set("maxResults", "250")
		values.Set("showDeleted", "true")
		if pageToken != "" {
			values.Set("pageToken", pageToken)
		}

		var page googleCalendarListResponse
		endpoint := googleCalendarAPIBaseURL + "/users/me/calendarList?" + values.Encode()
		if err := doGoogleJSON(ctx, http.MethodGet, endpoint, accessToken, nil, &page); err != nil {
			return nil, err
		}
		for _, entry := range page.Items {
			if entry.Deleted || strings.TrimSpace(entry.ID) == "" {
				continue
			}
			name := strings.TrimSpace(entry.SummaryOverride)
			if name == "" {
				name = strings.TrimSpace(entry.Summary)
			}
			calendars = append(calendars, calendar.RemoteCalendar{
				RemoteID:    strings.TrimSpace(entry.ID),
				Name:        name,
				Description: strings.TrimSpace(entry.Description),
				TimeZone:    strings.TrimSpace(entry.TimeZone),
				Color:       strings.TrimSpace(entry.BackgroundColor),
				AccessRole:  strings.TrimSpace(entry.AccessRole),
				Primary:     entry.Primary,
			})
		}

		pageToken = strings.TrimSpace(page.NextPageToken)
		if pageToken == "" {
			return calendars, nil
		}
		if seenPageTokens[pageToken] {
			return nil, fmt.Errorf("Google Calendar returned a repeated pagination token")
		}
		seenPageTokens[pageToken] = true
	}
}

func listGoogleCalendarEvents(ctx context.Context, accessToken, remoteCalendarID string, query calendar.EventQuery) (calendar.EventPage, error) {
	accessToken = strings.TrimSpace(accessToken)
	remoteCalendarID = strings.TrimSpace(remoteCalendarID)
	if accessToken == "" {
		return calendar.EventPage{}, fmt.Errorf("Google Calendar access token is empty")
	}
	if remoteCalendarID == "" {
		return calendar.EventPage{}, fmt.Errorf("Google Calendar id is empty")
	}
	if query.WindowStart.IsZero() || query.WindowEnd.IsZero() || !query.WindowEnd.After(query.WindowStart) {
		return calendar.EventPage{}, fmt.Errorf("Google Calendar event window is invalid")
	}

	var events []calendar.RemoteEvent
	pageToken := ""
	seenPageTokens := map[string]bool{}
	var nextSyncToken string
	for {
		values := url.Values{}
		values.Set("maxResults", "2500")
		values.Set("showDeleted", "true")
		values.Set("singleEvents", "true")
		values.Set("orderBy", "startTime")
		values.Set("timeMin", query.WindowStart.Format(time.RFC3339))
		values.Set("timeMax", query.WindowEnd.Format(time.RFC3339))
		if pageToken != "" {
			values.Set("pageToken", pageToken)
		}

		var page googleCalendarEventsResponse
		endpoint := googleCalendarAPIBaseURL + "/calendars/" + url.PathEscape(remoteCalendarID) + "/events?" + values.Encode()
		if err := doGoogleJSON(ctx, http.MethodGet, endpoint, accessToken, nil, &page); err != nil {
			return calendar.EventPage{}, err
		}
		for _, remote := range page.Items {
			normalized, err := normalizeGoogleCalendarEvent(remote)
			if err != nil {
				return calendar.EventPage{}, err
			}
			if strings.TrimSpace(normalized.RemoteID) != "" {
				events = append(events, normalized)
			}
		}

		pageToken = strings.TrimSpace(page.NextPageToken)
		if pageToken == "" {
			nextSyncToken = strings.TrimSpace(page.NextSyncToken)
			break
		}
		if seenPageTokens[pageToken] {
			return calendar.EventPage{}, fmt.Errorf("Google Calendar returned a repeated event pagination token")
		}
		seenPageTokens[pageToken] = true
	}

	result := calendar.EventPage{Events: events}
	if nextSyncToken != "" {
		result.NextSyncCursor = calendar.SyncCursor{Kind: "google-calendar", Value: nextSyncToken}
	}
	return result, nil
}

func normalizeGoogleCalendarEvent(remote googleCalendarEvent) (calendar.RemoteEvent, error) {
	event := calendar.RemoteEvent{
		RemoteID:       strings.TrimSpace(remote.ID),
		ICalUID:        strings.TrimSpace(remote.ICalUID),
		SeriesRemoteID: strings.TrimSpace(remote.RecurringEventID),
		ETag:           strings.TrimSpace(remote.ETag),
		Status:         strings.ToLower(strings.TrimSpace(remote.Status)),
		Summary:        strings.TrimSpace(remote.Summary),
		Description:    strings.TrimSpace(remote.Description),
		Location:       strings.TrimSpace(remote.Location),
		OrganizerName:  strings.TrimSpace(remote.Organizer.DisplayName),
		OrganizerEmail: strings.TrimSpace(remote.Organizer.Email),
		StartTimeZone:  strings.TrimSpace(remote.Start.TimeZone),
		EndTimeZone:    strings.TrimSpace(remote.End.TimeZone),
		HTMLLink:       strings.TrimSpace(remote.HTMLLink),
		Deleted:        strings.EqualFold(strings.TrimSpace(remote.Status), "cancelled"),
	}
	if event.Status == "" {
		event.Status = "confirmed"
	}

	if len(remote.Recurrence) > 0 {
		value, err := json.Marshal(remote.Recurrence)
		if err != nil {
			return calendar.RemoteEvent{}, fmt.Errorf("encode Google Calendar recurrence for %q: %w", event.RemoteID, err)
		}
		event.Recurrence = value
	} else {
		event.Recurrence = json.RawMessage("[]")
	}
	if len(remote.Attendees) > 0 && json.Valid(remote.Attendees) {
		event.Attendees = append(json.RawMessage(nil), remote.Attendees...)
	} else {
		event.Attendees = json.RawMessage("[]")
	}
	if len(remote.ConferenceData) > 0 && json.Valid(remote.ConferenceData) {
		event.OnlineMeeting = append(json.RawMessage(nil), remote.ConferenceData...)
	} else if strings.TrimSpace(remote.HangoutLink) != "" {
		event.OnlineMeeting = json.RawMessage(fmt.Sprintf(`{"url":%q}`, strings.TrimSpace(remote.HangoutLink)))
	} else {
		event.OnlineMeeting = json.RawMessage("{}")
	}

	var err error
	event.ProviderCreatedAt, err = parseGoogleCalendarTimestamp(remote.Created)
	if err != nil {
		return calendar.RemoteEvent{}, fmt.Errorf("parse Google Calendar created time for %q: %w", event.RemoteID, err)
	}
	event.ProviderUpdatedAt, err = parseGoogleCalendarTimestamp(remote.Updated)
	if err != nil {
		return calendar.RemoteEvent{}, fmt.Errorf("parse Google Calendar updated time for %q: %w", event.RemoteID, err)
	}

	if strings.TrimSpace(remote.Start.Date) != "" || strings.TrimSpace(remote.End.Date) != "" {
		event.AllDay = true
		event.StartDate = strings.TrimSpace(remote.Start.Date)
		event.EndDate = strings.TrimSpace(remote.End.Date)
		if !event.Deleted && (event.StartDate == "" || event.EndDate == "" || event.StartDate >= event.EndDate) {
			return calendar.RemoteEvent{}, fmt.Errorf("Google Calendar all-day event %q has an invalid date range", event.RemoteID)
		}
		return event, nil
	}

	event.StartAt, err = parseGoogleCalendarEventTime(remote.Start)
	if err != nil {
		return calendar.RemoteEvent{}, fmt.Errorf("parse Google Calendar start for %q: %w", event.RemoteID, err)
	}
	event.EndAt, err = parseGoogleCalendarEventTime(remote.End)
	if err != nil {
		return calendar.RemoteEvent{}, fmt.Errorf("parse Google Calendar end for %q: %w", event.RemoteID, err)
	}
	if !event.Deleted && (event.StartAt == nil || event.EndAt == nil || !event.EndAt.After(*event.StartAt)) {
		return calendar.RemoteEvent{}, fmt.Errorf("Google Calendar timed event %q has an invalid time range", event.RemoteID)
	}
	return event, nil
}

func parseGoogleCalendarTimestamp(value string) (*time.Time, error) {
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

func parseGoogleCalendarEventTime(value googleCalendarEventDateTime) (*time.Time, error) {
	raw := strings.TrimSpace(value.DateTime)
	if raw == "" {
		return nil, nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		parsed = parsed.UTC()
		return &parsed, nil
	}

	location := time.UTC
	if timezone := strings.TrimSpace(value.TimeZone); timezone != "" {
		loaded, err := time.LoadLocation(timezone)
		if err != nil {
			return nil, fmt.Errorf("load timezone %q: %w", timezone, err)
		}
		location = loaded
	}
	parsed, err := time.ParseInLocation("2006-01-02T15:04:05", raw, location)
	if err != nil {
		return nil, err
	}
	parsed = parsed.UTC()
	return &parsed, nil
}

func (h *Handler) syncGoogleCalendarWindow(ctx context.Context, userID string, windowStart, windowEnd time.Time) (int, error) {
	sources, err := h.db.ListSelectedCalendarSources(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("list selected calendar sources: %w", err)
	}

	tokens := make(map[string]string)
	total := 0
	var firstErr error
	for _, source := range sources {
		if source.Provider != providers.ProviderGmail {
			continue
		}
		token, ok := tokens[source.AccountID]
		if !ok {
			if h.mailCredentials() == nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("Google OAuth is not configured")
				}
				continue
			}
			var tokenErr error
			token, tokenErr = h.mailCredentials().GetGoogleCalendarTokenForAccount(ctx, source.AccountID)
			if tokenErr != nil {
				if firstErr == nil {
					firstErr = tokenErr
				}
				continue
			}
			tokens[source.AccountID] = token
		}

		page, fetchErr := listGoogleCalendarEvents(ctx, token, source.RemoteID, calendar.EventQuery{
			WindowStart: windowStart,
			WindowEnd:   windowEnd,
		})
		if fetchErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("sync Google calendar %q: %w", source.Name, fetchErr)
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
				firstErr = fmt.Errorf("store Google calendar %q events: %w", source.Name, err)
			}
			continue
		}
		total += len(events)
	}
	return total, firstErr
}

func (h *Handler) discoverGoogleCalendarSources(ctx context.Context, userID, accountID, accessToken string) (int, error) {
	calendars, err := discoverGoogleCalendars(ctx, accessToken)
	if err != nil {
		return 0, fmt.Errorf("discover Google calendars: %w", err)
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
			Provider:    providers.ProviderGmail,
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
	if err := h.db.ReplaceCalendarSources(ctx, userID, accountID, providers.ProviderGmail, sources); err != nil {
		return 0, fmt.Errorf("store discovered Google calendars: %w", err)
	}
	return len(sources), nil
}

func (h *Handler) handleDiscoverAccountCalendars(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accountID := strings.TrimSpace(r.PathValue("id"))
	if accountID == "" {
		http.Error(w, "account id required", http.StatusBadRequest)
		return
	}
	if !h.requireOwnedAccount(w, r, accountID) {
		return
	}

	data, err := h.accountStore.GetEditData(ctx, accountID)
	if err != nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	statusMessage := ""
	statusError := false
	discoverySucceeded := false
	userID := h.userID(ctx)
	switch data.Provider {
	case providers.ProviderGmail:
		if h.mailCredentials() == nil || !h.mailCredentials().HasGoogleOAuth() {
			statusMessage = "Google OAuth is not configured for this Gofer instance."
			statusError = true
			break
		}
		accessToken, tokenErr := h.mailCredentials().GetGoogleCalendarTokenForAccount(ctx, accountID)
		if tokenErr != nil {
			statusMessage = "Google Calendar access is missing. Reconnect this account from its actions menu, then discover calendars again."
			statusError = true
			break
		}
		count, discoverErr := h.discoverGoogleCalendarSources(ctx, userID, accountID, accessToken)
		if discoverErr != nil {
			statusMessage = "Calendar discovery failed: " + discoverErr.Error()
			statusError = true
			break
		}
		statusMessage = fmt.Sprintf("Discovered %d Google calendar source(s). The primary calendar is selected by default.", count)
		discoverySucceeded = true
	case providers.ProviderOutlook:
		if h.mailCredentials() == nil || !h.mailCredentials().HasMicrosoftOAuth() {
			statusMessage = "Microsoft OAuth is not configured for this Gofer instance."
			statusError = true
			break
		}
		accessToken, tokenErr := h.mailCredentials().GetMicrosoftGraphCalendarTokenForAccount(ctx, accountID)
		if tokenErr != nil {
			statusMessage = "Microsoft Calendar access is missing. Reconnect this account from its actions menu, then discover calendars again."
			statusError = true
			break
		}
		count, discoverErr := h.discoverOutlookCalendarSources(ctx, userID, accountID, accessToken)
		if discoverErr != nil {
			statusMessage = "Calendar discovery failed: " + discoverErr.Error()
			statusError = true
			break
		}
		statusMessage = fmt.Sprintf("Discovered %d Microsoft calendar source(s). The primary calendar is selected by default.", count)
		discoverySucceeded = true
	default:
		statusMessage = "Calendar discovery is available for Google and Microsoft accounts."
		statusError = true
	}
	if discoverySucceeded {
		if refreshed, refreshErr := h.accountStore.GetEditData(ctx, accountID); refreshErr == nil {
			data = refreshed
		}
	}

	if err := views.CalendarSyncSettingsResult(*data, statusMessage, statusError).Render(ctx, w); err != nil {
		http.Error(w, "failed to render calendar discovery result", http.StatusInternalServerError)
	}
}
