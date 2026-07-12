package smtp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"testing"
	"time"

	gosmtp "github.com/emersion/go-smtp"

	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

func waitForSMTPServer(t *testing.T, serverErr <-chan error) {
	t.Helper()
	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("fake SMTP server: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("fake SMTP server did not observe the closed connection")
	}
}

func TestNewClientRejectsUnencryptedTLSModes(t *testing.T) {
	for _, mode := range []string{"none", "", "optional", "plaintext"} {
		t.Run(mode, func(t *testing.T) {
			client, err := NewClient(context.Background(), &models.AccountConfig{
				SMTPHost:    "127.0.0.1",
				SMTPPort:    1,
				SMTPTLSMode: mode,
			}, "secret")
			if client != nil {
				_ = client.Close()
			}
			if err == nil {
				t.Fatalf("NewClient(mode=%q) error = nil, want transport policy rejection", mode)
			}
			if mode == "plaintext" && !strings.Contains(err.Error(), "admin-approved server exception") {
				t.Fatalf("NewClient(mode=%q) error = %v, want exception requirement", mode, err)
			}
			if mode != "plaintext" && !strings.Contains(err.Error(), "requires an encrypted connection") {
				t.Fatalf("NewClient(mode=%q) error = %v, want TLS requirement", mode, err)
			}
		})
	}
}

func TestNewClientRejectsOAuthOverApprovedPlaintext(t *testing.T) {
	client, err := NewClient(context.Background(), &models.AccountConfig{
		SMTPHost:           "127.0.0.1",
		SMTPPort:           1,
		SMTPTLSMode:        "plaintext",
		SMTPAllowPlaintext: true,
		AuthMethod:         "oauth2",
	}, "token")
	if client != nil {
		_ = client.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "OAuth authentication is not allowed") {
		t.Fatalf("NewClient() error = %v, want OAuth plaintext rejection", err)
	}
}

func TestNewClientStopsWhenGreetingContextExpires(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	}()

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("split listener address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse listener port: %v", err)
	}
	cfg := &models.AccountConfig{
		SMTPHost:    host,
		SMTPPort:    port,
		SMTPTLSMode: "starttls",
		Username:    "user@example.com",
		AuthMethod:  "plain",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	client, err := NewClient(ctx, cfg, "password")
	if client != nil {
		_ = client.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("NewClient() error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("NewClient() returned after %v, want prompt cancellation", elapsed)
	}

	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("stalled greeting connection was not closed")
	}
}

func TestSendRawStopsWhenSMTPCommandContextExpires(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	protocolClient := gosmtp.NewClient(clientConn)
	protocolClient.CommandTimeout = time.Minute
	protocolClient.SubmissionTimeout = time.Minute
	client := &Client{conn: clientConn, client: protocolClient}
	defer client.Close()

	serverErr := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		reader := bufio.NewReader(serverConn)
		if _, err := fmt.Fprint(serverConn, "220 test ESMTP ready\r\n"); err != nil {
			serverErr <- err
			return
		}
		line, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "EHLO ") {
			serverErr <- fmt.Errorf("read EHLO %q: %w", line, err)
			return
		}
		if _, err := fmt.Fprint(serverConn, "250 test\r\n"); err != nil {
			serverErr <- err
			return
		}
		line, err = reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "MAIL FROM:") {
			serverErr <- fmt.Errorf("read MAIL %q: %w", line, err)
			return
		}
		_, _ = io.Copy(io.Discard, reader)
		serverErr <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	result, err := client.SendRaw(ctx, "sender@example.com", []string{"recipient@example.com"}, []byte("Subject: test\r\n\r\nbody\r\n"))
	if result != models.SendFailed {
		t.Fatalf("SendRaw() result = %v, want failed", result)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SendRaw() error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("SendRaw() returned after %v, want prompt cancellation", elapsed)
	}
	waitForSMTPServer(t, serverErr)
}

func TestSendRawClassifiesSMTPRepliesBeforeAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reply     string
		retryable bool
	}{
		{name: "temporary", reply: "451 4.3.0 try again later\r\n", retryable: true},
		{name: "permanent", reply: "550 5.1.0 sender rejected\r\n", retryable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			protocolClient := gosmtp.NewClient(clientConn)
			protocolClient.CommandTimeout = time.Second
			protocolClient.SubmissionTimeout = time.Second
			client := &Client{conn: clientConn, client: protocolClient}
			defer client.Close()

			serverErr := make(chan error, 1)
			go func() {
				defer serverConn.Close()
				reader := bufio.NewReader(serverConn)
				if _, err := fmt.Fprint(serverConn, "220 test ESMTP ready\r\n"); err != nil {
					serverErr <- err
					return
				}
				line, err := reader.ReadString('\n')
				if err != nil || !strings.HasPrefix(line, "EHLO ") {
					serverErr <- fmt.Errorf("read EHLO %q: %w", line, err)
					return
				}
				if _, err := fmt.Fprint(serverConn, "250 test\r\n"); err != nil {
					serverErr <- err
					return
				}
				line, err = reader.ReadString('\n')
				if err != nil || !strings.HasPrefix(line, "MAIL FROM:") {
					serverErr <- fmt.Errorf("read MAIL %q: %w", line, err)
					return
				}
				_, err = fmt.Fprint(serverConn, tc.reply)
				serverErr <- err
			}()

			result, err := client.SendRaw(context.Background(), "sender@example.com", []string{"recipient@example.com"}, []byte("Subject: test\r\n\r\nbody\r\n"))
			if result != models.SendFailed || err == nil {
				t.Fatalf("SendRaw() = %v, %v; want failed SMTP reply", result, err)
			}
			if got := IsRetryable(err); got != tc.retryable {
				t.Fatalf("IsRetryable(%v) = %v, want %v", err, got, tc.retryable)
			}
			waitForSMTPServer(t, serverErr)
		})
	}
}

func TestSendRawIsAmbiguousWhenFinalDeliveryReplyTimesOut(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	protocolClient := gosmtp.NewClient(clientConn)
	protocolClient.CommandTimeout = time.Minute
	protocolClient.SubmissionTimeout = time.Minute
	client := &Client{conn: clientConn, client: protocolClient}
	defer client.Close()

	serverErr := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		reader := bufio.NewReader(serverConn)
		steps := []struct {
			prefix string
			reply  string
		}{
			{prefix: "EHLO ", reply: "250 test\r\n"},
			{prefix: "MAIL FROM:", reply: "250 sender ok\r\n"},
			{prefix: "RCPT TO:", reply: "250 recipient ok\r\n"},
			{prefix: "DATA", reply: "354 send message\r\n"},
		}
		if _, err := fmt.Fprint(serverConn, "220 test ESMTP ready\r\n"); err != nil {
			serverErr <- err
			return
		}
		for _, step := range steps {
			line, err := reader.ReadString('\n')
			if err != nil || !strings.HasPrefix(line, step.prefix) {
				serverErr <- fmt.Errorf("read %s command %q: %w", step.prefix, line, err)
				return
			}
			if _, err := fmt.Fprint(serverConn, step.reply); err != nil {
				serverErr <- err
				return
			}
		}
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				serverErr <- fmt.Errorf("read message body: %w", err)
				return
			}
			if strings.TrimSpace(line) == "." {
				break
			}
		}
		_, _ = io.Copy(io.Discard, reader)
		serverErr <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	result, err := client.SendRaw(ctx, "sender@example.com", []string{"recipient@example.com"}, []byte("Subject: test\r\n\r\nbody\r\n"))
	if result != models.SendAmbiguous {
		t.Fatalf("SendRaw() result = %v, want ambiguous", result)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SendRaw() error = %v, want context deadline", err)
	}
	waitForSMTPServer(t, serverErr)
}

func TestSendRawClassifiesExplicitFinalDeliveryReply(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reply     string
		retryable bool
	}{
		{name: "temporary", reply: "451 4.3.0 delivery temporarily unavailable\r\n", retryable: true},
		{name: "permanent", reply: "550 5.7.1 message rejected\r\n", retryable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			protocolClient := gosmtp.NewClient(clientConn)
			protocolClient.CommandTimeout = time.Second
			protocolClient.SubmissionTimeout = time.Second
			client := &Client{conn: clientConn, client: protocolClient}
			defer client.Close()

			serverErr := make(chan error, 1)
			go func() {
				defer serverConn.Close()
				reader := bufio.NewReader(serverConn)
				steps := []struct {
					prefix string
					reply  string
				}{
					{prefix: "EHLO ", reply: "250 test\r\n"},
					{prefix: "MAIL FROM:", reply: "250 sender ok\r\n"},
					{prefix: "RCPT TO:", reply: "250 recipient ok\r\n"},
					{prefix: "DATA", reply: "354 send message\r\n"},
				}
				if _, err := fmt.Fprint(serverConn, "220 test ESMTP ready\r\n"); err != nil {
					serverErr <- err
					return
				}
				for _, step := range steps {
					line, err := reader.ReadString('\n')
					if err != nil || !strings.HasPrefix(line, step.prefix) {
						serverErr <- fmt.Errorf("read %s command %q: %w", step.prefix, line, err)
						return
					}
					if _, err := fmt.Fprint(serverConn, step.reply); err != nil {
						serverErr <- err
						return
					}
				}
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						serverErr <- err
						return
					}
					if strings.TrimSpace(line) == "." {
						break
					}
				}
				_, err := fmt.Fprint(serverConn, tc.reply)
				serverErr <- err
			}()

			result, err := client.SendRaw(context.Background(), "sender@example.com", []string{"recipient@example.com"}, []byte("Subject: test\r\n\r\nbody\r\n"))
			if result != models.SendFailed || err == nil {
				t.Fatalf("SendRaw() = %v, %v; want explicit final delivery failure", result, err)
			}
			if got := IsRetryable(err); got != tc.retryable {
				t.Fatalf("IsRetryable(%v) = %v, want %v", err, got, tc.retryable)
			}
			waitForSMTPServer(t, serverErr)
		})
	}
}

func TestSendRawRejectsMessageLargerThanAdvertisedSizeBeforeMAIL(t *testing.T) {
	client, serverErr := startSMTPConversation(t, "250-test ESMTP\r\n250 SIZE 10\r\n", func(reader *bufio.Reader, conn net.Conn) error {
		_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		line, err := reader.ReadString('\n')
		if err == nil {
			return fmt.Errorf("unexpected SMTP command after EHLO: %q", line)
		}
		if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
			return fmt.Errorf("read after EHLO: %w", err)
		}
		return nil
	})

	result, err := client.SendRaw(context.Background(), "sender@example.com", []string{"recipient@example.com"}, []byte("12345678901"))
	if result != models.SendFailed || err == nil {
		t.Fatalf("SendRaw() = %v, %v; want permanent pre-MAIL failure", result, err)
	}
	if IsRetryable(err) {
		t.Fatalf("IsRetryable(%v) = true, want permanent size failure", err)
	}
	if !strings.Contains(err.Error(), "11 bytes") || !strings.Contains(err.Error(), "10 bytes") {
		t.Fatalf("size error = %v, want both message and server sizes", err)
	}
	waitForSMTPServer(t, serverErr)
}

func TestSendRawAdvertisesExactMessageSize(t *testing.T) {
	mimeData := []byte("Subject: test\r\n\r\nbody\r\n")
	client, serverErr := startSMTPConversation(t, "250-test ESMTP\r\n250-SIZE 1024\r\n250 SMTPUTF8\r\n", func(reader *bufio.Reader, conn net.Conn) error {
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read MAIL: %w", err)
		}
		if !strings.HasPrefix(line, "MAIL FROM:<sender@example.com>") {
			return fmt.Errorf("MAIL = %q", line)
		}
		if !strings.Contains(line, fmt.Sprintf("SIZE=%d", len(mimeData))) {
			return fmt.Errorf("MAIL = %q, want exact SIZE=%d", line, len(mimeData))
		}
		if strings.Contains(line, "SMTPUTF8") {
			return fmt.Errorf("MAIL = %q, did not expect SMTPUTF8 for ASCII envelope", line)
		}
		if _, err := fmt.Fprint(conn, "250 sender ok\r\n"); err != nil {
			return err
		}
		line, err = reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "RCPT TO:<recipient@example.com>") {
			return fmt.Errorf("RCPT = %q: %w", line, err)
		}
		if _, err := fmt.Fprint(conn, "250 recipient ok\r\n"); err != nil {
			return err
		}
		line, err = reader.ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "DATA" {
			return fmt.Errorf("DATA = %q: %w", line, err)
		}
		if _, err := fmt.Fprint(conn, "354 send message\r\n"); err != nil {
			return err
		}
		if err := readSMTPData(reader, mimeData); err != nil {
			return err
		}
		_, err = fmt.Fprint(conn, "250 queued\r\n")
		return err
	})

	result, err := client.SendRaw(context.Background(), "sender@example.com", []string{"recipient@example.com"}, mimeData)
	if result != models.SendSuccess || err != nil {
		t.Fatalf("SendRaw() = %v, %v; want success", result, err)
	}
	waitForSMTPServer(t, serverErr)
}

func TestSendRawWorksWithoutAdvertisedSize(t *testing.T) {
	mimeData := []byte("Subject: test\r\n\r\nbody\r\n")
	client, serverErr := startSMTPConversation(t, "250 ESMTP\r\n", func(reader *bufio.Reader, conn net.Conn) error {
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read MAIL: %w", err)
		}
		if strings.Contains(line, "SIZE=") {
			return fmt.Errorf("MAIL = %q, did not expect SIZE without capability", line)
		}
		if _, err := fmt.Fprint(conn, "250 sender ok\r\n"); err != nil {
			return err
		}
		line, err = reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "RCPT TO:") {
			return fmt.Errorf("RCPT = %q: %w", line, err)
		}
		if _, err := fmt.Fprint(conn, "250 recipient ok\r\n"); err != nil {
			return err
		}
		line, err = reader.ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "DATA" {
			return fmt.Errorf("DATA = %q: %w", line, err)
		}
		if _, err := fmt.Fprint(conn, "354 send message\r\n"); err != nil {
			return err
		}
		if err := readSMTPData(reader, mimeData); err != nil {
			return err
		}
		_, err = fmt.Fprint(conn, "250 queued\r\n")
		return err
	})

	result, err := client.SendRaw(context.Background(), "sender@example.com", []string{"recipient@example.com"}, mimeData)
	if result != models.SendSuccess || err != nil {
		t.Fatalf("SendRaw() = %v, %v; want success", result, err)
	}
	waitForSMTPServer(t, serverErr)
}

func TestSendRawTimingReportsOneConnectionAndTransaction(t *testing.T) {
	mimeData := []byte("Subject: timing\r\n\r\nbody\r\n")
	client, serverErr := startSMTPConversation(t, "250 ESMTP\r\n", func(reader *bufio.Reader, conn net.Conn) error {
		line, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "MAIL FROM:") {
			return fmt.Errorf("read MAIL: %q: %w", line, err)
		}
		if _, err := fmt.Fprint(conn, "250 sender ok\r\n"); err != nil {
			return err
		}
		line, err = reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "RCPT TO:") {
			return fmt.Errorf("read RCPT: %q: %w", line, err)
		}
		if _, err := fmt.Fprint(conn, "250 recipient ok\r\n"); err != nil {
			return err
		}
		line, err = reader.ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "DATA" {
			return fmt.Errorf("read DATA: %q: %w", line, err)
		}
		if _, err := fmt.Fprint(conn, "354 send message\r\n"); err != nil {
			return err
		}
		if err := readSMTPData(reader, mimeData); err != nil {
			return err
		}
		_, err = fmt.Fprint(conn, "250 queued\r\n")
		return err
	})

	result, err, timing := client.sendRawWithTiming(context.Background(), "sender@example.com", []string{"recipient@example.com"}, mimeData)
	if result != models.SendSuccess || err != nil {
		t.Fatalf("sendRawWithTiming() = %v, %v; want success", result, err)
	}
	if !timing.ConnectionEstablished || timing.MessagesPerConnection != 1 {
		t.Fatalf("timing connection usage = %#v, want one established connection and one message", timing)
	}
	if timing.Data <= 0 || timing.Total < timing.Data {
		t.Fatalf("timing durations = %#v, want positive transaction and total duration", timing)
	}
	waitForSMTPServer(t, serverErr)
}

func TestSendRawRequestsSMTPUTF8ForInternationalizedEnvelope(t *testing.T) {
	client, serverErr := startSMTPConversation(t, "250-test ESMTP\r\n250 SMTPUTF8\r\n", func(reader *bufio.Reader, conn net.Conn) error {
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read MAIL: %w", err)
		}
		if !strings.Contains(line, "SMTPUTF8") {
			return fmt.Errorf("MAIL = %q, want SMTPUTF8", line)
		}
		if _, err := fmt.Fprint(conn, "250 sender ok\r\n"); err != nil {
			return err
		}
		line, err = reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "RCPT TO:") {
			return fmt.Errorf("RCPT = %q: %w", line, err)
		}
		if _, err := fmt.Fprint(conn, "250 recipient ok\r\n"); err != nil {
			return err
		}
		line, err = reader.ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "DATA" {
			return fmt.Errorf("DATA = %q: %w", line, err)
		}
		if _, err := fmt.Fprint(conn, "354 send message\r\n"); err != nil {
			return err
		}
		if err := readSMTPData(reader, []byte("Subject: test\r\n\r\nbody\r\n")); err != nil {
			return err
		}
		_, err = fmt.Fprint(conn, "250 queued\r\n")
		return err
	})

	result, err := client.SendRaw(context.Background(), "sender@example.com", []string{"récipient@example.com"}, []byte("Subject: test\r\n\r\nbody\r\n"))
	if result != models.SendSuccess || err != nil {
		t.Fatalf("SendRaw() = %v, %v; want success", result, err)
	}
	waitForSMTPServer(t, serverErr)
}

func TestSendRawDoesNotRequestSMTPUTF8ForUnicodeMIMEHeaders(t *testing.T) {
	client, serverErr := startSMTPConversation(t, "250-test ESMTP\r\n250 SMTPUTF8\r\n", func(reader *bufio.Reader, conn net.Conn) error {
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read MAIL: %w", err)
		}
		if strings.Contains(line, "SMTPUTF8") {
			return fmt.Errorf("MAIL = %q, did not expect SMTPUTF8 for ASCII envelope", line)
		}
		if _, err := fmt.Fprint(conn, "250 sender ok\r\n"); err != nil {
			return err
		}
		line, err = reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "RCPT TO:") {
			return fmt.Errorf("RCPT = %q: %w", line, err)
		}
		if _, err := fmt.Fprint(conn, "250 recipient ok\r\n"); err != nil {
			return err
		}
		line, err = reader.ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "DATA" {
			return fmt.Errorf("DATA = %q: %w", line, err)
		}
		if _, err := fmt.Fprint(conn, "354 send message\r\n"); err != nil {
			return err
		}
		if err := readSMTPData(reader, []byte("Subject: =?UTF-8?Q?caf=C3=A9?=\r\n\r\nbody\r\n")); err != nil {
			return err
		}
		_, err = fmt.Fprint(conn, "250 queued\r\n")
		return err
	})

	result, err := client.SendRaw(context.Background(), "sender@example.com", []string{"recipient@example.com"}, []byte("Subject: =?UTF-8?Q?caf=C3=A9?=\r\n\r\nbody\r\n"))
	if result != models.SendSuccess || err != nil {
		t.Fatalf("SendRaw() = %v, %v; want success", result, err)
	}
	waitForSMTPServer(t, serverErr)
}

func TestSendRawRejectsInternationalizedEnvelopeWithoutSMTPUTF8(t *testing.T) {
	client, serverErr := startSMTPConversation(t, "250-test ESMTP\r\n250 SIZE 1024\r\n", func(reader *bufio.Reader, conn net.Conn) error {
		_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		line, err := reader.ReadString('\n')
		if err == nil {
			return fmt.Errorf("unexpected SMTP command after EHLO: %q", line)
		}
		if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
			return fmt.Errorf("read after EHLO: %w", err)
		}
		return nil
	})

	result, err := client.SendRaw(context.Background(), "sender@example.com", []string{"скрытый@example.com"}, []byte("Subject: test\r\n\r\nbody\r\n"))
	if result != models.SendFailed || err == nil {
		t.Fatalf("SendRaw() = %v, %v; want permanent pre-MAIL failure", result, err)
	}
	if IsRetryable(err) {
		t.Fatalf("IsRetryable(%v) = true, want permanent SMTPUTF8 failure", err)
	}
	if !strings.Contains(err.Error(), "SMTPUTF8") {
		t.Fatalf("SMTPUTF8 error = %v, want capability explanation", err)
	}
	if !strings.Contains(err.Error(), "recipient address") {
		t.Fatalf("SMTPUTF8 error = %v, want recipient context", err)
	}
	if strings.Contains(err.Error(), "скрытый@example.com") {
		t.Fatalf("SMTPUTF8 error exposed the envelope address: %v", err)
	}
	waitForSMTPServer(t, serverErr)
}

func TestSendKeepsBccAsEnvelopeOnly(t *testing.T) {
	client, serverErr := startSMTPConversation(t, "250 ESMTP\r\n", func(reader *bufio.Reader, conn net.Conn) error {
		line, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "MAIL FROM:") {
			return fmt.Errorf("MAIL = %q: %w", line, err)
		}
		if _, err := fmt.Fprint(conn, "250 sender ok\r\n"); err != nil {
			return err
		}
		for _, want := range []string{"visible@example.com", "hidden@example.com"} {
			line, err = reader.ReadString('\n')
			if err != nil || !strings.Contains(line, "RCPT TO:<"+want+">") {
				return fmt.Errorf("RCPT = %q, want %s: %w", line, want, err)
			}
			if _, err := fmt.Fprint(conn, "250 recipient ok\r\n"); err != nil {
				return err
			}
		}
		line, err = reader.ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "DATA" {
			return fmt.Errorf("DATA = %q: %w", line, err)
		}
		if _, err := fmt.Fprint(conn, "354 send message\r\n"); err != nil {
			return err
		}
		var body strings.Builder
		for {
			line, err = reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("read message body: %w", err)
			}
			if line == ".\r\n" {
				break
			}
			body.WriteString(line)
		}
		if strings.Contains(body.String(), "hidden@example.com") {
			return fmt.Errorf("SMTP body exposed Bcc address: %q", body.String())
		}
		_, err = fmt.Fprint(conn, "250 queued\r\n")
		return err
	})

	result, err := client.Send(context.Background(), &message.OutgoingMessage{
		FromEmail: "sender@example.com",
		To:        []*mail.Address{{Address: "visible@example.com"}},
		Bcc:       []*mail.Address{{Address: "hidden@example.com"}},
		Subject:   "test",
		TextBody:  "body",
		MessageID: "<smtp-bcc@example.com>",
	})
	if result != models.SendSuccess || err != nil {
		t.Fatalf("Send() = %v, %v; want success", result, err)
	}
	waitForSMTPServer(t, serverErr)
}

func startSMTPConversation(t *testing.T, ehloReply string, conversation func(*bufio.Reader, net.Conn) error) (*Client, <-chan error) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	protocolClient := gosmtp.NewClient(clientConn)
	protocolClient.CommandTimeout = time.Second
	protocolClient.SubmissionTimeout = time.Second
	client := &Client{conn: clientConn, client: protocolClient}
	serverErr := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		reader := bufio.NewReader(serverConn)
		if _, err := fmt.Fprint(serverConn, "220 test ESMTP ready\r\n"); err != nil {
			serverErr <- err
			return
		}
		line, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "EHLO ") {
			serverErr <- fmt.Errorf("read EHLO %q: %w", line, err)
			return
		}
		if _, err := fmt.Fprint(serverConn, ehloReply); err != nil {
			serverErr <- err
			return
		}
		serverErr <- conversation(reader, serverConn)
	}()
	t.Cleanup(func() { _ = client.Close() })
	return client, serverErr
}

func readSMTPData(reader *bufio.Reader, want []byte) error {
	var body strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read message body: %w", err)
		}
		if line == ".\r\n" {
			break
		}
		body.WriteString(line)
	}
	if body.String() != string(want) {
		return fmt.Errorf("message body = %q, want %q", body.String(), want)
	}
	return nil
}
