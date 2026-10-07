package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// Mail IDs are local to the authenticated owner's store; neither a submitted
// account nor the central database can resolve them. Raw recovery keeps no lease
// over its provider request and validates the saved retrieval identity afterward.
func (h *Handler) readUserMailCalendarEvents(ctx context.Context, mailID string) ([]mailCalendarEvent, *mail.RawMessage, *time.Location, error) {
	id, err := strconv.ParseInt(mailID, 10, 64)
	if err != nil || id <= 0 {
		return nil, nil, nil, sql.ErrNoRows
	}
	raw, err := h.userIMAP.ReadRawMessage(ctx, h.userID(ctx), id)
	if err != nil {
		return nil, nil, nil, err
	}
	var location *time.Location
	if err := h.userAccounts.WithAccountForUser(ctx, h.userID(ctx), raw.AccountID(), func(_ *config.AccountStore, db *storage.DB) error {
		location = viewsCalendarLocation(db.GetUISettings(ctx, h.userID(ctx)))
		return nil
	}); err != nil {
		return nil, nil, nil, err
	}
	if err := h.userIMAP.ValidateRawMessage(ctx, raw); err != nil {
		return nil, nil, nil, err
	}
	data := raw.Bytes()
	if len(data) > message.CalendarIncomingMaxSize {
		return nil, raw, location, nil
	}
	return mailCalendarEvents(data, raw.EmailAddress(), location), raw, location, nil
}

func (h *Handler) matchUserMailCalendarEvent(ctx context.Context, account string, item mailCalendarEvent) (*config.UserCalendarEventSnapshot, error) {
	var candidates []storage.CalendarEvent
	if err := h.userAccounts.WithAccountForUser(ctx, h.userID(ctx), account, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		candidates, err = db.ListMailCalendarEvents(ctx, h.userID(ctx), account, item.UID)
		return err
	}); err != nil {
		return nil, err
	}
	event, matched := matchMailCalendarEvents(candidates, item)
	if !matched {
		return nil, nil
	}
	snapshot, err := h.userAccounts.SnapshotCalendarEvent(ctx, h.userID(ctx), event.ID)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, storage.ErrAccountRoute) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if snapshot.Source().AccountID != account {
		return nil, storage.ErrCalendarEventChanged
	}
	// Requery after the snapshot lease wait: a newly added matching occurrence
	// must make the invitation ambiguous rather than authorizing the first row.
	// ListMailCalendarEvents also preserves Outlook's validated wrapped-UID match.
	if err := h.userAccounts.WithAccountForUser(ctx, h.userID(ctx), account, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		candidates, err = db.ListMailCalendarEvents(ctx, h.userID(ctx), account, item.UID)
		return err
	}); err != nil {
		return nil, err
	}
	if err := h.userAccounts.ValidateCalendarEvent(ctx, snapshot); err != nil {
		return nil, err
	}
	current, ok := matchMailCalendarEvents(candidates, item)
	if !ok || current.ID != event.ID {
		return nil, nil
	}
	return snapshot, nil
}

func (h *Handler) handleUserMailCalendarFooter(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	var cards []views.MailCalendarEventData
	err := h.userIMAP.RunUserServiceWork(ctx, h.userID(ctx), func(ctx context.Context) error {
		items, raw, location, err := h.readUserMailCalendarEvents(ctx, r.PathValue("id"))
		if err != nil {
			return err
		}
		if r.URL.Query().Get("refresh") == "1" && len(items) > 0 {
			start := time.Now()
			if items[0].Start != nil {
				start = *items[0].Start
			}
			month := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, start.Location())
			_, _ = h.syncUserCalendarWindow(ctx, h.userID(ctx), month.AddDate(0, 0, -7), month.AddDate(0, 1, 7), raw.AccountID(), false, true)
		}
		snapshots := make([]*config.UserCalendarEventSnapshot, 0, len(items))
		for _, item := range items {
			snapshot, err := h.matchUserMailCalendarEvent(ctx, raw.AccountID(), item)
			if err != nil {
				return err
			}
			var event storage.CalendarEvent
			allowed := false
			if snapshot != nil {
				event = snapshot.Event()
				source := snapshot.Source()
				allowed = calendarResponseRestriction(event, source) == "" && h.userCalendarWriteAuthorized(ctx, source)
				snapshots = append(snapshots, snapshot)
			}
			cards = append(cards, mailCalendarEventCard(item, event, snapshot != nil, location, r.PathValue("id"), func(storage.CalendarEvent) bool { return allowed }))
		}
		for _, snapshot := range snapshots {
			if err := h.userAccounts.ValidateCalendarEvent(ctx, snapshot); err != nil {
				return err
			}
		}
		return h.userIMAP.ValidateRawMessage(ctx, raw)
	})
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	_ = views.MailCalendarFooter(r.PathValue("id"), cards).Render(ctx, w)
}
