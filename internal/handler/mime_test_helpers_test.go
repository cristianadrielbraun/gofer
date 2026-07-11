package handler

import (
	"bytes"
	"strings"
	"testing"

	"github.com/emersion/go-message/mail"
)

func assertMIMEHeaders(t *testing.T, raw []byte, subject, messageID, bcc string) {
	t.Helper()
	reader, err := mail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("mail.CreateReader() error = %v\n%s", err, raw)
	}
	defer reader.Close()
	gotSubject, err := reader.Header.Subject()
	if err != nil || gotSubject != subject {
		t.Fatalf("MIME subject = %q, %v; want %q", gotSubject, err, subject)
	}
	gotMessageID, err := reader.Header.MessageID()
	if err != nil || gotMessageID == "" {
		t.Fatalf("MIME Message-ID = %q, %v; want a valid ID", gotMessageID, err)
	}
	if messageID != "" {
		if gotMessageID != strings.Trim(messageID, "<>") {
			t.Fatalf("MIME Message-ID = %q, %v; want %q", gotMessageID, err, messageID)
		}
	}
	bccList, err := reader.Header.AddressList("Bcc")
	if err != nil {
		t.Fatalf("MIME Bcc error = %v", err)
	}
	if bcc == "" {
		if len(bccList) != 0 {
			t.Fatalf("MIME Bcc = %#v, want none", bccList)
		}
		return
	}
	if len(bccList) != 1 || bccList[0].Address != bcc {
		t.Fatalf("MIME Bcc = %#v, want %q", bccList, bcc)
	}
}
