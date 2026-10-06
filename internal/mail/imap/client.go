package imap

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"

	"github.com/cristianadrielbraun/gofer/internal/mail/oauth2sasl"
	mailtransport "github.com/cristianadrielbraun/gofer/internal/mail/transport"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

type FolderInfo struct {
	Name       string
	Delimiter  rune
	Attributes []imap.MailboxAttr
	Role       string
	Selectable bool
}

type Client struct {
	accountID   string
	config      *models.AccountConfig
	client      *imapclient.Client
	mu          sync.Mutex
	closed      bool
	stopContext func() bool
}

func NewClient(ctx context.Context, cfg *models.AccountConfig, password string) (*Client, error) {
	// Legacy callers cache sessions across request contexts.
	return NewContextClient(context.Background(), cfg, password)
}

// NewContextClient owns a session for this operation only. Cancellation closes
// the protocol connection directly, without waiting for the command mutex.
func NewContextClient(ctx context.Context, cfg *models.AccountConfig, password string) (*Client, error) {
	c, err := connectWithContext(ctx, cfg, password, nil)
	if err != nil {
		return nil, err
	}
	client := &Client{
		accountID: cfg.AccountID,
		config:    cfg,
		client:    c,
	}
	client.stopContext = context.AfterFunc(ctx, func() { _ = c.Close() })
	return client, nil
}

func ConnectWithConfig(cfg *models.AccountConfig, password string, options *imapclient.Options) (*imapclient.Client, error) {
	return connectWithContext(context.Background(), cfg, password, options)
}
func connectWithContext(ctx context.Context, cfg *models.AccountConfig, password string, options *imapclient.Options) (*imapclient.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tlsMode, err := mailtransport.RequireTLSModeWithPlaintext("IMAP", cfg.IMAPTLSMode, cfg.IMAPAllowPlaintext)
	if err != nil {
		return nil, err
	}
	if tlsMode == mailtransport.TLSModePlaintext && !strings.EqualFold(strings.TrimSpace(cfg.AuthMethod), "plain") {
		return nil, fmt.Errorf("IMAP OAuth authentication is not allowed over a plaintext connection")
	}
	options = secureClientOptions(cfg.IMAPHost, options)

	addr := net.JoinHostPort(cfg.IMAPHost, strconv.Itoa(cfg.IMAPPort))

	var conn net.Conn
	switch tlsMode {
	case mailtransport.TLSModeImplicit:
		tlsOptions := options.TLSConfig.Clone()
		if tlsOptions.NextProtos == nil {
			tlsOptions.NextProtos = []string{"imap"}
		}
		dialer := &tls.Dialer{NetDialer: options.Dialer, Config: tlsOptions}
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	case mailtransport.TLSModeStartTLS, mailtransport.TLSModePlaintext:
		conn, err = options.Dialer.DialContext(ctx, "tcp", addr)
	default:
		return nil, fmt.Errorf("unsupported IMAP TLS mode %q", tlsMode)
	}
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	// This callback also covers the greeting and STARTTLS handshake, before the
	// high-level client is available. Closing the underlying connection interrupts
	// commands without acquiring Client.mu.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	var c *imapclient.Client
	if tlsMode == mailtransport.TLSModeStartTLS {
		c, err = imapclient.NewStartTLS(conn, options)
	} else {
		c = imapclient.New(conn, options)
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := c.WaitGreeting(); err != nil {
		c.Close()
		return nil, fmt.Errorf("wait for greeting: %w", err)
	}

	switch cfg.AuthMethod {
	case "plain":
		saslClient := sasl.NewPlainClient("", cfg.Username, password)
		err = c.Authenticate(saslClient)
	case "oauth2":
		saslClient := oauth2sasl.NewClient(cfg.Username, password)
		err = c.Authenticate(saslClient)
	default:
		saslClient := sasl.NewPlainClient("", cfg.Username, password)
		err = c.Authenticate(saslClient)
	}

	if err != nil {
		c.Close()
		return nil, fmt.Errorf("authenticate: %w", err)
	}

	return c, nil
}

func secureClientOptions(host string, options *imapclient.Options) *imapclient.Options {
	if options == nil {
		options = &imapclient.Options{}
	}
	clientOptions := *options
	tlsConfig := &tls.Config{}
	if options.TLSConfig != nil {
		tlsConfig = options.TLSConfig.Clone()
	}
	tlsConfig.ServerName = host
	tlsConfig.InsecureSkipVerify = false
	if tlsConfig.MinVersion < tls.VersionTLS12 {
		tlsConfig.MinVersion = tls.VersionTLS12
	}
	clientOptions.TLSConfig = tlsConfig
	options = &clientOptions
	if options.Dialer == nil {
		options.Dialer = &net.Dialer{Timeout: 15 * time.Second}
	}
	return options
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.stopContext != nil {
		c.stopContext()
	}
	return c.client.Close()
}

func (c *Client) ListFolders(ctx context.Context) ([]FolderInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, fmt.Errorf("client is closed")
	}

	cmd := c.client.List("", "%", nil)
	defer cmd.Close()

	var folders []FolderInfo
	for {
		data := cmd.Next()
		if data == nil {
			break
		}
		if data.Mailbox == "" || !isExistingMailbox(data.Attrs) {
			continue
		}
		role := detectFolderRole(data.Mailbox, data.Attrs)
		folders = append(folders, FolderInfo{
			Name:       data.Mailbox,
			Delimiter:  data.Delim,
			Attributes: data.Attrs,
			Role:       role,
			Selectable: isSelectableMailbox(data.Attrs),
		})
	}

	if err := cmd.Close(); err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}

	// Also get subfolders by listing with * pattern
	subCmd := c.client.List("", "*", nil)
	defer subCmd.Close()

	seen := make(map[string]bool)
	for _, f := range folders {
		seen[strings.ToUpper(f.Name)] = true
	}

	for {
		data := subCmd.Next()
		if data == nil {
			break
		}
		if data.Mailbox == "" || seen[strings.ToUpper(data.Mailbox)] || !isExistingMailbox(data.Attrs) {
			continue
		}
		seen[strings.ToUpper(data.Mailbox)] = true
		role := detectFolderRole(data.Mailbox, data.Attrs)
		folders = append(folders, FolderInfo{
			Name:       data.Mailbox,
			Delimiter:  data.Delim,
			Attributes: data.Attrs,
			Role:       role,
			Selectable: isSelectableMailbox(data.Attrs),
		})
	}

	return folders, subCmd.Close()
}

func isSelectableMailbox(attrs []imap.MailboxAttr) bool {
	for _, attr := range attrs {
		switch attr {
		case imap.MailboxAttrNoSelect, imap.MailboxAttrNonExistent:
			return false
		}
	}
	return true
}

func isExistingMailbox(attrs []imap.MailboxAttr) bool {
	for _, attr := range attrs {
		if attr == imap.MailboxAttrNonExistent {
			return false
		}
	}
	return true
}

func TestConnection(ctx context.Context, cfg *models.AccountConfig, password string) error {
	c, err := ConnectWithConfig(cfg, password, nil)
	if err != nil {
		return err
	}
	c.Close()
	return nil
}
