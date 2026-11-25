package message

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/store"
)

func TestParseMessageKeepsMixedHTMLAndInlineFiles(t *testing.T) {
	const htmlBody = `<html><body><p>Main message body</p></body></html>`
	pdf := []byte("%PDF-1.4\nsynthetic test document\n%%EOF")
	image := []byte("synthetic inline image")
	encode := func(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

	raw := "From: sender@example.test\r\n" +
		"To: recipient@example.test\r\n" +
		"Subject: MIME regression\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=alternative\r\n\r\n" +
		"--alternative\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Plain alternative\r\n" +
		"--alternative\r\n" +
		"Content-Type: multipart/mixed; boundary=mixed\r\n\r\n" +
		"--mixed\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" + htmlBody + "\r\n" +
		"--mixed\r\n" +
		"Content-Type: application/pdf; name=invoice.pdf\r\n" +
		"Content-Disposition: inline; filename=invoice.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" + encode(pdf) + "\r\n" +
		"--mixed\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" +
		"<html><body></body></html>\r\n" +
		"--mixed\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" +
		"<p>Visible mixed footer</p>\r\n" +
		"--mixed\r\n" +
		"Content-Type: application/pdf; name=receipt.pdf\r\n" +
		"Content-Disposition: inline; filename=receipt.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" + encode(pdf) + "\r\n" +
		"--mixed\r\n" +
		"Content-Type: image/png; name=logo.png\r\n" +
		"Content-ID: <logo@example.test>\r\n" +
		"Content-Disposition: inline; filename=logo.png\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" + encode(image) + "\r\n" +
		"--mixed--\r\n" +
		"--alternative--\r\n"

	parsed, err := ParseMessage(context.Background(), strings.NewReader(raw), store.NewBlobStore(t.TempDir()), "account", 1)
	if err != nil || parsed == nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.ParseError != nil {
		t.Fatal(parsed.ParseError)
	}
	if got := string(parsed.HTMLBody); !strings.Contains(got, htmlBody) || !strings.Contains(got, "<p>Visible mixed footer</p>") {
		t.Fatalf("HTML body = %q, want main body and mixed footer", got)
	} else if strings.Index(got, htmlBody) > strings.Index(got, "<p>Visible mixed footer</p>") {
		t.Fatalf("HTML fragments are out of order: %q", got)
	}
	if extracted, err := ExtractHTMLBody(strings.NewReader(raw)); err != nil || !strings.Contains(string(extracted), htmlBody) || !strings.Contains(string(extracted), "<p>Visible mixed footer</p>") {
		t.Fatalf("ExtractHTMLBody() = %q, %v; want main body and mixed footer", extracted, err)
	}
	if len(parsed.Attachments) != 3 {
		t.Fatalf("attachments = %d, want two inline PDFs and one CID image: %#v", len(parsed.Attachments), parsed.Attachments)
	}
	for i, name := range []string{"invoice.pdf", "receipt.pdf", "logo.png"} {
		attachment := parsed.Attachments[i]
		wantInline := name == "logo.png"
		if attachment.Filename != name || attachment.Inline != wantInline {
			t.Errorf("attachment %d = %#v, want inline=%t %q", i, attachment, wantInline, name)
		}
		wantContentID := ""
		wantData := pdf
		if name == "logo.png" {
			wantContentID = "logo@example.test"
			wantData = image
		}
		if attachment.ContentID != wantContentID {
			t.Errorf("attachment %q ContentID = %q, want %q", name, attachment.ContentID, wantContentID)
		}
		data, err := os.ReadFile(attachment.BlobPath)
		if err != nil || string(data) != string(wantData) {
			t.Errorf("attachment %q payload = %q, err %v", name, data, err)
		}
	}
}

func TestParseMessageDoesNotRenderHTMLAttachment(t *testing.T) {
	raw := "MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=mixed\r\n\r\n" +
		"--mixed\r\nContent-Type: text/html\r\n\r\n<p>Visible body</p>\r\n" +
		"--mixed\r\nContent-Type: text/html; name=attached.html\r\n" +
		"Content-Disposition: attachment; filename=attached.html\r\n\r\n<p>Attached content</p>\r\n" +
		"--mixed--\r\n"
	parsed, err := ParseMessage(context.Background(), strings.NewReader(raw), store.NewBlobStore(t.TempDir()), "account", 1)
	if err != nil || parsed == nil {
		t.Fatalf("parse: %v", err)
	}
	if got := string(parsed.HTMLBody); got != "<p>Visible body</p>" {
		t.Fatalf("HTML body = %q, want visible body only", got)
	}
	if len(parsed.Attachments) != 1 || parsed.Attachments[0].Filename != "attached.html" {
		t.Fatalf("HTML attachment was not retained: %#v", parsed.Attachments)
	}
}

func TestParseMessagePreservesImageOnlyHTML(t *testing.T) {
	const htmlBody = `<html><body><img src="cid:logo@example.test"></body></html>`
	raw := "MIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" + htmlBody
	parsed, err := ParseMessage(context.Background(), strings.NewReader(raw), store.NewBlobStore(t.TempDir()), "account", 1)
	if err != nil || parsed == nil {
		t.Fatalf("parse: %v", err)
	}
	if got := string(parsed.HTMLBody); got != htmlBody {
		t.Fatalf("HTML body = %q, want image-only body %q", got, htmlBody)
	}
}

func TestParseMessageKeepsHTMLBeforeMalformedLaterPart(t *testing.T) {
	raw := "MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=mixed\r\n\r\n" +
		"--mixed\r\nContent-Type: text/html\r\n\r\n<p>Readable body</p>\r\n" +
		"--mixed\r\nthis is not a MIME header\r\n\r\n<p>Malformed later part</p>\r\n" +
		"--mixed--\r\n"
	parsed, err := ParseMessage(context.Background(), strings.NewReader(raw), store.NewBlobStore(t.TempDir()), "account", 1)
	if err != nil || parsed == nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.ParseError == nil {
		t.Fatal("expected malformed later part to be reported")
	}
	if got := string(parsed.HTMLBody); got != "<p>Readable body</p>" {
		t.Fatalf("HTML body = %q, want previously read body", got)
	}
}

func TestParseMessageDoesNotDuplicateHTMLAlternative(t *testing.T) {
	const htmlBody = `<p>Preferred HTML alternative</p>`
	raw := "MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=alternative\r\n\r\n" +
		"--alternative\r\nContent-Type: text/plain\r\n\r\nPlain alternative\r\n" +
		"--alternative\r\nContent-Type: text/html\r\n\r\n<p>Earlier HTML alternative</p>\r\n" +
		"--alternative\r\nContent-Type: text/html\r\n\r\n" + htmlBody + "\r\n" +
		"--alternative--\r\n"
	parsed, err := ParseMessage(context.Background(), strings.NewReader(raw), store.NewBlobStore(t.TempDir()), "account", 1)
	if err != nil || parsed == nil {
		t.Fatalf("parse: %v", err)
	}
	if got := string(parsed.HTMLBody); got != htmlBody {
		t.Fatalf("HTML body = %q, want a single HTML alternative %q", got, htmlBody)
	}
}
