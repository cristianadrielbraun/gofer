package imap

import (
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestConnectionProbeCancellationInterruptsGreeting(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	host, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- TestConnection(ctx, &models.AccountConfig{IMAPHost: host, IMAPPort: port, IMAPTLSMode: "plaintext", IMAPAllowPlaintext: true, AuthMethod: "plain", Username: "test"}, "synthetic-only")
	}()
	select {
	case conn := <-accepted:
		defer conn.Close()
	case <-time.After(time.Second):
		t.Fatal("probe did not connect")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled probe succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("probe ignored cancellation during greeting")
	}
}

func TestConnectWithConfigRejectsUnencryptedTLSModes(t *testing.T) {
	for _, mode := range []string{"none", "", "optional", "plaintext"} {
		t.Run(mode, func(t *testing.T) {
			client, err := ConnectWithConfig(&models.AccountConfig{
				IMAPHost:    "127.0.0.1",
				IMAPPort:    1,
				IMAPTLSMode: mode,
			}, "secret", nil)
			if client != nil {
				_ = client.Close()
			}
			if err == nil {
				t.Fatalf("ConnectWithConfig(mode=%q) error = nil, want transport policy rejection", mode)
			}
			if mode == "plaintext" && !strings.Contains(err.Error(), "admin-approved server exception") {
				t.Fatalf("ConnectWithConfig(mode=%q) error = %v, want exception requirement", mode, err)
			}
			if mode != "plaintext" && !strings.Contains(err.Error(), "requires an encrypted connection") {
				t.Fatalf("ConnectWithConfig(mode=%q) error = %v, want TLS requirement", mode, err)
			}
		})
	}
}

func TestConnectWithConfigRejectsOAuthOverApprovedPlaintext(t *testing.T) {
	client, err := ConnectWithConfig(&models.AccountConfig{
		IMAPHost:           "127.0.0.1",
		IMAPPort:           1,
		IMAPTLSMode:        "plaintext",
		IMAPAllowPlaintext: true,
		AuthMethod:         "oauth2",
	}, "token", nil)
	if client != nil {
		_ = client.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "OAuth authentication is not allowed") {
		t.Fatalf("ConnectWithConfig() error = %v, want OAuth plaintext rejection", err)
	}
}

func TestSecureClientOptionsEnforcesCertificateChecksAndTLS12(t *testing.T) {
	originalTLS := &tls.Config{
		ServerName:         "wrong.example.com",
		MinVersion:         tls.VersionTLS10,
		InsecureSkipVerify: true,
	}
	original := &imapclient.Options{TLSConfig: originalTLS}

	got := secureClientOptions("imap.example.com", original)

	if got == original {
		t.Fatal("secureClientOptions returned the caller's options instead of a copy")
	}
	if got.TLSConfig == originalTLS {
		t.Fatal("secureClientOptions returned the caller's TLS config instead of a copy")
	}
	if got.TLSConfig.ServerName != "imap.example.com" {
		t.Fatalf("ServerName = %q, want imap.example.com", got.TLSConfig.ServerName)
	}
	if got.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %d, want TLS 1.2", got.TLSConfig.MinVersion)
	}
	if got.TLSConfig.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify remained enabled")
	}
	if got.Dialer == nil {
		t.Fatal("Dialer is nil")
	}
	if originalTLS.ServerName != "wrong.example.com" || originalTLS.MinVersion != tls.VersionTLS10 || !originalTLS.InsecureSkipVerify {
		t.Fatal("secureClientOptions modified the caller's TLS config")
	}
}

func TestContextClientCancellationInterruptsConnectionSetup(t *testing.T) {
	for _, mode := range []string{"plaintext", "tls", "starttls"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			accepted := make(chan net.Conn, 1)
			go func() {
				conn, err := listener.Accept()
				if err == nil {
					accepted <- conn
				}
			}()
			host, portText, _ := net.SplitHostPort(listener.Addr().String())
			port, _ := strconv.Atoi(portText)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				client, err := NewContextClient(ctx, &models.AccountConfig{IMAPHost: host, IMAPPort: port, IMAPTLSMode: mode, IMAPAllowPlaintext: true, AuthMethod: "plain", Username: "test"}, "synthetic-only")
				if client != nil {
					client.Close()
				}
				result <- err
			}()
			var conn net.Conn
			select {
			case conn = <-accepted:
				defer conn.Close()
			case <-time.After(3 * time.Second):
				t.Fatal("connection not established")
			}
			// The server intentionally sends no greeting/TLS handshake. Cancellation
			// must interrupt setup, before a high-level Client can be returned.
			cancel()
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("cancelled setup succeeded")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancellation did not interrupt connection setup")
			}
		})
	}
}
