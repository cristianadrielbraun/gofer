package imap

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"

	"github.com/cristianadrielbraun/gofer/internal/mail/oauth2sasl"
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
	accountID string
	config    *models.AccountConfig
	client    *imapclient.Client
	mu        sync.Mutex
	closed    bool
}

func NewClient(ctx context.Context, cfg *models.AccountConfig, password string) (*Client, error) {
	c, err := ConnectWithConfig(cfg, password, nil)
	if err != nil {
		return nil, err
	}
	return &Client{
		accountID: cfg.AccountID,
		config:    cfg,
		client:    c,
	}, nil
}

func ConnectWithConfig(cfg *models.AccountConfig, password string, options *imapclient.Options) (*imapclient.Client, error) {
	if options == nil {
		options = &imapclient.Options{}
	}
	if options.Dialer == nil {
		options.Dialer = &net.Dialer{Timeout: 15 * time.Second}
	}

	addr := fmt.Sprintf("%s:%d", cfg.IMAPHost, cfg.IMAPPort)

	var c *imapclient.Client
	var err error

	switch cfg.IMAPTLSMode {
	case "tls":
		c, err = imapclient.DialTLS(addr, options)
	case "starttls":
		c, err = imapclient.DialStartTLS(addr, options)
	default:
		c, err = imapclient.DialInsecure(addr, options)
	}

	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
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

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
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
