package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	netmail "net/mail"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	smtpclient "github.com/cristianadrielbraun/gofer/internal/mail/smtp"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// The mail worker already holds its account mail gate. The distinct Calendar
// gate and owner/source flight protect the invitation through SMTP. The local
// result checks the original attempt again after its writer wait. No store lease
// spans either provider HTTP or SMTP.
func (s *userMailDeliveryScope) sendCalendarReply(ctx context.Context, claimed storage.OutgoingSend) (bool, error) {
	var more bool
	err := s.runner.h.userIMAP.RunUserServiceWork(ctx, s.owner, func(operation context.Context) error {
		var err error
		more, err = s.deliverCalendarReply(operation, claimed)
		return err
	})
	return more, err
}

func (s *userMailDeliveryScope) deliverCalendarReply(ctx context.Context, claimed storage.OutgoingSend) (bool, error) {
	h := s.runner.h
	delivery, err := h.userAccounts.SnapshotCalendarReplyDelivery(ctx, s.owner, claimed.ID)
	if err != nil {
		return false, err
	} // Leave unverifiable claims for explicit recovery.
	send := delivery.Send()
	if send.AccountID != s.id || send.Status != claimed.Status || send.AttemptCount != claimed.AttemptCount {
		return false, storage.ErrCalendarEventChanged
	}
	var snapshot outgoingMessageSnapshot
	err = json.Unmarshal(send.MessageJSON, &snapshot)
	if err != nil || snapshot.MessageID == "" || snapshot.CalendarReply == "" || snapshot.CalendarNotification != nil {
		err = errors.New("queued calendar reply content is unavailable")
	}
	result := models.SendFailed
	if err == nil {
		err = h.userIMAP.RunAccountService(ctx, s.owner, s.id, mail.AccountServiceCalendar, 2*time.Minute, func(operation context.Context) error {
			authority, err := h.userAccounts.RestoreCalendarReply(operation, s.owner, send.ID, true)
			if err != nil {
				return err
			}
			job := authority.Job()
			var payload calendarReplyPayload
			if json.Unmarshal([]byte(job.Payload), &payload) != nil {
				return errors.New("queued calendar reply could not be verified")
			}
			source := authority.Source().Source()
			unlock, err := h.lockCalendarCreate(operation, source)
			if err != nil {
				return markOutgoingSendRetryable(fmt.Errorf("calendar is busy"))
			}
			defer unlock()
			p := &userCalendarRequest{h: h, event: authority.Event(), reply: authority}
			target, err := p.readResponseTarget(operation, payload.Scope)
			if err != nil {
				return err
			}
			if target.Endpoint != job.ResourceID || target.HTTPETag != job.Version || target.SelfEmail != payload.SelfEmail || target.CalDAV == nil || calendarReplyAddress(target.CalDAV.Selected.Props.Get("ORGANIZER").Value) != payload.Organizer || !strings.EqualFold(send.EnvelopeFrom, payload.SelfEmail) || len(send.EnvelopeRecipients) != 1 || send.EnvelopeRecipients[0] != payload.Organizer {
				return errors.New("the invitation changed; refresh it before replying")
			}
			service := authority.Event().Service()
			cfg, err := service.SMTPConfig(operation)
			if err != nil {
				return err
			}
			// CalDAV is configured only for IMAP accounts. Gmail/Graph RSVP
			// uses its native Calendar provider instead of this SMTP queue.
			if cfg.AuthMethod != "plain" {
				return errors.New("queued Calendar SMTP OAuth authority is unavailable")
			}
			password, err := service.SMTPPassword()
			if err != nil {
				return err
			}
			if err := h.userAccounts.ValidateCalendarReply(operation, authority); err != nil {
				return err
			}
			result, err, _ = smtpclient.SendRawMessageWithTiming(operation, cfg, password, send.EnvelopeFrom, send.EnvelopeRecipients, send.MIMEData)
			// A settings/cache change after SMTP must not erase its confirmed result.
			return err
		})
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	} // Recovery marks interruptions ambiguous.
	status := storage.OutgoingSendSent
	errText := ""
	retryDelay := outgoingSendRetryDelayWithJitter(send.AttemptCount, h.outgoingRandomValue())
	next := time.Now().Add(retryDelay)
	if result != models.SendSuccess {
		if err == nil {
			err = errors.New("SMTP delivery was not confirmed")
		}
		errText = sanitizeOutgoingErrorText(err.Error())
		status = storage.OutgoingSendFailed
		if result == models.SendAmbiguous {
			status = storage.OutgoingSendAmbiguous
		} else if (smtpclient.IsRetryable(err) || errors.Is(err, errOutgoingSendRetryable)) && send.AttemptCount < outgoingSendMaxAttempts {
			status = storage.OutgoingSendPending
		}
	}
	publication := storage.CalendarReplySendResult{Status: status, Error: errText}
	if status == storage.OutgoingSendSent {
		publication.InternetID = snapshot.MessageID
	}
	if status == storage.OutgoingSendPending {
		publication.NextAttemptAt = next
	}
	if finalErr := h.userAccounts.FinishCalendarReplySend(ctx, delivery, publication); finalErr != nil {
		return false, errors.Join(err, finalErr)
	}
	eventStatus := status
	payload := map[string]any{"send_id": send.ID}
	if status == storage.OutgoingSendPending {
		eventStatus = "retrying"
		payload["attempt"] = send.AttemptCount
		payload["next_attempt_at"] = next.Format(time.RFC3339)
		payload["retry_in_seconds"] = retryInSeconds(retryDelay)
	}
	h.userIMAP.Events().Publish(mail.Event{Type: mail.EventSendResult, UserID: s.owner, AccountID: s.id, Status: eventStatus, Error: errText, Payload: payload})
	if status == storage.OutgoingSendSent {
		err = errors.Join(err, h.WakeUserCalendarAccount(ctx, s.owner, s.id))
	}
	return true, err
}

// Build the native MIME and followup from a verified provider target, then let
// the repository bind both rows to the private reservation in one transaction.
func (p *userCalendarRequest) queueCalDAVReply(ctx context.Context, claim *config.UserCalendarResponseClaim, target calendarResponseTarget, response string) (calendarResponseResult, error) {
	if p == nil || p.event == nil || target.CalDAV == nil || !p.service().SMTPConfigured() {
		return calendarResponseResult{}, storage.ErrCalendarEventChanged
	}
	identity := p.service().Identity()
	if !strings.EqualFold(identity.EmailAddress, target.SelfEmail) {
		return calendarResponseResult{}, storage.ErrCalendarEventChanged
	}
	desired, reply, err := prepareCalDAVResponse(target, response)
	if err != nil {
		return calendarResponseResult{}, err
	}
	body, err := encodeCalendarReply(desired)
	if err != nil {
		return calendarResponseResult{}, err
	}
	organizer := calendarReplyAddress(target.CalDAV.Selected.Props.Get("ORGANIZER").Value)
	label := map[string]string{"accepted": "Accepted", "tentative": "Tentative", "declined": "Declined"}[response]
	msg := &message.OutgoingMessage{FromName: identity.DisplayName, FromEmail: identity.EmailAddress, To: []*netmail.Address{{Address: organizer}}, Subject: label + ": " + strings.NewReplacer("\r", " ", "\n", " ").Replace(target.Event.Summary), TextBody: "My response to this invitation: " + response + ".", CalendarReply: reply, MessageID: message.NewMessageID(), Date: time.Now().UTC()}
	raw, err := buildOutgoingMIME(storage.OutgoingTransportSMTP, msg)
	if err != nil {
		return calendarResponseResult{}, err
	}
	outgoing, err := json.Marshal(snapshotOutgoingMessage(msg))
	if err != nil {
		return calendarResponseResult{}, err
	}
	payload, err := json.Marshal(calendarReplyPayload{Event: p.event.Event(), Target: target.Event, Scope: target.Scope, SelfEmail: target.SelfEmail, Organizer: organizer, Calendar: body})
	if err != nil {
		return calendarResponseResult{}, err
	}
	id, err := p.h.userAccounts.QueueCalendarReply(ctx, claim, storage.CalendarReplyJob{ResourceID: target.Endpoint, RemoteID: target.Event.RemoteID, Version: target.HTTPETag, Response: response, Payload: string(payload)}, storage.QueueOutgoingSendInput{AccountID: p.service().AccountID(), Transport: storage.OutgoingTransportSMTP, EnvelopeFrom: msg.FromEmail, EnvelopeRecipients: []string{organizer}, MIMEData: raw, MessageJSON: outgoing})
	if err != nil {
		return calendarResponseResult{}, err
	}
	return calendarResponseResult{Event: target.Event, Pending: true, Delivery: "email", DeliveryID: id}, nil
}
