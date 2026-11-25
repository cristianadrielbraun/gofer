package imap

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// quotedHeaderMessage has quotes in its subject and in a sender's name, and
// one broken address among its recipients.
const quotedHeaderMessage = "Date: Fri, 2 Oct 2026 12:30:00 +0000\r\n" +
	"Subject: Lunch \"on\" Friday?\r\n" +
	"From: \"Alex \\\"Al\\\" Doe\" <alex@example.com>\r\n" +
	"To: Sam Lee <sam@example.com>, broken@@example.com, Kim <kim@example.com>\r\n" +
	"Message-ID: <lunch-friday@example.com>\r\n" +
	"In-Reply-To: <plans@example.com>\r\n" +
	"\r\n"

// serveUnescapedEnvelope plays a server that writes header text into ENVELOPE
// between quotes without escaping the quotes inside it, as some servers do.
// Asked for header lines instead, it sends them as a literal.
func serveUnescapedEnvelope(conn net.Conn) {
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
		parts := strings.Fields(line)
		if len(parts) < 2 {
			return
		}
		tag, upper := parts[0], strings.ToUpper(line)
		switch {
		case strings.Contains(upper, " SELECT "):
			fmt.Fprintf(writer, "* 1 EXISTS\r\n* OK [UIDVALIDITY 7] valid\r\n* OK [UIDNEXT 6] next\r\n%s OK [READ-WRITE] selected\r\n", tag)
		case strings.Contains(upper, " UID SEARCH"):
			fmt.Fprintf(writer, "* SEARCH 5\r\n%s OK search\r\n", tag)
		case strings.Contains(upper, " UID FETCH ") && strings.Contains(upper, "ENVELOPE"):
			fmt.Fprintf(writer, "* 1 FETCH (UID 5 ENVELOPE (\"Fri, 2 Oct 2026 12:30:00 +0000\" \"Lunch \"on\" Friday?\" ((\"Alex \"Al\" Doe\" NIL \"alex\" \"example.com\")) NIL NIL ((\"Sam Lee\" NIL \"sam\" \"example.com\")) NIL NIL \"<plans@example.com>\" \"<lunch-friday@example.com>\"))\r\n%s OK fetch\r\n", tag)
		case strings.Contains(upper, " UID FETCH ") && strings.Contains(upper, "HEADER.FIELDS"):
			fmt.Fprintf(writer, "* 1 FETCH (UID 5 FLAGS (\\Seen) BODY[HEADER.FIELDS (Subject)] {%d}\r\n%s)\r\n%s OK fetch\r\n", len(quotedHeaderMessage), quotedHeaderMessage, tag)
		case strings.Contains(upper, " LOGOUT"):
			fmt.Fprintf(writer, "* BYE closing\r\n%s OK logout\r\n", tag)
			writer.Flush()
			return
		default:
			fmt.Fprintf(writer, "%s OK done\r\n", tag)
		}
		writer.Flush()
	}
}

func newUnescapedEnvelopeClient(t *testing.T) *Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = serverConn.Close() })
	go serveUnescapedEnvelope(serverConn)
	raw := imapclient.New(clientConn, nil)
	if err := raw.WaitGreeting(); err != nil {
		t.Fatalf("WaitGreeting() error = %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return &Client{accountID: "acc", client: raw}
}

func checkQuotedHeaderMessage(t *testing.T, synced []storage.SyncMessage) {
	t.Helper()
	if len(synced) != 1 {
		t.Fatalf("synced %d messages, want 1", len(synced))
	}
	msg := synced[0]
	if msg.RemoteUID != 5 || !msg.IsRead {
		t.Fatalf("UID %d, read %v", msg.RemoteUID, msg.IsRead)
	}
	if msg.Subject != `Lunch "on" Friday?` {
		t.Fatalf("Subject = %q", msg.Subject)
	}
	if msg.FromName != `Alex "Al" Doe` || msg.FromEmail != "alex@example.com" {
		t.Fatalf("From = %q <%s>", msg.FromName, msg.FromEmail)
	}
	if msg.MessageID != "lunch-friday@example.com" || msg.InReplyTo != "plans@example.com" {
		t.Fatalf("Message-ID %q, In-Reply-To %q", msg.MessageID, msg.InReplyTo)
	}
	want := []storage.Recipient{{Name: "Sam Lee", Email: "sam@example.com"}, {Name: "Kim", Email: "kim@example.com"}}
	if fmt.Sprint(msg.ToRecipients) != fmt.Sprint(want) {
		t.Fatalf("To = %v, want %v", msg.ToRecipients, want)
	}
	if !msg.DateSent.Equal(time.Date(2026, 10, 2, 12, 30, 0, 0, time.UTC)) || msg.DateSentFallback {
		t.Fatalf("DateSent = %v (fallback %v)", msg.DateSent, msg.DateSentFallback)
	}
}

// A message whose headers hold quotes syncs with its subject and people,
// even from a server that would break ENVELOPE for it.
func TestSyncFolderReadsHeadersAServerBreaksInEnvelope(t *testing.T) {
	client := newUnescapedEnvelopeClient(t)
	var synced []storage.SyncMessage
	_, err := client.SyncFolder(context.Background(), "inbox", "INBOX", FolderSyncOptions{ChunkSize: 10}, func(messages []storage.SyncMessage) error {
		synced = append(synced, messages...)
		return nil
	})
	if err != nil {
		t.Fatalf("SyncFolder() error = %v", err)
	}
	checkQuotedHeaderMessage(t, synced)
}

func TestSyncFolderIncrementalReadsHeadersAServerBreaksInEnvelope(t *testing.T) {
	client := newUnescapedEnvelopeClient(t)
	var synced []storage.SyncMessage
	_, err := client.SyncFolderIncremental(context.Background(), "inbox", "INBOX", 4, 7, func(messages []storage.SyncMessage) error {
		synced = append(synced, messages...)
		return nil
	})
	if err != nil {
		t.Fatalf("SyncFolderIncremental() error = %v", err)
	}
	checkQuotedHeaderMessage(t, synced)
}
