package mail

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mail/jmap"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

const (
	// jmapPushPing is how often the server is asked to ping an idle stream.
	jmapPushPing = time.Minute
	// jmapPushUnavailableRetry is how long to wait when the server offers no
	// push or refuses the login; periodic sync covers the account meanwhile.
	jmapPushUnavailableRetry = 30 * time.Minute
	jmapPushMaxBackoff       = 5 * time.Minute
)

// jmapPushInitialBackoff is the first wait before reconnecting, and
// jmapPushStableAfter how long a connection must stay open before the wait
// starts over from it, so a server that accepts the connection and closes
// it at once isn't reconnected, and synced, every second. Tests shorten
// them.
var (
	jmapPushInitialBackoff = time.Second
	jmapPushStableAfter    = time.Minute
)

// JMAPPushWatcher keeps a JMAP account's push connection open and calls
// onNotify when the server reports changed mail or mailboxes, and
// onContacts, when set, for changed contacts or address books. One
// connection covers the whole account. It reconnects when the connection
// drops and reports its state through onStatus, as the IMAP IDLE watcher
// does.
type JMAPPushWatcher struct {
	config     *models.AccountConfig
	password   string
	onNotify   func()
	onContacts func()
	onStatus   func(imap.IdleWatcherStatus)

	mu     sync.Mutex
	stop   context.CancelFunc
	closed bool
}

func NewJMAPPushWatcher(cfg *models.AccountConfig, password string, onNotify func(), onStatus func(imap.IdleWatcherStatus)) *JMAPPushWatcher {
	if onNotify == nil {
		onNotify = func() {}
	}
	if onStatus == nil {
		onStatus = func(imap.IdleWatcherStatus) {}
	}
	return &JMAPPushWatcher{config: cfg, password: password, onNotify: onNotify, onStatus: onStatus}
}

// WithContacts makes the connection also report contact changes, when the
// server offers JMAP contacts to this login, by calling onContacts. It is
// called before Run.
func (w *JMAPPushWatcher) WithContacts(onContacts func()) *JMAPPushWatcher {
	w.onContacts = onContacts
	return w
}

// jmapPushError is why a push connection ended, as shown in the status.
type jmapPushError struct {
	code   string
	reason string
	// unavailable means retrying soon would not help.
	unavailable bool
	err         error
}

func (e *jmapPushError) Error() string { return e.err.Error() }
func (e *jmapPushError) Unwrap() error { return e.err }

// Run holds the push connection until ctx ends or Close is called,
// reconnecting after failures with a growing delay.
func (w *JMAPPushWatcher) Run(ctx context.Context) {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.stop = stop
	w.mu.Unlock()

	backoff := jmapPushInitialBackoff
	reconnecting := false
	for ctx.Err() == nil {
		var opened time.Time
		err := w.watch(ctx, func() {
			opened = time.Now()
			w.onStatus(imap.IdleWatcherStatus{Healthy: true})
			if reconnecting {
				// Changes made while disconnected were never pushed.
				w.onNotify()
				if w.onContacts != nil {
					w.onContacts()
				}
			}
		})
		reconnecting = true
		if ctx.Err() != nil {
			return
		}
		if !opened.IsZero() && time.Since(opened) >= jmapPushStableAfter {
			backoff = jmapPushInitialBackoff
		}
		status := imap.IdleWatcherStatus{ReasonCode: "connection_error", Reason: "the push connection failed"}
		retryIn := backoff
		var pushErr *jmapPushError
		if errors.As(err, &pushErr) {
			status.ReasonCode, status.Reason = pushErr.code, pushErr.reason
			if pushErr.unavailable {
				retryIn = jmapPushUnavailableRetry
			}
		}
		status.RetryAt = time.Now().Add(retryIn)
		w.onStatus(status)
		log.Printf("jmap push %s: %v (reconnecting in %v)", w.config.AccountID, err, retryIn)

		timer := time.NewTimer(retryIn)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if retryIn != jmapPushUnavailableRetry {
			backoff = min(backoff*2, jmapPushMaxBackoff)
		}
	}
}

// watch opens the session and holds one push connection until it ends.
func (w *JMAPPushWatcher) watch(ctx context.Context, onOpen func()) error {
	client, session, account, err := openJMAPMail(ctx, w.config, w.password)
	if err != nil {
		if jmapLoginRefused(err) {
			return &jmapPushError{code: "auth_error", reason: "the server refused the login", unavailable: true, err: err}
		}
		return &jmapPushError{code: "connection_error", reason: "the JMAP session could not be loaded", err: err}
	}
	types := []string{"Email", "Mailbox"}
	// Contacts may live in another JMAP account of the same login. A server
	// that doesn't offer them to this login isn't asked for their changes.
	contactsAccount := ""
	if w.onContacts != nil {
		contactsAccount = session.PrimaryAccount(jmap.CapabilityContacts)
	}
	if contactsAccount != "" {
		types = append(types, "ContactCard", "AddressBook")
	}
	err = client.WatchEvents(ctx, session, types, jmapPushPing, onOpen, func(change jmap.StateChange) {
		mail := change.Changed[account]
		if mail["Email"] != "" || mail["Mailbox"] != "" {
			w.onNotify()
		}
		if contactsAccount != "" {
			contacts := change.Changed[contactsAccount]
			if contacts["ContactCard"] != "" || contacts["AddressBook"] != "" {
				w.onContacts()
			}
		}
	})
	switch {
	case errors.Is(err, jmap.ErrNoPush):
		return &jmapPushError{code: "push_unsupported", reason: "the server does not offer JMAP push", unavailable: true, err: err}
	case jmapLoginRefused(err):
		return &jmapPushError{code: "auth_error", reason: "the server refused the push connection", unavailable: true, err: err}
	case errors.Is(err, jmap.ErrEventStreamSilent):
		return &jmapPushError{code: "connection_lost", reason: "the push connection went silent", err: err}
	}
	return &jmapPushError{code: "connection_lost", reason: "the push connection was closed", err: err}
}

func jmapLoginRefused(err error) bool {
	var httpErr *jmap.HTTPError
	return errors.As(err, &httpErr) && (httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden)
}

// Close ends Run and its connection.
func (w *JMAPPushWatcher) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.stop != nil {
		w.stop()
	}
}

// JMAPContactsChange refreshes the contacts of an account whose JMAP server
// pushed a contact or address book change. The push says only that
// something changed; the usual contact sync finds what.
type JMAPContactsChange func(ctx context.Context, accountID string)

// OnJMAPContactsChange sets what refreshes the contacts of the JMAP accounts
// whose server pushes a contact change. Without it, push covers mail only.
// It is called before Start.
func (o *SyncOrchestrator) OnJMAPContactsChange(change JMAPContactsChange) {
	o.jmapContactsChange = change
}

// OnJMAPContactsChange is the same for managed mode. It is called before
// Start.
func (s *UserIMAP) OnJMAPContactsChange(change JMAPContactsChange) {
	s.jmapContactsChange = change
}
