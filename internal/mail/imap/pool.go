package imap

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// Pool keeps logged-in sessions between uses, so a poll or a message fetch
// skips connecting, TLS and authentication when the account was used
// recently. A session is reused only for the same account, server and
// credentials, only when it has no folder selected, and only after a NOOP if
// it sat idle for a while. Sessions idle longer than the idle timeout are
// logged out.
type Pool struct {
	idleTimeout time.Duration
	checkAfter  time.Duration
	maxIdle     int
	maxPerKey   int
	dial        func(context.Context, *models.AccountConfig, string) (*Client, error)

	mu      sync.Mutex
	idle    *list.List // of *pooledSession, most recently returned first
	byKey   map[string][]*list.Element
	closed  bool
	stopped chan struct{}
}

type pooledSession struct {
	key      string
	client   *Client
	returned time.Time
}

// DefaultPool serves the background sync and message fetches.
var DefaultPool = NewPool(PoolOptions{})

type PoolOptions struct {
	// IdleTimeout closes sessions unused for this long. Mail servers may
	// log out idle sessions after 30 minutes; most polls are every few.
	IdleTimeout time.Duration
	// CheckAfter sends a NOOP before reusing a session idle this long.
	CheckAfter time.Duration
	// MaxIdle bounds idle sessions across all accounts.
	MaxIdle int
}

func NewPool(options PoolOptions) *Pool {
	if options.IdleTimeout <= 0 {
		options.IdleTimeout = 10 * time.Minute
	}
	if options.CheckAfter <= 0 {
		options.CheckAfter = 30 * time.Second
	}
	if options.MaxIdle <= 0 {
		options.MaxIdle = 512
	}
	p := &Pool{
		idleTimeout: options.IdleTimeout,
		checkAfter:  options.CheckAfter,
		maxIdle:     options.MaxIdle,
		maxPerKey:   2,
		dial:        dialPooled,
		idle:        list.New(),
		byKey:       make(map[string][]*list.Element),
		stopped:     make(chan struct{}),
	}
	go p.reap()
	return p
}

// dialPooled connects within ctx but, unlike NewContextClient, does not tie
// the session's life to it; Get binds each use separately.
func dialPooled(ctx context.Context, cfg *models.AccountConfig, password string) (*Client, error) {
	c, err := connectWithContext(ctx, cfg, password, nil)
	if err != nil {
		return nil, err
	}
	return &Client{accountID: cfg.AccountID, config: cfg, client: c}, nil
}

// sessionKey identifies who a session is logged in as. A changed password
// or token yields a new key, so a session never outlives the credentials it
// was opened with in a reused form; nor one opened through another proxy, or
// directly, the proxy it now has to go through.
func sessionKey(cfg *models.AccountConfig, password string) string {
	secret := sha256.Sum256([]byte(password))
	return strings.Join([]string{
		cfg.AccountID, strings.ToLower(cfg.IMAPHost), strconv.Itoa(cfg.IMAPPort), cfg.IMAPTLSMode,
		cfg.Username, cfg.AuthMethod, strconv.FormatBool(cfg.IMAPAllowPlaintext),
		strconv.FormatBool(cfg.IMAPAllowPrivate), hex.EncodeToString(secret[:]), cfg.Proxy.Key(),
	}, "\x00")
}

// Get returns a logged-in session for the account. ctx bounds this use:
// cancelling it closes the connection, as with NewContextClient. Close the
// client when done; a healthy session then returns to the pool.
func (p *Pool) Get(ctx context.Context, cfg *models.AccountConfig, password string) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := sessionKey(cfg, password)
	for {
		session := p.take(key)
		if session == nil {
			break
		}
		if time.Since(session.returned) >= p.checkAfter {
			if err := session.client.client.Noop().Wait(); err != nil {
				_ = session.client.closeConnection()
				continue
			}
		}
		session.client.bind(ctx, p, key)
		return session.client, nil
	}
	client, err := p.dial(ctx, cfg, password)
	if err != nil {
		return nil, err
	}
	client.bind(ctx, p, key)
	return client, nil
}

func (p *Pool) take(key string) *pooledSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	elements := p.byKey[key]
	if len(elements) == 0 {
		return nil
	}
	element := elements[len(elements)-1]
	p.removeLocked(element)
	return element.Value.(*pooledSession)
}

func (p *Pool) removeLocked(element *list.Element) {
	session := element.Value.(*pooledSession)
	p.idle.Remove(element)
	elements := p.byKey[session.key]
	for i, candidate := range elements {
		if candidate == element {
			elements = append(elements[:i], elements[i+1:]...)
			break
		}
	}
	if len(elements) == 0 {
		delete(p.byKey, session.key)
	} else {
		p.byKey[session.key] = elements
	}
}

// put keeps a returned session, or closes it when it cannot be reused or
// the pool is full.
func (p *Pool) put(key string, client *Client) {
	if !client.reusable() {
		_ = client.closeConnection()
		return
	}
	var evicted []*Client
	p.mu.Lock()
	if p.closed || len(p.byKey[key]) >= p.maxPerKey {
		p.mu.Unlock()
		_ = client.closeConnection()
		return
	}
	element := p.idle.PushFront(&pooledSession{key: key, client: client, returned: time.Now()})
	p.byKey[key] = append(p.byKey[key], element)
	for p.idle.Len() > p.maxIdle {
		oldest := p.idle.Back()
		p.removeLocked(oldest)
		evicted = append(evicted, oldest.Value.(*pooledSession).client)
	}
	p.mu.Unlock()
	logoutInBackground(evicted)
}

// DropAccount logs out the account's idle sessions, for example when it is
// removed or its sync stops.
func (p *Pool) DropAccount(accountID string) {
	prefix := accountID + "\x00"
	var dropped []*Client
	p.mu.Lock()
	for element := p.idle.Front(); element != nil; {
		next := element.Next()
		session := element.Value.(*pooledSession)
		if strings.HasPrefix(session.key, prefix) {
			p.removeLocked(element)
			dropped = append(dropped, session.client)
		}
		element = next
	}
	p.mu.Unlock()
	logoutInBackground(dropped)
}

// DropAll logs out every idle session, for example after a transport
// security policy is revoked. Pooling continues for new sessions.
func (p *Pool) DropAll() {
	p.mu.Lock()
	var all []*Client
	for element := p.idle.Front(); element != nil; element = element.Next() {
		all = append(all, element.Value.(*pooledSession).client)
	}
	p.idle.Init()
	p.byKey = make(map[string][]*list.Element)
	p.mu.Unlock()
	logoutInBackground(all)
}

// Close logs out every idle session and stops pooling.
func (p *Pool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.stopped)
	var all []*Client
	for element := p.idle.Front(); element != nil; element = element.Next() {
		all = append(all, element.Value.(*pooledSession).client)
	}
	p.idle.Init()
	p.byKey = make(map[string][]*list.Element)
	p.mu.Unlock()
	for _, client := range all {
		_ = client.logout()
	}
}

func (p *Pool) reap() {
	ticker := time.NewTicker(min(p.idleTimeout/2, time.Minute))
	defer ticker.Stop()
	for {
		select {
		case <-p.stopped:
			return
		case <-ticker.C:
		}
		var expired []*Client
		p.mu.Lock()
		for element := p.idle.Back(); element != nil; {
			previous := element.Prev()
			session := element.Value.(*pooledSession)
			if time.Since(session.returned) < p.idleTimeout {
				break
			}
			p.removeLocked(element)
			expired = append(expired, session.client)
			element = previous
		}
		p.mu.Unlock()
		logoutInBackground(expired)
	}
}

// IdleSessions reports how many sessions are waiting for reuse.
func (p *Pool) IdleSessions() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.idle.Len()
}

// bind attaches a pooled session to one use: ctx cancellation closes it, and
// Close hands it back to the pool.
func (c *Client) bind(ctx context.Context, pool *Pool, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopContext != nil {
		c.stopContext()
	}
	raw := c.client
	c.stopContext = context.AfterFunc(ctx, func() { _ = raw.Close() })
	c.pool, c.poolKey = pool, key
}

// reusable reports whether the session can serve another use: still logged
// in, no folder selected, and not cut off by a cancelled use.
func (c *Client) reusable() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.interrupted {
		return false
	}
	if c.client.State() == imap.ConnStateSelected && c.client.Caps().Has(imap.CapUnselect) {
		// Operations send UNSELECT without waiting for the reply. Replies
		// arrive in order, so a NOOP's reply means the UNSELECT completed.
		if err := c.client.Noop().Wait(); err != nil {
			return false
		}
	}
	return c.client.State() == imap.ConnStateAuthenticated
}

func (c *Client) closeConnection() error {
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

// logoutInBackground ends sessions already removed from the pool without
// making the caller wait for slow servers.
func logoutInBackground(clients []*Client) {
	for _, client := range clients {
		go func() { _ = client.logout() }()
	}
}

// logout ends an idle session politely, bounded so a silent server cannot
// hold the caller.
func (c *Client) logout() error {
	done := make(chan struct{})
	go func() {
		_ = c.client.Logout().Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	return c.closeConnection()
}
