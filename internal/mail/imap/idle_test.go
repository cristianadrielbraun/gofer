package imap

import (
	"context"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

type fakeIdleFetchMessage struct {
	items []imapclient.FetchItemData
	next  int
}

func (m *fakeIdleFetchMessage) Next() imapclient.FetchItemData {
	if m.next >= len(m.items) {
		return nil
	}
	item := m.items[m.next]
	m.next++
	return item
}

func TestIdleFetchHasFlagUpdateDetectsAndDrainsFlags(t *testing.T) {
	msg := &fakeIdleFetchMessage{items: []imapclient.FetchItemData{
		imapclient.FetchItemDataUID{UID: 42},
		imapclient.FetchItemDataFlags{Flags: []goimap.Flag{"Work"}},
	}}

	if !idleFetchHasFlagUpdate(msg) {
		t.Fatal("idleFetchHasFlagUpdate() = false, want true")
	}
	if msg.next != len(msg.items) {
		t.Fatalf("message consumed %d items, want %d", msg.next, len(msg.items))
	}
}

func TestIdleFetchHasFlagUpdateDrainsWithoutFlags(t *testing.T) {
	msg := &fakeIdleFetchMessage{items: []imapclient.FetchItemData{
		imapclient.FetchItemDataUID{UID: 42},
	}}

	if idleFetchHasFlagUpdate(msg) {
		t.Fatal("idleFetchHasFlagUpdate() = true, want false")
	}
	if msg.next != len(msg.items) {
		t.Fatalf("message consumed %d items, want %d", msg.next, len(msg.items))
	}
}

func TestIdleUnilateralDataHandlerNotifiesForMailboxAndExpunge(t *testing.T) {
	notifications := 0
	handler := newIdleUnilateralDataHandler(func() {
		notifications++
	})

	handler.Mailbox(&imapclient.UnilateralDataMailbox{})
	if notifications != 0 {
		t.Fatalf("notifications after unchanged mailbox = %d, want 0", notifications)
	}

	numMessages := uint32(12)
	handler.Mailbox(&imapclient.UnilateralDataMailbox{NumMessages: &numMessages})
	handler.Expunge(7)
	if notifications != 2 {
		t.Fatalf("notifications = %d, want 2", notifications)
	}
}

func TestIdleWatcherCloseIsTerminalBeforeRun(t *testing.T) {
	watcher := NewIdleWatcher(nil, "", "INBOX", nil)
	watcher.Close()
	done := make(chan struct{})
	go func() {
		watcher.Run(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closed watcher tried to connect or reconnect")
	}
}
