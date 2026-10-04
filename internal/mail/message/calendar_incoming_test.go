package message

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"
)

func incomingTestCalendar() string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REPLY\r\nBEGIN:VEVENT\r\nUID:meeting-uid\r\nSEQUENCE:2\r\nDTSTAMP:" + time.Now().Add(-time.Hour).UTC().Format("20060102T150405Z") + "\r\nORGANIZER:mailto:host@example.com\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:guest@example.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
}

func incomingTestMail(body string) []byte {
	return []byte("From: Guest <guest@example.com>\r\nTo: host@example.com\r\nMIME-Version: 1.0\r\nContent-Type: text/calendar; method=REPLY; charset=utf-8\r\n\r\n" + body)
}

func TestCalendarIncomingMIMEAndValidation(t *testing.T) {
	body := incomingTestCalendar()
	multipart := func(disposition, encoding, content string) []byte {
		return []byte("From: guest@example.com\r\nContent-Type: multipart/mixed; boundary=parts\r\n\r\n--parts\r\nContent-Type: text/plain\r\n\r\nAccepted.\r\n--parts\r\nContent-Type: text/calendar; method=REPLY\r\nContent-Disposition: " + disposition + "\r\nContent-Transfer-Encoding: " + encoding + "\r\n\r\n" + content + "\r\n--parts--\r\n")
	}
	for name, raw := range map[string][]byte{"direct": incomingTestMail(body), "attachment": multipart("attachment; filename=reply.ics", "base64", base64.StdEncoding.EncodeToString([]byte(body))), "inline": multipart("inline", "quoted-printable", strings.ReplaceAll(body, "=", "=3D"))} {
		t.Run(name, func(t *testing.T) {
			reply, err := ExtractCalendarIncomingReply(raw)
			if err != nil || reply == nil || reply.UID != "meeting-uid" || reply.Attendee != "guest@example.com" || reply.Sequence != 2 || reply.Status != "ACCEPTED" {
				t.Fatalf("reply=%+v err=%v", reply, err)
			}
		})
	}
	for name, change := range map[string]func(string) string{
		"foreign attendee": func(s string) string { return strings.Replace(s, "guest@example.com", "other@example.com", 1) },
		"duplicate attendee": func(s string) string {
			return strings.Replace(s, "END:VEVENT", "ATTENDEE;PARTSTAT=DECLINED:mailto:guest@example.com\r\nEND:VEVENT", 1)
		},
		"delegation": func(s string) string {
			return strings.Replace(s, "PARTSTAT=ACCEPTED", "PARTSTAT=ACCEPTED;SENT-BY=\"mailto:delegate@example.com\"", 1)
		},
		"repeat":            func(s string) string { return strings.Replace(s, "END:VEVENT", "RRULE:FREQ=DAILY\r\nEND:VEVENT", 1) },
		"sequence":          func(s string) string { return strings.Replace(s, "SEQUENCE:2", "SEQUENCE:-1", 1) },
		"missing timestamp": func(s string) string { return strings.Replace(s, "DTSTAMP:", "X-IGNORE:", 1) },
		"unknown response":  func(s string) string { return strings.Replace(s, "PARTSTAT=ACCEPTED", "PARTSTAT=DELEGATED", 1) },
		"extra calendar":    func(s string) string { return s + s },
		"duplicate status": func(s string) string {
			return strings.Replace(s, "PARTSTAT=ACCEPTED", "PARTSTAT=ACCEPTED;PARTSTAT=DECLINED", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if reply, err := ExtractCalendarIncomingReply(incomingTestMail(change(body))); err == nil && reply != nil {
				t.Fatal("accepted unsafe reply")
			}
		})
	}
	if reply, err := ExtractCalendarIncomingReply(incomingTestMail(strings.Replace(body, "METHOD:REPLY", "METHOD:REQUEST", 1))); err != nil || reply != nil {
		t.Fatal("REQUEST interpreted as a response")
	}
	forward := []byte("From: guest@example.com\r\nContent-Type: message/rfc822\r\n\r\n" + string(incomingTestMail(body)))
	if reply, _ := ExtractCalendarIncomingReply(forward); reply != nil {
		t.Fatal("forwarded message interpreted as guest response")
	}
	duplicate := []byte("From: attacker@example.com\r\n" + string(incomingTestMail(body)))
	if reply, _ := ExtractCalendarIncomingReply(duplicate); reply != nil {
		t.Fatal("ambiguous sender accepted")
	}
	if reply, _ := ExtractCalendarIncomingReply(bytes.Repeat([]byte("x"), CalendarIncomingMaxSize+1)); reply != nil {
		t.Fatal("oversized message accepted")
	}
}

func TestCalendarIncomingDKIMAuthentication(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{42}, ed25519.SeedSize))
	lookup := func(ctx context.Context, name string) ([]string, error) {
		return []string{"v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))}, nil
	}
	sign := func(domain string, headers []string) []byte {
		var out bytes.Buffer
		if err := dkim.Sign(&out, bytes.NewReader(incomingTestMail(incomingTestCalendar())), &dkim.SignOptions{Domain: domain, Selector: "test", Signer: key, HeaderKeys: headers}); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	raw := sign("example.com", nil)
	if err := VerifyCalendarIncomingSender(t.Context(), raw, "guest@example.com", lookup); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"forged authentication results": append([]byte("Authentication-Results: trusted.example.com; dkim=pass\r\n"), incomingTestMail(incomingTestCalendar())...),
		"foreign signer":                sign("attacker.example.net", nil),
		"unsigned MIME headers":         sign("example.com", []string{"From"}),
		"tampered reply":                []byte(strings.Replace(string(raw), "PARTSTAT=ACCEPTED", "PARTSTAT=DECLINED", 1)),
		"partial body":                  []byte(strings.Replace(string(raw), "v=1;", "v=1; l=10;", 1)),
	} {
		t.Run(name, func(t *testing.T) {
			if err := VerifyCalendarIncomingSender(t.Context(), raw, "guest@example.com", lookup); !errors.Is(err, ErrCalendarReplyAuthentication) {
				t.Fatalf("unsafe signature accepted: %v", err)
			}
		})
	}
	if err := VerifyCalendarIncomingSender(t.Context(), raw, "guest@example.com", func(context.Context, string) ([]string, error) { return nil, fmt.Errorf("DNS temporarily unavailable") }); !errors.Is(err, ErrCalendarReplyAuthenticationTemporary) {
		t.Fatalf("DNS failure not retryable: %v", err)
	}
}
