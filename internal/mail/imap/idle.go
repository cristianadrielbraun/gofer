package imap

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/emersion/go-imap/v2/imapclient"
)

type IdleWatcher struct {
	config     *models.AccountConfig
	password   string
	remoteName string
	onNotify   func()

	mu     sync.Mutex
	client *imapclient.Client
}

func NewIdleWatcher(cfg *models.AccountConfig, password, remoteName string, onNotify func()) *IdleWatcher {
	return &IdleWatcher{
		config:     cfg,
		password:   password,
		remoteName: remoteName,
		onNotify:   onNotify,
	}
}

func (w *IdleWatcher) Run(ctx context.Context) {
	backoff := time.Second
	maxBackoff := 5 * time.Minute

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := w.run(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("idle %s: %v (reconnecting in %v)", w.remoteName, err, backoff)
		}

		w.closeConnection()

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func (w *IdleWatcher) run(ctx context.Context) error {
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

	c, err := ConnectWithConfig(w.config, w.password, options)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	w.mu.Lock()
	w.client = c
	w.mu.Unlock()

	_, err = c.Select(w.remoteName, nil).Wait()
	if err != nil {
		return fmt.Errorf("select %s: %w", w.remoteName, err)
	}

	log.Printf("idle: watching %s", w.remoteName)

	for {
		idleCmd, err := c.Idle()
		if err != nil {
			return fmt.Errorf("idle start: %w", err)
		}

		select {
		case <-ctx.Done():
			idleCmd.Close()
			return ctx.Err()
		case <-notifyCh:
			idleCmd.Close()
			log.Printf("idle: notification received for %s", w.remoteName)
			w.onNotify()
			select {
			case <-notifyCh:
			default:
			}
		case <-c.Closed():
			idleCmd.Close()
			return fmt.Errorf("connection closed")
		}
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

func (w *IdleWatcher) Close() {
	w.closeConnection()
}
