package smtp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	smtp "github.com/emersion/go-smtp"
	"github.com/emersion/go-sasl"

	"gofer.email/internal/models"
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

	switch cfg.AuthMethod {
	case "plain":
		saslClient := sasl.NewPlainClient("", cfg.Username, password)
		err = client.Auth(saslClient)
	case "oauth2":
		saslClient := sasl.NewOAuthBearerClient(&sasl.OAuthBearerOptions{
			Username: cfg.Username,
			Token:    password,
		})
		err = client.Auth(saslClient)
	default:
		saslClient := sasl.NewPlainClient("", cfg.Username, password)
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
