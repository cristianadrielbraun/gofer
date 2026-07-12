package smtp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/mail/oauth2sasl"
	mailtransport "github.com/cristianadrielbraun/gofer/internal/mail/transport"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

type Client struct {
	config *models.AccountConfig
	conn   net.Conn
	client *smtp.Client
}

// DeliveryTiming contains the durations and connection usage for one SMTP
// delivery attempt. It is intentionally separate from the delivery result so
// callers can measure the existing one-message-per-connection behavior before
// deciding whether connection reuse is worth the added state machine.
type DeliveryTiming struct {
	ConnectAuth           time.Duration
	Data                  time.Duration
	Total                 time.Duration
	QueueWait             time.Duration
	ConnectionEstablished bool
	MessagesPerConnection int
}

type deliveryError struct {
	err       error
	retryable bool
}

func (e *deliveryError) Error() string { return e.err.Error() }
func (e *deliveryError) Unwrap() error { return e.err }

func markDeliveryError(err error, retryable bool) error {
	if err == nil {
		return nil
	}
	var deliveryErr *deliveryError
	if errors.As(err, &deliveryErr) {
		return err
	}
	return &deliveryError{err: err, retryable: retryable}
}

func classifyPreAcceptanceError(err error) error {
	var smtpErr *smtp.SMTPError
	if errors.As(err, &smtpErr) {
		return markDeliveryError(err, smtpErr.Code/100 == 4)
	}
	return markDeliveryError(err, true)
}

func IsRetryable(err error) bool {
	var deliveryErr *deliveryError
	return errors.As(err, &deliveryErr) && deliveryErr.retryable
}

const (
	connectTimeout         = 15 * time.Second
	setupTimeout           = 30 * time.Second
	commandTimeout         = 30 * time.Second
	submissionTimeout      = 5 * time.Minute
	outgoingMessageTimeout = 5 * time.Minute
)

func nonNilContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func contextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func deadlineForContext(ctx context.Context, fallback time.Duration) time.Time {
	deadline := time.Now().Add(fallback)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		return ctxDeadline
	}
	return deadline
}

func configureTimeouts(client *smtp.Client) {
	client.CommandTimeout = commandTimeout
	client.SubmissionTimeout = submissionTimeout
}

func NewClient(ctx context.Context, cfg *models.AccountConfig, password string) (*Client, error) {
	tlsMode, err := mailtransport.RequireTLSModeWithPlaintext("SMTP", cfg.SMTPTLSMode, cfg.SMTPAllowPlaintext)
	if err != nil {
		return nil, markDeliveryError(err, false)
	}
	if tlsMode == mailtransport.TLSModePlaintext && !strings.EqualFold(strings.TrimSpace(cfg.AuthMethod), "plain") {
		return nil, markDeliveryError(fmt.Errorf("SMTP OAuth authentication is not allowed over a plaintext connection"), false)
	}
	ctx = nonNilContext(ctx)
	setupCtx, cancelSetup := context.WithTimeout(ctx, setupTimeout)
	defer cancelSetup()
	if err := setupCtx.Err(); err != nil {
		return nil, markDeliveryError(err, true)
	}

	addr := net.JoinHostPort(cfg.SMTPHost, strconv.Itoa(cfg.SMTPPort))
	dialer := &net.Dialer{Timeout: connectTimeout}

	var conn net.Conn
	var client *smtp.Client

	switch tlsMode {
	case mailtransport.TLSModeImplicit:
		tlsConfig := &tls.Config{
			ServerName: cfg.SMTPHost,
			MinVersion: tls.VersionTLS12,
		}
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: tlsConfig}
		conn, err = tlsDialer.DialContext(setupCtx, "tcp", addr)
		if err != nil {
			return nil, markDeliveryError(fmt.Errorf("connect to %s: %w", addr, contextError(setupCtx, err)), true)
		}
		client = smtp.NewClient(conn)

	case mailtransport.TLSModeStartTLS:
		conn, err = dialer.DialContext(setupCtx, "tcp", addr)
		if err != nil {
			return nil, markDeliveryError(fmt.Errorf("connect to %s: %w", addr, contextError(setupCtx, err)), true)
		}
		stopCancellation := context.AfterFunc(setupCtx, func() { _ = conn.Close() })
		defer stopCancellation()
		tlsConfig := &tls.Config{
			ServerName: cfg.SMTPHost,
			MinVersion: tls.VersionTLS12,
		}
		client, err = smtp.NewClientStartTLS(conn, tlsConfig)
		if err != nil {
			_ = conn.Close()
			return nil, classifyPreAcceptanceError(fmt.Errorf("starttls: %w", contextError(setupCtx, err)))
		}

	case mailtransport.TLSModePlaintext:
		conn, err = dialer.DialContext(setupCtx, "tcp", addr)
		if err != nil {
			return nil, markDeliveryError(fmt.Errorf("connect to %s: %w", addr, contextError(setupCtx, err)), true)
		}
		client = smtp.NewClient(conn)

	default:
		return nil, markDeliveryError(fmt.Errorf("unsupported SMTP TLS mode %q", tlsMode), false)
	}
	stopCancellation := context.AfterFunc(setupCtx, func() { _ = conn.Close() })
	defer stopCancellation()
	configureTimeouts(client)

	if err := client.Hello("localhost"); err != nil {
		_ = conn.Close()
		return nil, classifyPreAcceptanceError(fmt.Errorf("ehlo: %w", contextError(setupCtx, err)))
	}

	smtpUsername := cfg.SmtpUsername
	if smtpUsername == "" {
		smtpUsername = cfg.Username
	}

	switch cfg.AuthMethod {
	case "plain":
		saslClient := sasl.NewPlainClient("", smtpUsername, password)
		err = client.Auth(saslClient)
	case "oauth2":
		saslClient := oauth2sasl.NewClient(smtpUsername, password)
		err = client.Auth(saslClient)
	default:
		saslClient := sasl.NewPlainClient("", smtpUsername, password)
		err = client.Auth(saslClient)
	}

	if err != nil {
		_ = conn.Close()
		return nil, classifyPreAcceptanceError(fmt.Errorf("authenticate: %w", contextError(setupCtx, err)))
	}

	return &Client{
		config: cfg,
		conn:   conn,
		client: client,
	}, nil
}

func (c *Client) Send(ctx context.Context, msg *message.OutgoingMessage) (models.SendResult, error) {
	recipients := message.AllRecipients(msg)
	if len(recipients) == 0 {
		return models.SendFailed, markDeliveryError(fmt.Errorf("no recipients"), false)
	}
	mimeData, err := message.BuildMIMEMessage(msg)
	if err != nil {
		return models.SendFailed, markDeliveryError(fmt.Errorf("build mime: %w", err), false)
	}
	return c.SendRaw(ctx, msg.FromEmail, recipients, mimeData)
}

func (c *Client) SendRaw(ctx context.Context, from string, recipients []string, mimeData []byte) (models.SendResult, error) {
	if len(recipients) == 0 {
		return models.SendFailed, markDeliveryError(fmt.Errorf("no recipients"), false)
	}
	ctx = nonNilContext(ctx)
	sendCtx, cancelSend := context.WithTimeout(ctx, outgoingMessageTimeout)
	defer cancelSend()
	if err := sendCtx.Err(); err != nil {
		return models.SendFailed, markDeliveryError(err, true)
	}
	stopCancellation := context.AfterFunc(sendCtx, func() { _ = c.conn.Close() })
	defer stopCancellation()

	messageSize := int64(len(mimeData))
	if maxSize, ok := c.client.MaxMessageSize(); ok && maxSize > 0 && messageSize > int64(maxSize) {
		return models.SendFailed, markDeliveryError(fmt.Errorf("message is %d bytes, but the SMTP server accepts at most %d bytes", messageSize, maxSize), false)
	}

	requiresSMTPUTF8 := smtpEnvelopeRequiresUTF8(from, recipients)
	mailOptions := &smtp.MailOptions{
		Size: messageSize,
		UTF8: requiresSMTPUTF8,
	}
	if err := c.client.Mail(from, mailOptions); err != nil {
		mailErr := fmt.Errorf("mail from: %w", contextError(sendCtx, err))
		if requiresSMTPUTF8 && strings.Contains(strings.ToLower(mailErr.Error()), "server does not support smtputf8") {
			return models.SendFailed, markDeliveryError(fmt.Errorf("SMTPUTF8 is required for %s, but the server does not advertise SMTPUTF8", smtpUTF8RequirementDescription(from, recipients)), false)
		}
		return models.SendFailed, classifyPreAcceptanceError(mailErr)
	}

	for _, rcpt := range recipients {
		if err := c.client.Rcpt(rcpt, nil); err != nil {
			return models.SendFailed, classifyPreAcceptanceError(fmt.Errorf("rcpt to %s: %w", rcpt, contextError(sendCtx, err)))
		}
	}

	dataw, err := c.client.Data()
	if err != nil {
		return models.SendFailed, classifyPreAcceptanceError(fmt.Errorf("data: %w", contextError(sendCtx, err)))
	}
	if err := c.conn.SetDeadline(deadlineForContext(sendCtx, submissionTimeout)); err != nil {
		_ = c.conn.Close()
		return models.SendFailed, markDeliveryError(fmt.Errorf("set data deadline: %w", contextError(sendCtx, err)), true)
	}

	if _, err := dataw.Write(mimeData); err != nil {
		_ = c.conn.Close()
		return models.SendAmbiguous, fmt.Errorf("write data: %w", contextError(sendCtx, err))
	}

	if err := dataw.Close(); err != nil {
		var smtpErr *smtp.SMTPError
		if errors.As(err, &smtpErr) {
			return models.SendFailed, classifyPreAcceptanceError(fmt.Errorf("close data: %w", contextError(sendCtx, err)))
		}
		return models.SendAmbiguous, fmt.Errorf("close data: %w", contextError(sendCtx, err))
	}

	return models.SendSuccess, nil
}

func (c *Client) sendRawWithTiming(ctx context.Context, from string, recipients []string, mimeData []byte) (models.SendResult, error, DeliveryTiming) {
	startedAt := time.Now()
	result, err := c.SendRaw(ctx, from, recipients, mimeData)
	return result, err, DeliveryTiming{
		Data:                  time.Since(startedAt),
		Total:                 time.Since(startedAt),
		ConnectionEstablished: true,
		MessagesPerConnection: 1,
	}
}

func smtpEnvelopeRequiresUTF8(from string, recipients []string) bool {
	if smtpAddressRequiresUTF8(from) {
		return true
	}
	for _, recipient := range recipients {
		if smtpAddressRequiresUTF8(recipient) {
			return true
		}
	}
	return false
}

func smtpUTF8RequirementDescription(from string, recipients []string) string {
	if smtpAddressRequiresUTF8(from) {
		return "the sender address"
	}
	return "a recipient address"
}

func smtpAddressRequiresUTF8(address string) bool {
	for _, r := range address {
		if r > 127 {
			return true
		}
	}
	return false
}

func (c *Client) Close() error {
	return c.client.Close()
}

func TestConnection(ctx context.Context, cfg *models.AccountConfig, password string) error {
	c, err := NewClient(ctx, cfg, password)
	if err != nil {
		return err
	}
	return c.Close()
}

func SendMessage(ctx context.Context, cfg *models.AccountConfig, password string, msg *message.OutgoingMessage) (models.SendResult, error) {
	c, err := NewClient(ctx, cfg, password)
	if err != nil {
		return models.SendFailed, err
	}
	defer c.Close()

	return c.Send(ctx, msg)
}

func SendRawMessage(ctx context.Context, cfg *models.AccountConfig, password, from string, recipients []string, mimeData []byte) (models.SendResult, error) {
	result, err, _ := SendRawMessageWithTiming(ctx, cfg, password, from, recipients, mimeData)
	return result, err
}

// SendRawMessageWithTiming sends one message with a fresh SMTP connection and
// returns timing data for profiling. It preserves the existing connection
// lifetime and delivery classification; the timing is observational only.
func SendRawMessageWithTiming(ctx context.Context, cfg *models.AccountConfig, password, from string, recipients []string, mimeData []byte) (models.SendResult, error, DeliveryTiming) {
	startedAt := time.Now()
	c, err := NewClient(ctx, cfg, password)
	timing := DeliveryTiming{ConnectAuth: time.Since(startedAt)}
	if err != nil {
		timing.Total = time.Since(startedAt)
		return models.SendFailed, err, timing
	}
	defer c.Close()

	timing.ConnectionEstablished = true
	timing.MessagesPerConnection = 1
	result, err, sendTiming := c.sendRawWithTiming(ctx, from, recipients, mimeData)
	timing.Data = sendTiming.Data
	timing.Total = time.Since(startedAt)
	return result, err, timing
}
