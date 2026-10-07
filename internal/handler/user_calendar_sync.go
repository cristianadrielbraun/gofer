package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mailauth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const calendarSyncMaxPages = 100
const calendarSyncMaxEvents = 100000

// Manual and scheduled reads coalesce by owner/source/window. This registry is
// separate from the native source lock: a queued sync must not hold that lock
// while waiting for an account gate owned by an action that needs the source.
// Every native sync/action acquires the account gate before the source lock.
func (h *Handler) syncUserCalendarSource(ctx context.Context, source storage.CalendarSource, start, end time.Time, scheduled, forced bool) (int, error) {
	key := source.UserID + "\x00" + source.ID
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		h.calendarSyncMu.Lock()
		if h.calendarUserSyncRunning == nil {
			h.calendarUserSyncRunning = make(map[string]*calendarSyncRun)
		}
		if active := h.calendarUserSyncRunning[key]; active != nil {
			h.calendarSyncMu.Unlock()
			if scheduled {
				return 0, nil
			}
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-active.done:
				if !active.start.After(start) && !active.end.Before(end) && !errors.Is(active.err, context.Canceled) {
					return active.count, active.err
				}
				continue
			}
		}
		run := &calendarSyncRun{start: start, end: end, done: make(chan struct{})}
		h.calendarUserSyncRunning[key] = run
		h.calendarSyncMu.Unlock()
		count, err := h.performUserCalendarSourceSync(ctx, source, start, end, scheduled, forced)
		h.calendarSyncMu.Lock()
		run.count, run.err = count, err
		delete(h.calendarUserSyncRunning, key)
		close(run.done)
		h.calendarSyncMu.Unlock()
		return count, err
	}
}

func (p *userCalendarRequest) fetchEvents(ctx context.Context, query calendar.EventQuery) (calendar.EventPage, error) {
	source := p.claim.Source()
	switch source.Provider {
	case "gmail", "outlook":
		if p.h.userCredentials == nil {
			return calendar.EventPage{}, errors.New("mailbox OAuth is not configured")
		}
		p.credentials = p.h.userCredentials.CalendarAccount(source.UserID, source.AccountID, false)
		if source.Provider == "gmail" {
			return listGoogleCalendarEventsWithFetch(ctx, source.RemoteID, query, func(endpoint string, out any) error { return p.getJSON(ctx, endpoint, out) })
		}
		return listOutlookCalendarEventsWithFetch(ctx, source.RemoteID, query, func(endpoint string, out any) error { return p.getJSON(ctx, endpoint, out) })
	case storage.CalendarSourceProviderCalDAV:
		service := p.service()
		base, username, useAccount := service.CalDAVSettings()
		base, err := normalizeCalDAVBaseURL(base)
		if err != nil {
			return calendar.EventPage{}, err
		}
		if _, err := resolveCalDAVHref(base, source.RemoteID); err != nil {
			return calendar.EventPage{}, err
		}
		if useAccount {
			username = strings.TrimSpace(service.Identity().Username)
			if username == "" {
				username = service.Identity().EmailAddress
			}
		}
		password, err := service.CalDAVPassword("", useAccount)
		if err != nil {
			return calendar.EventPage{}, err
		}
		if strings.TrimSpace(username) == "" || password == "" {
			return calendar.EventPage{}, errors.New("complete Calendar authentication in account setup")
		}
		ctx = context.WithValue(ctx, calendarDiscoveryDAVGuardKey{}, calendarDiscoveryDAVCallbacks{ready: p.ready, failure: p.recordRetry})
		return listCalDAVCalendarEvents(ctx, source, username, password, query)
	default:
		return calendar.EventPage{}, errors.New("calendar provider is not supported")
	}
}

func (h *Handler) performUserCalendarSourceSync(parent context.Context, source storage.CalendarSource, start, end time.Time, scheduled, forced bool) (count int, err error) {
	err = h.userIMAP.RunAccountService(parent, source.UserID, source.AccountID, mail.AccountServiceCalendar, 2*time.Minute, func(operation context.Context) error {
		unlock, err := h.lockCalendarCreate(operation, source)
		if err != nil {
			return err
		}
		defer unlock()
		interval := 5 * time.Minute
		var current *storage.CalendarSource
		if err := h.userAccounts.WithAccountForUser(operation, source.UserID, source.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
			if minutes := db.GetSyncInterval(operation, source.UserID); minutes > 0 {
				interval = time.Duration(minutes) * time.Minute
			}
			sources, err := db.ListSelectedCalendarSources(operation, source.UserID)
			if err != nil {
				return err
			}
			for i := range sources {
				if sources[i].ID == source.ID && sources[i].AccountID == source.AccountID {
					value := sources[i]
					current = &value
					break
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if current == nil {
			return storage.ErrCalendarSyncChanged
		}
		// An active in-process source flight was skipped above. A remaining
		// syncing row is interrupted work; a new guarded attempt supersedes it.
		if scheduled && !forced && current.SyncState != "syncing" && current.NextAttemptAt != nil && current.NextAttemptAt.After(time.Now()) {
			return nil
		}
		claim, err := h.userAccounts.StartCalendarSync(operation, source.UserID, source.AccountID, source.ID, start, end)
		if err != nil {
			return err
		}
		h.publishUserCalendarSync(operation, source, false)
		request := &userCalendarRequest{h: h, claim: claim}
		fetchCtx, cancel := context.WithTimeout(operation, calendarSourceTimeout)
		page, fetchErr := request.fetchEvents(fetchCtx, calendar.EventQuery{WindowStart: start, WindowEnd: end, IncludeDeleted: true})
		if fetchErr == nil {
			fetchErr = fetchCtx.Err()
		}
		cancel()
		if fetchErr == nil && (page.NextPageToken != "" || page.FullSyncRequired) {
			fetchErr = errors.New("incomplete calendar snapshot; keeping cached events")
		}
		if operation.Err() != nil {
			return operation.Err()
		}
		var guards []func() error
		if request.authorization != nil {
			guards = append(guards, func() error { return h.userCredentials.ValidateServiceAuthorization(operation, request.authorization) })
		}
		next := time.Now().Add(interval)
		var hint interface{ RetryAfter() (time.Time, bool) }
		if errors.As(fetchErr, &hint) {
			if at, ok := hint.RetryAfter(); ok && at.After(next) {
				next = at
			}
		}
		if fetchErr == nil {
			events := make([]storage.CalendarEvent, 0, len(page.Events))
			publishedCount := 0
			for _, remote := range page.Events {
				events = append(events, calendarStorageEvent(source.UserID, source.ID, remote))
				if !remote.Deleted {
					publishedCount++
				}
			}
			fetchErr = h.userAccounts.PublishCalendarSync(operation, claim, events, next, guards...)
			if fetchErr == nil {
				count = publishedCount
			}
		}
		if fetchErr != nil {
			// This parent is still joined to root/account cancellation. A child
			// fetch timeout can finalize; shutdown cannot write detached state.
			finishCtx, finish := context.WithTimeout(operation, 5*time.Second)
			markErr := h.userAccounts.FailCalendarSync(finishCtx, claim, fetchErr.Error(), next, guards...)
			finish()
			fetchErr = errors.Join(fetchErr, markErr)
		}
		h.publishUserCalendarSync(operation, source, fetchErr == nil)
		return fetchErr
	})
	return count, err
}

func (h *Handler) publishUserCalendarSync(ctx context.Context, source storage.CalendarSource, changed bool) {
	var copied *storage.CalendarSource
	err := h.userAccounts.WithAccountForUser(ctx, source.UserID, source.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
		sources, err := db.ListSelectedCalendarSources(ctx, source.UserID)
		if err != nil {
			return err
		}
		for _, current := range sources {
			if current.ID == source.ID && current.AccountID == source.AccountID {
				value := current
				copied = &value
				break
			}
		}
		return nil
	})
	if err == nil && copied != nil && h.syncer != nil {
		h.syncer.Events().Publish(mail.Event{Type: mail.EventCalendarSync, UserID: source.UserID, Payload: calendarSyncPayload(*copied, changed, false)})
	}
}

func (h *Handler) syncUserCalendarWindow(ctx context.Context, owner string, start, end time.Time, account string, scheduled, forced bool) (count int, err error) {
	if start.IsZero() || end.IsZero() || !end.After(start) {
		return 0, errors.New("invalid calendar synchronization window")
	}
	err = h.userIMAP.RunUserServiceWork(ctx, owner, func(ctx context.Context) error {
		var sources []storage.CalendarSource
		if account != "" {
			state, err := h.userStorage.AccountStateForUser(ctx, owner, account)
			if err != nil {
				return err
			}
			if state != storage.AccountActive {
				return storage.ErrAccountRoute
			}
		}
		if err := h.userAccounts.WithUser(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
			var err error
			sources, err = db.ListSelectedCalendarSources(ctx, owner)
			return err
		}); err != nil {
			return err
		}
		matched := account == ""
		var failures []error
		for _, source := range sources {
			if account != "" && source.AccountID != account {
				continue
			}
			matched = true
			if ctx.Err() != nil {
				return ctx.Err()
			}
			completed, err := h.syncUserCalendarSource(ctx, source, start, end, scheduled, forced)
			count += completed
			if err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", source.Name, err))
			}
		}
		if !matched {
			return errCalendarAccountNotConfigured
		}
		return errors.Join(failures...)
	})
	return count, err
}

func (h *Handler) handleUserCalendarSync(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid calendar refresh request", http.StatusBadRequest)
		return
	}
	values := r.URL.Query()
	for _, field := range []struct{ name, layout string }{{"month", "2006-01"}, {"date", "2006-01-02"}} {
		if value := strings.TrimSpace(r.FormValue(field.name)); value != "" {
			if _, err := time.Parse(field.layout, value); err != nil {
				http.Error(w, "invalid calendar "+field.name, http.StatusBadRequest)
				return
			}
			values.Set(field.name, value)
		}
	}
	if view := strings.TrimSpace(r.FormValue("view")); view != "" {
		if view != "month" && view != "week" {
			http.Error(w, "invalid calendar view", http.StatusBadRequest)
			return
		}
		values.Set("view", view)
	}
	r.URL.RawQuery = values.Encode()
	ctx, owner := r.Context(), h.userID(r.Context())
	var month views.CalendarMonthData
	var settings map[string]string
	if err := h.userAccounts.WithUser(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
		settings = db.GetUISettings(ctx, owner)
		month = calendarDataFromRequest(r, settings)
		return nil
	}); err != nil {
		userAccountError(w, r, err)
		return
	}
	start, end := calendarVisibleWindow(month)
	count, syncErr := h.syncUserCalendarWindow(ctx, owner, start, end, strings.TrimSpace(r.FormValue("account_id")), false, false)
	if errors.Is(syncErr, errCalendarAccountNotConfigured) {
		http.Error(w, syncErr.Error(), http.StatusNotFound)
		return
	}
	if errors.Is(syncErr, storage.ErrAccountRoute) || errors.Is(syncErr, storage.ErrUserStoreOwner) || errors.Is(syncErr, context.Canceled) {
		userAccountError(w, r, syncErr)
		return
	}
	if errors.Is(syncErr, storage.ErrCalendarSyncChanged) || errors.Is(syncErr, config.ErrAccountServicesChanged) || errors.Is(syncErr, mailauth.ErrMailboxAuthorizationChanged) {
		http.Error(w, "Calendar access or settings changed; retry refresh.", http.StatusConflict)
		return
	}
	readErr := h.userAccounts.WithUser(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
		sources, err := db.ListSelectedCalendarSources(ctx, owner)
		if err != nil {
			return err
		}
		applyCalendarSyncSummary(&month, sources)
		events, err := db.ListCalendarEvents(ctx, owner, start, end)
		if err != nil {
			return err
		}
		month.Events = calendarViewEvents(events)
		return nil
	})
	if readErr != nil {
		userAccountError(w, r, readErr)
		return
	}
	if syncErr != nil {
		month.SyncError = true
		month.SyncMessage = "Calendar refresh failed: " + syncErr.Error()
	} else if month.HasSources {
		month.SyncMessage = fmt.Sprintf("Synced %d event(s) · Updated %s", count, time.Now().In(month.Month.Location()).Format("15:04"))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if month.SyncError {
		w.Header().Set("X-Gofer-Status", "error")
	}
	if err := views.CalendarPage(month, settings).Render(ctx, w); err != nil {
		log.Printf("owned calendar: render refresh: %v", err)
	}
}
