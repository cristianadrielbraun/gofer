package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func ownedCalendarWorkerFixture(t *testing.T, api http.Handler, owners ...string) *userContactPushFixture {
	t.Helper()
	f := newUserContactPushFixture(t, "outlook", api)
	for _, owner := range owners {
		expires := time.Now().Add(time.Hour)
		if err := f.h.userCredentials.UpsertForUser(t.Context(), owner, f.accounts[owner].ID, "microsoft", "subject", owner+"-access", owner+"-refresh", "Bearer", &expires, "https://graph.microsoft.com/Calendars.ReadWrite"); err != nil {
			t.Fatal(err)
		}
		if err := f.h.userAccounts.WithUser(t.Context(), owner, func(_ *config.AccountStore, db *storage.DB) error {
			if err := db.ReplaceCalendarSources(t.Context(), owner, f.accounts[owner].ID, "outlook", []storage.CalendarSource{{ID: "same-source", RemoteID: "primary", Name: owner, IsSelected: true}}); err != nil {
				return err
			}
			return db.SetSetting(t.Context(), owner, "sync_interval_minutes", "2")
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestUserCalendarWorkersStartupWakeRecoveryAndCadence(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
		mu.Lock()
		calls[owner]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"value": []any{}})
	})
	f := ownedCalendarWorkerFixture(t, api, "alice")
	// Interrupted work can have a future old deadline. Startup must reclaim it.
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		_, err := db.Write().Exec(`UPDATE calendar_sync_state SET state='syncing',attempt_count=3,next_attempt_at=datetime('now','+1 day') WHERE source_id='same-source'; UPDATE calendar_sources SET is_hidden=1 WHERE id='same-source'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := f.h.StartUserCalendarSync(ctx, UserCalendarSyncOptions{ScanInterval: 20 * time.Millisecond, RecoveryInterval: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := f.h.StartUserCalendarSync(ctx, UserCalendarSyncOptions{}); err == nil {
		t.Fatal("duplicate worker start")
	}
	// Reservation sets a future central deadline before HTTP begins. Neither
	// that deadline nor the request count proves local publication completed.
	// Wait for the committed attempt and the worker's final schedule update.
	completedAttempt := func(attempt int) bool {
		complete := false
		if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
			var state string
			var found int
			if err := db.Read().QueryRow(`SELECT state,attempt_count FROM calendar_sync_state WHERE source_id='same-source'`).Scan(&state, &found); err != nil {
				return err
			}
			complete = state == "ok" && found == attempt
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		f.h.userCalendarWorkers.mu.Lock()
		pending := f.h.userCalendarWorkers.pending[userCalendarJob{owner: "alice", account: f.accounts["alice"].ID}]
		f.h.userCalendarWorkers.mu.Unlock()
		return complete && !pending
	}
	waitUserContactWorkers(t, func() bool {
		var scheduled int
		if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_account_service_schedule WHERE service='calendar' AND next_due_ms>?`, time.Now().Add(time.Minute).UnixMilli()).Scan(&scheduled); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		requested := calls["alice"] == 1 && calls["bob"] == 0
		mu.Unlock()
		return scheduled == 2 && requested && completedAttempt(4)
	})
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		var state string
		var attempt int
		if err := db.Read().QueryRow(`SELECT state,attempt_count FROM calendar_sync_state WHERE source_id='same-source'`).Scan(&state, &attempt); err != nil {
			return err
		}
		if state != "ok" || attempt != 4 {
			t.Fatal("interrupted attempt not reclaimed", state, attempt)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Pin Bob in the sole cache slot. Tick/global wake must only read central
	// deadlines and leave deferred Alice alone, without a local lease wait.
	if err := f.h.userAccounts.WithUser(t.Context(), "bob", func(*config.AccountStore, *storage.DB) error {
		f.h.signalUserCalendar()
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		if calls["alice"] != 1 {
			t.Fatal("deferred source pulled on every scan", calls)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Configuration wake must supersede a stored future source deadline.
	if err := f.h.WakeUserCalendarAccount(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	waitUserContactWorkers(t, func() bool {
		mu.Lock()
		requested := calls["alice"] == 2
		mu.Unlock()
		return requested && completedAttempt(5)
	})
	// Disabling service avoids provider work while maintaining a recovery hint.
	if err := f.h.userAccounts.SetCalendarServiceEnabled(t.Context(), "alice", f.accounts["alice"].ID, false); err != nil {
		t.Fatal(err)
	}
	if err := f.h.WakeUserCalendarAccount(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	waitUserContactWorkers(t, func() bool {
		var scheduled int
		if err := f.system.Read().QueryRow(`SELECT COUNT(*) FROM gofer_account_service_schedule WHERE service='calendar' AND account_id=? AND next_due_ms>?`, f.accounts["alice"].ID, time.Now().Add(30*time.Minute).UnixMilli()).Scan(&scheduled); err != nil {
			t.Fatal(err)
		}
		return scheduled == 1
	})
	mu.Lock()
	if calls["alice"] != 2 {
		t.Fatal("disabled calendar fetched", calls)
	}
	mu.Unlock()
	if err := f.h.userAccounts.SetCalendarServiceEnabled(t.Context(), "alice", f.accounts["alice"].ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.h.WakeUserCalendarAccount(t.Context(), "alice", f.accounts["alice"].ID); err != nil {
		t.Fatal(err)
	}
	waitUserContactWorkers(t, func() bool {
		mu.Lock()
		requested := calls["alice"] == 3
		mu.Unlock()
		return requested && completedAttempt(6)
	})
	cancel()
	select {
	case <-f.h.userCalendarWorkers.done:
	case <-time.After(5 * time.Second):
		t.Fatal("calendar dispatcher did not join")
	}
}

func TestUserCalendarWorkersRootShutdownJoinsBlockedProviderAndOtherOwner(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	calls := map[string]int{}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")[0]
		mu.Lock()
		calls[owner]++
		mu.Unlock()
		if owner == "alice" {
			once.Do(func() { close(entered) })
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":[]}`))
	})
	f := ownedCalendarWorkerFixture(t, api, "alice", "bob")
	if err := f.h.StartUserCalendarSync(t.Context(), UserCalendarSyncOptions{ScanInterval: 20 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	// Startup may reopen several stores with MaxOpen=1. This is a readiness
	// observation, not the runtime shutdown bound asserted separately below.
	case <-time.After(20 * time.Second):
		var profile bytes.Buffer
		_ = pprof.Lookup("goroutine").WriteTo(&profile, 2)
		t.Fatalf("provider not reached; worker readiness profile:\n%s", profile.String())
	}
	waitUserContactWorkers(t, func() bool { mu.Lock(); defer mu.Unlock(); return calls["bob"] == 1 })
	waitUserContactWorkers(t, func() bool {
		complete := false
		if err := f.h.userAccounts.WithUser(t.Context(), "bob", func(_ *config.AccountStore, db *storage.DB) error {
			sources, err := db.ListSelectedCalendarSources(t.Context(), "bob")
			if err == nil {
				complete = len(sources) == 1 && sources[0].SyncState == "ok"
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return complete
	})

	f.cancel()
	done := make(chan struct{})
	go func() { f.h.userIMAP.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("root did not join provider/worker")
	}
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		var state string
		if err := db.Read().QueryRow(`SELECT state FROM calendar_sync_state WHERE source_id='same-source'`).Scan(&state); err != nil {
			return err
		}
		if state != "syncing" {
			t.Fatal("shutdown published detached cleanup", state)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUserCalendarFlightJoinsMatchingWindowsAndSerializesOthers(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	calls := 0
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			once.Do(func() { close(entered) })
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":[]}`))
	})
	f := ownedCalendarWorkerFixture(t, api, "alice")
	defer close(release)
	var source storage.CalendarSource
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		sources, err := db.ListSelectedCalendarSources(t.Context(), "alice")
		if err == nil {
			source = sources[0]
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	first := make(chan error, 1)
	go func() { _, err := f.h.syncUserCalendarSource(ctx, source, start, end, false, false); first <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	joined := make(chan error, 1)
	joinedCtx := &calendarFlightWaitContext{Context: ctx, reached: make(chan struct{})}
	go func() {
		_, err := f.h.syncUserCalendarSource(joinedCtx, source, start, end, false, false)
		joined <- err
	}()
	other := make(chan error, 1)
	otherCtx := &calendarFlightWaitContext{Context: ctx, reached: make(chan struct{})}
	go func() {
		_, err := f.h.syncUserCalendarSource(otherCtx, source, start.AddDate(0, 1, 0), end.AddDate(0, 1, 0), false, false)
		other <- err
	}()
	if _, err := f.h.syncUserCalendarSource(ctx, source, start, end, true, true); err != nil {
		t.Fatal("scheduled work did not skip flight", err)
	}
	for _, reached := range []chan struct{}{joinedCtx.reached, otherCtx.reached} {
		select {
		case <-reached:
		case <-ctx.Done():
			t.Fatal("flight waiter not reached", ctx.Err())
		}
	}
	release <- struct{}{}
	for _, done := range []chan error{first, joined, other} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatal("same-window did not coalesce/different window overlap", calls)
	}
}

type calendarFlightWaitContext struct {
	context.Context
	reached chan struct{}
	once    sync.Once
}

func (c *calendarFlightWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.reached) })
	return c.Context.Done()
}

func TestUserCalendarFlightActionAndSyncUseSameGateOrder(t *testing.T) {
	// An action already owns the account gate when a sync begins. The sync may
	// reserve coalescing state, but must not take the source lock while waiting
	// for that gate; doing so would prevent this action from ever making progress.
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"value":[]}`))
	})
	f := ownedCalendarWorkerFixture(t, api, "alice")
	var source storage.CalendarSource
	if err := f.h.userAccounts.WithUser(t.Context(), "alice", func(_ *config.AccountStore, db *storage.DB) error {
		sources, err := db.ListSelectedCalendarSources(t.Context(), "alice")
		if err == nil {
			source = sources[0]
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	err := f.h.userIMAP.RunAccountService(ctx, "alice", source.AccountID, mail.AccountServiceCalendar, 3*time.Second, func(operation context.Context) error {
		wait := &calendarFlightWaitContext{Context: ctx, reached: make(chan struct{})}
		go func() {
			start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			_, err := f.h.syncUserCalendarSource(wait, source, start, start.AddDate(0, 1, 0), false, true)
			completed <- err
		}()
		select {
		case <-wait.reached:
		case <-operation.Done():
			return operation.Err()
		}
		unlock, err := f.h.lockCalendarCreate(operation, source)
		if err != nil {
			return err
		}
		unlock()
		return nil
	})
	if err != nil {
		t.Fatal("action and queued sync acquired locks in opposing order", err)
	}
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal("queued sync did not resume after action", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
