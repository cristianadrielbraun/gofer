package smtp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	gosmtp "github.com/emersion/go-smtp"

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
	for _, mode := range []string{"none", "", "optional"} {
		t.Run(mode, func(t *testing.T) {
			client, err := NewClient(context.Background(), &models.AccountConfig{
				SMTPHost:    "127.0.0.1",
				SMTPPort:    1,
				SMTPTLSMode: mode,
			}, "secret")
			if client != nil {
				_ = client.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "requires an encrypted connection") {
				t.Fatalf("NewClient(mode=%q) error = %v, want TLS requirement", mode, err)
			}
		})
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
