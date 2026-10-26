package imap

import (
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
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
		result <- TestConnection(ctx, &models.AccountConfig{IMAPHost: host, IMAPAllowPrivate: true, IMAPPort: port, IMAPTLSMode: "plaintext", IMAPAllowPlaintext: true, AuthMethod: "plain", Username: "test"}, "synthetic-only")
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
				IMAPHost: "127.0.0.1", IMAPAllowPrivate: true,
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
		IMAPHost: "127.0.0.1", IMAPAllowPrivate: true,
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
				client, err := NewContextClient(ctx, &models.AccountConfig{IMAPHost: host, IMAPAllowPrivate: true, IMAPPort: port, IMAPTLSMode: mode, IMAPAllowPlaintext: true, AuthMethod: "plain", Username: "test"}, "synthetic-only")
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

func TestListFoldersUsesServerNamespaceForRoles(t *testing.T) {
	tests := []struct {
		name string
		caps goimap.CapSet
		want string
	}{
		// go-imap's server reports an empty personal prefix, so INBOX/Trash
		// is a folder inside the inbox.
		{name: "NAMESPACE", caps: goimap.CapSet{goimap.CapIMAP4rev1: {}, goimap.CapNamespace: {}}, want: "custom"},
		{name: "no NAMESPACE", caps: goimap.CapSet{goimap.CapIMAP4rev1: {}}, want: "trash"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, recorder := startRecordedIMAPServerWithMailboxes(t, tt.caps, []string{"INBOX", "INBOX/Trash"})
			client, err := NewClient(context.Background(), cfg, "secret")
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			defer client.Close()
			folders, err := client.ListFolders(context.Background())
			if err != nil {
				t.Fatalf("ListFolders() error = %v", err)
			}
			var got string
			for _, folder := range folders {
				if folder.Name == "INBOX/Trash" {
					got = folder.Role
				}
			}
			if got != tt.want {
				t.Fatalf("INBOX/Trash role = %q, want %q; client sent:\n%s", got, tt.want, recorder.String())
			}
		})
	}
}

// Gmail names its folders in the account's language, so only the special-use
// attributes say which is the Trash.
func TestListFoldersAsksForSpecialUseRoles(t *testing.T) {
	tests := []struct {
		name string
		caps goimap.CapSet
	}{
		{name: "SPECIAL-USE", caps: goimap.CapSet{goimap.CapIMAP4rev1: {}, goimap.CapSpecialUse: {}}},
		{name: "SPECIAL-USE and LIST-STATUS", caps: goimap.CapSet{goimap.CapIMAP4rev1: {}, goimap.CapSpecialUse: {}, goimap.CapListStatus: {}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, recorder := startRecordedIMAPServerWithSpecialUse(t, tt.caps,
				[]string{"INBOX", "[Gmail]", "[Gmail]/Corbeille", "[Gmail]/Messages envoyés", "Corbeille"},
				map[string]goimap.MailboxAttr{"[Gmail]/Corbeille": goimap.MailboxAttrTrash, "[Gmail]/Messages envoyés": goimap.MailboxAttrSent})
			client, err := NewClient(context.Background(), cfg, "secret")
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			defer client.Close()
			folders, err := client.ListFolders(context.Background())
			if err != nil {
				t.Fatalf("ListFolders() error = %v", err)
			}
			want := map[string]string{"[Gmail]/Corbeille": "trash", "[Gmail]/Messages envoyés": "sent", "Corbeille": "custom"}
			for _, folder := range folders {
				if role, ok := want[folder.Name]; ok {
					if folder.Role != role {
						t.Errorf("%s role = %q, want %q; client sent:\n%s", folder.Name, folder.Role, role, recorder.String())
					}
					delete(want, folder.Name)
				}
			}
			if len(want) > 0 {
				t.Fatalf("folders not listed: %v", want)
			}
		})
	}
}

// A folder made for a role is marked with it where the server can, and is
// known by its name either way.
func TestCreateRoleMailbox(t *testing.T) {
	tests := []struct {
		name   string
		caps   goimap.CapSet
		marked bool
	}{
		{name: "CREATE-SPECIAL-USE", caps: goimap.CapSet{goimap.CapIMAP4rev1: {}, goimap.CapCreateSpecialUse: {}}, marked: true},
		{name: "no CREATE-SPECIAL-USE", caps: goimap.CapSet{goimap.CapIMAP4rev1: {}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, recorder := startRecordedIMAPServerWithMailboxes(t, tt.caps, []string{"INBOX"})
			client, err := NewClient(context.Background(), cfg, "secret")
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			defer client.Close()
			name, err := client.CreateRoleMailbox(context.Background(), "trash")
			if err != nil || name != "Trash" {
				t.Fatalf("CreateRoleMailbox() = %q, %v", name, err)
			}
			if got := strings.Contains(recorder.String(), `CREATE "Trash" (USE (\Trash))`); got != tt.marked {
				t.Fatalf("marked = %v, want %v; client sent:\n%s", got, tt.marked, recorder.String())
			}
			// Asking again leaves the folder as it is.
			if _, err := client.CreateRoleMailbox(context.Background(), "trash"); err != nil {
				t.Fatalf("second CreateRoleMailbox() error = %v", err)
			}
			folders, err := client.ListFolders(context.Background())
			if err != nil {
				t.Fatalf("ListFolders() error = %v", err)
			}
			found := false
			for _, folder := range folders {
				found = found || (folder.Name == "Trash" && folder.Role == "trash")
			}
			if !found {
				t.Fatalf("folders = %+v, want Trash as the trash", folders)
			}
			if _, err := client.CreateRoleMailbox(context.Background(), "inbox"); err == nil {
				t.Fatal("a folder was made for the inbox")
			}
		})
	}
}
