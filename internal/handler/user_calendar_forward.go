package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// handleUserCalendarForward forwards an invitation with the owner's own event
// and account authority, as handleUserCalendarResponse answers one.
func (h *Handler) handleUserCalendarForward(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	recipients, comment, err := parseCalendarForwardForm(r)
	if err != nil {
		calendarForwardError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), calendarSourceTimeout)
	defer cancel()
	var source storage.CalendarSource
	var delivery string
	err = h.userIMAP.RunUserServiceWork(ctx, h.userID(ctx), func(ctx context.Context) error {
		snapshot, err := h.userAccounts.SnapshotCalendarEvent(ctx, h.userID(ctx), r.PathValue("id"))
		if err != nil {
			return calendarForwardFailure{http.StatusNotFound, "This invitation is no longer available. Refresh the calendar."}
		}
		event := snapshot.Event()
		source = snapshot.Source()
		if !calendarForwardProvider(source.Provider) {
			return calendarForwardFailure{http.StatusForbidden, "Forwarding is unavailable for this event."}
		}
		return h.userIMAP.RunAccountService(ctx, source.UserID, source.AccountID, mail.AccountServiceCalendar, calendarSourceTimeout, func(operation context.Context) error {
			unlock, err := h.lockCalendarCreate(operation, source)
			if err != nil {
				return err
			}
			defer unlock()
			p := &userCalendarRequest{h: h, event: snapshot}
			if err := p.responseAccess(operation); err != nil {
				return calendarForwardFailure{http.StatusForbidden, "Forwarding is unavailable for this event."}
			}
			if storage.CalendarSourceMailServer(source.Provider) && !p.service().SendsEmail() {
				return calendarForwardFailure{http.StatusBadRequest, "Forwarding sends the invitation by email. Configure SMTP for this account first."}
			}
			target, err := p.readResponseTarget(operation, calendarForwardScope(event))
			if err != nil {
				return err
			}
			operation = context.WithValue(operation, userCalendarProviderKey{}, p)
			delivery, err = h.forwardCalendarInvitation(operation, source, target, recipients, comment, func(input storage.QueueOutgoingSendInput) error {
				return h.userAccounts.WithAccountForUser(operation, source.UserID, source.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
					_, err := db.QueueOutgoingSend(operation, input)
					return err
				})
			})
			return err
		})
	})
	if err != nil {
		var form userCalendarFormFailure
		if errors.As(err, &form) {
			calendarUpdateFailure(w, form.status, form.message, false, false)
			return
		}
		calendarForwardError(w, err)
		return
	}
	if delivery == "email" {
		// Sent by the account's own delivery worker, like an email reply.
		_ = h.userIMAP.QueueAccount(ctx, source.AccountID)
	}
	calendarForwardDone(w, delivery)
}
