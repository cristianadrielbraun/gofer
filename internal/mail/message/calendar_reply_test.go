package message

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

func TestCalendarReplyMIME(t *testing.T) {
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REPLY\r\nBEGIN:VEVENT\r\nUID:test-uid\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:me@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	raw, err := BuildMIMEMessage(&OutgoingMessage{FromEmail: "me@example.com", To: []*mail.Address{{Address: "host@example.com"}}, Subject: "Accepted: Meeting", TextBody: "Accepted.", CalendarReply: ics})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	ct, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || ct != "multipart/alternative" {
		t.Fatal("reply is not an alternative MIME part", err)
	}
	parts := multipart.NewReader(msg.Body, params["boundary"])
	count := 0
	for {
		part, err := parts.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
		ct, params, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil {
			t.Fatal(err)
		}
		if count == 1 && ct != "text/plain" {
			t.Fatal("missing human-readable fallback")
		}
		if count == 2 {
			data, err := io.ReadAll(part)
			if err != nil || ct != "text/calendar" || params["method"] != "REPLY" || params["charset"] != "utf-8" || !strings.Contains(string(data), "PARTSTAT=ACCEPTED") || part.Header.Get("Content-Disposition") != "inline" {
				t.Fatalf("invalid calendar MIME part: %v %s", part.Header, data)
			}
		}
	}
	if count != 2 {
		t.Fatal("wrong number of MIME alternatives")
	}
}

func TestCalendarOrganizerNotificationMIME(t *testing.T) {
	for _, method := range []string{"REQUEST", "CANCEL"} {
		t.Run(method, func(t *testing.T) {
			ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:" + method + "\r\nBEGIN:VEVENT\r\nUID:meeting\r\nORGANIZER:mailto:host@example.com\r\nATTENDEE:mailto:guest@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
			raw, err := BuildMIMEMessage(&OutgoingMessage{FromEmail: "host@example.com", To: []*mail.Address{{Address: "guest@example.com"}}, TextBody: "Meeting notification.", CalendarNotification: &CalendarNotification{Method: method, Calendar: ics, ExpectedCalendar: "private expected-resource metadata"}})
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte("private expected-resource metadata")) {
				t.Fatal("queue metadata leaked into MIME")
			}
			msg, err := mail.ReadMessage(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
			if err != nil {
				t.Fatal(err)
			}
			parts := multipart.NewReader(msg.Body, params["boundary"])
			if _, err := parts.NextPart(); err != nil {
				t.Fatal(err)
			}
			part, err := parts.NextPart()
			if err != nil {
				t.Fatal(err)
			}
			ct, params, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(part)
			if err != nil || ct != "text/calendar" || params["method"] != method || string(data) != ics {
				t.Fatalf("wrong organizer MIME: %s %v", data, part.Header)
			}
		})
	}
}
