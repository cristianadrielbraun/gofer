package handler

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// recordingSMTPServer accepts every message and records how each was sent.
type recordingSMTPServer struct {
	port int
	mu   sync.Mutex
	sent []recordedSMTPMessage
}

type recordedSMTPMessage struct {
	login, from string
	to          []string
	data        string
}

func newRecordingSMTPServer(t *testing.T) *recordingSMTPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	server := &recordingSMTPServer{port: listener.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.serve(conn)
		}
	}()
	return server
}

func (s *recordingSMTPServer) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := textproto.NewReader(bufio.NewReader(conn))
	write := func(line string) { _, _ = io.WriteString(conn, line+"\r\n") }
	write("220 fixture ready")
	var current recordedSMTPMessage
	for {
		line, err := r.ReadLine()
		if err != nil {
			return
		}
		switch {
		case strings.HasPrefix(line, "EHLO "):
			write("250-fixture\r\n250 AUTH PLAIN")
		case strings.HasPrefix(line, "AUTH PLAIN "):
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
			parts := strings.Split(string(raw), "\x00")
			current.login = strings.Join(parts[1:], ":")
			write("235 authenticated")
		case strings.HasPrefix(line, "MAIL FROM:"):
			current.from = strings.TrimPrefix(line, "MAIL FROM:")
			write("250 ok")
		case strings.HasPrefix(line, "RCPT TO:"):
			current.to = append(current.to, strings.TrimPrefix(line, "RCPT TO:"))
			write("250 ok")
		case line == "DATA":
			write("354 go ahead")
			data, _ := io.ReadAll(r.DotReader())
			current.data = string(data)
			s.mu.Lock()
			s.sent = append(s.sent, current)
			s.mu.Unlock()
			current = recordedSMTPMessage{login: current.login}
			write("250 queued")
		case line == "QUIT":
			write("221 bye")
			return
		default:
			write("250 ok")
		}
	}
}

func (s *recordingSMTPServer) messages() []recordedSMTPMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recordedSMTPMessage(nil), s.sent...)
}

// ownSMTPIdentity saves an identity of accountID that sends through server.
func ownSMTPIdentity(t *testing.T, h *Handler, userID, accountID string, server *recordingSMTPServer) models.Identity {
	t.Helper()
	ctx := context.Background()
	for _, add := range []func() error{
		func() error {
			return h.db.AddPlaintextTransportException(ctx, "smtp", "127.0.0.1", server.port, "administrator")
		},
		func() error {
			return h.db.AddPrivateTargetException(ctx, "smtp", "127.0.0.1", server.port, "administrator")
		},
	} {
		if err := add(); err != nil && !strings.Contains(err.Error(), "UNIQUE") {
			t.Fatal(err)
		}
	}
	identity, err := h.db.SaveAccountIdentityGuarded(ctx, userID, models.Identity{AccountID: accountID, Name: "Shop", Email: "shop@example.org", SMTPHost: "127.0.0.1", SMTPPort: server.port, SMTPTLSMode: "plaintext", SMTPUsername: "shop-login"}, nil)
	if err != nil {
		t.Fatalf("save identity: %v", err)
	}
	if err := h.accountStore.SetIdentitySMTPPassword(ctx, userID, accountID, identity.ID, "shop-secret", nil); err != nil {
		t.Fatalf("set password: %v", err)
	}
	return identity
}

func queueAsIdentity(t *testing.T, h *Handler, accountID string, identity models.Identity, subject string) storage.OutgoingSend {
	t.Helper()
	to, _ := message.ParseAddressList("recipient@example.com")
	msg := &message.OutgoingMessage{To: to, Subject: subject, TextBody: "Hello", MessageID: "<" + strings.ReplaceAll(subject, " ", "-") + "@example.org>", Date: time.Now().UTC()}
	identityComposeSender(identity).apply(msg)
	queued, err := h.queueOutgoingMessage(context.Background(), accountID, 0, "", msg, time.Now().Add(-time.Second), false)
	if err != nil {
		t.Fatalf("queueOutgoingMessage() error = %v", err)
	}
	return queued
}

func TestGmailIdentitySendsThroughItsOwnServer(t *testing.T) {
	ctx := context.Background()
	h, db, gmail := newGmailOutgoingTestHandler(t, "user@example.com")
	server := newRecordingSMTPServer(t)
	identity := ownSMTPIdentity(t, h, "default", "acc", server)

	queued := queueAsIdentity(t, h, "acc", identity, "From the shop")
	if queued.Transport != storage.OutgoingTransportSMTP || queued.EnvelopeFrom != "shop@example.org" {
		t.Fatalf("queued = %s from %s", queued.Transport, queued.EnvelopeFrom)
	}
	h.runDueOutgoingSends(ctx)

	sent := server.messages()
	if len(sent) != 1 || sent[0].login != "shop-login:shop-secret" || sent[0].from != "<shop@example.org>" || !strings.Contains(sent[0].data, `From: "Shop" <shop@example.org>`) {
		t.Fatalf("own server got %+v", sent)
	}
	if len(gmail.delivered) != 0 {
		t.Fatal("Gmail was asked to send it too")
	}
	completed, err := db.GetOutgoingSend(ctx, queued.ID)
	if err != nil || completed.Status != storage.OutgoingSendSent || completed.SentCopyStatus != storage.SentCopyPending {
		t.Fatalf("completed = %+v, %v", completed, err)
	}

	// Gmail files the copy in Sent without sending it. Its first answer is
	// lost; the retry finds the copy rather than filing a second one, and
	// the local copy is linked to it.
	gmail.mu.Lock()
	gmail.loseFileAnswer = true
	gmail.mu.Unlock()
	h.runDueSentCopies(ctx)
	if completed, _ = db.GetOutgoingSend(ctx, queued.ID); completed.SentCopyStatus != storage.SentCopyAmbiguous {
		t.Fatalf("Sent copy after a lost answer = %s", completed.SentCopyStatus)
	}
	if _, err := db.Write().Exec(`UPDATE outgoing_sends SET sent_copy_next_attempt_at = CURRENT_TIMESTAMP WHERE id = ?`, queued.ID); err != nil {
		t.Fatal(err)
	}
	h.runDueSentCopies(ctx)
	filed := gmail.filedCopies()
	if len(filed) != 1 || !strings.Contains(filed[0].raw, "Subject: From the shop") || len(filed[0].labels) != 1 || filed[0].labels[0] != "SENT" {
		t.Fatalf("filed = %+v", filed)
	}
	completed, _ = db.GetOutgoingSend(ctx, queued.ID)
	if completed.SentCopyStatus != storage.SentCopyComplete {
		t.Fatalf("Sent copy = %s %q", completed.SentCopyStatus, completed.SentCopyLastError)
	}
	var providerID string
	if err := db.Read().QueryRow(`SELECT COALESCE(s.remote_message_id, '') FROM message_folder_state s JOIN messages m ON m.id = s.message_id WHERE m.internet_message_id = ?`, "<From-the-shop@example.org>").Scan(&providerID); err != nil || providerID != "gmail-filed-1" {
		t.Fatalf("local Sent copy linked to %q, %v", providerID, err)
	}
}

func TestIMAPIdentitySendsThroughItsOwnServerAndKeepsASentCopy(t *testing.T) {
	ctx := context.Background()
	h, db := newAccountOwnershipTestHandler(t)
	server := newRecordingSMTPServer(t)
	identity := ownSMTPIdentity(t, h, "owner", "victim-account", server)

	queued := queueAsIdentity(t, h, "victim-account", identity, "Shop news")
	h.runDueOutgoingSends(ctx)
	if sent := server.messages(); len(sent) != 1 || sent[0].from != "<shop@example.org>" {
		t.Fatalf("own server got %+v", sent)
	}
	completed, err := db.GetOutgoingSend(ctx, queued.ID)
	if err != nil || completed.Status != storage.OutgoingSendSent || completed.SentCopyStatus != storage.SentCopyPending {
		t.Fatalf("completed = %+v, %v", completed, err)
	}
}

func TestIdentitySendFailsWhenItsServerIsGone(t *testing.T) {
	ctx := context.Background()
	h, db := newAccountOwnershipTestHandler(t)
	server := newRecordingSMTPServer(t)
	identity := ownSMTPIdentity(t, h, "owner", "victim-account", server)

	// Its password removed: nothing is sent, and the reason says so.
	noPassword := queueAsIdentity(t, h, "victim-account", identity, "No password")
	if err := h.accountStore.SetIdentitySMTPPassword(ctx, "owner", "victim-account", identity.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	h.runDueOutgoingSends(ctx)
	failed, _ := db.GetOutgoingSend(ctx, noPassword.ID)
	if failed.Status != storage.OutgoingSendFailed || !strings.Contains(failed.LastError, "no password stored") {
		t.Fatalf("without a password = %s %q", failed.Status, failed.LastError)
	}

	// Back on the account's server: still never sent the account's way.
	gone := queueAsIdentity(t, h, "victim-account", identity, "Server gone")
	identity.SMTPHost = ""
	if _, err := db.SaveAccountIdentityGuarded(ctx, "owner", identity, nil); err != nil {
		t.Fatal(err)
	}
	h.runDueOutgoingSends(ctx)
	failed, _ = db.GetOutgoingSend(ctx, gone.ID)
	if failed.Status != storage.OutgoingSendFailed || !strings.Contains(failed.LastError, "no longer set up") {
		t.Fatalf("without its server = %s %q", failed.Status, failed.LastError)
	}
	if sent := server.messages(); len(sent) != 0 {
		t.Fatalf("sent anyway: %+v", sent)
	}
}
