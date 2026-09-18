package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

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
	if data.Provider != providers.ProviderGmail {
		statusMessage = "Google Calendar discovery is available for Google accounts first. Microsoft Calendar support is next."
		statusError = true
	} else if h.mailCredentials() == nil || !h.mailCredentials().HasGoogleOAuth() {
		statusMessage = "Google OAuth is not configured for this Gofer instance."
		statusError = true
	} else {
		userID := h.userID(ctx)
		accessToken, tokenErr := h.mailCredentials().GetGoogleCalendarTokenForAccount(ctx, accountID)
		if tokenErr != nil {
			statusMessage = "Google Calendar access is missing. Reconnect this account from its actions menu, then discover calendars again."
			statusError = true
		} else if count, discoverErr := h.discoverGoogleCalendarSources(ctx, userID, accountID, accessToken); discoverErr != nil {
			statusMessage = "Calendar discovery failed: " + discoverErr.Error()
			statusError = true
		} else {
			statusMessage = fmt.Sprintf("Discovered %d Google calendar source(s). The primary calendar is selected by default.", count)
		}
	}

	if err := views.CalendarSyncSettingsResult(*data, statusMessage, statusError).Render(ctx, w); err != nil {
		http.Error(w, "failed to render calendar discovery result", http.StatusInternalServerError)
	}
}
