package imap

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// startInboxServer serves an INBOX holding count messages and returns their
// UIDs. With emptySearch, the proxy answers UID SEARCH ALL with an empty
// result while passing every other command through, as some servers do.
func startInboxServer(t *testing.T, count int, emptySearch bool) (*models.AccountConfig, []uint32, *commandRecorder) {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("user@example.com", "secret")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("create INBOX: %v", err)
	}
	mem.AddUser(user)
	listen := func(caps goimap.CapSet) string {
		server := imapserver.New(&imapserver.Options{
			NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
				return mem.NewSession(), nil, nil
			},
			InsecureAuth: true,
			Caps:         caps,
		})
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		go func() { _ = server.Serve(listener) }()
		t.Cleanup(func() { _ = server.Close() })
		return listener.Addr().String()
	}
	config := func(addr string) *models.AccountConfig {
		host, portString, _ := net.SplitHostPort(addr)
		port, _ := strconv.Atoi(portString)
		return &models.AccountConfig{
			AccountID: "acc", IMAPHost: host, IMAPPort: port,
			IMAPTLSMode: "plaintext", IMAPAllowPlaintext: true,
			AuthMethod: "plain", Username: "user@example.com",
		}
	}

	seed, err := NewClient(context.Background(), config(listen(goimap.CapSet{goimap.CapIMAP4rev1: {}, goimap.CapIMAP4rev2: {}})), "secret")
	if err != nil {
		t.Fatalf("seed client: %v", err)
	}
	var uids []uint32
	for i := range count {
		raw := []byte(fmt.Sprintf("From: sender@example.com\r\nMessage-ID: <inbox-%d@example.com>\r\nSubject: Message %d\r\n\r\nBody", i, i))
		result, err := seed.AppendMessage(context.Background(), "INBOX", raw, nil, time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("seed message %d: %v", i, err)
		}
		uids = append(uids, result.UID)
	}
	seed.Close()

	upstream := listen(goimap.CapSet{goimap.CapIMAP4rev1: {}})
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
			up, err := net.Dial("tcp", upstream)
			if err != nil {
				_ = down.Close()
				return
			}
			var writeMu sync.Mutex
			go func() {
				defer down.Close()
				reader := bufio.NewReader(up)
				for {
					line, err := reader.ReadString('\n')
					writeMu.Lock()
					_, writeErr := io.WriteString(down, line)
					writeMu.Unlock()
					if err != nil || writeErr != nil {
						return
					}
				}
			}()
			go func() {
				defer up.Close()
				reader := bufio.NewReader(down)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					_, _ = recorder.write([]byte(line))
					if fields := strings.Fields(line); emptySearch && len(fields) == 4 && strings.EqualFold(strings.Join(fields[1:], " "), "UID SEARCH ALL") {
						writeMu.Lock()
						_, _ = io.WriteString(down, "* SEARCH\r\n"+fields[0]+" OK SEARCH completed.\r\n")
						writeMu.Unlock()
						continue
					}
					if _, err := io.WriteString(up, line); err != nil {
						return
					}
				}
			}()
		}
	}()
	return config(proxyListener.Addr().String()), uids, recorder
}

func TestClientListsAllUIDsWhenServerSearchIsIncomplete(t *testing.T) {
	tests := []struct {
		name         string
		emptySearch  bool
		wantFallback bool
	}{
		{name: "compliant server", emptySearch: false, wantFallback: false},
		{name: "server answers SEARCH ALL with nothing", emptySearch: true, wantFallback: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, want, recorder := startInboxServer(t, 3, tt.emptySearch)
			client, err := NewClient(context.Background(), cfg, "secret")
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			defer client.Close()

			var synced []uint32
			result, err := client.SyncFolder(context.Background(), "acc_inbox", "INBOX", FolderSyncOptions{ChunkSize: 500}, func(msgs []storage.SyncMessage) error {
				for _, msg := range msgs {
					synced = append(synced, msg.RemoteUID)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("SyncFolder() error = %v", err)
			}
			slices.Sort(synced)
			if !slices.Equal(synced, want) || result.NumMessages != uint32(len(want)) {
				t.Fatalf("SyncFolder() synced UIDs %v (NumMessages %d), want %v", synced, result.NumMessages, want)
			}

			// Reconcile must see the same messages, or it would remove the
			// ones just synced as expunged.
			reconciled, _, _, err := client.FetchAllUIDs(context.Background(), "INBOX", result.UIDValidity)
			if err != nil {
				t.Fatalf("FetchAllUIDs() error = %v", err)
			}
			if !slices.Equal(reconciled, want) {
				t.Fatalf("FetchAllUIDs() = %v, want %v", reconciled, want)
			}

			fellBack := strings.Contains(strings.ToUpper(recorder.String()), "UID FETCH 1:* (UID)")
			if fellBack != tt.wantFallback {
				t.Fatalf("fell back to UID FETCH = %v, want %v; client sent:\n%s", fellBack, tt.wantFallback, recorder.String())
			}
		})
	}
}

func TestClientListsNoUIDsInEmptyMailboxWithoutFallback(t *testing.T) {
	cfg, _, recorder := startInboxServer(t, 0, true)
	client, err := NewClient(context.Background(), cfg, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.Close()

	uids, _, _, err := client.FetchAllUIDs(context.Background(), "INBOX", 0)
	if err != nil {
		t.Fatalf("FetchAllUIDs() error = %v", err)
	}
	if len(uids) != 0 {
		t.Fatalf("FetchAllUIDs() = %v, want none", uids)
	}
	if strings.Contains(strings.ToUpper(recorder.String()), "UID FETCH") {
		t.Fatalf("empty mailbox fell back to UID FETCH; client sent:\n%s", recorder.String())
	}
}
