package imap

import (
	"crypto/tls"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestConnectWithConfigRejectsUnencryptedTLSModes(t *testing.T) {
	for _, mode := range []string{"none", "", "optional"} {
		t.Run(mode, func(t *testing.T) {
			client, err := ConnectWithConfig(&models.AccountConfig{
				IMAPHost:    "127.0.0.1",
				IMAPPort:    1,
				IMAPTLSMode: mode,
			}, "secret", nil)
			if client != nil {
				_ = client.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "requires an encrypted connection") {
				t.Fatalf("ConnectWithConfig(mode=%q) error = %v, want TLS requirement", mode, err)
			}
		})
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
