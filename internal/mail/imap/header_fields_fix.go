package imap

import "bytes"

// Write passes go-imap's commands to the server, sending the header names of
// each HEADER.FIELDS list as atoms: (References In-Reply-To) rather than
// ("References" "In-Reply-To"). go-imap always quotes them, which IMAP allows,
// but some servers answer BAD to the quoted form and others return an empty
// header for it (https://github.com/emersion/go-imap/pull/589). Each command
// line is held until its line break so that it can be fixed whole; the
// literals after a line, such as a message being appended, go through as
// they come, unread.
func (c *quirksConn) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		if c.wliteral > 0 {
			size := len(p)
			if int64(size) > c.wliteral {
				size = int(c.wliteral)
			}
			m, err := c.Conn.Write(p[:size])
			c.wliteral -= int64(m)
			n += m
			if err != nil {
				return n, err
			}
			p = p[size:]
			continue
		}
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			c.line = append(c.line, p...)
			return n + len(p), nil
		}
		c.line = append(c.line, p[:end+1]...)
		_, err := c.Conn.Write(fixHeaderFieldNames(c.line))
		if size, ok := literalAtEnd(c.line); ok {
			c.wliteral = size
		}
		c.line = c.line[:0]
		if err != nil {
			return n, err
		}
		n += end + 1
		p = p[end+1:]
	}
	return n, nil
}

var headerFieldsKeywords = [][]byte{[]byte("HEADER.FIELDS.NOT ("), []byte("HEADER.FIELDS (")}

// fixHeaderFieldNames returns a command line with the quoted names of each
// HEADER.FIELDS or HEADER.FIELDS.NOT list written as atoms. A list holding
// anything else, such as a name with a space or a literal, stays as it is,
// as do quoted strings elsewhere on the line, like a search for those words.
func fixHeaderFieldNames(line []byte) []byte {
	var out []byte
	copied := 0
	for i := 0; i < len(line); i++ {
		if line[i] == '"' {
			i = quotedEnd(line, i)
			continue
		}
		if line[i] != '[' && line[i] != '.' {
			continue
		}
		for _, keyword := range headerFieldsKeywords {
			rest := line[i+1:]
			if len(rest) < len(keyword) || !bytes.EqualFold(rest[:len(keyword)], keyword) {
				continue
			}
			open := i + len(keyword)
			names, closing, ok := quotedAtomList(line, open+1)
			if !ok {
				break
			}
			out = append(out, line[copied:open+1]...)
			out = append(out, bytes.Join(names, []byte(" "))...)
			copied = closing
			i = closing - 1
			break
		}
	}
	if out == nil {
		return line
	}
	return append(out, line[copied:]...)
}

// quotedAtomList reads a list of quoted names from start, just past its
// opening paren, up to the closing paren. It returns the names and where
// the closing paren is, or false unless every name is quoted and can be an
// atom.
func quotedAtomList(line []byte, start int) ([][]byte, int, bool) {
	var names [][]byte
	i := start
	for {
		if i >= len(line) || line[i] != '"' {
			return nil, 0, false
		}
		end := quotedEnd(line, i)
		if end >= len(line) || line[end] != '"' {
			return nil, 0, false
		}
		name := line[i+1 : end]
		if !headerNameAtom(name) {
			return nil, 0, false
		}
		names = append(names, name)
		i = end + 1
		if i < len(line) && line[i] == ')' {
			return names, i, true
		}
		if i >= len(line) || line[i] != ' ' {
			return nil, 0, false
		}
		i++
	}
}

// quotedEnd returns where the quoted string opening at start ends: its
// closing quote, or the end of the line if it has none.
func quotedEnd(line []byte, start int) int {
	for i := start + 1; i < len(line); i++ {
		switch line[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return len(line)
}

// headerNameAtom reports whether a header name can go as an atom: ASCII
// letters, digits, hyphens, underscores and dots, as in every common header
// name. A quoted name holding an escape is never one.
func headerNameAtom(name []byte) bool {
	if len(name) == 0 {
		return false
	}
	for _, b := range name {
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '-', b == '_', b == '.':
		default:
			return false
		}
	}
	return true
}
