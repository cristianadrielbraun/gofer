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
)

type condStoreServerScript struct {
	advertise     bool
	rejectSelect  bool
	dropOnSelect  bool
	rejectChanged bool
	highestModSeq uint64
	mu            sync.Mutex
	commands      []string
}

func (s *condStoreServerScript) record(command string) {
	s.mu.Lock()
	s.commands = append(s.commands, command)
	s.mu.Unlock()
}

func (s *condStoreServerScript) commandLog() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.commands, "\n")
}

func newCondStoreTestClient(t *testing.T, script *condStoreServerScript) *Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = serverConn.Close() })
	go serveCondStoreScript(serverConn, script)
	raw := imapclient.New(clientConn, nil)
	if err := raw.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() error = %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return &Client{client: raw}
}

func serveCondStoreScript(conn net.Conn, script *condStoreServerScript) {
	defer conn.Close()
	writer := bufio.NewWriter(conn)
	caps := "IMAP4rev1"
	if script.advertise {
		caps += " CONDSTORE"
	}
	fmt.Fprintf(writer, "* PREAUTH [CAPABILITY %s] ready\r\n", caps)
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
			fmt.Fprintf(writer, "* CAPABILITY %s\r\n%s OK capability\r\n", caps, tag)
		case strings.Contains(upper, " SELECT "):
			condStore := strings.Contains(upper, "CONDSTORE")
			if condStore && script.dropOnSelect {
				return
			}
			if condStore && script.rejectSelect {
				fmt.Fprintf(writer, "%s BAD CONDSTORE unavailable\r\n", tag)
				break
			}
			fmt.Fprint(writer, "* FLAGS (\\Seen \\Flagged Work)\r\n* 2 EXISTS\r\n* OK [UIDVALIDITY 100] valid\r\n* OK [UIDNEXT 3] next\r\n")
			if condStore && script.highestModSeq > 0 {
				fmt.Fprintf(writer, "* OK [HIGHESTMODSEQ %d] modseq\r\n", script.highestModSeq)
			}
			fmt.Fprintf(writer, "%s OK [READ-WRITE] selected\r\n", tag)
		case strings.Contains(upper, " UID FETCH "):
			changedSince := strings.Contains(upper, "CHANGEDSINCE")
			if changedSince && script.rejectChanged {
				fmt.Fprintf(writer, "%s BAD CHANGEDSINCE unavailable\r\n", tag)
				break
			}
			if changedSince {
				fmt.Fprintf(writer, "* 2 FETCH (UID 2 FLAGS (\\Seen Work) MODSEQ (%d))\r\n", script.highestModSeq)
			} else {
				fmt.Fprint(writer, "* 1 FETCH (UID 1 FLAGS ())\r\n* 2 FETCH (UID 2 FLAGS (\\Seen Work))\r\n")
			}
			fmt.Fprintf(writer, "%s OK fetch\r\n", tag)
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

func TestFetchFlagChangesUsesCondStoreCheckpoint(t *testing.T) {
	script := &condStoreServerScript{advertise: true, highestModSeq: 20}
	client := newCondStoreTestClient(t, script)

	result, err := client.FetchFlagChanges(context.Background(), "INBOX", []uint32{1, 2}, 100, 10)
	if err != nil {
		t.Fatalf("FetchFlagChanges() error = %v", err)
	}
	if !result.CheckpointValid || !result.UsedCondStore || result.HighestModSeq != 20 || result.UIDValidity != 100 {
		t.Fatalf("FetchFlagChanges() result = %#v", result)
	}
	if len(result.Updates) != 1 || result.Updates[0].UID != 2 || !result.Updates[0].IsRead || len(result.Updates[0].Labels) != 1 || result.Updates[0].Labels[0].Name != "Work" {
		t.Fatalf("FetchFlagChanges() updates = %#v", result.Updates)
	}
	commands := strings.ToUpper(script.commandLog())
	if !strings.Contains(commands, "SELECT INBOX (CONDSTORE)") || !strings.Contains(commands, "CHANGEDSINCE 10") {
		t.Fatalf("commands = %q, want CONDSTORE and CHANGEDSINCE", commands)
	}
}

func TestFetchFlagChangesBootstrapsCondStoreWithFullFlags(t *testing.T) {
	script := &condStoreServerScript{advertise: true, highestModSeq: 20}
	client := newCondStoreTestClient(t, script)

	result, err := client.FetchFlagChanges(context.Background(), "INBOX", []uint32{1, 2}, 100, 0)
	if err != nil {
		t.Fatalf("FetchFlagChanges() error = %v", err)
	}
	if !result.CheckpointValid || result.UsedCondStore || result.HighestModSeq != 20 || len(result.Updates) != 2 {
		t.Fatalf("FetchFlagChanges() bootstrap result = %#v", result)
	}
	commands := strings.ToUpper(script.commandLog())
	if !strings.Contains(commands, "SELECT INBOX (CONDSTORE)") || strings.Contains(commands, "CHANGEDSINCE") {
		t.Fatalf("commands = %q, want full bootstrap under CONDSTORE", commands)
	}
}

func TestFetchFlagChangesRebuildsCheckpointWhenServerModSeqMovesBackward(t *testing.T) {
	script := &condStoreServerScript{advertise: true, highestModSeq: 20}
	client := newCondStoreTestClient(t, script)

	result, err := client.FetchFlagChanges(context.Background(), "INBOX", []uint32{1, 2}, 100, 30)
	if err != nil {
		t.Fatalf("FetchFlagChanges() error = %v", err)
	}
	if !result.CheckpointValid || result.UsedCondStore || result.HighestModSeq != 20 || len(result.Updates) != 2 {
		t.Fatalf("FetchFlagChanges() rebuilt result = %#v", result)
	}
	if commands := strings.ToUpper(script.commandLog()); strings.Contains(commands, "CHANGEDSINCE") {
		t.Fatalf("commands = %q, stale future checkpoint must force full flags", commands)
	}
}

func TestFetchFlagChangesDoesNotCheckpointWithoutHighestModSeq(t *testing.T) {
	script := &condStoreServerScript{advertise: true}
	client := newCondStoreTestClient(t, script)

	result, err := client.FetchFlagChanges(context.Background(), "INBOX", []uint32{1, 2}, 100, 10)
	if err != nil {
		t.Fatalf("FetchFlagChanges() error = %v", err)
	}
	if result.CheckpointValid || result.UsedCondStore || result.HighestModSeq != 0 || len(result.Updates) != 2 {
		t.Fatalf("FetchFlagChanges() missing modseq fallback = %#v", result)
	}
	if commands := strings.ToUpper(script.commandLog()); strings.Contains(commands, "CHANGEDSINCE") {
		t.Fatalf("commands = %q, missing HIGHESTMODSEQ must force full flags", commands)
	}
}

func TestFetchFlagChangesStopsBeforeFetchOnUIDValidityChange(t *testing.T) {
	script := &condStoreServerScript{advertise: true, highestModSeq: 20}
	client := newCondStoreTestClient(t, script)

	result, err := client.FetchFlagChanges(context.Background(), "INBOX", []uint32{1, 2}, 99, 10)
	if err != nil {
		t.Fatalf("FetchFlagChanges() error = %v", err)
	}
	if !result.UIDValidityChanged || result.UIDValidity != 100 || result.CheckpointValid || len(result.Updates) != 0 {
		t.Fatalf("FetchFlagChanges() UID reset result = %#v", result)
	}
	if commands := strings.ToUpper(script.commandLog()); strings.Contains(commands, "UID FETCH") {
		t.Fatalf("commands = %q, UIDVALIDITY reset must stop before fetching flags", commands)
	}
}

func TestFetchFlagChangesFallsBackWhenCondStoreIsUnavailable(t *testing.T) {
	script := &condStoreServerScript{}
	client := newCondStoreTestClient(t, script)

	result, err := client.FetchFlagChanges(context.Background(), "INBOX", []uint32{1, 2}, 100, 10)
	if err != nil {
		t.Fatalf("FetchFlagChanges() error = %v", err)
	}
	if result.CheckpointValid || result.UsedCondStore || result.HighestModSeq != 0 || len(result.Updates) != 2 {
		t.Fatalf("FetchFlagChanges() fallback result = %#v", result)
	}
	commands := strings.ToUpper(script.commandLog())
	if strings.Contains(commands, "CONDSTORE") || strings.Contains(commands, "CHANGEDSINCE") {
		t.Fatalf("unsupported server received CONDSTORE command: %q", commands)
	}
}

func TestFetchFlagChangesFallsBackWhenAdvertisedCondStoreIsRejected(t *testing.T) {
	script := &condStoreServerScript{advertise: true, rejectSelect: true, highestModSeq: 20}
	client := newCondStoreTestClient(t, script)

	result, err := client.FetchFlagChanges(context.Background(), "INBOX", []uint32{1, 2}, 100, 10)
	if err != nil {
		t.Fatalf("FetchFlagChanges() error = %v", err)
	}
	if result.CheckpointValid || result.UsedCondStore || len(result.Updates) != 2 {
		t.Fatalf("FetchFlagChanges() rejected SELECT fallback = %#v", result)
	}
	commands := strings.ToUpper(script.commandLog())
	if strings.Count(commands, "SELECT INBOX") != 2 || strings.Contains(commands, "CHANGEDSINCE") {
		t.Fatalf("commands = %q, want optimized SELECT followed by plain fallback", commands)
	}
}

func TestFetchFlagChangesDoesNotHideCondStoreConnectionFailure(t *testing.T) {
	script := &condStoreServerScript{advertise: true, dropOnSelect: true, highestModSeq: 20}
	client := newCondStoreTestClient(t, script)

	if _, err := client.FetchFlagChanges(context.Background(), "INBOX", []uint32{1, 2}, 100, 10); err == nil {
		t.Fatal("FetchFlagChanges() error = nil, want connection failure")
	}
	if commands := strings.ToUpper(script.commandLog()); strings.Count(commands, "SELECT INBOX") != 1 {
		t.Fatalf("commands = %q, connection failure must not be retried as unsupported CONDSTORE", commands)
	}
}

func TestFetchFlagChangesFallsBackWhenChangedSinceIsRejected(t *testing.T) {
	script := &condStoreServerScript{advertise: true, rejectChanged: true, highestModSeq: 20}
	client := newCondStoreTestClient(t, script)

	result, err := client.FetchFlagChanges(context.Background(), "INBOX", []uint32{1, 2}, 100, 10)
	if err != nil {
		t.Fatalf("FetchFlagChanges() error = %v", err)
	}
	if result.CheckpointValid || result.UsedCondStore || result.HighestModSeq != 0 || len(result.Updates) != 2 {
		t.Fatalf("FetchFlagChanges() rejected CHANGEDSINCE fallback = %#v", result)
	}
	commands := strings.ToUpper(script.commandLog())
	if strings.Count(commands, "UID FETCH") != 2 || !strings.Contains(commands, "CHANGEDSINCE 10") {
		t.Fatalf("commands = %q, want changed fetch followed by full fallback", commands)
	}
}
