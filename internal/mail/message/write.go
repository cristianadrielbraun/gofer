package message

import (
	"bytes"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
)

type OutgoingMessage struct {
	FromName   string
	FromEmail  string
	To         []*mail.Address
	CC         []*mail.Address
	Bcc        []*mail.Address
	Subject    string
	TextBody   string
	HTMLBody   string
	InReplyTo  string
	References string
}

func BuildMIMEMessage(msg *OutgoingMessage) ([]byte, error) {
	from := []*mail.Address{{Name: msg.FromName, Address: msg.FromEmail}}

	var buf bytes.Buffer

	buf.WriteString(fmt.Sprintf("From: %s\r\n", formatAddressList(from)))
	buf.WriteString(fmt.Sprintf("To: %s\r\n", formatAddressList(msg.To)))
	if len(msg.CC) > 0 {
		buf.WriteString(fmt.Sprintf("Cc: %s\r\n", formatAddressList(msg.CC)))
	}
	buf.WriteString(fmt.Sprintf("Subject: %s\r\n", mime.QEncoding.Encode("utf-8", msg.Subject)))
	buf.WriteString(fmt.Sprintf("Date: %s\r\n", time.Now().Format(time.RFC1123Z)))
	buf.WriteString(fmt.Sprintf("Message-ID: <%s@gofer>\r\n", uuid.New().String()))
	buf.WriteString("MIME-Version: 1.0\r\n")

	if msg.InReplyTo != "" {
		buf.WriteString(fmt.Sprintf("In-Reply-To: %s\r\n", msg.InReplyTo))
	}
	if msg.References != "" {
		buf.WriteString(fmt.Sprintf("References: %s\r\n", msg.References))
	}

	hasText := msg.TextBody != ""
	hasHTML := msg.HTMLBody != ""

	if hasText && hasHTML {
		boundary := uuid.New().String()
		buf.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=%s\r\n", boundary))
		buf.WriteString("\r\n")

		buf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.TextBody)
		buf.WriteString("\r\n")

		buf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		buf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.HTMLBody)
		buf.WriteString("\r\n")

		buf.WriteString(fmt.Sprintf("--%s--\r\n", boundary))
	} else if hasHTML {
		buf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.HTMLBody)
	} else {
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.TextBody)
	}

	return buf.Bytes(), nil
}

func formatAddressList(addrs []*mail.Address) string {
	parts := make([]string, len(addrs))
	for i, a := range addrs {
		if a.Name != "" {
			parts[i] = fmt.Sprintf("%s <%s>", mime.QEncoding.Encode("utf-8", a.Name), a.Address)
		} else {
			parts[i] = a.Address
		}
	}
	return strings.Join(parts, ", ")
}

func ParseAddressList(s string) ([]*mail.Address, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	addrs, err := mail.ParseAddressList(s)
	if err != nil {
		return nil, fmt.Errorf("parse addresses: %w", err)
	}
	return addrs, nil
}

func AllRecipients(msg *OutgoingMessage) []string {
	var recipients []string
	for _, a := range msg.To {
		recipients = append(recipients, a.Address)
	}
	for _, a := range msg.CC {
		recipients = append(recipients, a.Address)
	}
	for _, a := range msg.Bcc {
		recipients = append(recipients, a.Address)
	}
	return recipients
}
