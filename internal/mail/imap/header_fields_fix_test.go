package imap

import (
	"bytes"
	"errors"
	"math/rand"
	"net"
	"strconv"
	"testing"
)

// writerConn is a connection that only writes, into w.
type writerConn struct {
	net.Conn
	w bytes.Buffer
}

func (c *writerConn) Write(p []byte) (int, error) { return c.w.Write(p) }

// sent writes the client's bytes through the filter in pieces of the given
// sizes, cycling through them, and returns what reached the server.
func sent(t *testing.T, client string, sizes ...int) string {
	t.Helper()
	server := &writerConn{}
	conn := newQuirksConn(server)
	for i, start := 0, 0; start < len(client); i++ {
		end := min(start+sizes[i%len(sizes)], len(client))
		n, err := conn.Write([]byte(client[start:end]))
		if err != nil || n != end-start {
			t.Fatalf("Write(%q) = %d, %v; want %d, nil", client[start:end], n, err, end-start)
		}
		start = end
	}
	return server.w.String()
}

// sentEveryWay writes the client's bytes whole, a byte at a time and in
// random pieces, and checks that every way sends the same.
func sentEveryWay(t *testing.T, client string) string {
	t.Helper()
	want := sent(t, client, len(client))
	if got := sent(t, client, 1); got != want {
		t.Fatalf("one-byte writes sent\n%q\nwant\n%q", got, want)
	}
	random := rand.New(rand.NewSource(1))
	for range 20 {
		sizes := []int{random.Intn(9) + 1, random.Intn(40) + 1, random.Intn(3) + 1}
		if got := sent(t, client, sizes...); got != want {
			t.Fatalf("writes of %v sent\n%q\nwant\n%q", sizes, got, want)
		}
	}
	return want
}

func TestQuirksConnSendsHeaderFieldNamesAsAtoms(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{
			"T1 UID FETCH 1:5 (UID ENVELOPE BODY.PEEK[HEADER.FIELDS (\"References\" \"In-Reply-To\" \"List-Id\")])\r\n",
			"T1 UID FETCH 1:5 (UID ENVELOPE BODY.PEEK[HEADER.FIELDS (References In-Reply-To List-Id)])\r\n",
		},
		{
			"T2 FETCH 190:200 (UID BODY.PEEK[HEADER.FIELDS (\"Message-ID\")])\r\n",
			"T2 FETCH 190:200 (UID BODY.PEEK[HEADER.FIELDS (Message-ID)])\r\n",
		},
		{
			"T3 FETCH 1 (BODY[HEADER.FIELDS.NOT (\"Received\" \"X_Spam.Score\")] BODY[1.HEADER.FIELDS (\"Subject\")])\r\n",
			"T3 FETCH 1 (BODY[HEADER.FIELDS.NOT (Received X_Spam.Score)] BODY[1.HEADER.FIELDS (Subject)])\r\n",
		},
		{
			"t4 fetch 1 (body.peek[header.fields (\"subject\")])\r\n",
			"t4 fetch 1 (body.peek[header.fields (subject)])\r\n",
		},
	} {
		if got := sentEveryWay(t, tt.in); got != tt.want {
			t.Errorf("sent %q = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Anything that can't be an atom, and every other command, goes as written.
func TestQuirksConnLeavesOtherCommandsAlone(t *testing.T) {
	for _, in := range []string{
		"T1 FETCH 1 (BODY.PEEK[HEADER.FIELDS (\"Subject\" \"X Odd Name\")])\r\n",
		"T2 FETCH 1 (BODY.PEEK[HEADER.FIELDS (\"Sub\\\"ject\")])\r\n",
		"T3 FETCH 1 (BODY.PEEK[HEADER.FIELDS (\"\")])\r\n",
		"T4 FETCH 1 (BODY.PEEK[HEADER.FIELDS (Subject)])\r\n",
		"T5 FETCH 1 (BODY.PEEK[HEADER.FIELDS (\"Subject\" {4}\r\n",
		"T6 UID SEARCH SUBJECT \"[HEADER.FIELDS (\\\"Subject\\\")]\"\r\n",
		"T7 SELECT \"INBOX\"\r\n",
		"T8 FETCH 1 (BODY.PEEK[HEADER])\r\n",
	} {
		if got := sentEveryWay(t, in); got != in {
			t.Errorf("sent %q = %q, want it unchanged", in, got)
		}
	}
}

// A literal, such as a message being appended, goes through unread, even
// when it holds a line like a FETCH.
func TestQuirksConnPassesLiteralsThrough(t *testing.T) {
	message := "Subject: notes\r\n\r\nT1 FETCH 1 (BODY.PEEK[HEADER.FIELDS (\"Subject\")])\r\n"
	for _, plus := range []string{"", "+"} {
		in := "T1 APPEND \"INBOX\" {" + strconv.Itoa(len(message)) + plus + "}\r\n" + message +
			"\r\nT2 FETCH 1 (BODY.PEEK[HEADER.FIELDS (\"Subject\")])\r\n"
		want := "T1 APPEND \"INBOX\" {" + strconv.Itoa(len(message)) + plus + "}\r\n" + message +
			"\r\nT2 FETCH 1 (BODY.PEEK[HEADER.FIELDS (Subject)])\r\n"
		if got := sentEveryWay(t, in); got != want {
			t.Errorf("sent\n%q\nwant\n%q", got, want)
		}
	}
}

// A line goes to the server once it is whole, fixed.
func TestQuirksConnHoldsALineUntilItsEnd(t *testing.T) {
	server := &writerConn{}
	conn := newQuirksConn(server)
	if _, err := conn.Write([]byte("T1 FETCH 1 (BODY.PEEK[HEADER.FIELDS (\"Subject\"")); err != nil {
		t.Fatal(err)
	}
	if server.w.Len() != 0 {
		t.Fatalf("sent %q before the line ended", server.w.String())
	}
	if _, err := conn.Write([]byte(")])\r\n")); err != nil {
		t.Fatal(err)
	}
	if got, want := server.w.String(), "T1 FETCH 1 (BODY.PEEK[HEADER.FIELDS (Subject)])\r\n"; got != want {
		t.Fatalf("sent %q, want %q", got, want)
	}
}

type failingWriter struct {
	net.Conn
	err error
}

func (c failingWriter) Write(p []byte) (int, error) { return 0, c.err }

func TestQuirksConnReportsWriteErrors(t *testing.T) {
	failure := errors.New("connection reset")
	conn := newQuirksConn(failingWriter{err: failure})
	if n, err := conn.Write([]byte("T1 NOOP")); err != nil || n != 7 {
		t.Fatalf("Write(part of a line) = %d, %v; want 7, nil", n, err)
	}
	if _, err := conn.Write([]byte("\r\n")); !errors.Is(err, failure) {
		t.Fatalf("Write(line break) error = %v, want %v", err, failure)
	}
}
