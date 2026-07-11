package smtp

import (
	"context"
	"crypto/tls"
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
		return nil, err
	}
	if tlsMode == mailtransport.TLSModePlaintext && !strings.EqualFold(strings.TrimSpace(cfg.AuthMethod), "plain") {
		return nil, fmt.Errorf("SMTP OAuth authentication is not allowed over a plaintext connection")
	}
	ctx = nonNilContext(ctx)
	setupCtx, cancelSetup := context.WithTimeout(ctx, setupTimeout)
	defer cancelSetup()
	if err := setupCtx.Err(); err != nil {
		return nil, err
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
			return nil, fmt.Errorf("connect to %s: %w", addr, contextError(setupCtx, err))
		}
		client = smtp.NewClient(conn)

	case mailtransport.TLSModeStartTLS:
		conn, err = dialer.DialContext(setupCtx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("connect to %s: %w", addr, contextError(setupCtx, err))
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
			return nil, fmt.Errorf("starttls: %w", contextError(setupCtx, err))
		}

	case mailtransport.TLSModePlaintext:
		conn, err = dialer.DialContext(setupCtx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("connect to %s: %w", addr, contextError(setupCtx, err))
		}
		client = smtp.NewClient(conn)

	default:
		return nil, fmt.Errorf("unsupported SMTP TLS mode %q", tlsMode)
	}
	stopCancellation := context.AfterFunc(setupCtx, func() { _ = conn.Close() })
	defer stopCancellation()
	configureTimeouts(client)

	if err := client.Hello("localhost"); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ehlo: %w", contextError(setupCtx, err))
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
		return nil, fmt.Errorf("authenticate: %w", contextError(setupCtx, err))
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
		return models.SendFailed, fmt.Errorf("no recipients")
	}
	mimeData, err := message.BuildMIMEMessage(msg)
	if err != nil {
		return models.SendFailed, fmt.Errorf("build mime: %w", err)
	}
	return c.SendRaw(ctx, msg.FromEmail, recipients, mimeData)
}

func (c *Client) SendRaw(ctx context.Context, from string, recipients []string, mimeData []byte) (models.SendResult, error) {
	if len(recipients) == 0 {
		return models.SendFailed, fmt.Errorf("no recipients")
	}
	ctx = nonNilContext(ctx)
	sendCtx, cancelSend := context.WithTimeout(ctx, outgoingMessageTimeout)
	defer cancelSend()
	if err := sendCtx.Err(); err != nil {
		return models.SendFailed, err
	}
	stopCancellation := context.AfterFunc(sendCtx, func() { _ = c.conn.Close() })
	defer stopCancellation()

	if err := c.client.Mail(from, nil); err != nil {
		return models.SendFailed, fmt.Errorf("mail from: %w", contextError(sendCtx, err))
	}

	for _, rcpt := range recipients {
		if err := c.client.Rcpt(rcpt, nil); err != nil {
			return models.SendFailed, fmt.Errorf("rcpt to %s: %w", rcpt, contextError(sendCtx, err))
		}
	}

	dataw, err := c.client.Data()
	if err != nil {
		return models.SendFailed, fmt.Errorf("data: %w", contextError(sendCtx, err))
	}
	if err := c.conn.SetDeadline(deadlineForContext(sendCtx, submissionTimeout)); err != nil {
		_ = c.conn.Close()
		return models.SendFailed, fmt.Errorf("set data deadline: %w", contextError(sendCtx, err))
	}

	if _, err := dataw.Write(mimeData); err != nil {
		_ = c.conn.Close()
		return models.SendAmbiguous, fmt.Errorf("write data: %w", contextError(sendCtx, err))
	}

	if err := dataw.Close(); err != nil {
		return models.SendAmbiguous, fmt.Errorf("close data: %w", contextError(sendCtx, err))
	}

	return models.SendSuccess, nil
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
