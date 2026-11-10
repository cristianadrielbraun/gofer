package imap

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
)

// readerConn is a connection that only reads, from r.
type readerConn struct {
	net.Conn
	r io.Reader
}

func (c readerConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// filtered reads everything the server sent through the filter, reading
// size bytes at a time.
func filtered(t *testing.T, server io.Reader, size int) string {
	t.Helper()
	conn := newStatusFixConn(readerConn{r: server})
	var out bytes.Buffer
	buf := make([]byte, size)
	for {
		n, err := conn.Read(buf)
		out.Write(buf[:n])
		if errors.Is(err, io.EOF) {
			return out.String()
		}
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
	}
}

// filteredEveryWay reads the server's bytes through the filter in large and
// one-byte reads, with the server sending them all at once and one at a
// time, and checks that every way gives the same result.
func filteredEveryWay(t *testing.T, server string) string {
	t.Helper()
	want := filtered(t, strings.NewReader(server), 4096)
	for _, way := range []struct {
		name   string
		server io.Reader
		size   int
	}{
		{"one-byte reads", strings.NewReader(server), 1},
		{"server sends one byte at a time", iotest.OneByteReader(strings.NewReader(server)), 4096},
		{"both", iotest.OneByteReader(strings.NewReader(server)), 1},
		{"server sends half at a time", iotest.HalfReader(strings.NewReader(server)), 7},
	} {
		if got := filtered(t, way.server, way.size); got != want {
			t.Fatalf("%s gave\n%q\nwant\n%q", way.name, got, want)
		}
	}
	return want
}

func TestStatusFixDropsTheSpaceBeforeTheParen(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"* STATUS \"INBOX\" (MESSAGES 11 UIDVALIDITY 1 UNSEEN 7 )\r\n", "* STATUS \"INBOX\" (MESSAGES 11 UIDVALIDITY 1 UNSEEN 7)\r\n"},
		{"* STATUS INBOX (MESSAGES 1  )\r\n", "* STATUS INBOX (MESSAGES 1)\r\n"},
		{"* status \"Sent\" (UIDNEXT 4 )\r\n", "* status \"Sent\" (UIDNEXT 4)\r\n"},
		{"* STATUS \"Empty\" ( )\r\n", "* STATUS \"Empty\" ()\r\n"},
		{"* STATUS INBOX (MESSAGES 1 )\n", "* STATUS INBOX (MESSAGES 1)\n"},
	} {
		if got := filteredEveryWay(t, tt.in); got != tt.want {
			t.Errorf("filtered %q = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Anything but that one mistake passes through as the server sent it.
func TestStatusFixLeavesOtherResponsesAlone(t *testing.T) {
	server := strings.Join([]string{
		"* OK [CAPABILITY IMAP4rev1 LITERAL+ ID] ready",
		"* STATUS \"INBOX\" (MESSAGES 11 UIDVALIDITY 1 UNSEEN 7)",
		"* STATUS \"A )\" (MESSAGES 1)",
		"* LIST (\\HasNoChildren) \"/\" \"Notes )\"",
		"* 3 FETCH (FLAGS (\\Seen ) UID 9)",
		"* OK [ALERT] see * STATUS x (y )",
		"a1 OK STATUS completed )",
		"",
	}, "\r\n")
	if got := filteredEveryWay(t, server); got != server {
		t.Fatalf("filter changed the responses:\n%q\nwant\n%q", got, server)
	}
}

// Message contents come in literals, which pass through unread: a message
// holding a STATUS line with the space keeps it.
func TestStatusFixLeavesLiteralsAlone(t *testing.T) {
	body := "From: someone\r\n\r\n* STATUS \"INBOX\" (MESSAGES 1 )\r\nmore\r\n"
	binary := "\x00\x01* STATUS x (y )\r\n\xff"
	server := "* 1 FETCH (UID 4 BODY[] {" + strconv.Itoa(len(body)) + "}\r\n" + body + " BINARY[1] ~{" + strconv.Itoa(len(binary)) + "}\r\n" + binary + ")\r\n" +
		"* 2 FETCH (BODY[] {0}\r\n)\r\n" +
		"* STATUS \"INBOX\" (MESSAGES 2 )\r\n"
	want := strings.Replace(server, "(MESSAGES 2 )", "(MESSAGES 2)", 1)
	if got := filteredEveryWay(t, server); got != want {
		t.Fatalf("filtered\n%q\nwant\n%q", got, want)
	}
}

// A line longer than the filter's buffer passes in parts, and a literal
// announced at its end, even across two parts, is still skipped.
func TestStatusFixFollowsLiteralsAfterLongLines(t *testing.T) {
	inside := "* STATUS \"INBOX\" (MESSAGES 1 )\r\n"
	prefix, middle := "* 1 FETCH (ENVELOPE (\"", "\") BODY[] "
	// Lines whose literal announcement, "{33}" and the line break, is cut by
	// the end of the buffer at each place, then lines well past it.
	var lengths []int
	for cut := 0; cut <= len("{33}\r\n"); cut++ {
		lengths = append(lengths, statusFixBufferSize-cut-len(prefix)-len(middle))
	}
	lengths = append(lengths, statusFixBufferSize, 3*statusFixBufferSize)
	for _, length := range lengths {
		long := prefix + strings.Repeat("a", length) + middle + "{" + strconv.Itoa(len(inside)) + "}\r\n"
		server := long + inside + ")\r\n" + "* STATUS \"Sent\" (UIDNEXT 3 )\r\n"
		want := long + inside + ")\r\n" + "* STATUS \"Sent\" (UIDNEXT 3)\r\n"
		if got := filteredEveryWay(t, server); got != want {
			t.Fatalf("line of %d: filtered output differs at byte %d", length, firstDifference(got, want))
		}
	}
}

// Random message contents in literals never change, whatever they hold.
func TestStatusFixPassesRandomLiteralsThrough(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	pieces := []string{"* STATUS ", " )", "\r\n", "{12}", "~{3}", "(", ")", "x", "\x00", "\n"}
	var server, want strings.Builder
	for i := 0; i < 200; i++ {
		var body strings.Builder
		for j := rng.Intn(40); j > 0; j-- {
			body.WriteString(pieces[rng.Intn(len(pieces))])
		}
		line := "* " + strconv.Itoa(i+1) + " FETCH (BODY[] {" + strconv.Itoa(body.Len()) + "}\r\n" + body.String() + ")\r\n"
		server.WriteString(line)
		want.WriteString(line)
	}
	if got := filteredEveryWay(t, server.String()); got != want.String() {
		t.Fatalf("filtered output differs at byte %d", firstDifference(got, want.String()))
	}
}

// A read error comes after the bytes read before it.
func TestStatusFixReturnsErrorsAfterData(t *testing.T) {
	failure := errors.New("connection reset")
	conn := newStatusFixConn(readerConn{r: io.MultiReader(strings.NewReader("* STATUS x (y 1 )\r\n* OK no end"), iotest.ErrReader(failure))})
	got, err := io.ReadAll(conn)
	if !errors.Is(err, failure) {
		t.Fatalf("error = %v, want %v", err, failure)
	}
	if string(got) != "* STATUS x (y 1)\r\n* OK no end" {
		t.Fatalf("read %q before the error", got)
	}
}

// Through a whole connection: a server that ends STATUS with a space, as
// 139.com does, gives the folder's counts instead of a dropped connection.
func TestFolderStatusWithSpaceBeforeParen(t *testing.T) {
	cfg, _ := startScriptedIMAPServer(t, "IMAP4rev1 AUTH=PLAIN")
	client, err := NewClient(context.Background(), cfg, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.Close()
	status, ok, err := client.FolderStatus(context.Background(), "INBOX")
	if err != nil || !ok {
		t.Fatalf("FolderStatus() = %v, %v, %v", status, ok, err)
	}
	if status.Messages != 11 || status.UIDNext != 12 || status.UIDValidity != 1 || status.Unseen != 7 {
		t.Fatalf("FolderStatus() = %+v", status)
	}
	// The connection is still usable afterwards.
	if _, _, err := client.FolderStatus(context.Background(), "INBOX"); err != nil {
		t.Fatalf("second FolderStatus() error = %v", err)
	}
}

func firstDifference(a, b string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}
