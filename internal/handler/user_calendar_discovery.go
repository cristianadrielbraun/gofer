package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const calendarDiscoveryMaxPages = 100
const calendarDiscoveryMaxSources = 4096

func calendarDiscoveryPageURL(collection, next string) (string, error) {
	base, err := url.Parse(collection)
	if err != nil {
		return "", err
	}
	link, err := url.Parse(strings.TrimSpace(next))
	if err != nil {
		return "", err
	}
	link = base.ResolveReference(link)
	if link.Scheme != base.Scheme || link.Host != base.Host || link.Path != base.Path || link.User != nil || link.Fragment != "" || link.EscapedPath() != base.EscapedPath() {
		return "", errors.New("calendar pagination is outside the configured collection")
	}
	return link.String(), nil
}

type userCalendarRequest struct {
	h                *Handler
	snapshot         *config.UserCalendarDiscoverySnapshot
	claim            *config.UserCalendarSyncClaim
	event            *config.UserCalendarEventSnapshot
	source           *config.UserCalendarSourceSnapshot
	cleanup          *config.UserCalendarMeetingCleanupSnapshot
	cleanupReadID    string
	cleanupDeleteID  string
	cleanupETag      string
	reply            *config.UserCalendarReplySnapshot
	notification     *config.UserCalendarNotificationSnapshot
	followup         *config.UserCalendarReplyFollowupClaim
	incoming         *config.UserCalendarIncomingMessageSnapshot
	incomingResponse *config.UserCalendarIncomingResponseClaim
	response         *config.UserCalendarResponseClaim
	responseAttempt  *userCalendarResponseAttempt
	writeAttempt     *userCalendarResponseAttempt
	meeting          *config.UserCalendarMeetingDraftSnapshot
	meetingEvent     *config.UserCalendarEventSnapshot
	create           *config.UserCalendarCreateClaim
	write            bool
	credentials      *mailauth.UserAccountCredentials
	authorization    *mailauth.UserServiceAuthorization
	token            string
}

func (p *userCalendarRequest) service() *config.AccountServiceSnapshot {
	if p.cleanup != nil {
		return p.cleanup.Service()
	}
	if p.event != nil {
		return p.event.Service()
	}
	if p.source != nil {
		return p.source.Service()
	}
	if p.claim != nil {
		return p.claim.Service()
	}
	return p.snapshot.Service()
}
func (p *userCalendarRequest) provider() string {
	if p.cleanup != nil {
		return p.cleanup.Source().Provider
	}
	if p.event != nil {
		return p.event.Source().Provider
	}
	if p.source != nil {
		return p.source.Source().Provider
	}
	if p.claim != nil {
		return p.claim.Source().Provider
	}
	return p.snapshot.Provider()
}
func (p *userCalendarRequest) validate(ctx context.Context) error {
	if p == nil || p.h == nil || p.h.userAccounts == nil {
		return storage.ErrCalendarSourceChanged
	}
	scopes := 0
	for _, present := range []bool{p.snapshot != nil, p.claim != nil, p.event != nil, p.source != nil, p.cleanup != nil} {
		if present {
			scopes++
		}
	}
	if scopes != 1 || (p.write && p.event == nil && p.source == nil && p.cleanup == nil) {
		return storage.ErrCalendarSourceChanged
	}
	if p.cleanup != nil && (p.reply != nil || p.notification != nil || p.followup != nil || p.response != nil || p.meeting != nil || p.meetingEvent != nil || p.create != nil) {
		return storage.ErrCalendarSourceChanged
	}
	var guards []func() error
	if p.authorization != nil {
		guards = append(guards, func() error {
			service := p.service()
			return p.h.userCredentials.ValidateCalendarAuthorization(ctx, p.authorization, service.OwnerID(), service.AccountID(), p.write)
		})
	}
	var err error
	if p.cleanup != nil {
		err = p.h.userAccounts.ValidateCalendarMeetingCleanup(ctx, p.cleanup, guards...)
	} else if p.event != nil {
		err = p.h.userAccounts.ValidateCalendarEvent(ctx, p.event, guards...)
	} else if p.source != nil {
		err = p.h.userAccounts.ValidateCalendarSource(ctx, p.source, guards...)
	} else if p.claim != nil {
		err = p.h.userAccounts.ValidateCalendarSync(ctx, p.claim)
	} else {
		err = p.h.userAccounts.ValidateCalendarDiscovery(ctx, p.snapshot)
	}
	if err != nil {
		return err
	}
	if p.reply != nil {
		if err := p.h.userAccounts.ValidateCalendarReply(ctx, p.reply); err != nil {
			return err
		}
	}
	if p.create != nil {
		if p.source == nil || !p.write || p.create.Source() != p.source || (p.provider() != storage.CalendarSourceProviderCalDAV && p.authorization == nil) {
			return storage.ErrCalendarCreateConflict
		}
		if err := p.h.userAccounts.ValidateCalendarCreate(ctx, p.create, guards...); err != nil {
			return err
		}
	}
	if p.meetingEvent != nil {
		if p.source == nil || p.meetingEvent.Source().ID != p.source.Source().ID || p.meetingEvent.Source().UserID != p.source.Source().UserID {
			return storage.ErrCalendarEventChanged
		}
		if err := p.h.userAccounts.ValidateCalendarEvent(ctx, p.meetingEvent, guards...); err != nil {
			return err
		}
	}
	if p.meeting != nil {
		if !p.write || p.authorization == nil {
			return storage.ErrCalendarCreateConflict
		}
		source := p.meeting.Source().Source()
		original := p.actionSource()
		if source.ID != original.ID || source.UserID != original.UserID || source.AccountID != original.AccountID || source.Provider != original.Provider || source.RemoteID != original.RemoteID {
			return storage.ErrCalendarSourceChanged
		}
		if err := p.h.userAccounts.ValidateCalendarMeetingDraft(ctx, p.meeting, guards...); err != nil {
			return err
		}
	}
	if p.notification != nil {
		if err := p.h.userAccounts.ValidateCalendarNotification(ctx, p.notification); err != nil {
			return err
		}
	}
	if p.followup != nil {
		if err := p.h.userAccounts.ValidateCalendarReplyFollowup(ctx, p.followup); err != nil {
			return err
		}
	}
	if p.response != nil {
		if p.event == nil || !p.write || p.response.Event() != p.event || (p.provider() != storage.CalendarSourceProviderCalDAV && p.authorization == nil) {
			return storage.ErrCalendarEventChanged
		}
		if err := p.h.userAccounts.ValidateCalendarResponse(ctx, p.response, guards...); err != nil {
			return err
		}
	}
	if p.incoming != nil {
		if p.event == nil || !p.write || p.provider() != storage.CalendarSourceProviderCalDAV || p.incoming.Candidate().UserID != p.event.Service().OwnerID() || p.incoming.Candidate().AccountID != p.event.Service().AccountID() {
			return storage.ErrCalendarIncomingChanged
		}
		if err := p.h.userAccounts.ValidateCalendarIncomingAssociation(ctx, p.incoming, p.event); err != nil {
			return err
		}
	}
	if p.incomingResponse != nil {
		if p.incoming == nil || p.incomingResponse.Message() != p.incoming || p.incomingResponse.Event() != p.event {
			return storage.ErrCalendarIncomingChanged
		}
		if err := p.h.userAccounts.ValidateCalendarIncomingResponse(ctx, p.incomingResponse); err != nil {
			return err
		}
	}
	if p.authorization != nil {
		if err := guards[0](); err != nil {
			return err
		}
	}
	return nil
}

func (p *userCalendarRequest) ready(ctx context.Context) error {
	if err := p.validate(ctx); err != nil {
		return err
	}
	if p.h.userStorage == nil {
		return storage.ErrCalendarSourceChanged
	}
	service := p.service()
	until, err := p.h.userStorage.ProviderRetryUntil(ctx, service.OwnerID(), service.AccountID())
	if err != nil {
		return err
	}
	if until.After(time.Now()) {
		return userContactDeferred{at: until}
	}
	return nil
}

func (p *userCalendarRequest) recordRetry(ctx context.Context, err error) error {
	var hint interface{ RetryAfter() (time.Time, bool) }
	if errors.As(err, &hint) {
		if at, ok := hint.RetryAfter(); ok && at.After(time.Now()) {
			if changed := p.validate(ctx); changed != nil {
				return errors.Join(err, changed)
			}
			service := p.service()
			return errors.Join(err, p.h.userStorage.DeferProviderRetry(ctx, service.OwnerID(), service.AccountID(), at))
		}
	}
	return err
}

func (p *userCalendarRequest) getJSON(ctx context.Context, endpoint string, out any) error {
	if err := p.ready(ctx); err != nil {
		return err
	}
	if p.token == "" {
		var err error
		p.authorization, err = p.credentials.ServiceAuthorization(ctx, false)
		if err != nil {
			return p.recordRetry(ctx, err)
		}
		p.token = p.authorization.Token()
	}
	if err := p.validate(ctx); err != nil {
		return err
	}
	request := func() error {
		// Only the bounded HTTP/JSON helper is shared with contacts; credentials
		// and lifecycle authorization above are exclusively calendar-scoped.
		return userContactProviderJSON(ctx, p.provider(), http.MethodGet, endpoint, p.token, nil, out, func() error { return p.ready(ctx) })
	}
	err := request()
	if !contactAPIUnauthorized(err) {
		return p.recordRetry(ctx, err)
	}
	authorization, refreshErr := p.h.userCredentials.RefreshServiceAuthorization(ctx, p.authorization)
	if refreshErr != nil {
		return p.recordRetry(ctx, refreshErr)
	}
	p.authorization = authorization
	p.token = p.authorization.Token()
	if err := p.ready(ctx); err != nil {
		return err
	}
	return p.recordRetry(ctx, request())
}

type calendarDiscoveryDAVGuardKey struct{}
type calendarDiscoveryDAVCallbacks struct {
	ready   func(context.Context) error
	failure func(context.Context, error) error
}

func calendarDiscoveryDAVGuard(ctx context.Context) error {
	if callbacks, ok := ctx.Value(calendarDiscoveryDAVGuardKey{}).(calendarDiscoveryDAVCallbacks); ok {
		return callbacks.ready(ctx)
	}
	return nil
}

func calendarDiscoveryDAVFailure(ctx context.Context, err error) error {
	if callbacks, ok := ctx.Value(calendarDiscoveryDAVGuardKey{}).(calendarDiscoveryDAVCallbacks); ok {
		return callbacks.failure(ctx, err)
	}
	return err
}

func calendarDiscoveredSources(calendars []calendar.RemoteCalendar) []storage.CalendarSource {
	hasPrimary := false
	for _, remote := range calendars {
		hasPrimary = hasPrimary || remote.Primary
	}
	sources := make([]storage.CalendarSource, 0, len(calendars))
	for index, remote := range calendars {
		sources = append(sources, storage.CalendarSource{RemoteID: remote.RemoteID, Name: remote.Name, Description: remote.Description,
			TimeZone: remote.TimeZone, Color: remote.Color, AccessRole: remote.AccessRole, IsPrimary: remote.Primary,
			IsSelected: remote.Primary || (!hasPrimary && index == 0)})
	}
	return sources
}

func (p *userCalendarRequest) discoverDAV(ctx context.Context, r *http.Request) ([]storage.CalendarSource, *config.DiscoveredCalDAVSettings, error) {
	service := p.service()
	identity := service.Identity()
	savedURL, savedUsername, _ := service.CalDAVSettings()
	autodiscover, useAccount := r.PostForm.Get("autodiscover") == "1", r.PostForm.Get("use_account_credentials") == "1"
	baseURL := strings.TrimSpace(r.PostForm.Get("caldav_url"))
	if baseURL == "" && !autodiscover {
		baseURL = savedURL
	}
	username := strings.TrimSpace(r.PostForm.Get("username"))
	if useAccount {
		username = strings.TrimSpace(identity.Username)
		if username == "" {
			username = identity.EmailAddress
		}
	} else if username == "" {
		username = savedUsername
	}
	password, err := service.CalDAVPassword(r.PostForm.Get("password"), useAccount)
	if err != nil {
		return nil, nil, err
	}
	if username == "" || strings.TrimSpace(password) == "" {
		return nil, nil, errors.New("Enter the CalDAV username and password or use the incoming-mail credentials.")
	}
	ctx = context.WithValue(ctx, calendarDiscoveryDAVGuardKey{}, calendarDiscoveryDAVCallbacks{ready: p.ready, failure: p.recordRetry})
	var sources []storage.CalendarSource
	if autodiscover {
		candidates := calDAVAutodiscoveryCandidates(ctx, baseURL, identity.EmailAddress, identity.IMAPHost, identity.SMTPHost)
		sources, baseURL, err = discoverCalDAVCalendarsCandidates(ctx, candidates, username, password, service.OwnerID(), service.AccountID())
	} else {
		baseURL, err = normalizeCalDAVBaseURL(baseURL)
		if err == nil {
			sources, err = discoverCalDAVCalendars(ctx, baseURL, username, password, service.OwnerID(), service.AccountID())
		}
	}
	if err != nil {
		return nil, nil, err
	}
	if len(sources) > calendarDiscoveryMaxSources {
		return nil, nil, errors.New("CalDAV discovery exceeded its source limit")
	}
	return sources, &config.DiscoveredCalDAVSettings{BaseURL: baseURL, Username: username, Password: password, UseAccountCredentials: useAccount}, nil
}

func (h *Handler) handleUserDiscoverCalendars(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid calendar discovery request", http.StatusBadRequest)
		return
	}
	owner, id := h.userID(r.Context()), strings.TrimSpace(r.PathValue("id"))
	message, failed := "", false
	err := h.userIMAP.RunAccountService(r.Context(), owner, id, mail.AccountServiceCalendar, cardDAVDiscoveryOverallTimeout, func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotCalendarDiscovery(ctx, owner, id)
		if err != nil {
			return err
		}
		request := &userCalendarRequest{h: h, snapshot: snapshot}
		var sources []storage.CalendarSource
		var dav *config.DiscoveredCalDAVSettings
		if snapshot.Provider() == storage.CalendarSourceProviderCalDAV {
			sources, dav, err = request.discoverDAV(ctx, r)
		} else if h.userCredentials == nil {
			err = errors.New("Mailbox OAuth is not configured for this Gofer instance.")
		} else {
			request.credentials = h.userCredentials.CalendarAccount(owner, id, false)
			var calendars []calendar.RemoteCalendar
			if snapshot.Provider() == "gmail" {
				calendars, err = discoverGoogleCalendarsWithFetch(ctx, func(endpoint string, out any) error { return request.getJSON(ctx, endpoint, out) })
			} else {
				calendars, err = discoverOutlookCalendarsWithFetch(ctx, func(endpoint string, out any) error { return request.getJSON(ctx, endpoint, out) })
			}
			sources = calendarDiscoveredSources(calendars)
		}
		if err != nil {
			if changed := request.validate(ctx); changed != nil {
				return changed
			}
			if errors.Is(err, mailauth.ErrMailboxAuthorizationChanged) || errors.Is(err, storage.ErrCalendarDiscoveryChanged) || errors.Is(err, config.ErrAccountServicesChanged) || errors.Is(err, storage.ErrAccountRoute) || errors.Is(err, storage.ErrUserStoreOwner) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			message, failed = "Calendar discovery failed: "+err.Error(), true
			return nil
		}
		var guards []func() error
		if request.authorization != nil {
			guards = append(guards, func() error { return h.userCredentials.ValidateServiceAuthorization(ctx, request.authorization) })
		}
		if _, err := h.userAccounts.PublishCalendarDiscovery(ctx, snapshot, sources, dav, guards...); err != nil {
			return err
		}
		h.wakeUserCalendarAccount(ctx, owner, id)
		label := "Google calendar source(s)"
		if snapshot.Provider() == "outlook" {
			label = "Microsoft calendar source(s)"
		} else if snapshot.Provider() == storage.CalendarSourceProviderCalDAV {
			label = "CalDAV calendar(s)"
		}
		message = fmt.Sprintf("Discovered %d %s. The primary calendar is selected by default.", len(sources), label)
		return nil
	})
	if err != nil {
		if errors.Is(err, storage.ErrCalendarDiscoveryChanged) || errors.Is(err, config.ErrAccountServicesChanged) || errors.Is(err, mailauth.ErrMailboxAuthorizationChanged) {
			http.Error(w, "Calendar access or settings changed; retry discovery.", http.StatusConflict)
		} else {
			userAccountError(w, r, err)
		}
		return
	}
	data, err := h.userEditData(r.Context(), owner, id)
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := views.CalendarSyncSettingsResult(*data, message, failed).Render(r.Context(), w); err != nil {
		log.Printf("owned calendar: render discovery: %v", err)
	}
}
