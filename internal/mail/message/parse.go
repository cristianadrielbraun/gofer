package message

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"

	"gofer.email/internal/store"
)

type ParsedMessage struct {
	MessageID   string
	Subject     string
	FromName    string
	FromEmail   string
	To          []Recipient
	CC          []Recipient
	DateSent    time.Time
	TextBody    string
	HTMLBody    []byte
	Snippet     string
	Attachments []AttachmentMeta
	Size        int64
	ParseError  error
	RawPath     string
}

type Recipient struct {
	Name  string
	Email string
}

type AttachmentMeta struct {
	Filename    string
	ContentType string
	ContentID   string
	Size        int64
	Inline      bool
	BlobPath    string
}

func ParseMessage(ctx context.Context, r io.Reader, blobStore *store.BlobStore, accountID string, localID int64) (*ParsedMessage, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read raw message: %w", err)
	}

	rawPath, err := blobStore.StoreRaw(ctx, accountID, localID, raw)
	if err != nil {
		return nil, fmt.Errorf("store raw: %w", err)
	}

	parsed := &ParsedMessage{
		Size:    int64(len(raw)),
		RawPath: rawPath,
	}

	msgReader, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		parsed.ParseError = fmt.Errorf("parse message: %w", err)
		parsed.Snippet = truncate(string(raw), 200)
		return parsed, nil
	}

	header := msgReader.Header

	parsed.MessageID, _ = header.MessageID()
	parsed.Subject, _ = header.Subject()
	if parsed.Subject == "" {
		parsed.Subject = "(no subject)"
	}

	if date, err := header.Date(); err == nil {
		parsed.DateSent = date
	}

	if fromList, err := header.AddressList("From"); err == nil && len(fromList) > 0 {
		parsed.FromName = fromList[0].Name
		parsed.FromEmail = fromList[0].Address
	}

	if toList, err := header.AddressList("To"); err == nil {
		for _, addr := range toList {
			parsed.To = append(parsed.To, Recipient{Name: addr.Name, Email: addr.Address})
		}
	}

	if ccList, err := header.AddressList("Cc"); err == nil {
		for _, addr := range ccList {
			parsed.CC = append(parsed.CC, Recipient{Name: addr.Name, Email: addr.Address})
		}
	}

	attID := int64(0)
	for {
		part, err := msgReader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			parsed.ParseError = fmt.Errorf("read part: %w", err)
			break
		}

		switch h := part.Header.(type) {
		case *mail.InlineHeader:
			ct, _, _ := h.ContentType()
			content, readErr := io.ReadAll(part.Body)
			if readErr != nil {
				continue
			}

			switch ct {
			case "text/plain":
				parsed.TextBody = string(content)
			case "text/html":
				parsed.HTMLBody = content
			default:
				if strings.HasPrefix(ct, "text/") {
					parsed.TextBody = string(content)
				}
			}

		case *mail.AttachmentHeader:
			attID++
			filename, _ := h.Filename()
			ct, _, _ := h.ContentType()
			if filename == "" {
				filename = fmt.Sprintf("attachment-%d", attID)
			}
			if ct == "" {
				ct = "application/octet-stream"
			}

			cid := h.Get("Content-Id")
			cid = strings.Trim(cid, "<>")
			isInline := cid != ""

			var sizeBuf countingWriter
			teeReader := io.TeeReader(part.Body, &sizeBuf)

			bp, storeErr := blobStore.StoreAttachment(ctx, accountID, localID, attID, filename, teeReader)
			if storeErr != nil {
				parsed.ParseError = fmt.Errorf("store attachment %s: %w", filename, storeErr)
				continue
			}

			parsed.Attachments = append(parsed.Attachments, AttachmentMeta{
				Filename:    filename,
				ContentType: ct,
				ContentID:   cid,
				Inline:      isInline,
				BlobPath:    bp,
				Size:        sizeBuf.n,
			})
		}
	}

	parsed.Snippet = generateSnippet(parsed.TextBody, parsed.HTMLBody)

	return parsed, nil
}

func generateSnippet(text string, html []byte) string {
	if text != "" {
		return truncate(strings.TrimSpace(text), 200)
	}
	if len(html) > 0 {
		return truncate(stripHTMLTags(string(html)), 200)
	}
	return ""
}

func stripHTMLTags(s string) string {
	var buf strings.Builder
	inTag := false
	for _, r := range s {
		if r == '<' {
			inTag = true
			continue
		}
		if r == '>' {
			inTag = false
			continue
		}
		if !inTag {
			buf.WriteRune(r)
		}
	}
	return strings.TrimSpace(buf.String())
}

func truncate(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

func init() {
	_ = message.CharsetReader
}

type countingWriter struct {
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n := len(p)
	c.n += int64(n)
	return n, nil
}
