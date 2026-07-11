package imap

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	idleRefreshInterval         = 25 * time.Minute
	idleUnsupportedRetry        = 30 * time.Minute
	idleInitialReconnectBackoff = time.Second
	idleMaxReconnectBackoff     = 5 * time.Minute
)

type IdleWatcherStatus struct {
	Healthy    bool
	ReasonCode string
	Reason     string
	RetryAt    time.Time
}

type idleRunError struct {
	code      string
	reason    string
	permanent bool
	err       error
}

func (e *idleRunError) Error() string {
	if e.err == nil {
		return e.reason
	}
	return e.err.Error()
}

func (e *idleRunError) Unwrap() error { return e.err }

type IdleWatcher struct {
	config     *models.AccountConfig
	password   string
	remoteName string
	onNotify   func()
	onStatus   func(IdleWatcherStatus)
	refreshIn  time.Duration
	connect    func(*models.AccountConfig, string, *imapclient.Options) (*imapclient.Client, error)

	mu     sync.Mutex
	client *imapclient.Client
	closed bool
}

func NewIdleWatcher(cfg *models.AccountConfig, password, remoteName string, onNotify func(), onStatus func(IdleWatcherStatus)) *IdleWatcher {
	if onNotify == nil {
		onNotify = func() {}
	}
	if onStatus == nil {
		onStatus = func(IdleWatcherStatus) {}
	}
	return &IdleWatcher{
		config:     cfg,
		password:   password,
		remoteName: remoteName,
		onNotify:   onNotify,
		onStatus:   onStatus,
		refreshIn:  idleRefreshInterval,
		connect:    ConnectWithConfig,
	}
}

func (w *IdleWatcher) Run(ctx context.Context) {
	backoff := idleInitialReconnectBackoff

	for {
		if w.isClosed() {
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}

		becameHealthy := false
		err := w.run(ctx, func() {
			becameHealthy = true
			w.onStatus(IdleWatcherStatus{Healthy: true})
		})
		if ctx.Err() != nil || w.isClosed() {
			return
		}
		if becameHealthy {
			backoff = idleInitialReconnectBackoff
		}
		retryIn := backoff
		status := idleStatusForError(err)
		if runErr, ok := err.(*idleRunError); ok && runErr.permanent {
			retryIn = idleUnsupportedRetry
		} else {
			status.RetryAt = time.Now().Add(retryIn)
		}
		w.onStatus(status)
		log.Printf("idle %s: %v (reconnecting in %v)", w.remoteName, err, retryIn)

		w.closeConnection()

		timer := time.NewTimer(retryIn)
		select {
		case <-ctx.Done():
			stopIdleTimer(timer)
			return
		case <-timer.C:
		}

		if retryIn != idleUnsupportedRetry && backoff < idleMaxReconnectBackoff {
			backoff *= 2
			if backoff > idleMaxReconnectBackoff {
				backoff = idleMaxReconnectBackoff
			}
		}
	}
}

func idleStatusForError(err error) IdleWatcherStatus {
	status := IdleWatcherStatus{
		ReasonCode: "connection_error",
		Reason:     "the IDLE connection failed",
	}
	if runErr, ok := err.(*idleRunError); ok {
		status.ReasonCode = runErr.code
		status.Reason = runErr.reason
	}
	return status
}

func (w *IdleWatcher) run(ctx context.Context, onHealthy func()) error {
	notifyCh := make(chan struct{}, 1)
	notify := func() {
		select {
		case notifyCh <- struct{}{}:
		default:
		}
	}

	options := &imapclient.Options{
		UnilateralDataHandler: newIdleUnilateralDataHandler(notify),
	}

	connect := w.connect
	if connect == nil {
		connect = ConnectWithConfig
	}
	c, err := connect(w.config, w.password, options)
	if err != nil {
		return &idleRunError{code: "connection_error", reason: "the IDLE connection could not be established", err: fmt.Errorf("connect: %w", err)}
	}

	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		c.Close()
		return context.Canceled
	}
	w.client = c
	w.mu.Unlock()
	if !c.Caps().Has(imap.CapIdle) {
		return &idleRunError{code: "idle_unsupported", reason: "the server does not advertise IMAP IDLE", permanent: true}
	}

	_, err = c.Select(w.remoteName, nil).Wait()
	if err != nil {
		return &idleRunError{code: "select_error", reason: "the folder could not be selected for IDLE", err: fmt.Errorf("select %s: %w", w.remoteName, err)}
	}

	log.Printf("idle: watching %s", w.remoteName)

	for {
		idleCmd, err := c.Idle()
		if err != nil {
			return &idleRunError{code: "idle_error", reason: "the server rejected the IDLE command", err: fmt.Errorf("idle start: %w", err)}
		}
		onHealthy()
		refreshIn := w.refreshIn
		if refreshIn <= 0 {
			refreshIn = idleRefreshInterval
		}
		refresh := time.NewTimer(refreshIn)

		select {
		case <-ctx.Done():
			stopIdleTimer(refresh)
			idleCmd.Close()
			return ctx.Err()
		case <-notifyCh:
			stopIdleTimer(refresh)
			idleCmd.Close()
			log.Printf("idle: notification received for %s", w.remoteName)
			w.onNotify()
			select {
			case <-notifyCh:
			default:
			}
		case <-refresh.C:
			idleCmd.Close()
			log.Printf("idle: refreshed %s after %v", w.remoteName, refreshIn)
		case <-c.Closed():
			stopIdleTimer(refresh)
			idleCmd.Close()
			return &idleRunError{code: "connection_lost", reason: "the IDLE connection was closed", err: fmt.Errorf("connection closed")}
		}
	}
}

func stopIdleTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func newIdleUnilateralDataHandler(notify func()) *imapclient.UnilateralDataHandler {
	if notify == nil {
		notify = func() {}
	}
	return &imapclient.UnilateralDataHandler{
		Expunge: func(seqNum uint32) {
			notify()
		},
		Mailbox: func(data *imapclient.UnilateralDataMailbox) {
			if data != nil && data.NumMessages != nil {
				notify()
			}
		},
		Fetch: func(msg *imapclient.FetchMessageData) {
			if idleFetchHasFlagUpdate(msg) {
				notify()
			}
		},
	}
}

type idleFetchItemReader interface {
	Next() imapclient.FetchItemData
}

func idleFetchHasFlagUpdate(msg idleFetchItemReader) bool {
	if msg == nil {
		return false
	}
	hasFlags := false
	for {
		item := msg.Next()
		if item == nil {
			return hasFlags
		}
		if _, ok := item.(imapclient.FetchItemDataFlags); ok {
			hasFlags = true
		}
	}
}

func (w *IdleWatcher) closeConnection() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.client != nil {
		w.client.Close()
		w.client = nil
	}
}

func (w *IdleWatcher) isClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

func (w *IdleWatcher) Close() {
	w.mu.Lock()
	w.closed = true
	client := w.client
	w.client = nil
	w.mu.Unlock()
	if client != nil {
		client.Close()
	}
}
