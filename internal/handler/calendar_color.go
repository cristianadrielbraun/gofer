package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Why a color picked for a calendar is shown only in Gofer.
const (
	// calendarColorGoogleAccess: Google changes the account's calendar list
	// only with full calendar access.
	calendarColorGoogleAccess = "google_access"
	// calendarColorProvider: Gofer doesn't change colors on this calendar's
	// provider.
	calendarColorProvider = "provider"
	// calendarColorReconnect: the account's sign-in can't change its
	// calendars until it is reconnected.
	calendarColorReconnect = "reconnect"
	// calendarColorRefused: the provider didn't take the color.
	calendarColorRefused = "refused"
)

// errCalendarColorRefused is the calendar's provider not taking a color,
// such as Outlook for a calendar shared with the account.
var errCalendarColorRefused = errors.New("the calendar's provider didn't take the color")

// calendarColorResult is what a color change did: the color Gofer shows
// now, and whether only Gofer shows it, and why.
type calendarColorResult struct {
	Color       string `json:"color"`
	InGoferOnly bool   `json:"in_gofer_only"`
	Reason      string `json:"reason,omitempty"`
}

// handleCalendarSourceColor saves a color picked for a synced calendar, as
// #rrggbb. A Google calendar's color goes to the account's Google calendar
// list, which Google allows only with full calendar access, and an Outlook
// calendar's to Outlook, which needs the account's calendar write access.
// Without the access, when the provider refuses it,
// and on providers Gofer doesn't change colors on, Gofer keeps the color
// itself and says so. An empty color goes back to the provider's. A
// temporary failure on the provider changes nothing.
func (h *Handler) handleCalendarSourceColor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sourceID, owner := strings.TrimSpace(r.PathValue("id")), h.userID(ctx)
	var request struct {
		Color *string `json:"color"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || request.Color == nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, serverText(ctx, "Choose a color for the calendar."), http.StatusBadRequest)
		return
	}
	color := strings.ToLower(strings.TrimSpace(*request.Color))
	if sourceID == "" || color != "" && !isCalendarHexColor(color) {
		http.Error(w, serverText(ctx, "Choose a color for the calendar."), http.StatusBadRequest)
		return
	}
	result, err := h.changeCalendarColor(ctx, owner, sourceID, color)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		http.Error(w, i18n.T(ctx, "server.error.this_calendar_is_no_longer_synced_refresh"), http.StatusNotFound)
	case errors.Is(err, storage.ErrAccountRoute) || errors.Is(err, storage.ErrUserStoreOwner):
		userAccountError(w, r, err)
	case err != nil:
		http.Error(w, serverText(ctx, err.Error()), http.StatusBadGateway)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, no-store")
		_ = json.NewEncoder(w).Encode(result)
	}
}

// changeCalendarColor sets color on the calendar's provider where Gofer can,
// then saves it, in whichever store the owner's calendars are in.
func (h *Handler) changeCalendarColor(ctx context.Context, owner, sourceID, color string) (calendarColorResult, error) {
	source, err := h.calendarColorSource(ctx, owner, sourceID)
	if err != nil {
		return calendarColorResult{}, err
	}
	change := storage.CalendarColorChange{Color: color}
	result := calendarColorResult{Color: color}
	switch {
	case color == "":
		result.Color = source.Color
	case source.Provider == storage.CalendarSourceProviderICS:
		// A subscription's color is Gofer's own.
		change.OnProvider = true
	case source.Provider == providers.ProviderGmail:
		if !h.googleCalendarFullAccess(ctx, owner, source.AccountID) {
			result.Reason = calendarColorGoogleAccess
			break
		}
		err := h.onCalendarProvider(ctx, owner, source, func(ctx context.Context, login calendarManageLogin) error {
			return setGoogleCalendarColor(ctx, source, login.token, color)
		})
		switch {
		case err == nil:
			change.OnProvider = true
		case errors.Is(err, errGoogleCalendarListNeedsAccess):
			result.Reason = calendarColorGoogleAccess
		default:
			return calendarColorResult{}, err
		}
	case source.Provider == providers.ProviderOutlook:
		if !h.calendarWriteAccess(ctx, owner, source) {
			result.Reason = calendarColorReconnect
			break
		}
		err := h.onCalendarProvider(ctx, owner, source, func(ctx context.Context, login calendarManageLogin) error {
			return setOutlookCalendarColor(ctx, source, login.token, color)
		})
		switch {
		case err == nil:
			change.OnProvider = true
		case errors.Is(err, errCalendarColorRefused):
			result.Reason = calendarColorRefused
		default:
			return calendarColorResult{}, err
		}
	default:
		result.Reason = calendarColorProvider
	}
	result.InGoferOnly = result.Reason != ""
	if h.userStorage != nil {
		err = h.userIMAP.RunUserServiceWork(ctx, owner, func(ctx context.Context) error {
			return h.userAccounts.SetCalendarSourceColor(ctx, owner, sourceID, change)
		})
	} else {
		err = h.db.SetCalendarSourceColor(ctx, owner, sourceID, change)
	}
	return result, err
}

// calendarColorSource is one of the owner's synced calendars.
func (h *Handler) calendarColorSource(ctx context.Context, owner, sourceID string) (storage.CalendarSource, error) {
	if h.userStorage == nil {
		return selectedCalendarSource(ctx, h.db, owner, sourceID)
	}
	var source storage.CalendarSource
	err := h.userAccounts.WithUser(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		source, err = selectedCalendarSource(ctx, db, owner, sourceID)
		return err
	})
	return source, err
}

// googleCalendarFullAccess reports whether a Google account allowed Gofer
// full calendar access, which changing its calendar list needs.
func (h *Handler) googleCalendarFullAccess(ctx context.Context, owner, accountID string) bool {
	if h.userCredentials != nil {
		return h.userCredentials.GoogleCalendarFullAccessForUser(ctx, owner, accountID)
	}
	return h.mailCredentials() != nil && h.mailCredentials().GoogleCalendarFullAccess(ctx, accountID)
}

// calendarWriteAccess reports whether a Google or Microsoft account's
// sign-in lets Gofer change its calendars, which older sign-ins don't until
// the account is reconnected.
func (h *Handler) calendarWriteAccess(ctx context.Context, owner string, source storage.CalendarSource) bool {
	if h.userCredentials != nil {
		return h.userCredentials.CalendarCreateAuthorizedForUser(ctx, owner, source.AccountID)
	}
	return h.mailCredentials() != nil && h.mailCredentials().CalendarCreateAuthorized(ctx, source.AccountID, source.Provider)
}

// onCalendarProvider signs in to a synced calendar's provider and makes
// change there.
func (h *Handler) onCalendarProvider(ctx context.Context, owner string, source storage.CalendarSource, change func(context.Context, calendarManageLogin) error) error {
	if h.userStorage != nil {
		return h.withUserCalendarLogin(ctx, owner, source, change)
	}
	login, err := h.sharedCalendarLogin(ctx, owner, source)
	if err != nil {
		return err
	}
	return change(ctx, login)
}

// setGoogleCalendarColor sets a Google calendar's color in the account's
// own calendar list, which is where Google keeps it: other people the
// calendar is shared with keep theirs. Google refusing it is
// errGoogleCalendarListNeedsAccess.
func setGoogleCalendarColor(ctx context.Context, source storage.CalendarSource, token, color string) error {
	endpoint := googleCalendarAPIBaseURL + "/users/me/calendarList/" + url.PathEscape(source.RemoteID) + "?colorRgbFormat=true"
	payload := map[string]any{"backgroundColor": color, "foregroundColor": calendarTextColor(color)}
	err := userContactProviderJSON(ctx, providers.ProviderGmail, http.MethodPatch, endpoint, token, payload, nil)
	var google googleAPIError
	if err == nil || !errors.As(err, &google) {
		return err
	}
	switch google.Status {
	case http.StatusForbidden, http.StatusUnauthorized:
		return errGoogleCalendarListNeedsAccess
	case http.StatusNotFound:
		return errors.New("This calendar is no longer in your Google calendar list. Refresh the page.")
	}
	return err
}

// setOutlookCalendarColor sets an Outlook calendar's exact color, which
// Microsoft documents as read-only but takes. Microsoft refusing it, or
// answering with another color, is errCalendarColorRefused; a calendar gone
// from Outlook is sql.ErrNoRows.
func setOutlookCalendarColor(ctx context.Context, source storage.CalendarSource, token, color string) error {
	endpoint := outlookGraphBaseURL + "/me/calendars/" + url.PathEscape(source.RemoteID)
	var reply struct {
		HexColor string `json:"hexColor"`
	}
	err := userContactProviderJSON(ctx, providers.ProviderOutlook, http.MethodPatch, endpoint, token, map[string]any{"hexColor": color}, &reply)
	var outlook outlookAPIError
	switch {
	case err == nil && !strings.EqualFold(strings.TrimSpace(reply.HexColor), color):
		return errCalendarColorRefused
	case err == nil || !errors.As(err, &outlook):
		return err
	case outlook.Status == http.StatusNotFound || outlook.Status == http.StatusGone:
		return sql.ErrNoRows
	case outlook.Status == http.StatusBadRequest || outlook.Status == http.StatusForbidden:
		return errCalendarColorRefused
	}
	return err
}

// calendarTextColor is black or white, whichever reads better on color,
// for the text Google Calendar puts on events of that color.
func calendarTextColor(color string) string {
	value, err := strconv.ParseUint(strings.TrimPrefix(color, "#"), 16, 32)
	if err != nil {
		return "#000000"
	}
	r, g, b := float64(value>>16&0xff), float64(value>>8&0xff), float64(value&0xff)
	if 0.299*r+0.587*g+0.114*b < 140 {
		return "#ffffff"
	}
	return "#000000"
}
