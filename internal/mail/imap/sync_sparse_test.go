package imap

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type sparseSyncServerScript struct {
	rejectSearch bool
	mu           sync.Mutex
	commands     []string
}

func (s *sparseSyncServerScript) record(command string) {
	s.mu.Lock()
	s.commands = append(s.commands, command)
	s.mu.Unlock()
}

func (s *sparseSyncServerScript) commandLog() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.commands, "\n")
}

func newSparseSyncTestClient(t *testing.T, script *sparseSyncServerScript) *Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = serverConn.Close() })
	go serveSparseSyncScript(serverConn, script)
	raw := imapclient.New(clientConn, nil)
	if err := raw.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() error = %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return &Client{accountID: "acc", client: raw}
}

func serveSparseSyncScript(conn net.Conn, script *sparseSyncServerScript) {
	defer conn.Close()
	writer := bufio.NewWriter(conn)
	fmt.Fprint(writer, "* PREAUTH [CAPABILITY IMAP4rev1] ready\r\n")
	writer.Flush()

	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		script.record(line)
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return
		}
		tag := parts[0]
		upper := strings.ToUpper(line)
		switch {
		case strings.Contains(upper, " CAPABILITY"):
			fmt.Fprintf(writer, "* CAPABILITY IMAP4rev1\r\n%s OK capability\r\n", tag)
		case strings.Contains(upper, " SELECT "):
			fmt.Fprint(writer, "* FLAGS (\\Seen)\r\n* 2 EXISTS\r\n* OK [UIDVALIDITY 100] valid\r\n* OK [UIDNEXT 1000001] next\r\n")
			fmt.Fprintf(writer, "%s OK [READ-WRITE] selected\r\n", tag)
		case strings.Contains(upper, " UID SEARCH "):
			if script.rejectSearch {
				fmt.Fprintf(writer, "%s NO search unavailable\r\n", tag)
			} else {
				fmt.Fprintf(writer, "* SEARCH 2 1000000\r\n%s OK search\r\n", tag)
			}
		case strings.Contains(upper, " UID FETCH "):
			if strings.Contains(upper, "UID FETCH 2,1000000 ") {
				fmt.Fprint(writer, "* 1 FETCH (UID 2)\r\n* 2 FETCH (UID 1000000)\r\n")
				fmt.Fprintf(writer, "%s OK fetch\r\n", tag)
			} else {
				fmt.Fprintf(writer, "%s BAD unexpected UID set\r\n", tag)
			}
		case strings.Contains(upper, " UNSELECT"):
			fmt.Fprintf(writer, "%s OK unselected\r\n", tag)
		case strings.Contains(upper, " LOGOUT"):
			fmt.Fprintf(writer, "* BYE closing\r\n%s OK logout\r\n", tag)
			writer.Flush()
			return
		default:
			fmt.Fprintf(writer, "%s BAD unexpected command\r\n", tag)
		}
		writer.Flush()
	}
}

func TestSyncFolderFetchesOnlyUIDsThatExist(t *testing.T) {
	script := &sparseSyncServerScript{}
	client := newSparseSyncTestClient(t, script)

	var synced []storage.SyncMessage
	result, err := client.SyncFolder(context.Background(), "inbox", "INBOX", 2, func(messages []storage.SyncMessage) error {
		synced = append(synced, messages...)
		return nil
	})
	if err != nil {
		t.Fatalf("SyncFolder() error = %v", err)
	}
	if result.TotalFetched != 2 || result.NumMessages != 2 || result.HighestUID != 1000000 {
		t.Fatalf("SyncFolder() result = %#v", result)
	}
	if len(synced) != 2 || synced[0].RemoteUID != 2 || synced[1].RemoteUID != 1000000 {
		t.Fatalf("synced messages = %#v", synced)
	}
	commands := strings.ToUpper(script.commandLog())
	if !strings.Contains(commands, "UID SEARCH ALL") {
		t.Fatalf("commands = %q, want UID SEARCH ALL", commands)
	}
	if strings.Count(commands, " UID FETCH ") != 1 || !strings.Contains(commands, "UID FETCH 2,1000000 ") {
		t.Fatalf("commands = %q, want one FETCH containing only the real UID batch", commands)
	}
	if strings.Contains(commands, "UID FETCH 1:") {
		t.Fatalf("commands = %q, sparse mailbox must not scan numeric UID ranges", commands)
	}
}

func TestSyncFolderDoesNotRangeScanWhenUIDSearchFails(t *testing.T) {
	script := &sparseSyncServerScript{rejectSearch: true}
	client := newSparseSyncTestClient(t, script)

	if _, err := client.SyncFolder(context.Background(), "inbox", "INBOX", 500, func([]storage.SyncMessage) error { return nil }); err == nil {
		t.Fatal("SyncFolder() error = nil, want UID SEARCH failure")
	}
	commands := strings.ToUpper(script.commandLog())
	if strings.Contains(commands, " UID FETCH ") {
		t.Fatalf("commands = %q, UID SEARCH failure must not fall back to range scanning", commands)
	}
}
