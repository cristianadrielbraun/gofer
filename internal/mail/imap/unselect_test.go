package imap

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// commandRecorder relays one IMAP connection and keeps what the client sent.
type commandRecorder struct {
	mu   sync.Mutex
	sent strings.Builder
}

func (r *commandRecorder) write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent.Write(p)
}

func (r *commandRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent.String()
}

type recorderWriter struct{ r *commandRecorder }

func (w recorderWriter) Write(p []byte) (int, error) { return w.r.write(p) }

// relayHidingCaps copies server responses, dropping the given capabilities
// from CAPABILITY lines. go-imap's server always advertises some extensions,
// such as UNSELECT, that real servers like imap.2925.com lack.
func relayHidingCaps(dst io.Writer, src io.Reader, hidden []goimap.Cap) {
	reader := bufio.NewReader(src)
	for {
		line, err := reader.ReadString('\n')
		if strings.Contains(line, "CAPABILITY") {
			for _, capability := range hidden {
				line = strings.ReplaceAll(line, " "+string(capability)+" ", " ")
				line = strings.ReplaceAll(line, " "+string(capability)+"]", "]")
				line = strings.ReplaceAll(line, " "+string(capability)+"\r\n", "\r\n")
			}
		}
		if _, writeErr := io.WriteString(dst, line); writeErr != nil || err != nil {
			return
		}
	}
}

func startRecordedIMAPServer(t *testing.T, caps goimap.CapSet, hidden ...goimap.Cap) (*models.AccountConfig, *commandRecorder) {
	t.Helper()
	return startRecordedIMAPServerWithMailboxes(t, caps, []string{"INBOX"}, hidden...)
}

func startRecordedIMAPServerWithMailboxes(t *testing.T, caps goimap.CapSet, mailboxes []string, hidden ...goimap.Cap) (*models.AccountConfig, *commandRecorder) {
	t.Helper()
	return startRecordedIMAPServerWithSpecialUse(t, caps, mailboxes, nil, hidden...)
}

// startRecordedIMAPServerWithSpecialUse is startRecordedIMAPServerWithMailboxes
// with special-use attributes on some of the mailboxes.
func startRecordedIMAPServerWithSpecialUse(t *testing.T, caps goimap.CapSet, mailboxes []string, specialUse map[string]goimap.MailboxAttr, hidden ...goimap.Cap) (*models.AccountConfig, *commandRecorder) {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("user@example.com", "secret")
	for _, mailbox := range mailboxes {
		if err := user.Create(mailbox, nil); err != nil {
			t.Fatalf("create %s: %v", mailbox, err)
		}
	}
	mem.AddUser(user)
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			if specialUse != nil {
				return &specialUseSession{Session: mem.NewSession(), mailboxes: mailboxes, specialUse: specialUse}, nil, nil
			}
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         caps,
	})
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = server.Serve(serverListener) }()
	t.Cleanup(func() { _ = server.Close() })

	recorder := &commandRecorder{}
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	t.Cleanup(func() { _ = proxyListener.Close() })
	go func() {
		for {
			down, err := proxyListener.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", serverListener.Addr().String())
			if err != nil {
				_ = down.Close()
				return
			}
			go func() { relayHidingCaps(down, up, hidden); _ = down.Close() }()
			go func() { _, _ = io.Copy(io.MultiWriter(up, recorderWriter{recorder}), down); _ = up.Close() }()
		}
	}()

	host, portString, err := net.SplitHostPort(proxyListener.Addr().String())
	if err != nil {
		t.Fatalf("split proxy address: %v", err)
	}
	port, err := strconv.Atoi(portString)
	if err != nil {
		t.Fatalf("parse proxy port: %v", err)
	}
	return &models.AccountConfig{
		AccountID: "acc",
		IMAPHost:  host, IMAPAllowPrivate: true,
		IMAPPort:           port,
		IMAPTLSMode:        "plaintext",
		IMAPAllowPlaintext: true,
		AuthMethod:         "plain",
		Username:           "user@example.com",
	}, recorder
}

func TestClientSendsUnselectOnlyWhenServerSupportsIt(t *testing.T) {
	tests := []struct {
		name   string
		caps   goimap.CapSet
		hidden []goimap.Cap
		want   bool
	}{
		{name: "IMAP4rev1 without UNSELECT", caps: goimap.CapSet{goimap.CapIMAP4rev1: {}}, hidden: []goimap.Cap{goimap.CapUnselect}, want: false},
		{name: "UNSELECT extension", caps: goimap.CapSet{goimap.CapIMAP4rev1: {}}, want: true},
		{name: "IMAP4rev2", caps: goimap.CapSet{goimap.CapIMAP4rev2: {}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, recorder := startRecordedIMAPServer(t, tt.caps, tt.hidden...)
			client, err := NewClient(context.Background(), cfg, "secret")
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			if _, err := client.FetchUIDSnapshot(context.Background(), "INBOX", 0); err != nil {
				t.Fatalf("FetchUIDSnapshot() error = %v", err)
			}
			// A second command proves the session survived and flushes the
			// deferred UNSELECT, if one was sent, through the proxy.
			if _, err := client.ListFolders(context.Background()); err != nil {
				t.Fatalf("ListFolders() error = %v", err)
			}
			client.Close()

			sent := strings.Contains(strings.ToUpper(recorder.String()), " UNSELECT\r\n")
			if sent != tt.want {
				t.Fatalf("UNSELECT sent = %v, want %v; client sent:\n%s", sent, tt.want, recorder.String())
			}
		})
	}
}

func TestClientConnectionLostAfterServerCloses(t *testing.T) {
	cfg, _ := startRecordedIMAPServer(t, goimap.CapSet{goimap.CapIMAP4rev1: {}})
	client, err := NewClient(context.Background(), cfg, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.Close()
	if client.ConnectionLost() {
		t.Fatal("ConnectionLost() = true for a fresh session")
	}
	if err := client.client.Logout().Wait(); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if _, err := client.FetchUIDSnapshot(context.Background(), "INBOX", 0); err == nil {
		t.Fatal("FetchUIDSnapshot() succeeded after the server closed the session")
	}
	if !client.ConnectionLost() {
		t.Fatal("ConnectionLost() = false after the server closed the session")
	}
}

// specialUseSession lists mailboxes with their special-use attributes, which
// the in-memory server cannot store, and like some servers sends them only
// when the client asks for them.
type specialUseSession struct {
	imapserver.Session
	mailboxes  []string
	specialUse map[string]goimap.MailboxAttr
}

func (s *specialUseSession) List(w *imapserver.ListWriter, ref string, patterns []string, options *goimap.ListOptions) error {
	for _, mailbox := range s.mailboxes {
		matches := false
		for _, pattern := range patterns {
			matches = matches || pattern == "*" || (pattern == "%" && !strings.Contains(mailbox, "/"))
		}
		if !matches {
			continue
		}
		data := &goimap.ListData{Mailbox: mailbox, Delim: '/'}
		if attr, ok := s.specialUse[mailbox]; ok && options != nil && options.ReturnSpecialUse {
			data.Attrs = append(data.Attrs, attr)
		}
		if err := w.WriteList(data); err != nil {
			return err
		}
	}
	return nil
}
