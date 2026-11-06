package handler

import (
	"context"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// Gofer notifies the owner when an event's reminder falls due, through the
// notification method set for new mail (calendar-reminder events).

// calendarReminderLookBack is how far back the first check looks, so a
// reminder due while Gofer was starting isn't missed.
const calendarReminderLookBack = time.Minute

type calendarReminderState struct {
	mu   sync.Mutex
	last time.Time
	sent map[string]time.Time // event and reminder time, to notify once
}

// calendarReminderWindow returns the span to check now, and records it.
func (s *calendarReminderState) window(now time.Time) (time.Time, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	from := s.last
	if from.IsZero() || now.Sub(from) > 10*time.Minute {
		from = now.Add(-calendarReminderLookBack)
	}
	s.last = now
	for key, at := range s.sent {
		if now.Sub(at) > 2*time.Hour {
			delete(s.sent, key)
		}
	}
	return from, now
}

// first reports whether this reminder hasn't been notified yet.
func (s *calendarReminderState) first(owner string, due storage.CalendarReminderDue) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sent == nil {
		s.sent = map[string]time.Time{}
	}
	key := owner + "\x00" + due.Event.ID + "\x00" + due.FireAt.UTC().Format(time.RFC3339)
	if _, done := s.sent[key]; done {
		return false
	}
	s.sent[key] = due.FireAt
	return true
}

// calendarReminderEvent is the notification for a due reminder.
func calendarReminderEvent(owner string, due storage.CalendarReminderDue, location *time.Location, accountID string) mail.Event {
	// Written in the owner's language and time format, as no page asked for it.
	ctx := i18n.ForUser(context.Background(), owner)
	dates := i18n.Dates(ctx)
	event := due.Event
	title := event.Summary
	if title == "" {
		title = i18n.T(ctx, "calendar.untitled_event")
	}
	start := event.StartAt.In(location)
	when := i18n.T(ctx, "push.reminder.starts_now")
	if minutes := *event.ReminderMinutes; minutes > 0 {
		when = i18n.T(ctx, "push.reminder.in", "span", views.CalendarReminderSpan(ctx, minutes))
	}
	clock := dates.Time(start)
	if event.EndAt != nil && !event.EndAt.Equal(*event.StartAt) {
		clock = dates.TimeRange(start, event.EndAt.In(location))
	}
	body := i18n.T(ctx, "push.reminder.body", "when", when, "time", clock)
	if event.Location != "" {
		body += " · " + event.Location
	}
	return mail.Event{Type: mail.EventCalendarReminder, UserID: owner, AccountID: accountID, Payload: map[string]any{
		"title": title, "body": body, "event_id": event.ID, "tag": "gofer-calendar-reminder-" + event.ID,
		"url": "/calendar?view=week&date=" + start.Format("2006-01-02"),
	}}
}

// runCalendarReminderTick notifies reminders due since the last check, for
// every user of a single shared database.
func (h *Handler) runCalendarReminderTick(ctx context.Context, now time.Time) {
	if h.db == nil || h.syncer == nil {
		return
	}
	from, to := h.calendarReminders.window(now)
	owners, err := h.db.CalendarReminderOwners(ctx)
	if err != nil {
		log.Printf("calendar reminders: list owners: %v", err)
		return
	}
	for _, owner := range owners {
		due, err := h.db.ListDueCalendarReminders(ctx, owner, from, to)
		if err != nil {
			log.Printf("calendar reminders: %v", err)
			continue
		}
		if len(due) == 0 {
			continue
		}
		location := viewsCalendarLocation(h.db.GetUISettings(ctx, owner))
		sources, _ := h.db.ListSelectedCalendarSources(ctx, owner)
		for _, item := range due {
			if h.calendarReminders.first(owner, item) {
				h.syncer.Events().Publish(calendarReminderEvent(owner, item, location, calendarReminderAccount(sources, item.Event.SourceID)))
			}
		}
	}
}

// runUserCalendarReminders notifies reminders for owners with their own
// databases, checking every half minute until ctx ends.
func (h *Handler) runUserCalendarReminders(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		h.runUserCalendarReminderTick(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (h *Handler) runUserCalendarReminderTick(ctx context.Context, now time.Time) {
	if h.userStorage == nil || h.userAccounts == nil || h.userIMAP == nil {
		return
	}
	from, to := h.calendarReminders.window(now)
	owners, err := h.userStorage.CalendarOwners(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("calendar reminders: list owners: %v", err)
		}
		return
	}
	for _, owner := range owners {
		var due []storage.CalendarReminderDue
		var location *time.Location
		var sources []storage.CalendarSource
		if err := h.userAccounts.WithUser(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
			var err error
			if due, err = db.ListDueCalendarReminders(ctx, owner, from, to); err != nil || len(due) == 0 {
				return err
			}
			location = viewsCalendarLocation(db.GetUISettings(ctx, owner))
			sources, _ = db.ListSelectedCalendarSources(ctx, owner)
			return nil
		}); err != nil {
			continue
		}
		for _, item := range due {
			if h.calendarReminders.first(owner, item) {
				h.userIMAP.Events().Publish(calendarReminderEvent(owner, item, location, calendarReminderAccount(sources, item.Event.SourceID)))
			}
		}
	}
}

func calendarReminderAccount(sources []storage.CalendarSource, sourceID string) string {
	for _, source := range sources {
		if source.ID == sourceID {
			return source.AccountID
		}
	}
	return ""
}
