package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func calendarWorkerFixture(t *testing.T) *Handler {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "calendar-worker.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`
		INSERT INTO users (id, username, username_normalized) VALUES ('one', 'one', 'one'), ('two', 'two', 'two');
		INSERT INTO accounts (id, user_id, provider, email_address, email_sync_enabled) VALUES ('one-account', 'one', 'gmail', 'one@example.com', 0), ('two-account', 'two', 'outlook', 'two@example.com', 0);`); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"one", "two"} {
		provider := "gmail"
		if user == "two" {
			provider = "outlook"
		}
		if err := db.ReplaceCalendarSources(t.Context(), user, user+"-account", provider, []storage.CalendarSource{
			{ID: user + "-source", RemoteID: "primary", Name: user, IsSelected: true},
			{ID: user + "-unselected", RemoteID: "unselected", IsSelected: false},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return &Handler{db: db, syncer: mail.NewSyncOrchestrator(db, nil, nil, nil)}
}

func TestCalendarRefreshTargetsOnlySelectedAccount(t *testing.T) {
	h := calendarWorkerFixture(t)
	ctx := t.Context()
	if _, err := h.db.Write().Exec(`INSERT INTO accounts (id, user_id, provider, email_address) VALUES ('other-account', 'one', 'gmail', 'other@example.com')`); err != nil {
		t.Fatal(err)
	}
	if err := h.db.ReplaceCalendarSources(ctx, "one", "other-account", "gmail", []storage.CalendarSource{
		{ID: "other-source", RemoteID: "primary", Name: "Other account", IsSelected: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.ReplaceCalendarSources(ctx, "one", "one-account", "gmail", []storage.CalendarSource{
		{ID: "one-source", RemoteID: "primary", Name: "Primary", IsSelected: true},
		{ID: "one-shared", RemoteID: "shared", Name: "Shared", IsSelected: true},
		{ID: "one-unselected", RemoteID: "unselected", IsSelected: false},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetCalendarSourceVisibility(ctx, "one", "one-shared", false); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, accountID string
		want            []string
		status          int
	}{
		{"one account including hidden calendars", "one-account", []string{"one-source", "one-shared"}, http.StatusOK},
		{"other owned account", "other-account", []string{"other-source"}, http.StatusOK},
		{"all accounts", "", []string{"one-source", "one-shared", "other-source"}, http.StatusOK},
		{"another user's account", "two-account", nil, http.StatusNotFound},
		{"unknown account", "missing", nil, http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := make(map[string]int)
			h.calendarFetchEvents = func(_ context.Context, source storage.CalendarSource, query calendar.EventQuery) (calendar.EventPage, error) {
				calls[source.ID]++
				return calendar.EventPage{}, nil
			}
			form := url.Values{"account_id": {test.accountID}, "view": {"week"}, "date": {"2026-10-02"}}
			r := httptest.NewRequest(http.MethodPost, "/api/calendar/sync", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "one"}))
			w := httptest.NewRecorder()
			h.handleCalendarSync(w, r)
			if w.Code != test.status || w.Header().Get("X-Gofer-Status") == "error" {
				t.Fatalf("refresh status = %d, body = %s", w.Code, w.Body.String())
			}
			if len(calls) != len(test.want) {
				t.Fatalf("refreshed calendars = %v, want %v", calls, test.want)
			}
			for _, sourceID := range test.want {
				if calls[sourceID] != 1 {
					t.Errorf("calendar %s refreshed %d times, want 1", sourceID, calls[sourceID])
				}
			}
			if test.status == http.StatusOK && !strings.Contains(w.Body.String(), `data-calendar-period="week:2026-09-28"`) {
				t.Fatal("account refresh changed the visible week")
			}
		})
	}
}

func TestCalendarWorkerRefreshesHiddenSourcesIndependentlyOfMailAndRespectsDueTime(t *testing.T) {
	h := calendarWorkerFixture(t)
	ctx := t.Context()
	if err := h.db.SetCalendarSourceVisibility(ctx, "one", "one-source", false); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetSetting(ctx, "one", "sync_interval_minutes", "2"); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetSetting(ctx, "two", "sync_interval_minutes", "7"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	h.calendarFetchEvents = func(_ context.Context, source storage.CalendarSource, query calendar.EventQuery) (calendar.EventPage, error) {
		calls.Add(1)
		start := query.WindowStart.Add(24 * time.Hour)
		end := start.Add(time.Hour)
		return calendar.EventPage{Events: []calendar.RemoteEvent{{RemoteID: source.ID, Summary: "Synced", StartAt: &start, EndAt: &end}}}, nil
	}
	bus := h.syncer.Events()
	events := bus.Subscribe()
	defer bus.Unsubscribe(events)
	h.runCalendarSyncTick(ctx, time.Now())
	if calls.Load() != 2 {
		t.Fatalf("provider reads = %d, want both owners including hidden calendars", calls.Load())
	}
	for _, user := range []string{"one", "two"} {
		sources, err := h.db.ListSelectedCalendarSources(ctx, user)
		if err != nil || len(sources) != 1 {
			t.Fatalf("owned sources = %#v, %v", sources, err)
		}
		source := sources[0]
		if source.SyncState != "ok" || source.SyncAttempt != 1 || source.LastSuccessAt == nil || source.NextAttemptAt == nil {
			t.Fatalf("sync status = %#v", source)
		}
		wantDelay := h.calendarSyncInterval(ctx, user)
		if delta := source.NextAttemptAt.Sub(time.Now()); delta < wantDelay-5*time.Second || delta > wantDelay+time.Second {
			t.Fatalf("%s retry delay = %v, want %v", user, delta, wantDelay)
		}
		if user == "one" && !source.IsHidden {
			t.Fatal("background sync changed visibility")
		}
		cache, err := h.db.ListCalendarEvents(ctx, user, *source.WindowStart, *source.WindowEnd)
		if err != nil || len(cache) != 1 {
			t.Fatalf("cache = %#v, %v", cache, err)
		}
	}
	h.runCalendarSyncTick(ctx, time.Now())
	if calls.Load() != 2 {
		t.Fatal("scheduler ignored persisted due times")
	}
	for range 4 {
		select {
		case event := <-events:
			if event.Type != mail.EventCalendarSync || event.UserID == "" || sseEventVisible(event, "foreign", nil, false) {
				t.Fatalf("unscoped calendar event: %#v", event)
			}
		case <-time.After(time.Second):
			t.Fatal("missing real-time source progress")
		}
	}
}

func TestCalendarSyncFailuresAndIncompleteReadsPreserveCacheAndRetry(t *testing.T) {
	h := calendarWorkerFixture(t)
	ctx := t.Context()
	start := time.Now().UTC().Truncate(time.Hour)
	end := start.Add(time.Hour)
	if err := h.db.ReplaceCalendarEvents(ctx, "one", "one-source", []storage.CalendarEvent{{ID: "cached", RemoteID: "cached", Summary: "Keep me", StartAt: &start, EndAt: &end}}, start.Add(-time.Hour), end.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		page calendar.EventPage
		err  error
	}{
		{"provider failure", calendar.EventPage{}, errors.New("provider unavailable")},
		{"partial page", calendar.EventPage{NextPageToken: "unfinished"}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			h.calendarFetchEvents = func(context.Context, storage.CalendarSource, calendar.EventQuery) (calendar.EventPage, error) {
				return test.page, test.err
			}
			if _, err := h.syncCalendarWindow(ctx, "one", start.Add(-time.Hour), end.Add(time.Hour)); err == nil {
				t.Fatal("failed refresh reported success")
			}
			event, err := h.db.GetCalendarEvent(ctx, "one", "cached")
			if err != nil || event.Summary != "Keep me" {
				t.Fatalf("failed read changed cache: %#v, %v", event, err)
			}
			sources, err := h.db.ListSelectedCalendarSources(ctx, "one")
			if err != nil || sources[0].SyncState != "failed" || sources[0].SyncError == "" || sources[0].LastSuccessAt == nil || !sources[0].NextAttemptAt.After(time.Now()) {
				t.Fatal("failure did not retain success time and schedule retry")
			}
		})
	}
	if err := h.db.ScheduleCalendarSync(ctx, "one", "one-source", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	h.calendarFetchEvents = func(context.Context, storage.CalendarSource, calendar.EventQuery) (calendar.EventPage, error) {
		return calendar.EventPage{}, nil
	}
	h.runCalendarSyncTick(ctx, time.Now())
	sources, err := h.db.ListSelectedCalendarSources(ctx, "one")
	if err != nil || sources[0].SyncState != "ok" || sources[0].SyncError != "" {
		t.Fatal("due failed source did not recover")
	}
}

func TestCalendarSyncSerializesDifferentWindowsAndCoalescesMatchingReads(t *testing.T) {
	h := calendarWorkerFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := time.Now().UTC().Truncate(time.Hour)
	end := start.Add(time.Hour)
	entered, release := make(chan struct{}, 4), make(chan struct{})
	var calls, active, maximum atomic.Int32
	h.calendarFetchEvents = func(ctx context.Context, _ storage.CalendarSource, _ calendar.EventQuery) (calendar.EventPage, error) {
		calls.Add(1)
		n := active.Add(1)
		defer active.Add(-1)
		for prior := maximum.Load(); n > prior && !maximum.CompareAndSwap(prior, n); prior = maximum.Load() {
		}
		entered <- struct{}{}
		select {
		case <-release:
			return calendar.EventPage{}, nil
		case <-ctx.Done():
			return calendar.EventPage{}, ctx.Err()
		}
	}
	var group sync.WaitGroup
	results := make(chan error, 3)
	run := func(windowStart, windowEnd time.Time) {
		defer group.Done()
		_, err := h.syncCalendarWindow(ctx, "one", windowStart, windowEnd)
		results <- err
	}
	group.Add(1)
	go run(start, end)
	<-entered
	group.Add(2)
	go run(start, end)
	go run(start.Add(-time.Hour), end.Add(time.Hour))
	select {
	case <-entered:
		t.Fatal("same source read concurrently")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if maximum.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("active max=%d, reads=%d, want serialized windows and one joined read", maximum.Load(), calls.Load())
	}
}

func TestCalendarSyncCancellationDoesNotLeaveSourceBusy(t *testing.T) {
	h := calendarWorkerFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	h.calendarFetchEvents = func(ctx context.Context, _ storage.CalendarSource, _ calendar.EventQuery) (calendar.EventPage, error) {
		close(started)
		<-ctx.Done()
		return calendar.EventPage{}, ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, err := h.syncCalendarWindow(ctx, "one", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	sources, err := h.db.ListSelectedCalendarSources(t.Context(), "one")
	if err != nil || sources[0].SyncState != "failed" || sources[0].NextAttemptAt == nil {
		t.Fatal("cancelled source left busy or unscheduled")
	}
}

func TestCalendarPassiveCacheReadNeverStartsProviderSync(t *testing.T) {
	h := calendarWorkerFixture(t)
	var calls atomic.Int32
	h.calendarFetchEvents = func(context.Context, storage.CalendarSource, calendar.EventQuery) (calendar.EventPage, error) {
		calls.Add(1)
		return calendar.EventPage{}, nil
	}
	r := httptest.NewRequest(http.MethodGet, "/calendar?view=week&date=2026-10-25&cache=1", nil)
	r.Header.Set("HX-Request", "true")
	r.Header.Set("HX-Target", "main-content")
	r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "one"}))
	w := httptest.NewRecorder()
	h.handleCalendar(w, r)
	if w.Code != 200 || calls.Load() != 0 || strings.Contains(w.Body.String(), "data-calendar-auto-sync") || !strings.Contains(w.Body.String(), "data-calendar-sync-sources=") || !strings.Contains(w.Body.String(), `data-calendar-view="week"`) {
		t.Fatal("passive cache read started sync or changed view")
	}
}

func TestCalendarSyncDiscardedWhenSourceIsDeselectedDuringRead(t *testing.T) {
	h := calendarWorkerFixture(t)
	ctx := t.Context()
	sources, err := h.db.ListSelectedCalendarSources(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	start, end := time.Now(), time.Now().Add(time.Hour)
	h.calendarFetchEvents = func(context.Context, storage.CalendarSource, calendar.EventQuery) (calendar.EventPage, error) {
		if err := h.db.SetCalendarSourceSelection(ctx, "one", "one-account", nil); err != nil {
			t.Fatal(err)
		}
		return calendar.EventPage{Events: []calendar.RemoteEvent{{RemoteID: "late", StartAt: &start, EndAt: &end}}}, nil
	}
	if _, err := h.syncCalendarSource(ctx, sources[0], start, end, false, nil); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deselected source accepted an in-flight cache write: %v", err)
	}
	var count int
	if err := h.db.Read().QueryRowContext(ctx, `SELECT COUNT(*) FROM calendar_events WHERE source_id = 'one-source'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deselected source cache was changed: count=%d, error=%v", count, err)
	}
}

func TestCalendarSourceRefreshUsesFreshnessWindowAndRetryState(t *testing.T) {
	now := time.Now()
	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	oldStart, oldEnd, success, retry := start.Add(-time.Hour), end.Add(time.Hour), now.Add(-time.Minute), now.Add(time.Minute)
	fresh := storage.CalendarSource{SyncState: "ok", LastSuccessAt: &success, WindowStart: &oldStart, WindowEnd: &oldEnd}
	for _, test := range []struct {
		name string
		edit func(*storage.CalendarSource)
		want bool
	}{
		{"fresh coverage", func(*storage.CalendarSource) {}, false},
		{"first refresh", func(s *storage.CalendarSource) { s.LastSuccessAt = nil }, true},
		{"outside cached window", func(s *storage.CalendarSource) { s.WindowEnd = &start }, true},
		{"stale success", func(s *storage.CalendarSource) { stale := now.Add(-time.Hour); s.LastSuccessAt = &stale }, true},
		{"failure cooldown", func(s *storage.CalendarSource) {
			s.SyncState = "failed"
			s.NextAttemptAt = &retry
			s.LastSuccessAt = nil
		}, false},
		{"running covered window", func(s *storage.CalendarSource) { s.SyncState = "syncing" }, false},
		{"running different window", func(s *storage.CalendarSource) { s.SyncState = "syncing"; s.WindowEnd = &start }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := fresh
			test.edit(&source)
			if got := calendarSourceNeedsRefresh(source, start, end, now, 5*time.Minute); got != test.want {
				t.Fatalf("needs refresh = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCalendarBackgroundWindowIncludesMonthGridsAcrossDST(t *testing.T) {
	zone, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatal(err)
	}
	start, end := calendarBackgroundWindow(time.Date(2026, 10, 25, 12, 0, 0, 0, zone), zone)
	if start.Format("2006-01-02") != "2026-08-31" || end.Format("2006-01-02") != "2027-02-01" || start.Weekday() != time.Monday || end.Weekday() != time.Monday {
		t.Fatalf("background window = %v to %v", start, end)
	}
}
