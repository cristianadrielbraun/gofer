package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	netmail "net/mail"
	"sort"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/calendar"
	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	ical "github.com/emersion/go-ical"
	"github.com/google/uuid"
)

func (h *Handler) wakeCalendarNotificationWorker(ctx context.Context, source storage.CalendarSource) {
	if p, owned := ctx.Value(userCalendarProviderKey{}).(*userCalendarRequest); owned {
		if p == nil || p.h != h || (p.event == nil && p.source == nil) {
			return
		}
		original := p.actionSource()
		if source.UserID != original.UserID || source.AccountID != original.AccountID || source.ID != original.ID {
			return
		}
		if err := h.userIMAP.QueueAccount(ctx, original.AccountID); err != nil {
			log.Printf("owned calendar: notification wake: %v", err)
		}
		return
	}
	h.signalOutgoingWorker()
}

func (p *userCalendarRequest) queueNotification(ctx context.Context, source storage.CalendarSource, endpoint, key, method string, cal, expected *ical.Calendar, guests []calendar.GuestDraft, deleted bool) error {
	if p == nil || (p.event == nil && p.source == nil) {
		return storage.ErrCalendarSourceChanged
	}
	if err := p.ready(ctx); err != nil {
		return err
	}
	original := p.actionSource()
	if source.ID != original.ID || source.UserID != original.UserID || source.AccountID != original.AccountID || source.RemoteID != original.RemoteID || source.Provider != original.Provider || !calendarSourceWritable(original) || original.Provider != storage.CalendarSourceProviderCalDAV {
		return storage.ErrCalendarSourceChanged
	}
	identity := p.service().Identity()
	guests = filterCalendarOrganizerGuests(guests, identity.EmailAddress)
	if len(guests) == 0 {
		return nil
	}
	var recipients []string
	for _, guest := range guests {
		if calendarReplyAddress("mailto:"+guest.Email) == "" {
			return fmt.Errorf("invalid guest")
		}
		recipients = append(recipients, guest.Email)
	}
	sort.Strings(recipients)
	// Preserve the legacy deterministic identity. Local IDs are isolated by the
	// owner's store; repeated saves never replay accepted/ambiguous deliveries.
	id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(source.ID+"\x00"+endpoint+"\x00"+key+"\x00"+method+"\x00"+strings.Join(recipients, ","))).String()
	var previous storage.OutgoingSend
	err := p.h.userAccounts.WithAccountForUser(ctx, source.UserID, source.AccountID, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		previous, err = db.GetOutgoingSend(ctx, id)
		return err
	})
	if err == nil {
		if previous.AccountID != source.AccountID {
			return fmt.Errorf("notification identity changed")
		}
		if previous.Status == storage.OutgoingSendFailed {
			_, err = p.h.userAccounts.RetryCalendarNotification(ctx, source.UserID, id, false)
			return err
		}
		if previous.Status == storage.OutgoingSendCanceled {
			return fmt.Errorf("this meeting notification was canceled; reopen the event before saving")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	body, err := calendarNotificationICS(cal, method, guests)
	if err != nil {
		return err
	}
	var desired string
	if expected != nil {
		desired, err = encodeCalendarReply(expected)
		if err != nil {
			return err
		}
	}
	summary, _ := cal.Events()[0].Props.Text("SUMMARY")
	label := "Invitation"
	if method == "CANCEL" {
		label = "Canceled"
	}
	msg := &message.OutgoingMessage{FromName: identity.DisplayName, FromEmail: identity.EmailAddress, Subject: label + ": " + strings.NewReplacer("\r", " ", "\n", " ").Replace(summary), TextBody: label + ": " + summary, MessageID: message.NewMessageID(), Date: time.Now().UTC(), CalendarNotification: &message.CalendarNotification{UserID: source.UserID, SourceID: source.ID, ResourceID: endpoint, Method: method, Calendar: body, ExpectedCalendar: desired, Deleted: deleted}}
	for _, guest := range guests {
		msg.To = append(msg.To, &netmail.Address{Name: guest.Name, Address: guest.Email})
	}
	raw, err := buildOutgoingMIME(storage.OutgoingTransportSMTP, msg)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(snapshotOutgoingMessage(msg))
	if err != nil {
		return err
	}
	selected := p.source
	var events []*config.UserCalendarEventSnapshot
	if p.event != nil {
		selected, err = p.h.userAccounts.SnapshotCalendarSource(ctx, source.UserID, source.ID)
		if err != nil {
			return err
		}
		events = append(events, p.event)
	}
	_, err = p.h.userAccounts.QueueCalendarNotification(ctx, selected, storage.QueueOutgoingSendInput{ID: id, AccountID: source.AccountID, Transport: storage.OutgoingTransportSMTP, EnvelopeFrom: msg.FromEmail, EnvelopeRecipients: recipients, MIMEData: raw, MessageJSON: payload}, events...)
	return err
}
