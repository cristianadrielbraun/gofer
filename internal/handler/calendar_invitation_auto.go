package handler

import (
	"context"
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// Invitations Gofer adds to a CalDAV calendar by itself, as Google and
// Outlook do for theirs: added, not answered. Only an invitation that comes
// from its organizer, in an email their domain signed, is; anyone can write
// any organizer into an invitation, so any other, a forwarded one too, waits
// for the person to add it from the email. A later invitation from the
// organizer changes the event, and a cancellation removes it.

const (
	calendarInvitationBatch       = 12
	calendarInvitationMaxAttempts = 8
)

// importMailInvitations does what the cards of an email's invitations
// offer, for those its organizer sent and signed. offer says what a card
// offers, and apply does it.
func (h *Handler) importMailInvitations(ctx context.Context, raw []byte, items []mailCalendarEvent, address string, offer func(mailCalendarEvent) (*views.MailCalendarInvitation, error), apply func(invitation mailInvitation, uid, sourceID string) error) error {
	done := map[string]bool{}
	verified := false
	var problems []error
	for _, item := range items {
		if done[item.UID] {
			continue
		}
		done[item.UID] = true
		card, err := offer(item)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if card == nil {
			continue
		}
		invitation, err := readMailInvitation(raw, item.UID, address, false)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if !verified {
			if err := message.VerifyCalendarIncomingSender(ctx, raw, invitation.organizer, h.calendarIncomingLookupTXT); err != nil {
				return err
			}
			verified = true
		}
		if err := apply(invitation, item.UID, card.SourceID); err != nil {
			problems = append(problems, err)
		}
	}
	return errors.Join(problems...)
}

// calendarInvitationOutcome is how reading an email for its invitations
// ended: done, never to be done by itself, or to try again.
func calendarInvitationOutcome(err error, attempts int) (state, note string) {
	if err == nil {
		return "complete", ""
	}
	note = err.Error()
	if len(note) > 500 {
		note = note[:500]
	}
	if calendarInvitationFinal(err) || attempts+1 >= calendarInvitationMaxAttempts {
		return "ignored", note
	}
	return "retry", note
}

// calendarInvitationFinal reports whether trying again can't change err:
// Gofer won't do it, or the email isn't its organizer's.
func calendarInvitationFinal(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, part := range joined.Unwrap() {
			if !calendarInvitationFinal(part) {
				return false
			}
		}
		return true
	}
	var refusal calendarInvitationRefusal
	return errors.As(err, &refusal) || errors.Is(err, message.ErrCalendarReplyAuthentication)
}

// mailCalendarRefreshWindow is the part of the calendar an email's events
// are in, refreshed once they are added: the month of the first.
func mailCalendarRefreshWindow(items []mailCalendarEvent) (time.Time, time.Time) {
	start := time.Now()
	if len(items) > 0 && items[0].Start != nil {
		start = *items[0].Start
	}
	month := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, start.Location())
	return month.AddDate(0, 0, -7), month.AddDate(0, 1, 7)
}

// runCalendarInvitationTick reads new mail for invitations, oldest first.
// An email to try again holds back its account's later ones.
func (h *Handler) runCalendarInvitationTick(ctx context.Context) {
	if h.accountStore == nil {
		return
	}
	messages, err := h.db.ListCalendarInvitationMessages(ctx, "", "", message.CalendarIncomingMaxSize, calendarInvitationBatch)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("calendar invitations from email: %v", err)
		}
		return
	}
	held := map[string]bool{}
	for _, candidate := range messages {
		if ctx.Err() != nil {
			return
		}
		if held[candidate.AccountID] || (candidate.Retry && !candidate.Due) {
			held[candidate.AccountID] = true
			continue
		}
		work, cancel := context.WithTimeout(auth.ContextWithUser(ctx, &auth.User{ID: candidate.UserID, Username: candidate.UserID}), time.Minute)
		err := h.processCalendarInvitationMessage(work, candidate)
		cancel()
		if ctx.Err() != nil {
			return
		}
		state, note := calendarInvitationOutcome(err, candidate.Attempts)
		held[candidate.AccountID] = state == "retry"
		finish, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if err := h.db.FinishCalendarInvitationMessage(finish, candidate, state, note); err != nil {
			log.Printf("calendar invitation email %d: could not record outcome: %v", candidate.ID, err)
			held[candidate.AccountID] = true
		}
		done()
	}
}

func (h *Handler) processCalendarInvitationMessage(ctx context.Context, candidate storage.CalendarInvitationMessage) error {
	items, accountID, raw, address, err := h.readMailCalendarMessage(ctx, strconv.FormatInt(candidate.ID, 10))
	if err != nil {
		return err
	}
	if accountID != candidate.AccountID {
		return calendarInvitationRefusal("the email isn't in this account any more")
	}
	if len(items) == 0 {
		return nil
	}
	sources, err := h.db.ListSelectedCalendarSources(ctx, candidate.UserID)
	if err != nil {
		return err
	}
	applied := false
	err = h.importMailInvitations(ctx, raw, items, address, func(item mailCalendarEvent) (*views.MailCalendarInvitation, error) {
		candidates, err := h.db.ListMailCalendarEvents(ctx, candidate.UserID, accountID, item.UID)
		if err != nil {
			return nil, err
		}
		return mailCalendarInvitationOffer(ctx, item, candidates, sources, accountID, address), nil
	}, func(invitation mailInvitation, uid, sourceID string) error {
		err := h.applyMailInvitation(ctx, accountID, invitation, uid, sourceID)
		applied = applied || err == nil
		return err
	})
	// The next email may be for the same event: it is read against the
	// calendar as it is now.
	if applied {
		start, end := mailCalendarRefreshWindow(items)
		if _, syncErr := h.syncCalendarScopedWindow(ctx, candidate.UserID, start, end, "", accountID); syncErr != nil && err == nil {
			err = syncErr
		}
	}
	return err
}

// runUserCalendarInvitations is runCalendarInvitationTick for an owner's
// account.
func (h *Handler) runUserCalendarInvitations(ctx context.Context, owner, account string) error {
	var messages []storage.CalendarInvitationMessage
	if err := h.userAccounts.WithAccountForUser(ctx, owner, account, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		messages, err = db.ListCalendarInvitationMessages(ctx, owner, account, message.CalendarIncomingMaxSize, calendarInvitationBatch)
		return err
	}); err != nil {
		return err
	}
	for _, candidate := range messages {
		if candidate.Retry && !candidate.Due {
			return nil
		}
		work, cancel := context.WithTimeout(auth.ContextWithUser(ctx, &auth.User{ID: owner, Username: owner}), time.Minute)
		err := h.processUserCalendarInvitation(work, candidate)
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		state, note := calendarInvitationOutcome(err, candidate.Attempts)
		if err := h.userAccounts.WithAccountForUser(ctx, owner, account, func(_ *config.AccountStore, db *storage.DB) error {
			return db.FinishCalendarInvitationMessage(ctx, candidate, state, note)
		}); err != nil {
			return err
		}
		if state == "retry" {
			return nil
		}
	}
	return nil
}

func (h *Handler) processUserCalendarInvitation(ctx context.Context, candidate storage.CalendarInvitationMessage) error {
	return h.userIMAP.RunUserServiceWork(ctx, candidate.UserID, func(ctx context.Context) error {
		items, raw, _, err := h.readUserMailCalendarEvents(ctx, strconv.FormatInt(candidate.ID, 10))
		if err != nil {
			return err
		}
		if raw.AccountID() != candidate.AccountID {
			return calendarInvitationRefusal("the email isn't in this account any more")
		}
		if len(items) == 0 {
			return nil
		}
		var sources []storage.CalendarSource
		if err := h.userAccounts.WithAccountForUser(ctx, candidate.UserID, candidate.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
			var err error
			sources, err = db.ListSelectedCalendarSources(ctx, candidate.UserID)
			return err
		}); err != nil {
			return err
		}
		applied := false
		err = h.importMailInvitations(ctx, raw.Bytes(), items, raw.EmailAddress(), func(item mailCalendarEvent) (*views.MailCalendarInvitation, error) {
			var candidates []storage.CalendarEvent
			err := h.userAccounts.WithAccountForUser(ctx, candidate.UserID, candidate.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
				var err error
				candidates, err = db.ListMailCalendarEvents(ctx, candidate.UserID, candidate.AccountID, item.UID)
				return err
			})
			if err != nil {
				return nil, err
			}
			return mailCalendarInvitationOffer(ctx, item, candidates, sources, candidate.AccountID, raw.EmailAddress()), nil
		}, func(invitation mailInvitation, uid, sourceID string) error {
			err := h.applyUserMailInvitation(ctx, candidate.UserID, candidate.AccountID, invitation, uid, sourceID)
			applied = applied || err == nil
			return err
		})
		// The next email may be for the same event: it is read against the
		// calendar as it is now.
		if applied {
			start, end := mailCalendarRefreshWindow(items)
			if _, syncErr := h.syncUserCalendarWindow(ctx, candidate.UserID, start, end, candidate.AccountID, false, true); syncErr != nil && err == nil {
				err = syncErr
			}
		}
		return err
	})
}
