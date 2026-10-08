package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func (h *Handler) runUserCalendarIncoming(ctx context.Context, owner, account string) error {
	candidates, err := h.userAccounts.ListCalendarIncomingMessages(ctx, owner, account, 12)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := h.processUserCalendarIncoming(ctx, candidate); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("owned calendar incoming reply %d: %v", candidate.ID, err)
		}
	}
	return nil
}

func (h *Handler) processUserCalendarIncoming(ctx context.Context, candidate storage.CalendarIncomingMessage) error {
	// Capture before waiting for an account slot; never adopt edited mail/config.
	claim, err := h.userAccounts.SnapshotCalendarIncomingMessage(ctx, candidate)
	if err != nil {
		return err
	}
	return h.userIMAP.RunAccountService(ctx, candidate.UserID, candidate.AccountID, mail.AccountServiceCalendar, 30*time.Second, func(operation context.Context) error {
		if err := h.userAccounts.ValidateCalendarIncomingMessage(operation, claim); err != nil {
			return err
		}
		work := func() error {
			raw, err := h.userIMAP.ReadRawMessage(operation, candidate.UserID, candidate.ID)
			if err != nil {
				return err
			}
			next, err := h.userIMAP.RefreshCalendarIncomingRaw(operation, claim, raw)
			if err != nil {
				return err
			}
			claim = next
			bytes := raw.Bytes()
			reply, err := message.ExtractCalendarIncomingReply(bytes)
			if err != nil || reply == nil || reply.From != candidate.From {
				return fmt.Errorf("%w: no supported reply from the invited guest", errCalendarIncomingIgnored)
			}
			event, err := h.userAccounts.FindCalendarIncomingEvent(operation, claim, reply.UID)
			if errors.Is(err, sql.ErrNoRows) {
				return errCalendarIncomingIgnored
			}
			if err != nil {
				return err
			}
			if !strings.EqualFold(event.Event().AccountEmail, reply.Organizer) || !strings.EqualFold(event.Event().OrganizerEmail, reply.Organizer) {
				return errCalendarIncomingIgnored
			}
			next, err = h.userAccounts.BeginCalendarIncomingDelivery(operation, claim, event)
			if err != nil {
				return err
			}
			claim = next
			// A diagnostic association is never sender authentication. No native
			// request or response reservation precedes successful DKIM verification.
			if err := message.VerifyCalendarIncomingSender(operation, bytes, reply.From, h.calendarIncomingLookupTXT); err != nil {
				return err
			}
			if err := h.userAccounts.ValidateCalendarIncomingMessage(operation, claim); err != nil {
				return err
			}
			unlock, err := h.lockCalendarCreate(operation, event.Source())
			if err != nil {
				return err
			}
			defer unlock()
			p := &userCalendarRequest{h: h, event: event, incoming: claim}
			action, err := p.actionContext(operation, true)
			if err != nil {
				return err
			}
			credentials, err := p.actionCredentials()
			if err != nil {
				return err
			}
			return applyCalendarIncomingCalDAV(action, event.Source(), event.Event(), *reply, credentials,
				func(ctx context.Context) (bool, error) {
					var err error
					p.incomingResponse, err = h.userAccounts.ReserveCalendarIncomingResponse(ctx, claim, event, reply.Sequence, reply.Stamp, reply.Status)
					if errors.Is(err, storage.ErrCalendarIncomingChanged) {
						return false, nil
					}
					return err == nil, err
				}, func(ctx context.Context, updated storage.CalendarEvent) error {
					if err := h.userAccounts.PublishCalendarIncomingResponse(ctx, p.incomingResponse, updated); err != nil {
						return err
					}
					h.userIMAP.Events().Publish(mail.Event{Type: mail.EventCalendarChanged, UserID: candidate.UserID, Payload: map[string]any{"source_id": event.Source().ID, "event_id": event.Event().ID}})
					return nil
				})
		}
		err := work()
		state, note, reason := calendarIncomingOutcome(err)
		// Finalization remains inside the joined operation and revalidates the
		// exact MIME/config/receipt after its own writer/cache wait.
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(operation), 5*time.Second)
		defer cancel()
		_, finishErr := h.userAccounts.FinishCalendarIncomingDelivery(finishCtx, claim, state, note, reason)
		return errors.Join(err, finishErr)
	})
}

func calendarIncomingOutcome(err error) (state, note, reason string) {
	state, reason = "complete", "applied"
	if err == nil {
		return
	}
	note, state, reason = err.Error(), "retry", "retry"
	if errors.Is(err, errCalendarIncomingIgnored) || errors.Is(err, message.ErrCalendarReplyAuthentication) {
		state, reason = "ignored", "not_applied"
	}
	if errors.Is(err, message.ErrCalendarReplyAuthentication) {
		reason = "unverified"
	}
	if errors.Is(err, message.ErrCalendarReplyAuthenticationTemporary) {
		reason = "verification_pending"
	}
	if errors.Is(err, errCalendarIncomingServerManaged) {
		reason = "server_managed"
	}
	return
}
