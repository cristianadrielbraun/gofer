package handler

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// A cleanup grant never authorizes creation, PATCH, RSVP or unrelated reads.
// Deletion additionally needs the exact version from a verified native read.
func (p *userCalendarRequest) meetingCleanupEndpoint(req *http.Request) error {
	denied := storage.ErrCalendarCreateConflict
	if p.cleanup == nil || p.cleanup.Prune() || !p.write || (req.Method != http.MethodGet && req.Method != http.MethodDelete) {
		return denied
	}
	source := p.cleanup.Source()
	baseURL := googleCalendarAPIBaseURL
	collection := "/calendars/" + url.PathEscape(source.RemoteID) + "/events"
	if source.Provider == "outlook" {
		baseURL = outlookGraphBaseURL
		collection = "/me/calendars/" + url.PathEscape(source.RemoteID) + "/events"
	}
	base, err := url.Parse(baseURL)
	if err != nil || !calendarProviderSameOrigin(base, req.URL) {
		return denied
	}
	path := strings.TrimSuffix(base.EscapedPath(), "/") + collection
	if !strings.HasPrefix(req.URL.EscapedPath(), path) {
		return denied
	}
	parts, ok := calendarProviderChildSegments(strings.TrimPrefix(req.URL.EscapedPath(), path))
	if !ok {
		return denied
	}
	query, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		return denied
	}
	if source.Provider == "outlook" && len(parts) == 0 && req.Method == http.MethodGet && p.cleanup.Teams().RemoteID == "" {
		expected := url.Values{"$expand": {calendarTeamsDraftExpand()}, "$top": {"2"}, "$filter": {"singleValueExtendedProperties/Any(ep: ep/id eq '" + calendarTeamsDraftProperty + "' and ep/value eq 'draft:" + p.cleanup.Teams().DraftID + "')"}}
		if query.Encode() == expected.Encode() {
			return nil
		}
		return denied
	}
	if len(parts) != 1 {
		return denied
	}
	id, err := url.PathUnescape(parts[0])
	if err != nil {
		return denied
	}
	expected := p.cleanup.Google().RemoteID
	if source.Provider == "outlook" {
		expected = p.cleanup.Teams().RemoteID
		if expected == "" {
			expected = p.cleanupReadID
		}
	}
	if expected == "" || id != expected {
		return denied
	}
	if req.Method == http.MethodDelete {
		if id != p.cleanupDeleteID || p.cleanupETag == "" || req.Header.Get("If-Match") != p.cleanupETag || !calendarUpdateValidETag(p.cleanupETag, source.Provider == "outlook") {
			return denied
		}
		if source.Provider == "gmail" && query.Encode() != (url.Values{"sendUpdates": {"none"}}).Encode() {
			return denied
		}
		if source.Provider == "outlook" && len(query) != 0 {
			return denied
		}
		return nil
	}
	if source.Provider == "outlook" {
		if query.Encode() != (url.Values{"$expand": {calendarTeamsDraftExpand()}}).Encode() {
			return denied
		}
	} else if len(query) != 0 {
		return denied
	}
	return nil
}

func (p *userCalendarRequest) allowMeetingCleanupRead(ctx context.Context, draft, id string) error {
	if p.cleanup == nil || p.cleanup.Prune() || p.provider() != "outlook" || draft != p.cleanup.Teams().DraftID || id == "" || p.cleanup.Teams().RemoteID != "" || (p.cleanupReadID != "" && p.cleanupReadID != id) {
		return storage.ErrCalendarCreateConflict
	}
	if err := p.ready(ctx); err != nil {
		return err
	}
	p.cleanupReadID = id
	return nil
}

func (p *userCalendarRequest) allowMeetingCleanupDelete(ctx context.Context, source storage.CalendarSource, draft, id, etag string) error {
	if p.cleanup == nil || p.cleanup.Prune() || !p.write || source.ID != p.cleanup.Source().ID || source.UserID != p.cleanup.Source().UserID || source.AccountID != p.cleanup.Source().AccountID || source.Provider != p.provider() || source.RemoteID != p.cleanup.Source().RemoteID {
		return storage.ErrCalendarCreateConflict
	}
	expected, expectedDraft := p.cleanup.Google().RemoteID, p.cleanup.Google().DraftID
	if p.provider() == "outlook" {
		expected, expectedDraft = p.cleanup.Teams().RemoteID, p.cleanup.Teams().DraftID
		if expected == "" {
			expected = p.cleanupReadID
		}
		if p.cleanup.Teams().State != "abandoned" || p.cleanup.FinalResult() != (storage.CalendarCreateRequest{}) {
			return storage.ErrCalendarCreateConflict
		}
	}
	if expected == "" || expected != id || expectedDraft != draft || !calendarUpdateValidETag(etag, p.provider() == "outlook") {
		return storage.ErrCalendarCreateConflict
	}
	if err := p.ready(ctx); err != nil {
		return err
	}
	p.cleanupDeleteID, p.cleanupETag = id, etag
	return nil
}

func (p *userCalendarRequest) cleanupContext(ctx context.Context) (context.Context, error) {
	if p.cleanup == nil || p.cleanup.Prune() || p.h.userCredentials == nil {
		return nil, storage.ErrCalendarCreateConflict
	}
	p.write = true
	if err := p.ready(ctx); err != nil {
		return nil, err
	}
	service := p.service()
	p.credentials = p.h.userCredentials.CalendarAccount(service.OwnerID(), service.AccountID(), true)
	var err error
	p.authorization, err = p.h.userCredentials.SnapshotCalendarAuthorization(ctx, service.OwnerID(), service.AccountID(), p.provider(), true)
	if err != nil {
		return nil, err
	}
	if err := p.ready(ctx); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, userCalendarProviderKey{}, p), nil
}

func (p *userCalendarRequest) publishMeetingCleanup(ctx context.Context, transition storage.CalendarMeetingDraftTransition) error {
	guards, err := p.meetingWriteGuards(ctx)
	if err != nil {
		return err
	}
	next, err := p.h.userAccounts.PublishCalendarMeetingCleanup(ctx, p.cleanup, transition, guards...)
	if err == nil {
		p.cleanup = next
	}
	return err
}

func (p *userCalendarRequest) runMeetingCleanup(ctx context.Context) error {
	if p.provider() == "outlook" && p.cleanup.Teams().State == "saving" && p.cleanup.FinalResult().RemoteID != "" {
		// The typed completed create proves acceptance even after deselection.
		// This local recovery never refreshes a token or inspects a marker prefix.
		return p.publishMeetingCleanup(ctx, storage.CalendarMeetingDraftSaved)
	}
	if p.provider() == "outlook" && p.cleanup.Teams().State == "active" {
		if err := p.publishMeetingCleanup(ctx, storage.CalendarMeetingDraftAbandon); err != nil {
			return err
		}
	}
	authorization, err := p.h.userCredentials.EnsureServiceAuthorization(ctx, p.authorization)
	if err != nil {
		return p.recordRetry(ctx, err)
	}
	p.authorization, p.token = authorization, authorization.Token()
	if err := p.ready(ctx); err != nil {
		return err
	}
	if p.provider() == "gmail" {
		if err := cleanupGoogleMeetDraft(ctx, p.actionSource(), p.cleanup.Google(), p.token); err != nil {
			return err
		}
		return p.publishMeetingCleanup(ctx, storage.CalendarMeetingDraftCleaned)
	}
	if p.cleanup.Teams().State == "saving" {
		draft := p.cleanup.Teams()
		remote, err := readCalendarTeamsDraft(ctx, p.actionSource(), draft.RemoteID, p.token)
		var missing calendarCreateProviderError
		gone := errors.As(err, &missing) && (missing.Status == 404 || missing.Status == 410)
		if err != nil && !gone {
			return err
		}
		if !gone && !calendarTeamsDraftPrivate(remote, draft) {
			// A finalized native save with a missing local commit must survive.
			// Neither a marker prefix nor a guessed post-conference hash proves
			// a completed original create. Leave the reservation unresolved.
			return nil
		}
		if err := p.publishMeetingCleanup(ctx, storage.CalendarMeetingDraftExpire); err != nil {
			return err
		}
	}
	if err := cleanupCalendarTeamsDraft(ctx, p.actionSource(), p.cleanup.Teams(), p.token); err != nil {
		return err
	}
	return p.publishMeetingCleanup(ctx, storage.CalendarMeetingDraftCleaned)
}

func (h *Handler) runUserCalendarMeetingCleanup(parent context.Context, owner, account string) error {
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	var failures []error
	visited := make(map[storage.CalendarMeetingCleanupCandidate]bool, 8)
	for range 8 {
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
		candidate, err := h.userAccounts.NextCalendarMeetingCleanup(ctx, owner, account)
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		if candidate == nil || visited[*candidate] {
			break
		}
		visited[*candidate] = true
		c, err := h.userAccounts.SnapshotCalendarMeetingCleanup(ctx, owner, account, candidate.SourceID, candidate.DraftID, candidate.Prune)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		p := &userCalendarRequest{h: h, cleanup: c}
		err = h.userIMAP.RunAccountService(ctx, owner, account, mail.AccountServiceCalendar, 15*time.Second, func(operation context.Context) error {
			unlock, err := h.lockCalendarCreate(operation, c.Source())
			if err != nil {
				return err
			}
			defer unlock()
			if err := p.validate(operation); err != nil {
				return err
			}
			if c.Prune() {
				return h.userAccounts.PruneCalendarMeetingCleanup(operation, c)
			}
			operation, err = p.cleanupContext(operation)
			if err != nil {
				return err
			}
			return p.runMeetingCleanup(operation)
		})
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
