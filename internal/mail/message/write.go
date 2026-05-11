package message

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

type OutgoingMessage struct {
	FromName    string
	FromEmail   string
	To          []*mail.Address
	CC          []*mail.Address
	Bcc         []*mail.Address
	Subject     string
	TextBody    string
	HTMLBody    string
	InReplyTo   string
	References  string
	MessageID   string
	Date        time.Time
	Attachments []OutgoingAttachment
}

type OutgoingAttachment struct {
	Filename    string
	ContentType string
	Path        string
	Size        int64
	ContentID   string
	Inline      bool
}

func NewMessageID() string {
	return fmt.Sprintf("<%s@gofer>", uuid.New().String())
}

func BuildMIMEMessage(msg *OutgoingMessage) ([]byte, error) {
	from := []*mail.Address{{Name: msg.FromName, Address: msg.FromEmail}}
	if msg.MessageID == "" {
		msg.MessageID = NewMessageID()
	}
	if msg.Date.IsZero() {
		msg.Date = time.Now().UTC()
	}

	var buf bytes.Buffer

	buf.WriteString(fmt.Sprintf("From: %s\r\n", formatAddressList(from)))
	buf.WriteString(fmt.Sprintf("To: %s\r\n", formatAddressList(msg.To)))
	if len(msg.CC) > 0 {
		buf.WriteString(fmt.Sprintf("Cc: %s\r\n", formatAddressList(msg.CC)))
	}
	buf.WriteString(fmt.Sprintf("Subject: %s\r\n", mime.QEncoding.Encode("utf-8", msg.Subject)))
	buf.WriteString(fmt.Sprintf("Date: %s\r\n", msg.Date.Format(time.RFC1123Z)))
	buf.WriteString(fmt.Sprintf("Message-ID: %s\r\n", msg.MessageID))
	buf.WriteString("MIME-Version: 1.0\r\n")

	if msg.InReplyTo != "" {
		buf.WriteString(fmt.Sprintf("In-Reply-To: %s\r\n", msg.InReplyTo))
	}
	if msg.References != "" {
		buf.WriteString(fmt.Sprintf("References: %s\r\n", msg.References))
	}

	body, bodyContentType := buildMessageBody(msg)
	inlineAttachments, fileAttachments := splitOutgoingAttachments(msg.Attachments)
	if len(inlineAttachments) > 0 {
		var err error
		body, bodyContentType, err = buildRelatedBody(body, bodyContentType, inlineAttachments)
		if err != nil {
			return nil, err
		}
	}
	if len(fileAttachments) > 0 {
		boundary := uuid.New().String()
		buf.WriteString(fmt.Sprintf("Content-Type: multipart/mixed; boundary=%s\r\n", boundary))
		buf.WriteString("\r\n")
		buf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		buf.WriteString(bodyContentType)
		buf.WriteString("\r\n\r\n")
		buf.Write(body)
		buf.WriteString("\r\n")
		for _, att := range fileAttachments {
			if err := writeAttachmentPart(&buf, boundary, att); err != nil {
				return nil, err
			}
		}
		buf.WriteString(fmt.Sprintf("--%s--\r\n", boundary))
		return buf.Bytes(), nil
	}

	buf.WriteString(bodyContentType)
	buf.WriteString("\r\n\r\n")
	buf.Write(body)

	return buf.Bytes(), nil
}

func splitOutgoingAttachments(atts []OutgoingAttachment) (inline []OutgoingAttachment, files []OutgoingAttachment) {
	for _, att := range atts {
		if att.Inline && att.ContentID != "" {
			inline = append(inline, att)
			continue
		}
		files = append(files, att)
	}
	return inline, files
}

func buildRelatedBody(body []byte, bodyContentType string, inlineAttachments []OutgoingAttachment) ([]byte, string, error) {
	boundary := uuid.New().String()
	var buf bytes.Buffer
	buf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
	buf.WriteString(bodyContentType)
	buf.WriteString("\r\n\r\n")
	buf.Write(body)
	buf.WriteString("\r\n")
	for _, att := range inlineAttachments {
		if err := writeAttachmentPart(&buf, boundary, att); err != nil {
			return nil, "", err
		}
	}
	buf.WriteString(fmt.Sprintf("--%s--\r\n", boundary))
	return buf.Bytes(), fmt.Sprintf("Content-Type: multipart/related; boundary=%s", boundary), nil
}

func buildMessageBody(msg *OutgoingMessage) ([]byte, string) {
	hasText := msg.TextBody != ""
	hasHTML := msg.HTMLBody != ""
	var buf bytes.Buffer

	if hasText && hasHTML {
		boundary := uuid.New().String()
		buf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.TextBody)
		buf.WriteString("\r\n")

		buf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		buf.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		buf.WriteString(msg.HTMLBody)
		buf.WriteString("\r\n")

		buf.WriteString(fmt.Sprintf("--%s--\r\n", boundary))
		return buf.Bytes(), fmt.Sprintf("Content-Type: multipart/alternative; boundary=%s", boundary)
	} else if hasHTML {
		buf.WriteString(msg.HTMLBody)
		return buf.Bytes(), "Content-Type: text/html; charset=utf-8"
	} else {
		buf.WriteString(msg.TextBody)
		return buf.Bytes(), "Content-Type: text/plain; charset=utf-8"
	}
}

func writeAttachmentPart(buf *bytes.Buffer, boundary string, att OutgoingAttachment) error {
	f, err := os.Open(att.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	contentType := att.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	filename := mime.QEncoding.Encode("utf-8", att.Filename)
	buf.WriteString(fmt.Sprintf("--%s\r\n", boundary))
	buf.WriteString(fmt.Sprintf("Content-Type: %s; name=\"%s\"\r\n", contentType, filename))
	buf.WriteString("Content-Transfer-Encoding: base64\r\n")
	if att.Inline && att.ContentID != "" {
		buf.WriteString(fmt.Sprintf("Content-ID: <%s>\r\n", att.ContentID))
		buf.WriteString(fmt.Sprintf("Content-Disposition: inline; filename=\"%s\"\r\n\r\n", filename))
	} else {
		buf.WriteString(fmt.Sprintf("Content-Disposition: attachment; filename=\"%s\"\r\n\r\n", filename))
	}
	enc := base64.NewEncoder(base64.StdEncoding, newBase64LineWriter(buf))
	if _, err := io.Copy(enc, f); err != nil {
		enc.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	buf.WriteString("\r\n")
	return nil
}

type base64LineWriter struct {
	buf *bytes.Buffer
	n   int
}

func newBase64LineWriter(buf *bytes.Buffer) io.Writer { return &base64LineWriter{buf: buf} }

func (w *base64LineWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if w.n == 76 {
			w.buf.WriteString("\r\n")
			w.n = 0
		}
		w.buf.WriteByte(b)
		w.n++
	}
	return len(p), nil
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
