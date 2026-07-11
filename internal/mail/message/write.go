package message

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"os"
	"sort"
	"strings"
	"time"

	gomessage "github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
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
	return buildMIMEMessage(msg, false, nil)
}

func BuildMIMEMessageForGraph(msg *OutgoingMessage) ([]byte, error) {
	return buildMIMEMessage(msg, true, nil)
}

func BuildMIMEMessageForIMAPDraft(msg *OutgoingMessage, revisionToken string) ([]byte, error) {
	revisionToken = strings.TrimSpace(revisionToken)
	if revisionToken == "" || strings.ContainsAny(revisionToken, "\r\n") {
		return nil, fmt.Errorf("invalid draft revision token")
	}
	return buildMIMEMessage(msg, true, map[string]string{"X-Gofer-Draft-Revision": revisionToken})
}

func buildMIMEMessage(msg *OutgoingMessage, includeBcc bool, extraHeaders map[string]string) ([]byte, error) {
	if msg == nil {
		return nil, fmt.Errorf("message is required")
	}
	if msg.MessageID == "" {
		msg.MessageID = NewMessageID()
	}
	if msg.Date.IsZero() {
		msg.Date = time.Now().UTC()
	}

	header, err := outgoingMIMEHeader(msg, includeBcc, extraHeaders)
	if err != nil {
		return nil, err
	}
	root := outgoingMIMEBody(msg)
	if err := applyMIMEEntityHeaders(&header.Header, root); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	w, err := gomessage.CreateWriter(&buf, header.Header)
	if err != nil {
		return nil, fmt.Errorf("create MIME writer: %w", err)
	}
	if err := writeMIMEEntity(w, root); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type mimeEntity struct {
	contentType       string
	contentTypeParams map[string]string
	transferEncoding  string
	disposition       string
	dispositionParams map[string]string
	contentID         string
	body              []byte
	attachment        *OutgoingAttachment
	children          []mimeEntity
}

func outgoingMIMEHeader(msg *OutgoingMessage, includeBcc bool, extraHeaders map[string]string) (gomail.Header, error) {
	var header gomail.Header
	header.SetAddressList("From", []*mail.Address{{Name: msg.FromName, Address: msg.FromEmail}})
	header.SetAddressList("To", msg.To)
	header.SetAddressList("Cc", msg.CC)
	if includeBcc {
		header.SetAddressList("Bcc", msg.Bcc)
	}
	header.SetSubject(msg.Subject)
	header.SetDate(msg.Date)
	messageID := NormalizeMessageID(msg.MessageID)
	if messageID == "" {
		return gomail.Header{}, fmt.Errorf("invalid Message-ID")
	}
	header.SetMessageID(messageID)

	if value := strings.TrimSpace(msg.InReplyTo); value != "" {
		ids := ParseMessageIDs(value)
		if len(ids) == 0 {
			return gomail.Header{}, fmt.Errorf("invalid In-Reply-To header")
		}
		header.SetMsgIDList("In-Reply-To", ids[:1])
	}
	if value := strings.TrimSpace(msg.References); value != "" {
		ids := ParseMessageIDs(value)
		if len(ids) == 0 {
			return gomail.Header{}, fmt.Errorf("invalid References header")
		}
		header.SetMsgIDList("References", ids)
	}

	names := make([]string, 0, len(extraHeaders))
	for name := range extraHeaders {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := extraHeaders[name]
		if strings.ContainsAny(name, "\r\n:") || strings.ContainsAny(value, "\r\n") {
			return gomail.Header{}, fmt.Errorf("invalid extra MIME header")
		}
		header.Set(name, value)
	}
	return header, nil
}

func outgoingMIMEBody(msg *OutgoingMessage) mimeEntity {
	body := textMIMEEntity("text/plain", msg.TextBody)
	if msg.HTMLBody != "" {
		html := textMIMEEntity("text/html", msg.HTMLBody)
		if msg.TextBody != "" {
			body = multipartMIMEEntity("multipart/alternative", body, html)
		} else {
			body = html
		}
	}

	inlineAttachments, fileAttachments := splitOutgoingAttachments(msg.Attachments)
	if len(inlineAttachments) > 0 {
		children := make([]mimeEntity, 0, len(inlineAttachments)+1)
		children = append(children, body)
		for i := range inlineAttachments {
			children = append(children, attachmentMIMEEntity(inlineAttachments[i], true))
		}
		body = multipartMIMEEntity("multipart/related", children...)
	}
	if len(fileAttachments) > 0 {
		children := make([]mimeEntity, 0, len(fileAttachments)+1)
		children = append(children, body)
		for i := range fileAttachments {
			children = append(children, attachmentMIMEEntity(fileAttachments[i], false))
		}
		body = multipartMIMEEntity("multipart/mixed", children...)
	}
	return body
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

func textMIMEEntity(contentType, body string) mimeEntity {
	return mimeEntity{
		contentType:       contentType,
		contentTypeParams: map[string]string{"charset": "utf-8"},
		transferEncoding:  "quoted-printable",
		body:              []byte(normalizeMIMEText(body)),
	}
}

func multipartMIMEEntity(contentType string, children ...mimeEntity) mimeEntity {
	return mimeEntity{contentType: contentType, children: children}
}

func attachmentMIMEEntity(att OutgoingAttachment, inline bool) mimeEntity {
	entity := mimeEntity{
		transferEncoding: "base64",
		attachment:       &att,
		disposition:      "attachment",
	}
	if inline {
		entity.disposition = "inline"
		entity.contentID = att.ContentID
	}
	return entity
}

func applyMIMEEntityHeaders(header *gomessage.Header, entity mimeEntity) error {
	contentType := entity.contentType
	params := cloneStringMap(entity.contentTypeParams)
	if entity.attachment != nil {
		contentType = strings.TrimSpace(entity.attachment.ContentType)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		mediaType, parsedParams, err := mime.ParseMediaType(contentType)
		if err != nil {
			return fmt.Errorf("invalid attachment content type %q: %w", contentType, err)
		}
		contentType = mediaType
		params = cloneStringMap(parsedParams)
		if filename := strings.TrimSpace(entity.attachment.Filename); filename != "" {
			if params == nil {
				params = map[string]string{}
			}
			params["name"] = filename
		}
	}
	if contentType == "" {
		return fmt.Errorf("MIME content type is required")
	}
	header.SetContentType(contentType, params)
	if entity.transferEncoding != "" {
		header.Set("Content-Transfer-Encoding", entity.transferEncoding)
	}
	if entity.disposition != "" {
		dispositionParams := cloneStringMap(entity.dispositionParams)
		if entity.attachment != nil {
			if filename := strings.TrimSpace(entity.attachment.Filename); filename != "" {
				if dispositionParams == nil {
					dispositionParams = map[string]string{}
				}
				dispositionParams["filename"] = filename
			}
		}
		header.SetContentDisposition(entity.disposition, dispositionParams)
	}
	if entity.contentID != "" {
		contentID := strings.TrimSpace(entity.contentID)
		if strings.HasPrefix(contentID, "<") && strings.HasSuffix(contentID, ">") {
			contentID = strings.TrimSpace(contentID[1 : len(contentID)-1])
		}
		if contentID == "" || strings.ContainsAny(contentID, "\r\n<>") {
			return fmt.Errorf("invalid inline attachment Content-ID")
		}
		header.Set("Content-ID", "<"+contentID+">")
	}
	return nil
}

func writeMIMEEntity(w *gomessage.Writer, entity mimeEntity) error {
	if len(entity.children) > 0 {
		for _, child := range entity.children {
			var header gomessage.Header
			if err := applyMIMEEntityHeaders(&header, child); err != nil {
				_ = w.Close()
				return err
			}
			part, err := w.CreatePart(header)
			if err != nil {
				_ = w.Close()
				return fmt.Errorf("create MIME part: %w", err)
			}
			if err := writeMIMEEntity(part, child); err != nil {
				_ = w.Close()
				return err
			}
		}
		if err := w.Close(); err != nil {
			return fmt.Errorf("close multipart MIME body: %w", err)
		}
		return nil
	}

	if entity.attachment != nil {
		f, err := os.Open(entity.attachment.Path)
		if err != nil {
			_ = w.Close()
			return fmt.Errorf("open attachment %q: %w", entity.attachment.Filename, err)
		}
		_, copyErr := io.Copy(w, f)
		closeErr := f.Close()
		if copyErr != nil {
			_ = w.Close()
			return fmt.Errorf("read attachment %q: %w", entity.attachment.Filename, copyErr)
		}
		if closeErr != nil {
			_ = w.Close()
			return fmt.Errorf("close attachment %q: %w", entity.attachment.Filename, closeErr)
		}
	} else if _, err := w.Write(entity.body); err != nil {
		_ = w.Close()
		return fmt.Errorf("write MIME body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close MIME body: %w", err)
	}
	return nil
}

func normalizeMIMEText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.ReplaceAll(value, "\n", "\r\n")
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
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
