package imap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func newIdleTestClient(t *testing.T, capabilities string) *imapclient.Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	go serveIdleTestServer(serverConn, capabilities)
	raw := imapclient.New(clientConn, nil)
	if err := raw.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() error = %v", err)
	}
	t.Cleanup(func() {
		_ = raw.Close()
		_ = serverConn.Close()
	})
	return raw
}

func serveIdleTestServer(conn net.Conn, capabilities string) {
	defer conn.Close()
	writer := bufio.NewWriter(conn)
	fmt.Fprintf(writer, "* PREAUTH [CAPABILITY %s] ready\r\n", capabilities)
	writer.Flush()
	reader := bufio.NewReader(conn)
	idleTag := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if strings.EqualFold(line, "DONE") && idleTag != "" {
			fmt.Fprintf(writer, "%s OK IDLE complete\r\n", idleTag)
			writer.Flush()
			idleTag = ""
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return
		}
		tag := parts[0]
		switch strings.ToUpper(parts[1]) {
		case "CAPABILITY":
			fmt.Fprintf(writer, "* CAPABILITY %s\r\n%s OK capability\r\n", capabilities, tag)
		case "SELECT":
			fmt.Fprintf(writer, "* FLAGS (\\Seen)\r\n* 0 EXISTS\r\n* OK [UIDVALIDITY 1] valid\r\n* OK [UIDNEXT 1] next\r\n%s OK [READ-WRITE] selected\r\n", tag)
		case "IDLE":
			idleTag = tag
			fmt.Fprint(writer, "+ idling\r\n")
		case "LOGOUT":
			fmt.Fprintf(writer, "* BYE closing\r\n%s OK logout\r\n", tag)
			writer.Flush()
			return
		default:
			fmt.Fprintf(writer, "%s BAD unexpected\r\n", tag)
		}
		writer.Flush()
	}
}

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
	watcher := NewIdleWatcher(nil, "", "INBOX", nil, nil)
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

func TestIdleWatcherRejectsServerWithoutIDLECapability(t *testing.T) {
	raw := newIdleTestClient(t, "IMAP4rev1")
	watcher := NewIdleWatcher(nil, "", "INBOX", nil, nil)
	watcher.connect = func(*models.AccountConfig, string, *imapclient.Options) (*imapclient.Client, error) {
		return raw, nil
	}

	err := watcher.run(context.Background(), func() {
		t.Fatal("watcher reported healthy without IDLE support")
	})
	runErr, ok := err.(*idleRunError)
	if !ok || runErr.code != "idle_unsupported" || !runErr.permanent || runErr.reason != "the server does not advertise IMAP IDLE" {
		t.Fatalf("run() error = %#v, want permanent unsupported status", err)
	}
}

func TestIdleWatcherReportsTemporaryFailureAndRetryTime(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got IdleWatcherStatus
	watcher := NewIdleWatcher(nil, "", "INBOX", nil, func(status IdleWatcherStatus) {
		got = status
		cancel()
	})
	watcher.connect = func(*models.AccountConfig, string, *imapclient.Options) (*imapclient.Client, error) {
		return nil, errors.New("temporary network failure")
	}
	started := time.Now()
	watcher.Run(ctx)
	if got.Healthy || got.ReasonCode != "connection_error" || got.Reason != "the IDLE connection could not be established" || !got.RetryAt.After(started) {
		t.Fatalf("temporary failure status = %#v", got)
	}
}

func TestIdleWatcherRefreshesLongRunningSession(t *testing.T) {
	raw := newIdleTestClient(t, "IMAP4rev1 IDLE")
	watcher := NewIdleWatcher(nil, "", "INBOX", nil, nil)
	watcher.refreshIn = 10 * time.Millisecond
	watcher.connect = func(*models.AccountConfig, string, *imapclient.Options) (*imapclient.Client, error) {
		return raw, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	healthy := make(chan struct{}, 3)
	done := make(chan error, 1)
	go func() {
		done <- watcher.run(ctx, func() { healthy <- struct{}{} })
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-healthy:
		case <-time.After(time.Second):
			t.Fatalf("IDLE session became healthy %d times, want refresh", i+1)
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("run() error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("refreshed IDLE watcher did not stop")
	}
}
