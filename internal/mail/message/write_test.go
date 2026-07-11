package message

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gomessage "github.com/emersion/go-message"
	gomail "github.com/emersion/go-message/mail"
)

func readBuiltMail(t *testing.T, raw []byte) *gomail.Reader {
	t.Helper()
	reader, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("mail.CreateReader() error = %v\n%s", err, raw)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return reader
}

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
	reader := readBuiltMail(t, raw)
	if reader.Header.Get("Bcc") != "" {
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
	reader := readBuiltMail(t, raw)
	bccHeader, err := reader.Header.AddressList("Bcc")
	if err != nil || len(bccHeader) != 1 || bccHeader[0].Address != "hidden@example.com" {
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
	reader := readBuiltMail(t, raw)
	bccHeader, err := reader.Header.AddressList("Bcc")
	if err != nil || len(bccHeader) != 1 || bccHeader[0].Address != "hidden@example.com" || reader.Header.Get("X-Gofer-Draft-Revision") != "revision-1" {
		t.Fatalf("draft MIME = %q, want Bcc and revision headers", raw)
	}
}

func TestBuildMIMEMessageEncodesUnicodeAndFoldsHeaders(t *testing.T) {
	to, _ := ParseAddressList(`"Žofie Příjemce" <recipient@example.com>`)
	subject := strings.Repeat("Dlouhý předmět s diakritikou a emoji 🚀 ", 8)
	body := "Dobrý den 👋\nDruhý řádek s diakritikou."
	raw, err := BuildMIMEMessage(&OutgoingMessage{
		FromName:  "Český Odesílatel",
		FromEmail: "sender@example.com",
		To:        to,
		Subject:   subject,
		TextBody:  body,
		MessageID: "<unicode@example.com>",
		Date:      time.Date(2026, time.July, 11, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("BuildMIMEMessage() error = %v", err)
	}
	if bytes.Contains(raw, []byte("Dobrý den")) || !bytes.Contains(raw, []byte("Content-Transfer-Encoding: quoted-printable")) {
		t.Fatalf("Unicode body was not quoted-printable encoded:\n%s", raw)
	}
	if hasBareLF(raw) {
		t.Fatalf("MIME contains a bare LF:\n%s", raw)
	}
	for _, line := range bytes.Split(raw, []byte("\r\n")) {
		if len(line) > 998 {
			t.Fatalf("MIME line length = %d, want <= 998", len(line))
		}
	}
	if !bytes.Contains(raw, []byte("\r\n ")) {
		t.Fatalf("long headers were not folded:\n%s", raw)
	}

	reader := readBuiltMail(t, raw)
	gotSubject, err := reader.Header.Subject()
	if err != nil || gotSubject != subject {
		t.Fatalf("decoded subject = %q, %v", gotSubject, err)
	}
	from, err := reader.Header.AddressList("From")
	if err != nil || len(from) != 1 || from[0].Name != "Český Odesílatel" {
		t.Fatalf("decoded From = %#v, %v", from, err)
	}
	part, err := reader.NextPart()
	if err != nil {
		t.Fatalf("NextPart() error = %v", err)
	}
	decodedBody, err := io.ReadAll(part.Body)
	if err != nil || string(decodedBody) != normalizeMIMEText(body) {
		t.Fatalf("decoded body = %q, %v", decodedBody, err)
	}
}

type parsedMIMELeaf struct {
	contentType string
	disposition string
	filename    string
	contentID   string
	body        []byte
}

func collectMIMELeaves(t *testing.T, entity *gomessage.Entity) []parsedMIMELeaf {
	t.Helper()
	if multipart := entity.MultipartReader(); multipart != nil {
		var leaves []parsedMIMELeaf
		for {
			part, err := multipart.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("NextPart() error = %v", err)
			}
			leaves = append(leaves, collectMIMELeaves(t, part)...)
		}
		return leaves
	}
	contentType, _, _ := entity.Header.ContentType()
	disposition, params, _ := entity.Header.ContentDisposition()
	body, err := io.ReadAll(entity.Body)
	if err != nil {
		t.Fatalf("read MIME leaf: %v", err)
	}
	return []parsedMIMELeaf{{
		contentType: contentType,
		disposition: disposition,
		filename:    params["filename"],
		contentID:   strings.Trim(entity.Header.Get("Content-ID"), "<>"),
		body:        body,
	}}
}

func mimeStructure(t *testing.T, entity *gomessage.Entity) string {
	t.Helper()
	contentType, _, _ := entity.Header.ContentType()
	disposition, _, _ := entity.Header.ContentDisposition()
	if disposition != "" {
		contentType += "[" + disposition + "]"
	}
	multipart := entity.MultipartReader()
	if multipart == nil {
		return contentType
	}
	var children []string
	for {
		part, err := multipart.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("NextPart() error = %v", err)
		}
		children = append(children, mimeStructure(t, part))
	}
	return contentType + "(" + strings.Join(children, ",") + ")"
}

func TestBuildMIMEMessagePreservesMultipartAndAttachmentSemantics(t *testing.T) {
	dir := t.TempDir()
	inlinePath := filepath.Join(dir, "logo.png")
	attachmentPath := filepath.Join(dir, "resume.txt")
	inlineData := []byte{0, 1, 2, 3, 254, 255}
	attachmentData := []byte("Příloha with Unicode\nSecond line")
	if err := os.WriteFile(inlinePath, inlineData, 0o600); err != nil {
		t.Fatalf("write inline fixture: %v", err)
	}
	if err := os.WriteFile(attachmentPath, attachmentData, 0o600); err != nil {
		t.Fatalf("write attachment fixture: %v", err)
	}
	to, _ := ParseAddressList("recipient@example.com")
	raw, err := BuildMIMEMessage(&OutgoingMessage{
		FromEmail: "sender@example.com",
		To:        to,
		Subject:   "Multipart",
		TextBody:  "Plain body",
		HTMLBody:  `<html><body>HTML <img src="cid:logo@example.com"></body></html>`,
		MessageID: "<multipart@example.com>",
		Attachments: []OutgoingAttachment{
			{Filename: "logo č.png", ContentType: "image/png", Path: inlinePath, ContentID: "logo@example.com", Inline: true},
			{Filename: "résumé.txt", ContentType: "text/plain; charset=utf-8", Path: attachmentPath},
		},
	})
	if err != nil {
		t.Fatalf("BuildMIMEMessage() error = %v", err)
	}
	root, err := gomessage.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("message.Read() error = %v\n%s", err, raw)
	}
	rootType, _, _ := root.Header.ContentType()
	if rootType != "multipart/mixed" {
		t.Fatalf("root Content-Type = %q", rootType)
	}
	structure := mimeStructure(t, root)
	wantStructure := "multipart/mixed(multipart/related(multipart/alternative(text/plain,text/html),image/png[inline]),text/plain[attachment])"
	if structure != wantStructure {
		t.Fatalf("MIME structure = %q, want %q", structure, wantStructure)
	}
	root, err = gomessage.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("message.Read() for leaves error = %v", err)
	}
	leaves := collectMIMELeaves(t, root)
	if len(leaves) != 4 {
		t.Fatalf("MIME leaves = %#v", leaves)
	}
	if leaves[0].contentType != "text/plain" || string(leaves[0].body) != "Plain body" {
		t.Fatalf("plain leaf = %#v", leaves[0])
	}
	if leaves[1].contentType != "text/html" || !strings.Contains(string(leaves[1].body), "cid:logo@example.com") {
		t.Fatalf("HTML leaf = %#v", leaves[1])
	}
	if leaves[2].contentType != "image/png" || leaves[2].disposition != "inline" || leaves[2].filename != "logo č.png" || leaves[2].contentID != "logo@example.com" || !bytes.Equal(leaves[2].body, inlineData) {
		t.Fatalf("inline leaf = %#v", leaves[2])
	}
	if leaves[3].contentType != "text/plain" || leaves[3].disposition != "attachment" || leaves[3].filename != "résumé.txt" || !bytes.Equal(leaves[3].body, attachmentData) {
		t.Fatalf("attachment leaf = %#v", leaves[3])
	}
}

func TestBuildMIMEMessageValidatesAllAttachmentsBeforeReturning(t *testing.T) {
	to, _ := ParseAddressList("recipient@example.com")
	_, err := BuildMIMEMessage(&OutgoingMessage{
		FromEmail: "sender@example.com",
		To:        to,
		MessageID: "<missing-attachment@example.com>",
		TextBody:  "Body",
		Attachments: []OutgoingAttachment{{
			Filename: "missing.txt", ContentType: "text/plain", Path: filepath.Join(t.TempDir(), "missing.txt"),
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "open attachment") {
		t.Fatalf("BuildMIMEMessage() error = %v, want missing attachment failure", err)
	}
}

func hasBareLF(raw []byte) bool {
	for i, b := range raw {
		if b == '\n' && (i == 0 || raw[i-1] != '\r') {
			return true
		}
	}
	return false
}
