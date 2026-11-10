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

	"github.com/cristianadrielbraun/gofer/internal/buildinfo"
	"github.com/cristianadrielbraun/gofer/internal/mail/oauth2sasl"
	mailtransport "github.com/cristianadrielbraun/gofer/internal/mail/transport"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/netguard"
)

type FolderInfo struct {
	Name       string
	Delimiter  rune
	Attributes []imap.MailboxAttr
	Role       string
	Selectable bool
	// Status comes with the folder list on servers with LIST-STATUS, so a
	// poll learns which folders changed without a command per folder.
	Status *FolderStatus
}

type Client struct {
	accountID   string
	config      *models.AccountConfig
	client      *imapclient.Client
	mu          sync.Mutex
	closed      bool
	stopContext func() bool
	pool        *Pool
	poolKey     string
	interrupted bool
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
	if err := defaultLoginCooldowns.check(cfg); err != nil {
		return nil, err
	}

	addr := net.JoinHostPort(cfg.IMAPHost, strconv.Itoa(cfg.IMAPPort))

	switch tlsMode {
	case mailtransport.TLSModeImplicit, mailtransport.TLSModeStartTLS, mailtransport.TLSModePlaintext:
	default:
		return nil, fmt.Errorf("unsupported IMAP TLS mode %q", tlsMode)
	}
	conn, err := netguard.Dial(netguard.WithProxy(ctx, cfg.Proxy), options.Dialer, "imap", cfg.IMAPHost, cfg.IMAPPort, cfg.IMAPAllowPrivate)
	if err == nil && tlsMode == mailtransport.TLSModeImplicit {
		tlsOptions := options.TLSConfig.Clone()
		if tlsOptions.NextProtos == nil {
			tlsOptions.NextProtos = []string{"imap"}
		}
		conn, err = tlsHandshake(ctx, conn, tlsOptions, options.Dialer.Timeout)
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
		// STARTTLS can't have the filter: go-imap starts its TLS beneath it.
		c = imapclient.New(newStatusFixConn(conn), options)
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := c.WaitGreeting(); err != nil {
		c.Close()
		return nil, fmt.Errorf("wait for greeting: %w", err)
	}

	err = defaultLoginCooldowns.record(cfg, authenticate(c, cfg, password))
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("authenticate: %w", err)
	}

	// Some servers, such as 163.com, refuse SELECT with "Unsafe Login" until
	// the client identifies itself (RFC 2971). Their answer doesn't matter.
	if c.Caps().Has(imap.CapID) {
		_, _ = c.ID(&imap.IDData{Name: "Gofer", Version: buildinfo.Version}).Wait()
	}

	return c, nil
}

func authenticate(c *imapclient.Client, cfg *models.AccountConfig, password string) error {
	if cfg.AuthMethod == "oauth2" {
		return c.Authenticate(oauth2sasl.NewClient(cfg.Username, password))
	}
	// IMAP4rev1 servers don't have to offer AUTHENTICATE PLAIN: 163.com
	// advertises only AUTH=XOAUTH2 and 139.com no mechanism at all. Both
	// accept LOGIN.
	if caps := c.Caps(); caps != nil && !caps.Has(imap.AuthCap(sasl.Plain)) && !caps.Has(imap.CapLoginDisabled) {
		return c.Login(cfg.Username, password).Wait()
	}
	return c.Authenticate(sasl.NewPlainClient("", cfg.Username, password))
}

// tlsHandshake starts TLS on a connection opened by netguard. The timeout
// bounds the handshake as it did when the TLS dialer also opened the
// connection, since some callers pass a context without a deadline.
func tlsHandshake(ctx context.Context, conn net.Conn, config *tls.Config, timeout time.Duration) (net.Conn, error) {
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
	tlsConn := tls.Client(conn, config)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return tlsConn, nil
}

// imapCipherSuites are Go's defaults followed by RSA key exchange with
// AES-GCM. Go no longer offers RSA key exchange as it has no forward secrecy,
// but imap.139.com accepts nothing else. A server only gets it by choosing it
// over every default suite, and TLS 1.3 never uses this list.
var imapCipherSuites = func() []uint16 {
	var suites []uint16
	for _, suite := range tls.CipherSuites() {
		suites = append(suites, suite.ID)
	}
	return append(suites, tls.TLS_RSA_WITH_AES_128_GCM_SHA256, tls.TLS_RSA_WITH_AES_256_GCM_SHA384)
}()

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
	if tlsConfig.CipherSuites == nil {
		tlsConfig.CipherSuites = imapCipherSuites
	}
	clientOptions.TLSConfig = tlsConfig
	options = &clientOptions
	if options.Dialer == nil {
		options.Dialer = &net.Dialer{Timeout: 15 * time.Second}
	}
	return options
}

// Close ends this use of the client. A session from a Pool goes back to the
// pool for reuse when it is still healthy; the caller must not use it again.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	if pool, key := c.pool, c.poolKey; pool != nil {
		c.pool, c.poolKey = nil, ""
		// A use whose context already fired has closed the connection.
		if c.stopContext != nil && !c.stopContext() {
			c.interrupted = true
		}
		c.stopContext = nil
		c.mu.Unlock()
		pool.put(key, c)
		return nil
	}
	defer c.mu.Unlock()
	c.closed = true
	if c.stopContext != nil {
		c.stopContext()
	}
	return c.client.Close()
}

// ConnectionLost reports whether the server connection has ended, so callers
// can reconnect instead of failing every later command on a dead session.
func (c *Client) ConnectionLost() bool {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	return closed || c.client.State() == imap.ConnStateLogout
}

// unselect leaves the selected mailbox when the server supports UNSELECT.
// Servers without it reject the command, and some disconnect after repeated
// rejections; the next SELECT replaces the selection anyway.
func (c *Client) unselect() {
	if c.closed {
		return
	}
	if c.client.Caps().Has(imap.CapUnselect) {
		c.client.Unselect()
	}
}

func (c *Client) ListFolders(ctx context.Context) ([]FolderInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, fmt.Errorf("client is closed")
	}

	namespaces := c.personalNamespaces()

	cmd := c.client.List("", "%", listOptions(c, false))
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
		role := detectFolderRole(data.Mailbox, data.Delim, data.Attrs, namespaces)
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

	// Also get subfolders by listing with * pattern. Where the server can,
	// this listing also returns every folder's status.
	subCmd := c.client.List("", "*", listOptions(c, true))
	defer subCmd.Close()

	seen := make(map[string]bool)
	index := make(map[string]int)
	for i, f := range folders {
		seen[strings.ToUpper(f.Name)] = true
		index[strings.ToUpper(f.Name)] = i
	}

	for {
		data := subCmd.Next()
		if data == nil {
			break
		}
		if data.Mailbox != "" && data.Status != nil {
			if i, ok := index[strings.ToUpper(data.Mailbox)]; ok {
				if status, ok := folderStatusFromData(data.Status); ok {
					folders[i].Status = &status
				}
			}
		}
		if data.Mailbox == "" || seen[strings.ToUpper(data.Mailbox)] || !isExistingMailbox(data.Attrs) {
			continue
		}
		var status *FolderStatus
		if data.Status != nil {
			if parsed, ok := folderStatusFromData(data.Status); ok {
				status = &parsed
			}
		}
		seen[strings.ToUpper(data.Mailbox)] = true
		role := detectFolderRole(data.Mailbox, data.Delim, data.Attrs, namespaces)
		folders = append(folders, FolderInfo{
			Name:       data.Mailbox,
			Delimiter:  data.Delim,
			Attributes: data.Attrs,
			Role:       role,
			Selectable: isSelectableMailbox(data.Attrs),
			Status:     status,
		})
	}

	return folders, subCmd.Close()
}

// personalNamespaces returns the prefixes of the user's own folders, or nil
// when the server cannot say. The caller holds c.mu.
func (c *Client) personalNamespaces() []string {
	if !c.client.Caps().Has(imap.CapNamespace) {
		return nil
	}
	data, err := c.client.Namespace().Wait()
	if err != nil {
		return nil
	}
	prefixes := make([]string, 0, len(data.Personal))
	for _, ns := range data.Personal {
		prefixes = append(prefixes, ns.Prefix)
	}
	return prefixes
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
	c, err := NewContextClient(ctx, cfg, password)
	if err != nil {
		return err
	}
	c.Close()
	return nil
}
