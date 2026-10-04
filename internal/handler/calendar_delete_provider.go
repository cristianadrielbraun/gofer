package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// A failed read-before-delete cannot have removed anything, unlike an ambiguous
// response after sending DELETE. Keep that distinction in the UI's retry state.
type calendarDeletePreflightError struct{ err error }

func (e calendarDeletePreflightError) Error() string { return e.err.Error() }
func (e calendarDeletePreflightError) Unwrap() error { return e.err }

func calendarDeleteClient(transport http.RoundTripper) *http.Client {
	return &http.Client{Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func calendarDeleteHTTP(client *http.Client, req *http.Request, allowOK bool) error {
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent || (allowOK && response.StatusCode == http.StatusOK) {
		return nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return calendarUpdateHTTPError(calendarCreateProviderError{response.StatusCode})
	}
	// 202 (accepted) and DAV 207 (multi-status) do not confirm deletion.
	return fmt.Errorf("calendar provider did not confirm deletion (HTTP %d)", response.StatusCode)
}

func calendarDeleteJSON(ctx context.Context, endpoint, token, etag string) error {
	if !calendarUpdateValidETag(etag, true) {
		return calendarUpdateUnsupported("The provider did not supply a safe event version.")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return calendarDeletePreflightError{err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("If-Match", etag)
	req.Header.Set("Prefer", `IdType="ImmutableId"`)
	return calendarDeleteHTTP(calendarDeleteClient(nil), req, false)
}

// Google: https://developers.google.com/workspace/calendar/api/v3/reference/events/delete
func deleteGoogleCalendarEvent(ctx context.Context, token, calendarID string, existing storage.CalendarEvent) error {
	return deleteGoogleCalendarEventScope(ctx, token, calendarID, existing, false)
}

func deleteGoogleCalendarEventScope(ctx context.Context, token, calendarID string, existing storage.CalendarEvent, series bool, occurrenceScope ...bool) error {
	occurrence := calendarOccurrenceScope(occurrenceScope)
	if err := calendarUpdateExistingScopeRestriction(existing, series, occurrence); err != nil {
		return err
	}
	if !calendarUpdateValidETag(existing.ETag, false) {
		return calendarUpdateUnsupported("Refresh the calendar to retrieve a safe event version.")
	}
	endpoint := googleCalendarAPIBaseURL + "/calendars/" + url.PathEscape(calendarID) + "/events/" + url.PathEscape(existing.RemoteID)
	var current googleCalendarUpdateEvent
	if err := calendarCreateJSON(ctx, http.MethodGet, endpoint, token, nil, &current); err != nil {
		return calendarDeletePreflightError{calendarUpdateHTTPError(err)}
	}
	if current.ID != existing.RemoteID || current.ETag != existing.ETag {
		return errCalendarUpdateConflict
	}
	if series {
		if _, err := calendarGoogleSeriesEvent(current); err != nil {
			return calendarDeletePreflightError{err}
		}
	} else if occurrence {
		if reason := current.occurrenceRestriction(existing.SeriesRemoteID); reason != "" {
			return calendarUpdateUnsupported(reason)
		}
	} else if reason := current.restriction(); reason != "" {
		return calendarUpdateUnsupported(reason)
	}
	if current.Organizer.Self != nil && !*current.Organizer.Self {
		return calendarUpdateUnsupported("Invitations cannot be deleted in Gofer yet.")
	}
	if calendarUpdateHasDetails(current.Attendees) {
		endpoint += "?sendUpdates=all"
	}
	return calendarDeleteJSON(ctx, endpoint, token, current.ETag)
}

// Graph: https://learn.microsoft.com/en-us/graph/api/event-delete
func deleteOutlookCalendarEvent(ctx context.Context, token, calendarID string, existing storage.CalendarEvent) error {
	return deleteOutlookCalendarEventScope(ctx, token, calendarID, existing, false)
}

func deleteOutlookCalendarEventScope(ctx context.Context, token, calendarID string, existing storage.CalendarEvent, series bool, occurrenceScope ...bool) error {
	occurrence := calendarOccurrenceScope(occurrenceScope)
	if err := calendarUpdateExistingScopeRestriction(existing, series, occurrence); err != nil {
		return err
	}
	endpoint := outlookGraphBaseURL + "/me/calendars/" + url.PathEscape(calendarID) + "/events/" + url.PathEscape(existing.RemoteID)
	var current outlookCalendarUpdateEvent
	if err := calendarCreateJSON(ctx, http.MethodGet, endpoint, token, nil, &current); err != nil {
		return calendarDeletePreflightError{calendarUpdateHTTPError(err)}
	}
	if calendarOutlookOnline(current) {
		return calendarDeletePreflightError{calendarUpdateUnsupported("Online meetings cannot be deleted in Gofer yet.")}
	}
	if current.ID != existing.RemoteID || (existing.ETag != current.ChangeKey && existing.ETag != current.ODataETag) {
		return errCalendarUpdateConflict
	}
	if strings.TrimSpace(current.ChangeKey) == "" || !calendarUpdateValidETag(current.ODataETag, true) {
		return calendarUpdateUnsupported("Microsoft did not supply a safe event version.")
	}
	if series {
		if _, err := calendarOutlookSeriesEvent(current); err != nil {
			return calendarDeletePreflightError{err}
		}
	} else if occurrence {
		if reason := current.occurrenceRestriction(existing.SeriesRemoteID); reason != "" {
			return calendarUpdateUnsupported(reason)
		}
	} else if reason := current.restriction(); reason != "" {
		return calendarUpdateUnsupported(reason)
	}
	if current.IsOrganizer != nil && !*current.IsOrganizer {
		return calendarUpdateUnsupported("Invitations cannot be deleted in Gofer yet.")
	}
	// changeKey is not an HTTP validator; use the freshly read OData entity tag.
	return calendarDeleteJSON(ctx, endpoint, token, current.ODataETag)
}

func deleteCalDAVCalendarEvent(ctx context.Context, source storage.CalendarSource, username, password string, existing storage.CalendarEvent) error {
	return deleteCalDAVCalendarEventScope(ctx, source, username, password, existing, false)
}

func deleteCalDAVCalendarEventScope(ctx context.Context, source storage.CalendarSource, username, password string, existing storage.CalendarEvent, series bool) error {
	if err := calendarUpdateExistingScopeRestriction(existing, series); err != nil {
		return err
	}
	if !calendarUpdateValidETag(existing.ETag, false) {
		return calendarUpdateUnsupported("This calendar provider must supply a strong event version before deleting.")
	}
	endpoint, err := calendarUpdateCalDAVEndpoint(source, existing)
	if err != nil {
		return err
	}
	collection, _ := url.Parse(source.RemoteID)
	resource, _ := url.Parse(endpoint)
	collectionPath := strings.TrimSuffix(collection.Path, "/") + "/"
	if !strings.HasPrefix(resource.Path, collectionPath) || strings.HasSuffix(resource.Path, "/") || path.Clean(resource.Path) != resource.Path {
		return calendarUpdateUnsupported("Only an event resource inside this calendar can be deleted.")
	}
	client := calendarDeleteClient(calDAVHTTPTransport)
	current, headers, err := calendarUpdateCalDAVGet(ctx, client, endpoint, username, password)
	if err != nil {
		return calendarDeletePreflightError{err}
	}
	if headers.Get("ETag") != existing.ETag {
		return errCalendarUpdateConflict
	}
	// A DAV resource may contain a whole series: inspect the complete resource,
	// never DELETE just because one occurrence in the cached view looked simple.
	if series {
		location := time.UTC
		if source.TimeZone != "" {
			location, err = calendarSeriesLocation(source.TimeZone)
			if err != nil {
				return calendarDeletePreflightError{err}
			}
		}
		if _, err := calendarCalDAVDeleteSeriesEvent(current, headers, endpoint, location); err != nil {
			return calendarDeletePreflightError{err}
		}
	} else if reason := calendarUpdateCalDAVRestriction(current, headers); reason != "" {
		return calendarUpdateUnsupported(reason)
	}
	uid := current.Events()[0].Props.Get("UID")
	if uid == nil || strings.TrimSpace(uid.Value) == "" || (existing.ICalUID != "" && uid.Value != existing.ICalUID) {
		return errCalendarUpdateConflict
	}
	req, err := newCardDAVRequest(ctx, http.MethodDelete, endpoint, username, password, nil)
	if err != nil {
		return calendarDeletePreflightError{err}
	}
	req.Header.Set("If-Match", existing.ETag)
	return calendarDeleteHTTP(client, req, true)
}
