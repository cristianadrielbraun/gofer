package handler

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	smtpclient "github.com/cristianadrielbraun/gofer/internal/mail/smtp"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Organizer messages retain the native DAV preflight and SMTP MIME. The account
// and source gates span protocol work, while each repository lease is short.
func (s *userMailDeliveryScope) sendCalendarNotification(ctx context.Context, claimed storage.OutgoingSend) (bool, error) {
	var more bool
	err := s.runner.h.userIMAP.RunUserServiceWork(ctx, s.owner, func(operation context.Context) error {
		var err error
		more, err = s.deliverCalendarNotification(operation, claimed)
		return err
	})
	return more, err
}

func (s *userMailDeliveryScope) deliverCalendarNotification(ctx context.Context, claimed storage.OutgoingSend) (bool, error) {
	h := s.runner.h
	delivery, err := h.userAccounts.SnapshotCalendarNotificationDelivery(ctx, s.owner, claimed.ID)
	if err != nil {
		return false, err
	} // Leave unverifiable claims for explicit recovery.
	send := delivery.Send()
	if send.AccountID != s.id || send.Status != claimed.Status || send.AttemptCount != claimed.AttemptCount {
		return false, storage.ErrCalendarEventChanged
	}
	var snapshot outgoingMessageSnapshot
	err = json.Unmarshal(send.MessageJSON, &snapshot)
	if err != nil || snapshot.MessageID == "" || snapshot.CalendarReply != "" || snapshot.CalendarNotification == nil {
		err = errors.New("queued meeting notification content is unavailable")
	}
	result := models.SendFailed
	if err == nil {
		err = h.userIMAP.RunAccountService(ctx, s.owner, s.id, mail.AccountServiceCalendar, 2*time.Minute, func(operation context.Context) error {
			if err := h.userAccounts.RestoreCalendarNotification(operation, delivery); err != nil {
				return err
			}
			p := &userCalendarRequest{h: h, source: delivery.Source(), notification: delivery}
			operation, err := p.actionContext(operation, false)
			if err != nil {
				return err
			}
			unlock, err := h.beforeCalendarNotificationSend(operation, send, snapshot.outgoingMessage())
			defer unlock()
			if err != nil {
				return err
			}
			service := delivery.Source().Service()
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
			if err := h.userAccounts.ValidateCalendarNotification(operation, delivery); err != nil {
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
	if finalErr := h.userAccounts.FinishCalendarNotificationSend(ctx, delivery, publication); finalErr != nil {
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
