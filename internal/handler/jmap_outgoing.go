package handler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// sendJMAPOutgoingRaw sends a queued message of a JMAP account and returns
// the sent email's id. Failures are marked for the outgoing queue: nothing
// sent and worth retrying, nothing sent for good, or possibly sent.
func (h *Handler) sendJMAPOutgoingRaw(ctx context.Context, cfg *models.AccountConfig, send storage.OutgoingSend) (string, error) {
	password, err := h.accountStore.DecryptPassword(ctx, send.AccountID)
	if err != nil {
		return "", markOutgoingSendRetryable(fmt.Errorf("failed to get credentials: %w", err))
	}
	emailID, err := mail.SendJMAPMessage(ctx, cfg, password, send.EnvelopeFrom, send.EnvelopeRecipients, send.MIMEData, jmapEarlierAttempt(send))
	var notSent *mail.JMAPNotSentError
	switch {
	case err == nil:
		return emailID, nil
	case errors.As(err, &notSent) && notSent.Retryable:
		return "", markOutgoingSendRetryable(err)
	case errors.As(err, &notSent):
		return "", err
	default:
		return "", fmt.Errorf("%w: %v", errOutgoingSendAmbiguous, err)
	}
}

// jmapEarlierAttempt is when an earlier attempt at send could have started,
// or zero on its first attempt. A send is tried no sooner than it is due.
func jmapEarlierAttempt(send storage.OutgoingSend) time.Time {
	if send.AttemptCount <= 1 {
		return time.Time{}
	}
	if send.SendAfter.After(send.CreatedAt) {
		return send.SendAfter
	}
	return send.CreatedAt
}

// cacheJMAPSentMessageID ties the local Sent copy to the email the server
// keeps in Sent, so syncing finds it instead of adding it again.
func (h *Handler) cacheJMAPSentMessageID(ctx context.Context, accountID string, msg *message.OutgoingMessage, emailID string) {
	emailID = strings.TrimSpace(emailID)
	if emailID == "" {
		return
	}
	localID, err := h.db.GetMessageLocalIDByInternetIDInternal(ctx, accountID, msg.MessageID)
	if err != nil || localID == 0 {
		return
	}
	if err := h.db.SetMessageProviderMessageID(ctx, localID, emailID); err != nil {
		log.Printf("jmap sent copy account=%s message=%d: %v", accountID, localID, err)
	}
}
