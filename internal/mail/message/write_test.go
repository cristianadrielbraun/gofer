package message

import (
	"strings"
	"testing"
)

func TestBuildMIMEMessageOmitsBccForSMTPEnvelope(t *testing.T) {
	to, _ := ParseAddressList("to@example.com")
	bcc, _ := ParseAddressList("hidden@example.com")

	raw, err := BuildMIMEMessage(&OutgoingMessage{
		FromEmail: "sender@example.com",
		To:        to,
		Bcc:       bcc,
		Subject:   "SMTP",
		TextBody:  "Body",
		MessageID: "<smtp@example.com>",
	})
	if err != nil {
		t.Fatalf("BuildMIMEMessage() error = %v", err)
	}
	if strings.Contains(string(raw), "\r\nBcc:") {
		t.Fatalf("BuildMIMEMessage() included Bcc header: %q", string(raw))
	}
}

func TestBuildMIMEMessageForGraphIncludesBcc(t *testing.T) {
	to, _ := ParseAddressList("to@example.com")
	bcc, _ := ParseAddressList("hidden@example.com")

	raw, err := BuildMIMEMessageForGraph(&OutgoingMessage{
		FromEmail: "sender@example.com",
		To:        to,
		Bcc:       bcc,
		Subject:   "Graph",
		TextBody:  "Body",
		MessageID: "<graph@example.com>",
	})
	if err != nil {
		t.Fatalf("BuildMIMEMessageForGraph() error = %v", err)
	}
	if !strings.Contains(string(raw), "\r\nBcc: hidden@example.com\r\n") {
		t.Fatalf("BuildMIMEMessageForGraph() did not include Bcc header: %q", string(raw))
	}
}
