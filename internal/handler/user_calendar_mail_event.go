package handler

import (
	"context"
	"database/sql"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
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
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	h.renderUserMailCalendarFooter(ctx, w, r.PathValue("id"), r.URL.Query().Get("refresh") == "1", "", "")
}

// renderUserMailCalendarFooter shows an email's calendar events; note, when
// set, replaces what the card for event uid says.
func (h *Handler) renderUserMailCalendarFooter(ctx context.Context, w http.ResponseWriter, emailID string, refresh bool, uid, note string) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var cards []views.MailCalendarEventData
	err := h.userIMAP.RunUserServiceWork(ctx, h.userID(ctx), func(ctx context.Context) error {
		items, raw, location, err := h.readUserMailCalendarEvents(ctx, emailID)
		if err != nil {
			return err
		}
		if refresh && len(items) > 0 {
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
			card := mailCalendarEventCard(ctx, item, event, snapshot != nil, location, emailID, func(storage.CalendarEvent) bool { return allowed })
			var candidates []storage.CalendarEvent
			var sources []storage.CalendarSource
			if err := h.userAccounts.WithAccountForUser(ctx, h.userID(ctx), raw.AccountID(), func(_ *config.AccountStore, db *storage.DB) error {
				candidates, _ = db.ListMailCalendarEvents(ctx, h.userID(ctx), raw.AccountID(), item.UID)
				sources, _ = db.ListSelectedCalendarSources(ctx, h.userID(ctx))
				return nil
			}); err != nil {
				return err
			}
			mailCalendarInvitationCard(ctx, &card, item, candidates, sources, raw.AccountID(), raw.EmailAddress(), location)
			if note != "" && item.UID == uid {
				card.Note, card.Invitation = note, nil
			}
			cards = append(cards, card)
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
	_ = views.MailCalendarFooter(emailID, cards).Render(ctx, w)
}

// handleUserMailCalendarInvitation adds an email's invitation to a CalDAV
// calendar of the owner's account, or updates or removes the event it is for.
func (h *Handler) handleUserMailCalendarInvitation(w http.ResponseWriter, r *http.Request) {
	if r.ParseForm() != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.the_request_could_not_be_read"), http.StatusBadRequest)
		return
	}
	uid, sourceID := r.PostForm.Get("uid"), r.PostForm.Get("source_id")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), calendarSourceTimeout)
	defer cancel()
	emailID, owner := r.PathValue("id"), h.userID(r.Context())
	if uid == "" || len(uid) > 1024 {
		h.renderUserMailCalendarFooter(ctx, w, emailID, false, uid, "The invitation could not be read.")
		return
	}
	err := h.userIMAP.RunUserServiceWork(ctx, owner, func(ctx context.Context) error {
		_, raw, _, err := h.readUserMailCalendarEvents(ctx, emailID)
		if err != nil {
			return err
		}
		// Asked for, a forwarded invitation can be added too.
		invitation, err := readMailInvitation(raw.Bytes(), uid, raw.EmailAddress(), true)
		if err != nil {
			return err
		}
		return h.applyUserMailInvitation(ctx, owner, raw.AccountID(), invitation, uid, sourceID)
	})
	if err != nil {
		h.renderUserMailCalendarFooter(ctx, w, emailID, false, uid, mailCalendarInvitationFailure(err))
		return
	}
	h.renderUserMailCalendarFooter(ctx, w, emailID, true, "", "")
}

// applyUserMailInvitation adds the invitation for uid to the owner's calendar
// sourceID, or updates or removes the event it is for in its own calendar.
func (h *Handler) applyUserMailInvitation(ctx context.Context, owner, account string, invitation mailInvitation, uid, sourceID string) error {
	var candidates []storage.CalendarEvent
	var sources []storage.CalendarSource
	if err := h.userAccounts.WithAccountForUser(ctx, owner, account, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		if candidates, err = db.ListMailCalendarEvents(ctx, owner, account, uid); err != nil {
			return err
		}
		sources, err = db.ListSelectedCalendarSources(ctx, owner)
		return err
	}); err != nil {
		return err
	}
	source, existing, err := mailCalendarInvitationTarget(candidates, sources, account, sourceID)
	if err != nil {
		return err
	}
	p := &userCalendarRequest{h: h}
	if existing.ID != "" {
		if p.event, err = h.userAccounts.SnapshotCalendarEvent(ctx, owner, existing.ID); err != nil {
			return err
		}
		existing, source = p.event.Event(), p.event.Source()
	} else if p.source, err = h.userAccounts.SnapshotCalendarSource(ctx, owner, source.ID); err != nil {
		return err
	} else {
		source = p.source.Source()
	}
	if source.AccountID != account || !h.userCalendarWriteAuthorized(ctx, source) {
		return calendarInvitationRefusal("this account can't write to its calendar")
	}
	return h.userIMAP.RunAccountService(ctx, owner, source.AccountID, mail.AccountServiceCalendar, calendarSourceTimeout, func(operation context.Context) error {
		unlock, err := h.lockCalendarCreate(operation, source)
		if err != nil {
			return err
		}
		defer unlock()
		action, err := p.actionContext(operation, true)
		if err != nil {
			return err
		}
		credentials, err := p.actionCredentials()
		if err != nil {
			return err
		}
		_, err = applyCalDAVInvitation(action, source, credentials, invitation, existing)
		return err
	})
}
