package message

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/store"
)

func TestCalendarMIMEPartsAreDownloadableWithoutReplacingBody(t *testing.T) {
	calendar := "BEGIN:VCALENDAR\r\nMETHOD:REPLY\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:meeting@example.com\r\nATTENDEE;PARTSTAT=ACCEPTED;CN=Guest:mailto:guest@example.\r\n com\r\nSUMMARY:Accepted: team meeting\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	calendarPart := "Content-Type: text/calendar; charset=utf-8; method=REPLY\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString([]byte(calendar))
	plain := "Content-Type: text/plain; charset=utf-8\r\n\r\nAccepted."
	html := "Content-Type: text/html; charset=utf-8\r\n\r\n<p>Accepted.</p>"
	multipart := func(parts ...string) string {
		return "Content-Type: multipart/alternative; boundary=parts\r\n\r\n--parts\r\n" + strings.Join(parts, "\r\n--parts\r\n") + "\r\n--parts--\r\n"
	}
	for _, test := range []struct {
		name, mime, text, html, filename string
	}{
		{"Exchange empty plain alternative", multipart("Content-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", calendarPart), "Calendar file attached.", "", "calendar.ics"},
		{"plain alternative before calendar", multipart(plain, calendarPart), "Accepted.", "", "calendar.ics"},
		{"plain alternative after calendar", multipart(calendarPart, plain), "Accepted.", "", "calendar.ics"},
		{"HTML alternative", multipart(html, calendarPart), "", "<p>Accepted.</p>", "calendar.ics"},
		{"top-level calendar", calendarPart, "Calendar file attached.", "", "calendar.ics"},
		{"named inline calendar", strings.Replace(calendarPart, "Content-Transfer-Encoding:", "Content-Disposition: inline; filename=reply.ics\r\nContent-Transfer-Encoding:", 1), "Calendar file attached.", "", "reply.ics"},
		{"unnamed attached calendar", strings.Replace(calendarPart, "Content-Transfer-Encoding:", "Content-Disposition: attachment\r\nContent-Transfer-Encoding:", 1), "Calendar file attached.", "", "calendar.ics"},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := "From: guest@example.com\r\nTo: host@example.com\r\nSubject: Accepted: team meeting\r\nMIME-Version: 1.0\r\n" + test.mime
			parsed, err := ParseMessage(context.Background(), strings.NewReader(raw), store.NewBlobStore(t.TempDir()), "account", 1)
			if err != nil || parsed == nil {
				t.Fatalf("parse: %v", err)
			}
			if parsed.ParseError != nil {
				t.Fatal(parsed.ParseError)
			}
			if parsed.TextBody != test.text || string(parsed.HTMLBody) != test.html {
				t.Fatalf("body = %q / %q, want %q / %q", parsed.TextBody, parsed.HTMLBody, test.text, test.html)
			}
			if len(parsed.Attachments) != 1 {
				t.Fatalf("attachments = %d, want one calendar", len(parsed.Attachments))
			}
			attachment := parsed.Attachments[0]
			if attachment.Filename != test.filename || attachment.ContentType != "text/calendar" || attachment.Inline || attachment.Size != int64(len(calendar)) {
				t.Fatalf("incorrect calendar attachment: %#v", attachment)
			}
			data, err := os.ReadFile(attachment.BlobPath)
			if err != nil || string(data) != calendar {
				t.Fatalf("calendar payload changed: %q, %v", data, err)
			}
			if strings.Contains(parsed.Snippet, "BEGIN:VCALENDAR") {
				t.Fatalf("calendar leaked into preview: %q", parsed.Snippet)
			}
		})
	}
}

func TestPlainTextCalendarRemainsPlainText(t *testing.T) {
	text := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n"
	raw := "From: guest@example.com\r\nContent-Type: text/plain\r\n\r\n" + text
	parsed, err := ParseMessage(context.Background(), strings.NewReader(raw), store.NewBlobStore(t.TempDir()), "account", 1)
	if err != nil || parsed == nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.TextBody != text || len(parsed.Attachments) != 0 {
		t.Fatalf("plain text misclassified: %#v", parsed)
	}
}
