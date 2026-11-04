package imap

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// ErrLoginRateLimited means the server answered a login with [LIMIT] and
// Gofer is holding off further logins to it for a while.
var ErrLoginRateLimited = errors.New("IMAP server is limiting logins")

const (
	loginCooldownFirst = 5 * time.Minute
	loginCooldownMax   = time.Hour
)

// LoginCooldownError reports when the next login may be tried.
type LoginCooldownError struct {
	Until time.Time
}

// LoginCooldownMessage is the text of a LoginCooldownError. It names no
// time: the server's clock and zone may not be the reader's.
const LoginCooldownMessage = "the server is limiting logins for this account; Gofer waits a few minutes before trying again"

func (e *LoginCooldownError) Error() string { return LoginCooldownMessage }

func (e *LoginCooldownError) Unwrap() error { return ErrLoginRateLimited }

// loginCooldowns remembers mailboxes whose server refused a login with
// [LIMIT]. Every attempt during the limit counts against it on servers like
// Yahoo, so Gofer stops trying until the cooldown ends. Each limit in a row
// doubles the wait; a successful login forgets it.
type loginCooldowns struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]loginCooldown
}

type loginCooldown struct {
	until time.Time
	wait  time.Duration
}

var defaultLoginCooldowns = &loginCooldowns{now: time.Now, entries: make(map[string]loginCooldown)}

// loginCooldownKey names the mailbox the server limits: the same login on
// the same server, whatever the password.
func loginCooldownKey(cfg *models.AccountConfig) string {
	return strings.ToLower(strings.TrimSpace(cfg.IMAPHost)) + "\x00" + strconv.Itoa(cfg.IMAPPort) + "\x00" + strings.ToLower(strings.TrimSpace(cfg.Username))
}

// check returns an error while the mailbox is cooling down.
func (l *loginCooldowns) check(cfg *models.AccountConfig) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[loginCooldownKey(cfg)]
	if !ok || !l.now().Before(entry.until) {
		return nil
	}
	return &LoginCooldownError{Until: entry.until}
}

// record notes the outcome of a login. A [LIMIT] refusal starts or extends
// the cooldown and returns the error to report; any other outcome returns
// err unchanged, and success clears the mailbox's history.
func (l *loginCooldowns) record(cfg *models.AccountConfig, err error) error {
	key := loginCooldownKey(cfg)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil {
		delete(l.entries, key)
		return nil
	}
	if !isLoginLimit(err) {
		return err
	}
	entry := l.entries[key]
	entry.wait = min(max(entry.wait*2, loginCooldownFirst), loginCooldownMax)
	entry.until = l.now().Add(entry.wait)
	l.entries[key] = entry
	return fmt.Errorf("%w (%w)", &LoginCooldownError{Until: entry.until}, err)
}

func isLoginLimit(err error) bool {
	var statusErr *imap.Error
	return errors.As(err, &statusErr) && statusErr.Code == imap.ResponseCodeLimit
}
