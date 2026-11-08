// Package providerquota keeps Gofer's calls to a mail provider inside the
// quota the provider gives each mailbox.
//
// Gmail allows each mailbox 6,000 quota units a minute per app, and every
// call costs units: reading a message 5, sending 100. All of Gofer's Gmail
// work for a mailbox draws on that one allowance: syncing, opening messages,
// label changes, size lookups. So every Gmail request goes through one budget
// per mailbox. Background work leaves a reserve for what the reader is doing
// right now, and when Gmail still refuses a request for going over quota, the
// budget halves its pace and recovers slowly once requests succeed again.
package providerquota

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/netguard"
	"github.com/cristianadrielbraun/gofer/internal/retry"
)

const (
	// Gofer aims for 80% of Gmail's 6,000 units a minute, leaving room for
	// other apps signed in to the same Google project.
	gmailUnitsPerMinute    = 4800
	gmailMinUnitsPerMinute = 600
	// Recovery after a refusal: the pace grows by this much each minute.
	gmailRecoveryPerMinute = 600
	// At most this many units go out at once after a quiet period.
	gmailBurstUnits = 500
	// Background work waits while fewer units than this are left, so opening
	// a message never waits behind a sync.
	gmailInteractiveReserve = 150
	// After a refusal, background work stops at least this long.
	gmailRefusalPause = 30 * time.Second
	// Access tokens last an hour; their link to a mailbox is kept a while longer.
	tokenBindingLifetime = 2 * time.Hour
)

// GmailHost is the host whose requests are budgeted. Tests point it at their
// local server.
var GmailHost = "gmail.googleapis.com"

type backgroundKey struct{}

// Background marks work that nobody is waiting for, such as syncing. It
// yields to requests the reader is waiting on.
func Background(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

func isBackground(ctx context.Context) bool {
	background, _ := ctx.Value(backgroundKey{}).(bool)
	return background
}

type mailboxBudget struct {
	mu          sync.Mutex
	rate        float64 // units a minute
	level       float64 // units available now
	updated     time.Time
	pausedUntil time.Time
	lastRefusal time.Time
}

type tokenBinding struct {
	mailbox string
	expires time.Time
}

type budget struct {
	mu        sync.Mutex
	mailboxes map[string]*mailboxBudget
	tokens    map[[32]byte]tokenBinding
	now       func() time.Time
	sleep     func(context.Context, time.Duration) error
}

func newBudget() *budget {
	return &budget{
		mailboxes: map[string]*mailboxBudget{},
		tokens:    map[[32]byte]tokenBinding{},
		now:       time.Now,
		sleep:     sleepContext,
	}
}

var gmail = newBudget()

// BindGmailToken records which mailbox an access token belongs to, so all of
// the mailbox's requests share one budget across token refreshes. A token
// that was never bound gets a budget of its own.
func BindGmailToken(token, mailbox string) {
	gmail.bind(token, mailbox)
}

func (b *budget) bind(token, mailbox string) {
	if token == "" || mailbox == "" {
		return
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, binding := range b.tokens {
		if now.After(binding.expires) {
			delete(b.tokens, key)
		}
	}
	b.tokens[sha256.Sum256([]byte(token))] = tokenBinding{mailbox: mailbox, expires: now.Add(tokenBindingLifetime)}
}

func (b *budget) mailbox(token string) *mailboxBudget {
	sum := sha256.Sum256([]byte(token))
	b.mu.Lock()
	defer b.mu.Unlock()
	name := "token:" + string(sum[:])
	if binding, ok := b.tokens[sum]; ok {
		name = binding.mailbox
	}
	now := b.now()
	m := b.mailboxes[name]
	if m == nil {
		// A budget unused this long has fully recovered, so forgetting it
		// changes nothing; it keeps budgets of expired tokens from piling up.
		for other, unused := range b.mailboxes {
			unused.mu.Lock()
			idle := now.Sub(unused.updated) > tokenBindingLifetime
			unused.mu.Unlock()
			if idle {
				delete(b.mailboxes, other)
			}
		}
		m = &mailboxBudget{rate: gmailUnitsPerMinute, level: gmailBurstUnits, updated: now}
		b.mailboxes[name] = m
	}
	return m
}

// refill adds the units earned since the last update. Caller holds m.mu.
func (m *mailboxBudget) refill(now time.Time) {
	elapsed := now.Sub(m.updated)
	if elapsed <= 0 {
		return
	}
	m.updated = now
	if m.rate < gmailUnitsPerMinute && now.Sub(m.lastRefusal) >= time.Minute {
		m.rate = min(gmailUnitsPerMinute, m.rate+gmailRecoveryPerMinute*elapsed.Minutes())
	}
	m.level = min(gmailBurstUnits, m.level+m.rate*elapsed.Minutes())
}

// wait blocks until the request's units are available, then takes them.
func (b *budget) wait(ctx context.Context, m *mailboxBudget, cost float64, background bool) error {
	for {
		now := b.now()
		m.mu.Lock()
		m.refill(now)
		need := cost
		var delay time.Duration
		if background {
			need += gmailInteractiveReserve
			if now.Before(m.pausedUntil) {
				delay = m.pausedUntil.Sub(now)
			}
		}
		if delay == 0 {
			if m.level >= need || (!background && m.level >= min(cost, gmailBurstUnits)) {
				m.level -= cost
				m.mu.Unlock()
				return nil
			}
			delay = time.Duration((need - m.level) / m.rate * float64(time.Minute))
		}
		m.mu.Unlock()
		if err := b.sleep(ctx, max(delay, 10*time.Millisecond)); err != nil {
			return err
		}
	}
}

// refused slows the mailbox down after Gmail refused a request for going over
// its quota.
func (b *budget) refused(m *mailboxBudget, retryAt time.Time) {
	now := b.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refill(now)
	m.rate = max(gmailMinUnitsPerMinute, m.rate/2)
	m.level = 0
	m.lastRefusal = now
	m.pausedUntil = now.Add(gmailRefusalPause)
	if retryAt.After(m.pausedUntil) {
		m.pausedUntil = retryAt
	}
}

// gmailCost returns what a request costs in Gmail quota units.
func gmailCost(method, path string) float64 {
	i := strings.Index(path, "/users/")
	if i < 0 {
		return 5
	}
	parts := strings.Split(strings.Trim(path[i+len("/users/"):], "/"), "/")
	if len(parts) < 2 {
		return 5
	}
	resource, rest := parts[1], parts[2:]
	switch resource {
	case "profile":
		return 1
	case "history":
		return 2
	case "labels":
		if method == http.MethodGet {
			return 1
		}
		return 5
	case "drafts":
		switch {
		case len(rest) > 0 && rest[len(rest)-1] == "send":
			return 100
		case method == http.MethodPost:
			return 10
		case method == http.MethodPut:
			return 15
		case method == http.MethodDelete:
			return 10
		}
		return 5
	case "messages":
		if len(rest) > 0 {
			switch rest[len(rest)-1] {
			case "send":
				return 100
			case "batchModify", "batchDelete":
				return 50
			case "import", "insert":
				return 25
			}
		}
		if method == http.MethodDelete {
			return 10
		}
		return 5
	}
	return 5
}

// gmailRefusal reports whether a response says the mailbox went over its
// quota, and when Gmail asks to try again.
func gmailRefusal(resp *http.Response, now time.Time) (bool, time.Time) {
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusForbidden {
		return false, time.Time{}
	}
	retryAt, _ := retry.ParseRetryAfter(resp.Header.Get("Retry-After"), now)
	if resp.StatusCode == http.StatusTooManyRequests {
		return true, retryAt
	}
	// A 403 is also how Gmail reports quota, but it may be a permission
	// problem. The error body tells them apart; it is put back for the caller.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return false, time.Time{}
	}
	text := strings.ToLower(string(body))
	return strings.Contains(text, "ratelimitexceeded") || strings.Contains(text, "rate_limit_exceeded") ||
		strings.Contains(text, "quota exceeded"), retryAt
}

// Do sends a request over netguard.APITransport, so it takes its account's
// proxy, first waiting until a Gmail request fits its mailbox's quota.
// Requests to other hosts go straight out.
func Do(req *http.Request) (*http.Response, error) {
	return gmail.do(req, apiClient.Do)
}

var apiClient = netguard.APIClient(0)

func (b *budget) do(req *http.Request, send func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	if req.URL == nil || req.URL.Host != GmailHost {
		return send(req)
	}
	token := strings.TrimSpace(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
	m := b.mailbox(token)
	if err := b.wait(req.Context(), m, gmailCost(req.Method, req.URL.Path), isBackground(req.Context())); err != nil {
		if req.Body != nil {
			req.Body.Close()
		}
		return nil, err
	}
	resp, err := send(req)
	if err != nil {
		return resp, err
	}
	if refused, retryAt := gmailRefusal(resp, b.now()); refused {
		b.refused(m, retryAt)
	}
	return resp, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
