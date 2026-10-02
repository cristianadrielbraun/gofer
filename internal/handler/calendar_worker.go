package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const calendarSourceTimeout = 90 * time.Second

type calendarSyncRun struct {
	start, end time.Time
	done       chan struct{}
	count      int
	err        error
}

// StartCalendarSync is independent of the active browser tab and mail sync.
// The persistent due time uses each owner's existing sync interval.
func (h *Handler) StartCalendarSync(ctx context.Context) {
	h.calendarWorkerOnce.Do(func() {
		go func() {
			h.runCalendarSyncTick(ctx, time.Now())
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					h.runCalendarSyncTick(ctx, now)
				}
			}
		}()
	})
}

func (h *Handler) calendarSyncInterval(ctx context.Context, userID string) time.Duration {
	minutes := h.db.GetSyncInterval(ctx, userID)
	if minutes < 1 {
		minutes = 5
	}
	return time.Duration(minutes) * time.Minute
}

func calendarBackgroundWindow(now time.Time, location *time.Location) (time.Time, time.Time) {
	now = now.In(location)
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, location)
	start, _ := calendarVisibleWindow(views.NewCalendarMonthData(month.AddDate(0, -1, 0)))
	_, end := calendarVisibleWindow(views.NewCalendarMonthData(month.AddDate(0, 3, 0)))
	return start, end
}

func (h *Handler) runCalendarSyncTick(ctx context.Context, now time.Time) {
	sources, err := h.db.ListInstanceCalendarSyncSources(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("calendar sync: list sources: %v", err)
		}
		return
	}
	jobs := make(chan storage.CalendarSource)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for source := range jobs {
				settings := h.db.GetUISettings(ctx, source.UserID)
				start, end := calendarBackgroundWindow(now, viewsCalendarLocation(settings))
				if _, err := h.syncCalendarSource(ctx, source, start, end, true, make(map[string]calendarCredentials)); err != nil && ctx.Err() == nil {
					log.Printf("calendar sync %s: %v", source.ID, err)
				}
			}
		}()
	}
send:
	for _, source := range sources {
		if source.NextAttemptAt != nil && source.NextAttemptAt.After(now) {
			continue
		}
		select {
		case <-ctx.Done():
			break send
		case jobs <- source:
		}
	}
	close(jobs)
	workers.Wait()
}

// Single-flight per owned source: matching reads join, different windows wait,
// and scheduled reads skip active work. No provider or cache writes overlap.
func (h *Handler) syncCalendarSource(ctx context.Context, source storage.CalendarSource, start, end time.Time, scheduled bool, credentials map[string]calendarCredentials) (int, error) {
	key := source.UserID + "\x00" + source.ID
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		h.calendarSyncMu.Lock()
		if h.calendarSyncRunning == nil {
			h.calendarSyncRunning = make(map[string]*calendarSyncRun)
		}
		if active := h.calendarSyncRunning[key]; active != nil {
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
		if scheduled {
			// Recheck after queueing: a manual refresh may have just completed.
			latest, err := h.db.ListSelectedCalendarSources(ctx, source.UserID)
			if err != nil {
				h.calendarSyncMu.Unlock()
				return 0, err
			}
			var current *storage.CalendarSource
			for i := range latest {
				if latest[i].ID == source.ID {
					current = &latest[i]
					break
				}
			}
			if current == nil || (current.NextAttemptAt != nil && current.NextAttemptAt.After(time.Now())) {
				h.calendarSyncMu.Unlock()
				return 0, nil
			}
			source = *current
		}
		run := &calendarSyncRun{start: start, end: end, done: make(chan struct{})}
		h.calendarSyncRunning[key] = run
		h.calendarSyncMu.Unlock()
		count, err := h.performCalendarSourceSync(ctx, source, start, end, credentials)
		h.calendarSyncMu.Lock()
		run.count, run.err = count, err
		delete(h.calendarSyncRunning, key)
		close(run.done)
		h.calendarSyncMu.Unlock()
		return count, err
	}
}

func (h *Handler) performCalendarSourceSync(parent context.Context, source storage.CalendarSource, start, end time.Time, credentials map[string]calendarCredentials) (int, error) {
	ctx, cancel := context.WithTimeout(parent, calendarSourceTimeout)
	defer cancel()
	if err := h.db.StartCalendarSync(ctx, source.UserID, source.ID); err != nil {
		return 0, err
	}
	h.publishCalendarSync(ctx, source, false)
	page, err := h.fetchCalendarSource(ctx, source, calendar.EventQuery{WindowStart: start, WindowEnd: end, IncludeDeleted: true}, credentials)
	if err == nil && (page.NextPageToken != "" || page.FullSyncRequired) {
		err = fmt.Errorf("incomplete calendar snapshot; keeping cached events")
	}
	count := 0
	if err == nil {
		events := make([]storage.CalendarEvent, 0, len(page.Events))
		for _, remote := range page.Events {
			events = append(events, calendarStorageEvent(source.UserID, source.ID, remote))
			if !remote.Deleted {
				count++
			}
		}
		err = h.db.ReplaceCalendarEvents(ctx, source.UserID, source.ID, events, start, end)
	}
	// Cancellation must not leave a durable "syncing" state after work stops.
	stateCtx, finish := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer finish()
	if err != nil {
		count = 0
		if markErr := h.db.FailCalendarSync(stateCtx, source.UserID, source.ID, err.Error()); markErr != nil && parent.Err() == nil {
			log.Printf("calendar sync %s: record failure: %v", source.ID, markErr)
		}
	}
	if scheduleErr := h.db.ScheduleCalendarSync(stateCtx, source.UserID, source.ID, time.Now().Add(h.calendarSyncInterval(stateCtx, source.UserID))); scheduleErr != nil {
		log.Printf("calendar sync %s: schedule next refresh: %v", source.ID, scheduleErr)
	}
	h.publishCalendarSync(stateCtx, source, err == nil)
	return count, err
}

func calendarSyncPayload(source storage.CalendarSource, changed, snapshot bool) map[string]any {
	stamp := func(at *time.Time) string {
		if at == nil {
			return ""
		}
		return at.UTC().Format(time.RFC3339Nano)
	}
	return map[string]any{
		"source_id": source.ID, "account_id": source.AccountID,
		"state": source.SyncState, "attempt": source.SyncAttempt, "error": source.SyncError,
		"last_synced_at": stamp(source.LastSuccessAt), "next_attempt_at": stamp(source.NextAttemptAt),
		"changed": changed, "snapshot": snapshot,
	}
}

func (h *Handler) publishCalendarSync(ctx context.Context, source storage.CalendarSource, changed bool) {
	if h.syncer == nil {
		return
	}
	sources, err := h.db.ListSelectedCalendarSources(ctx, source.UserID)
	if err != nil {
		return
	}
	for _, current := range sources {
		if current.ID == source.ID {
			h.syncer.Events().Publish(mail.Event{Type: mail.EventCalendarSync, UserID: current.UserID, Payload: calendarSyncPayload(current, changed, false)})
			return
		}
	}
}
