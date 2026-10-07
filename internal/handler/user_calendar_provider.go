package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type userCalendarProviderKey struct{}

// Existing provider helpers keep their response/version/idempotency handling.
// An owned action attaches this private dispatch boundary to its joined context.
// Legacy shared-mode callers retain their existing transport behavior.
func calendarProviderDo(client *http.Client, req *http.Request) (*http.Response, error) {
	if request, ok := req.Context().Value(userCalendarProviderKey{}).(*userCalendarRequest); ok {
		return request.doProvider(client, req)
	}
	return client.Do(req)
}

func (p *userCalendarRequest) actionContext(ctx context.Context, write bool) (context.Context, error) {
	if p == nil || (p.event == nil && p.source == nil) {
		return nil, storage.ErrCalendarSourceChanged
	}
	p.write = write
	if err := p.ready(ctx); err != nil {
		return nil, err
	}
	if p.provider() != storage.CalendarSourceProviderCalDAV {
		if p.h.userCredentials == nil {
			return nil, errors.New("mailbox OAuth is not configured")
		}
		service := p.service()
		p.credentials = p.h.userCredentials.CalendarAccount(service.OwnerID(), service.AccountID(), write)
		var err error
		p.authorization, err = p.credentials.ServiceAuthorization(ctx, false)
		if err != nil {
			return nil, p.recordRetry(ctx, err)
		}
		p.token = p.authorization.Token()
	}
	if err := p.ready(ctx); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, userCalendarProviderKey{}, p), nil
}

func (p *userCalendarRequest) actionSource() storage.CalendarSource {
	if p.cleanup != nil {
		return p.cleanup.Source()
	}
	if p.event != nil {
		return p.event.Source()
	}
	return p.source.Source()
}

// A provider helper cannot turn a caller-supplied account/source into authority.
// Validate the private copied scope before credentials or legacy callbacks.
func (p *userCalendarRequest) credentialsForSource(ctx context.Context, h *Handler, source storage.CalendarSource) (calendarCredentials, error) {
	if p == nil || p.h != h || (p.event == nil && p.source == nil) || !p.write {
		return calendarCredentials{}, storage.ErrCalendarSourceChanged
	}
	original := p.actionSource()
	if source.ID != original.ID || source.UserID != original.UserID || source.AccountID != original.AccountID || source.RemoteID != original.RemoteID || source.Provider != original.Provider {
		return calendarCredentials{}, storage.ErrCalendarSourceChanged
	}
	if err := p.ready(ctx); err != nil {
		return calendarCredentials{}, err
	}
	return p.actionCredentials()
}

func calendarProviderSameOrigin(a, b *url.URL) bool {
	return a != nil && b != nil && a.Scheme == b.Scheme && a.Host != "" && a.Host == b.Host && a.User == nil && a.Fragment == "" && a.Opaque == "" && b.User == nil && b.Fragment == "" && b.Opaque == ""
}

// OAuth IDs are opaque, including an encoded slash. Structural separators and
// the collection prefix must still match exactly; decoded prefixes would let a
// different calendar impersonate an encoded opaque identifier.
func calendarProviderChildSegments(suffix string) ([]string, bool) {
	if suffix == "" {
		return nil, true
	}
	if !strings.HasPrefix(suffix, "/") {
		return nil, false
	}
	segments := strings.Split(suffix[1:], "/")
	for _, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil || decoded == "" || decoded == "." || decoded == ".." || strings.ContainsAny(decoded, "\\\x00\r\n") || url.PathEscape(decoded) != segment {
			return nil, false
		}
	}
	return segments, true
}

func (p *userCalendarRequest) providerEndpoint(req *http.Request) error {
	if p.cleanup != nil {
		return p.meetingCleanupEndpoint(req)
	}
	source := p.actionSource()
	u := req.URL
	denied := errors.New("calendar request is outside the selected source")
	if source.RemoteID == "" || u == nil {
		return denied
	}
	if !p.write && req.Method != http.MethodGet && req.Method != http.MethodHead && req.Method != "OPTIONS" && req.Method != "PROPFIND" && req.Method != "REPORT" {
		return denied
	}
	if source.Provider == storage.CalendarSourceProviderCalDAV {
		base, err := url.Parse(source.RemoteID)
		configured, _, _ := p.service().CalDAVSettings()
		trusted, parseErr := url.Parse(configured)
		if err != nil || parseErr != nil || base.Scheme != "https" || base.RawQuery != "" || !calendarProviderSameOrigin(trusted, base) || !calendarProviderSameOrigin(base, u) || strings.ContainsAny(u.Path, "\\\x00\r\n") {
			return denied
		}
		clean := path.Clean(u.Path)
		if strings.TrimSuffix(u.Path, "/") != strings.TrimSuffix(clean, "/") || strings.Contains(strings.ToLower(u.EscapedPath()), "%2f") {
			return denied
		}
		// RFC 6638 principal discovery can leave the calendar collection, but
		// may only read properties on this exact configured credential origin.
		if req.Method == "PROPFIND" {
			return nil
		}
		collection := strings.TrimSuffix(base.EscapedPath(), "/")
		inside := u.EscapedPath() == collection || u.EscapedPath() == collection+"/" || strings.HasPrefix(u.EscapedPath(), collection+"/")
		if !inside {
			return denied
		}
		switch req.Method {
		case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, "OPTIONS", "REPORT":
			return nil
		}
		return denied
	}
	baseURL := googleCalendarAPIBaseURL
	collection := "/calendars/" + url.PathEscape(source.RemoteID)
	if source.Provider == "outlook" {
		baseURL = outlookGraphBaseURL
		collection = "/me/calendars/" + url.PathEscape(source.RemoteID)
	} else if source.Provider != "gmail" {
		return denied
	}
	base, err := url.Parse(baseURL)
	if err != nil || !calendarProviderSameOrigin(base, u) {
		return denied
	}
	basePath := strings.TrimSuffix(base.EscapedPath(), "/")
	if source.Provider == "gmail" && req.Method == http.MethodGet && u.EscapedPath() == basePath+"/users/me/calendarList/"+url.PathEscape(source.RemoteID) {
		return nil // Calendar-specific Google Meet capability.
	}
	calendarPath := basePath + collection
	if source.Provider == "outlook" && req.Method == http.MethodGet && u.EscapedPath() == calendarPath {
		return nil // Calendar-specific Teams capability.
	}
	events := calendarPath + "/events"
	if !strings.HasPrefix(u.EscapedPath(), events) {
		return denied
	}
	segments, ok := calendarProviderChildSegments(strings.TrimPrefix(u.EscapedPath(), events))
	if !ok {
		return denied
	}
	switch req.Method {
	case http.MethodGet:
		if len(segments) <= 1 || (len(segments) == 2 && segments[1] == "instances") {
			return nil
		}
	case http.MethodPost:
		if len(segments) == 0 || (source.Provider == "outlook" && len(segments) == 2 && (segments[1] == "accept" || segments[1] == "tentativelyAccept" || segments[1] == "decline")) {
			return nil
		}
	case http.MethodPatch, http.MethodDelete:
		if len(segments) == 1 {
			return nil
		}
	}
	return denied
}

// Retry only the single request definitively rejected with 401, never an entire
// create/update/RSVP sequence or an ambiguous transport/5xx result. Every attempt
// validates source, configuration, grant revision and the durable cooldown.
func (p *userCalendarRequest) doProvider(client *http.Client, original *http.Request) (*http.Response, error) {
	if err := p.ready(original.Context()); err != nil {
		return nil, err
	}
	if p.event == nil && p.source == nil && p.cleanup == nil {
		return nil, storage.ErrCalendarSourceChanged
	}
	if err := p.providerEndpoint(original); err != nil {
		return nil, err
	}
	// Refuse redirects before any second destination is dispatched. Copy rather
	// than mutate a client that a compound action may reuse for later requests.
	bounded := *client
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	dispatch := func(req *http.Request) (*http.Response, error) {
		if err := p.ready(req.Context()); err != nil {
			return nil, err
		}
		copy := req.Clone(req.Context())
		if p.provider() != storage.CalendarSourceProviderCalDAV {
			if p.authorization == nil || p.token == "" {
				return nil, errors.New("calendar dispatch requires scoped authorization")
			}
			copy.Header.Set("Authorization", "Bearer "+p.token)
		} else {
			service := p.service()
			_, username, useAccount := service.CalDAVSettings()
			if useAccount {
				username = strings.TrimSpace(service.Identity().Username)
				if username == "" {
					username = service.Identity().EmailAddress
				}
			}
			password, err := service.CalDAVPassword("", useAccount)
			if err != nil {
				return nil, err
			}
			if username == "" || password == "" {
				return nil, errors.New("complete Calendar authentication in account setup")
			}
			copy.SetBasicAuth(username, password)
		}
		if p.response != nil {
			p.responseAttempt.started(copy)
		}
		p.writeAttempt.started(copy)
		response, err := bounded.Do(copy)
		if err != nil {
			return nil, err
		}
		if p.response != nil {
			p.responseAttempt.received(copy, response.StatusCode)
		}
		p.writeAttempt.received(copy, response.StatusCode)
		if err := p.validate(req.Context()); err != nil {
			response.Body.Close()
			return nil, err // Remote write may have happened; no success claim.
		}
		if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable {
			if at := providerRetryAfter(response); at.After(time.Now()) {
				service := p.service()
				if err := p.h.userStorage.DeferProviderRetry(req.Context(), service.OwnerID(), service.AccountID(), at); err != nil {
					response.Body.Close()
					return nil, errors.Join(calendarCreateProviderError{response.StatusCode}, err)
				}
			}
		}
		return response, nil
	}
	response, err := dispatch(original)
	if err != nil || response.StatusCode != http.StatusUnauthorized || p.provider() == storage.CalendarSourceProviderCalDAV || p.credentials == nil || (original.Body != nil && original.Body != http.NoBody && original.GetBody == nil) {
		return response, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	response.Body.Close()
	// p.ready checks the old revision before a forced refresh. Reconnect while
	// waiting must not turn this operation into an action on a new grant.
	if err := p.ready(original.Context()); err != nil {
		return nil, err
	}
	authorization, refreshErr := p.h.userCredentials.RefreshServiceAuthorization(original.Context(), p.authorization)
	if refreshErr != nil {
		return nil, p.recordRetry(original.Context(), refreshErr)
	}
	p.authorization = authorization
	p.token = p.authorization.Token()
	replay := original.Clone(original.Context())
	if original.GetBody != nil {
		replay.Body, err = original.GetBody()
		if err != nil {
			return nil, err
		}
		defer replay.Body.Close()
	}
	return dispatch(replay)
}
