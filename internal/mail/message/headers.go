package message

import (
	"bufio"
	"bytes"
	"io"
	"mime"
	"strings"
	"time"

	gomessage "github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	gomsgtextproto "github.com/emersion/go-message/textproto"
	"golang.org/x/text/encoding/htmlindex"
)

func DecodeHeader(s string) string {
	if !strings.Contains(s, "=?") {
		return s
	}
	decoder := mime.WordDecoder{
		CharsetReader: func(charset string, input io.Reader) (io.Reader, error) {
			enc, err := htmlindex.Get(charset)
			if err != nil {
				return nil, err
			}
			return enc.NewDecoder().Reader(input), nil
		},
	}
	out, err := decoder.DecodeHeader(s)
	if err != nil || out == "" {
		return s
	}
	return out
}

// SyncHeaders holds what a message list row shows and threads by, read from
// the message's own header lines rather than from a server's summary of them.
type SyncHeaders struct {
	Subject    string
	MessageID  string
	InReplyTo  string
	References string
	ListID     string
	// Date is zero when the Date header is missing or can't be read.
	Date      time.Time
	FromName  string
	FromEmail string
	To        []Recipient
	CC        []Recipient
	BCC       []Recipient
}

// ParseSyncHeaders reads the header lines of a message, or a selection of
// them such as an IMAP HEADER.FIELDS fetch returns. It reads what it can: a
// malformed line ends the header, and the fields before it are kept. A
// Message-ID that can't be read is left empty. When an address list has a
// broken entry, the other addresses in it are still returned.
func ParseSyncHeaders(raw []byte) SyncHeaders {
	fields, _ := gomsgtextproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	header := mail.Header{Header: gomessage.Header{Header: fields}}

	var parsed SyncHeaders
	if subject, err := header.Subject(); err == nil {
		parsed.Subject = subject
	} else {
		parsed.Subject = DecodeHeader(header.Get("Subject"))
	}
	parsed.MessageID, _ = header.MessageID()
	parsed.InReplyTo, parsed.References = ParseThreadHeaders(raw)
	parsed.ListID = ParseListID(raw)
	if date, err := header.Date(); err == nil {
		parsed.Date = date
	}
	if from := syncAddressList(header.Get("From")); len(from) > 0 {
		parsed.FromName = from[0].Name
		parsed.FromEmail = from[0].Email
	}
	parsed.To = syncAddressList(header.Get("To"))
	parsed.CC = syncAddressList(header.Get("Cc"))
	parsed.BCC = syncAddressList(header.Get("Bcc"))
	return parsed
}

// syncAddressList reads an address header. Go rejects a whole list for one
// broken entry, so when that happens each entry is read on its own and the
// broken ones are left out.
func syncAddressList(value string) []Recipient {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	addresses, err := mail.ParseAddressList(value)
	if err != nil {
		addresses = nil
		for _, entry := range splitAddressList(value) {
			if address, err := mail.ParseAddress(entry); err == nil {
				addresses = append(addresses, address)
			}
		}
	}
	var recipients []Recipient
	for _, address := range addresses {
		if address.Address == "" {
			continue
		}
		recipients = append(recipients, Recipient{Name: DecodeHeader(address.Name), Email: address.Address})
	}
	return recipients
}

// splitAddressList cuts an address header into its entries at the commas
// and semicolons that are outside quotes, comments and angle brackets. A
// group's name ("Team:") is dropped, so its members come out as entries.
func splitAddressList(value string) []string {
	var entries []string
	var entry strings.Builder
	quoted, escaped := false, false
	comment, angle := 0, 0
	flush := func() {
		if text := strings.TrimSpace(entry.String()); text != "" {
			entries = append(entries, text)
		}
		entry.Reset()
	}
	for _, r := range value {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && (quoted || comment > 0):
			escaped = true
		case quoted:
			quoted = r != '"'
		case r == '"':
			quoted = true
		case r == '(':
			comment++
		case r == ')' && comment > 0:
			comment--
		case comment > 0:
		case r == '<':
			angle++
		case r == '>' && angle > 0:
			angle--
		case angle > 0:
		case r == ',' || r == ';':
			flush()
			continue
		case r == ':':
			entry.Reset()
			continue
		}
		entry.WriteRune(r)
	}
	flush()
	return entries
}
