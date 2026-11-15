package smtp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/emersion/go-smtp"
)

// SenderCheck is what the server said when asked to take mail from an
// address.
type SenderCheck struct {
	// Accepted says the server took the address as the sender, and the
	// recipient after it.
	Accepted bool
	// Stage is "mail" or "rcpt", the command the server refused.
	Stage   string
	Code    int
	Message string
}

// CheckSender asks the account's SMTP server whether it accepts mail from
// address, signed in as the account: it gives MAIL FROM and RCPT TO, then
// RSET, and never DATA, so nothing is sent. Some servers decide only once
// the message is sent, so an accepted address can still be refused then. An
// error means the server could not be asked (connection or sign-in failed).
func CheckSender(ctx context.Context, cfg *models.AccountConfig, password, address, recipient string) (SenderCheck, error) {
	client, err := NewClient(ctx, cfg, password)
	if err != nil {
		return SenderCheck{}, err
	}
	defer client.Close()
	return client.checkSender(ctx, address, recipient)
}

func (client *Client) checkSender(ctx context.Context, address, recipient string) (SenderCheck, error) {
	stop := context.AfterFunc(nonNilContext(ctx), func() { _ = client.conn.Close() })
	defer stop()

	refused := func(stage string, err error) (SenderCheck, error) {
		var smtpErr *smtp.SMTPError
		if errors.As(err, &smtpErr) {
			return SenderCheck{Stage: stage, Code: smtpErr.Code, Message: strings.TrimSpace(smtpErr.Message)}, nil
		}
		return SenderCheck{}, fmt.Errorf("%s: %w", stage, err)
	}
	if err := client.client.Mail(address, nil); err != nil {
		return refused("mail", err)
	}
	if strings.TrimSpace(recipient) != "" {
		if err := client.client.Rcpt(recipient, nil); err != nil {
			_ = client.client.Reset()
			return refused("rcpt", err)
		}
	}
	if err := client.client.Reset(); err != nil {
		return SenderCheck{}, fmt.Errorf("rset: %w", err)
	}
	return SenderCheck{Accepted: true}, nil
}

// IsSenderRefused reports whether a send failed because the server refused
// the sender for good: a permanent reply to MAIL FROM, as servers give for
// an address the account may not send as.
func IsSenderRefused(err error) bool {
	if err == nil || IsRetryable(err) {
		return false
	}
	var smtpErr *smtp.SMTPError
	return errors.As(err, &smtpErr) && smtpErr.Code >= 500 && strings.Contains(err.Error(), "mail from:")
}
