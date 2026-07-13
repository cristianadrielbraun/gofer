package message

import (
	"bytes"
	"context"
	"fmt"
	stdhtml "html"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/emersion/go-message"
	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	xhtml "golang.org/x/net/html"

	"github.com/cristianadrielbraun/gofer/internal/store"
)

var (
	reHTMLPreviewTag = regexp.MustCompile(`(?is)<\s*/?\s*(?:!doctype|html|head|body|style|script|meta|link|div|span|p|br|table|tbody|thead|tr|td|th|a|img|font|center|blockquote|ul|ol|li)\b[^>]*>`)
	reCSSBlock       = regexp.MustCompile(`(?is)(?:^|[\s}])(?:body|html|[.#][a-z0-9_-]+|[a-z][a-z0-9_-]*)\s*\{[^}]*\}`)
)

type ParsedMessage struct {
	MessageID   string
	InReplyTo   string
	References  string
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
		parsed.TextBody = string(raw)
		parsed.Snippet = truncate(string(raw), 200)
		return parsed, nil
	}

	header := msgReader.Header

	parsed.MessageID, _ = header.MessageID()
	parsed.Subject, _ = header.Subject()
	if parsed.Subject == "" {
		parsed.Subject = "(no subject)"
	}

	if ids := ParseMessageIDs(header.Get("In-Reply-To")); len(ids) > 0 {
		parsed.InReplyTo = ids[0]
	}
	parsed.References = header.Get("References")

	if date, err := header.Date(); err == nil {
		parsed.DateSent = date
	}

	if fromList, err := header.AddressList("From"); err == nil && len(fromList) > 0 {
		parsed.FromName = DecodeHeader(fromList[0].Name)
		parsed.FromEmail = fromList[0].Address
	}

	if toList, err := header.AddressList("To"); err == nil {
		for _, addr := range toList {
			parsed.To = append(parsed.To, Recipient{Name: DecodeHeader(addr.Name), Email: addr.Address})
		}
	}

	if ccList, err := header.AddressList("Cc"); err == nil {
		for _, addr := range ccList {
			parsed.CC = append(parsed.CC, Recipient{Name: DecodeHeader(addr.Name), Email: addr.Address})
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

			switch {
			case ct == "text/plain":
				parsed.TextBody = string(content)
			case ct == "text/html":
				parsed.HTMLBody = content
			case strings.HasPrefix(ct, "text/"):
				parsed.TextBody = string(content)
			default:
				cid := h.Get("Content-Id")
				cid = strings.Trim(cid, "<>")
				if cid == "" {
					continue
				}
				attID++
				filename := cid
				if fn := h.Get("Content-Disposition"); fn != "" {
					if idx := strings.Index(fn, "filename="); idx != -1 {
						filename = strings.Trim(fn[idx+9:], ` "';`)
					}
				}
				bp, storeErr := blobStore.StoreAttachment(ctx, accountID, localID, attID, filename, bytes.NewReader(content))
				if storeErr != nil {
					continue
				}
				parsed.Attachments = append(parsed.Attachments, AttachmentMeta{
					Filename:    filename,
					ContentType: ct,
					ContentID:   cid,
					Inline:      true,
					BlobPath:    bp,
					Size:        int64(len(content)),
				})
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
			isInline := strings.HasPrefix(strings.ToLower(strings.TrimSpace(h.Get("Content-Disposition"))), "inline")

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

	parsed.Snippet = GenerateSnippet(parsed.TextBody, parsed.HTMLBody)

	return parsed, nil
}

func ExtractHTMLBody(r io.Reader) ([]byte, error) {
	msgReader, err := mail.CreateReader(r)
	if err != nil {
		return nil, fmt.Errorf("parse message: %w", err)
	}
	for {
		part, err := msgReader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read part: %w", err)
		}
		if h, ok := part.Header.(*mail.InlineHeader); ok {
			ct, _, _ := h.ContentType()
			if ct == "text/html" {
				return io.ReadAll(part.Body)
			}
		}
	}
	return nil, nil
}

func GenerateSnippet(text string, html []byte) string {
	preview := PreviewFromText(text)
	if preview != "" && !looksLikeMarkup(preview) {
		return preview
	}
	if htmlPreview := PreviewFromHTML(html); htmlPreview != "" {
		return htmlPreview
	}
	return preview
}

func PreviewFromText(text string) string {
	text = normalizePreviewText(text)
	if text == "" {
		return ""
	}
	if reHTMLPreviewTag.MatchString(text) {
		if preview := PreviewFromHTML([]byte(text)); preview != "" {
			return preview
		}
	}
	text = normalizePreviewText(reCSSBlock.ReplaceAllString(text, " "))
	return truncate(text, 200)
}

func PreviewFromHTML(raw []byte) string {
	return truncate(TextFromHTML(raw), 200)
}

// TextFromHTML extracts the complete visible text from an HTML message body.
// Unlike PreviewFromHTML, it does not truncate the result, so it is suitable
// for full-text indexing.
func TextFromHTML(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	doc, err := xhtml.Parse(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	var buf strings.Builder
	appendHTMLText(doc, &buf)
	return strings.TrimSpace(buf.String())
}

func appendHTMLText(n *xhtml.Node, buf *strings.Builder) {
	if n == nil {
		return
	}

	if n.Type == xhtml.TextNode {
		writePreviewToken(buf, n.Data)
		return
	}

	if n.Type == xhtml.ElementNode {
		name := strings.ToLower(n.Data)
		if shouldSkipPreviewElement(name) || elementHiddenFromPreview(n) {
			return
		}
		if name == "br" {
			writePreviewSpace(buf)
			return
		}
		if name == "img" {
			alt, _ := htmlAttr(n, "alt")
			writePreviewToken(buf, alt)
		}
		if isPreviewBlockElement(name) {
			writePreviewSpace(buf)
		}
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		appendHTMLText(c, buf)
	}

	if n.Type == xhtml.ElementNode && isPreviewBlockElement(strings.ToLower(n.Data)) {
		writePreviewSpace(buf)
	}
}

func writePreviewToken(buf *strings.Builder, s string) {
	s = normalizePreviewText(s)
	if s == "" {
		return
	}
	if buf.Len() > 0 && !startsWithPunctuation(s) {
		buf.WriteByte(' ')
	}
	buf.WriteString(s)
}

func startsWithPunctuation(s string) bool {
	for _, r := range s {
		switch r {
		case '.', ',', ':', ';', '!', '?', ')', ']', '}':
			return true
		default:
			return false
		}
	}
	return false
}

func writePreviewSpace(buf *strings.Builder) {
	if buf.Len() > 0 {
		buf.WriteByte(' ')
	}
}

func shouldSkipPreviewElement(name string) bool {
	switch name {
	case "head", "style", "script", "noscript", "template", "svg", "meta", "link", "title":
		return true
	default:
		return false
	}
}

func elementHiddenFromPreview(n *xhtml.Node) bool {
	if _, ok := htmlAttr(n, "hidden"); ok {
		return true
	}
	if attr, ok := htmlAttr(n, "aria-hidden"); ok && strings.EqualFold(attr, "true") {
		return true
	}
	style, _ := htmlAttr(n, "style")
	style = strings.ToLower(style)
	return strings.Contains(style, "display:none") || strings.Contains(style, "display: none") || strings.Contains(style, "visibility:hidden") || strings.Contains(style, "visibility: hidden")
}

func htmlAttr(n *xhtml.Node, key string) (string, bool) {
	for _, attr := range n.Attr {
		if strings.EqualFold(attr.Key, key) {
			return attr.Val, true
		}
	}
	return "", false
}

func isPreviewBlockElement(name string) bool {
	switch name {
	case "address", "article", "aside", "blockquote", "body", "br", "center", "dd", "div", "dl", "dt", "fieldset", "figcaption", "figure", "footer", "form", "h1", "h2", "h3", "h4", "h5", "h6", "header", "hr", "li", "main", "nav", "ol", "p", "pre", "section", "table", "tbody", "td", "tfoot", "th", "thead", "tr", "ul":
		return true
	default:
		return false
	}
}

func looksLikeMarkup(s string) bool {
	return reHTMLPreviewTag.MatchString(s) || reCSSBlock.MatchString(s)
}

func normalizePreviewText(s string) string {
	s = stdhtml.UnescapeString(s)
	s = strings.ReplaceAll(s, "\u00a0", " ")
	s = strings.ReplaceAll(s, "\u200b", "")
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, maxLen int) string {
	s = normalizePreviewText(s)
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen])
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
