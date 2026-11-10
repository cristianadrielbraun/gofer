package imap

import (
	"bufio"
	"bytes"
	"net"
	"strconv"
)

// statusFixConn passes an IMAP server's responses to go-imap, fixing one
// mistake on the way: a space before the closing paren of a STATUS response,
// as imap.139.com sends in `* STATUS "INBOX" (MESSAGES 11 UNSEEN 7 )`.
// go-imap reads an empty value there and drops the connection. Every other
// byte passes through as it came, above all the literals that carry message
// contents, which the filter skips without looking at them, so a message
// holding a line like that is never changed.
//
// It wraps the connection go-imap reads, so it only works where Gofer does
// the TLS itself or there is none: with STARTTLS, go-imap starts TLS inside
// itself, beneath this filter. Remove it once go-imap accepts the space
// (https://github.com/emersion/go-imap/pull/788).
type statusFixConn struct {
	net.Conn
	r *bufio.Reader

	// out is what has been read and filtered but not returned yet, and buf
	// the space it is kept in.
	out []byte
	buf []byte
	// err is the read error to return once out has been returned.
	err error

	// literal is how many bytes of a literal are still to pass through.
	literal int64
	// midLine is set when the next bytes continue a response rather than
	// start one: after a literal, or after part of a line longer than the
	// buffer.
	midLine bool
	// tail holds the end of a line read in parts, to find a literal
	// announced at its end.
	tail []byte
}

// statusFixBufferSize bounds the lines the filter reads at once. Longer
// lines pass in parts, unchanged; a STATUS response is far shorter.
const statusFixBufferSize = 8 << 10

// literalTailSize is enough of a line's end to hold a literal's announcement:
// "~{", up to 19 digits, "+}" and CRLF.
const literalTailSize = 32

func newStatusFixConn(conn net.Conn) *statusFixConn {
	return &statusFixConn{Conn: conn, r: bufio.NewReaderSize(conn, statusFixBufferSize)}
}

func (c *statusFixConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(c.out) == 0 {
		if c.err != nil {
			err := c.err
			c.err = nil
			return 0, err
		}
		if c.literal > 0 {
			// A literal's bytes go straight through.
			if int64(len(p)) > c.literal {
				p = p[:c.literal]
			}
			n, err := c.r.Read(p)
			c.literal -= int64(n)
			return n, err
		}
		line, err := c.r.ReadSlice('\n')
		partial := err == bufio.ErrBufferFull
		if len(line) > 0 {
			c.out = c.filter(line, partial || err != nil)
		}
		if err != nil && !partial {
			c.err = err
		}
	}
	n := copy(p, c.out)
	c.out = c.out[n:]
	return n, nil
}

// filter returns a line, or part of one, as go-imap should read it. partial
// is set when the line goes on past these bytes.
func (c *statusFixConn) filter(line []byte, partial bool) []byte {
	start := !c.midLine
	if partial {
		c.midLine = true
		c.keepTail(line)
		return c.keep(line)
	}
	end := line
	if len(c.tail) > 0 {
		end = append(c.tail, line...)
	}
	c.tail = c.tail[:0]
	if size, ok := literalAtEnd(end); ok {
		c.literal = size
		c.midLine = true
		return c.keep(line)
	}
	c.midLine = false
	if start {
		if fixed, ok := fixStatusLine(line); ok {
			return c.keep(fixed)
		}
	}
	return c.keep(line)
}

// keep copies b into the filter's own buffer, since the bufio.Reader reuses
// its own.
func (c *statusFixConn) keep(b []byte) []byte {
	c.buf = append(c.buf[:0], b...)
	return c.buf
}

func (c *statusFixConn) keepTail(part []byte) {
	c.tail = append(c.tail, part...)
	if len(c.tail) > literalTailSize {
		c.tail = append(c.tail[:0], c.tail[len(c.tail)-literalTailSize:]...)
	}
}

// literalAtEnd reports the size of a literal announced at the end of a line:
// {123}, or ~{123} for a binary one, before the line break.
func literalAtEnd(line []byte) (int64, bool) {
	line = bytes.TrimSuffix(line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	if len(line) < 3 || line[len(line)-1] != '}' {
		return 0, false
	}
	open := bytes.LastIndexByte(line, '{')
	if open < 0 {
		return 0, false
	}
	digits := bytes.TrimSuffix(line[open+1:len(line)-1], []byte("+"))
	if len(digits) == 0 {
		return 0, false
	}
	for _, b := range digits {
		if b < '0' || b > '9' {
			return 0, false
		}
	}
	size, err := strconv.ParseInt(string(digits), 10, 64)
	if err != nil {
		return 0, false
	}
	return size, true
}

var statusPrefix = []byte("* STATUS ")

// fixStatusLine drops the spaces before the closing paren of a STATUS
// response, reporting false for any other line or one already right.
func fixStatusLine(line []byte) ([]byte, bool) {
	if len(line) < len(statusPrefix) || !bytes.EqualFold(line[:len(statusPrefix)], statusPrefix) {
		return nil, false
	}
	body := bytes.TrimSuffix(line, []byte("\n"))
	body = bytes.TrimSuffix(body, []byte("\r"))
	if !bytes.HasSuffix(body, []byte(" )")) {
		return nil, false
	}
	inner := bytes.TrimRight(body[:len(body)-1], " ")
	fixed := make([]byte, 0, len(line))
	fixed = append(fixed, inner...)
	fixed = append(fixed, ')')
	return append(fixed, line[len(body):]...), true
}
