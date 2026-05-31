package smtp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/emersion/go-sasl"
	smtp "github.com/emersion/go-smtp"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/mail/oauth2sasl"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

type Client struct {
	config *models.AccountConfig
	conn   net.Conn
	client *smtp.Client
}

func NewClient(ctx context.Context, cfg *models.AccountConfig, password string) (*Client, error) {
	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)
	dialer := &net.Dialer{Timeout: 15 * time.Second}

	var conn net.Conn
	var client *smtp.Client
	var err error

	switch cfg.SMTPTLSMode {
	case "tls":
		tlsConfig := &tls.Config{
			ServerName: cfg.SMTPHost,
			MinVersion: tls.VersionTLS12,
		}
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
		if err != nil {
			return nil, fmt.Errorf("connect to %s: %w", addr, err)
		}
		client = smtp.NewClient(conn)

	case "starttls":
		conn, err = dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("connect to %s: %w", addr, err)
		}
		tlsConfig := &tls.Config{
			ServerName: cfg.SMTPHost,
			MinVersion: tls.VersionTLS12,
		}
		client, err = smtp.NewClientStartTLS(conn, tlsConfig)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("starttls: %w", err)
		}

	default:
		conn, err = dialer.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("connect to %s: %w", addr, err)
		}
		client = smtp.NewClient(conn)
	}

	if err := client.Hello("localhost"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ehlo: %w", err)
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
		conn.Close()
		return nil, fmt.Errorf("authenticate: %w", err)
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

	if err := c.client.Mail(msg.FromEmail, nil); err != nil {
		return models.SendFailed, fmt.Errorf("mail from: %w", err)
	}

	for _, rcpt := range recipients {
		if err := c.client.Rcpt(rcpt, nil); err != nil {
			return models.SendFailed, fmt.Errorf("rcpt to %s: %w", rcpt, err)
		}
	}

	dataw, err := c.client.Data()
	if err != nil {
		return models.SendFailed, fmt.Errorf("data: %w", err)
	}

	mimeData, err := message.BuildMIMEMessage(msg)
	if err != nil {
		dataw.Close()
		return models.SendFailed, fmt.Errorf("build mime: %w", err)
	}

	if _, err := dataw.Write(mimeData); err != nil {
		dataw.Close()
		return models.SendAmbiguous, fmt.Errorf("write data: %w", err)
	}

	if err := dataw.Close(); err != nil {
		return models.SendAmbiguous, fmt.Errorf("close data: %w", err)
	}

	return models.SendSuccess, nil
}

func (c *Client) SendRaw(ctx context.Context, from string, recipients []string, mimeData []byte) (models.SendResult, error) {
	if err := c.client.Mail(from, nil); err != nil {
		return models.SendFailed, fmt.Errorf("mail from: %w", err)
	}

	for _, rcpt := range recipients {
		if err := c.client.Rcpt(rcpt, nil); err != nil {
			return models.SendFailed, fmt.Errorf("rcpt to %s: %w", rcpt, err)
		}
	}

	dataw, err := c.client.Data()
	if err != nil {
		return models.SendFailed, fmt.Errorf("data: %w", err)
	}

	if _, err := dataw.Write(mimeData); err != nil {
		dataw.Close()
		return models.SendAmbiguous, fmt.Errorf("write data: %w", err)
	}

	if err := dataw.Close(); err != nil {
		return models.SendAmbiguous, fmt.Errorf("close data: %w", err)
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
