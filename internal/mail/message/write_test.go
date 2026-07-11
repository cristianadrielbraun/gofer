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

func TestBuildMIMEMessageForIMAPDraftIncludesRevisionAndBcc(t *testing.T) {
	to, _ := ParseAddressList("recipient@example.com")
	bcc, _ := ParseAddressList("hidden@example.com")
	raw, err := BuildMIMEMessageForIMAPDraft(&OutgoingMessage{
		FromEmail: "sender@example.com",
		To:        to,
		Bcc:       bcc,
		MessageID: "<draft@example.com>",
		TextBody:  "draft body",
	}, "revision-1")
	if err != nil {
		t.Fatalf("BuildMIMEMessageForIMAPDraft() error = %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "\r\nBcc: hidden@example.com\r\n") || !strings.Contains(text, "\r\nX-Gofer-Draft-Revision: revision-1\r\n") {
		t.Fatalf("draft MIME = %q, want Bcc and revision headers", text)
	}
}
