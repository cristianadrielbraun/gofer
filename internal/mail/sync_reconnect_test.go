package mail

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

// startDroppingIMAPServer serves the folders through a proxy that closes the
// first connection when it receives its dropAtSelect-th SELECT, as a server
// that disconnects mid-sync would. Later connections pass through.
func startDroppingIMAPServer(t *testing.T, folders []string, dropAtSelect int) (host string, port int, connections *atomic.Int32) {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("user@example.com", "secret")
	for _, folder := range folders {
		if err := user.Create(folder, nil); err != nil {
			t.Fatalf("create %s: %v", folder, err)
		}
	}
	mem.AddUser(user)
	server := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps:         goimap.CapSet{goimap.CapIMAP4rev1: {}, goimap.CapIMAP4rev2: {}},
	})
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = server.Serve(serverListener) }()
	t.Cleanup(func() { _ = server.Close() })

	serverHost, serverPortString, _ := net.SplitHostPort(serverListener.Addr().String())
	serverPort, _ := strconv.Atoi(serverPortString)
	seed, err := imap.NewClient(context.Background(), &models.AccountConfig{
		AccountID: "seed", IMAPHost: serverHost, IMAPPort: serverPort,
		IMAPTLSMode: "plaintext", IMAPAllowPlaintext: true,
		AuthMethod: "plain", Username: "user@example.com",
	}, "secret")
	if err != nil {
		t.Fatalf("seed client: %v", err)
	}
	for i, folder := range folders {
		raw := []byte(fmt.Sprintf("From: sender@example.com\r\nMessage-ID: <seed-%d@example.com>\r\nSubject: Seed %d\r\n\r\nBody", i, i))
		if _, err := seed.AppendMessage(context.Background(), folder, raw, nil, time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)); err != nil {
			t.Fatalf("seed %s: %v", folder, err)
		}
	}
	seed.Close()

	connections = &atomic.Int32{}
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
			first := connections.Add(1) == 1
			go func() { _, _ = io.Copy(down, up); _ = down.Close() }()
			go func() {
				defer up.Close()
				defer down.Close()
				reader := bufio.NewReader(down)
				selects := 0
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if fields := strings.Fields(line); len(fields) >= 2 && strings.EqualFold(fields[1], "SELECT") {
						selects++
						if first && selects == dropAtSelect {
							return
						}
					}
					if _, err := io.WriteString(up, line); err != nil {
						return
					}
				}
			}()
		}
	}()

	proxyHost, proxyPortString, _ := net.SplitHostPort(proxyListener.Addr().String())
	proxyPort, _ := strconv.Atoi(proxyPortString)
	return proxyHost, proxyPort, connections
}

func TestSyncAccountReconnectsWhenServerDropsConnectionMidSync(t *testing.T) {
	ctx := context.Background()
	folders := []string{"INBOX", "草稿箱", "已删除", "垃圾邮件"}
	host, port, connections := startDroppingIMAPServer(t, folders, 3)

	db := newLabelSyncTestDB(t)
	if err := db.AddPlaintextTransportException(ctx, "imap", host, port, "default"); err != nil {
		t.Fatalf("AddPlaintextTransportException() error = %v", err)
	}
	accountStore, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewAccountStore() error = %v", err)
	}
	account, err := accountStore.CreateAccount(ctx, "default", &models.CreateAccountRequest{
		EmailAddress: "user@example.com",
		IMAPHost:     host,
		IMAPPort:     port,
		IMAPTLSMode:  "plaintext",
		SMTPHost:     "smtp.example.com",
		Username:     "user@example.com",
		Password:     "secret",
	})
	if err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	orchestrator := NewSyncOrchestrator(db, accountStore, nil, nil)

	if err := orchestrator.syncAccount(ctx, account.ID, true); err != nil {
		t.Fatalf("syncAccount() error = %v", err)
	}
	if got := connections.Load(); got != 2 {
		t.Fatalf("connections = %d, want 2 (original and one reconnect)", got)
	}
	rows, err := db.Read().QueryContext(ctx, `
		SELECT id, remote_id, last_full_sync_at IS NOT NULL, COALESCE(sync_error, '')
		FROM folders WHERE account_id = ?`, account.ID)
	if err != nil {
		t.Fatalf("query folders: %v", err)
	}
	type folderState struct {
		id, remoteID, syncError string
		fullySynced             bool
	}
	var states []folderState
	for rows.Next() {
		var state folderState
		if err := rows.Scan(&state.id, &state.remoteID, &state.fullySynced, &state.syncError); err != nil {
			t.Fatalf("scan folder: %v", err)
		}
		states = append(states, state)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read folders: %v", err)
	}
	rows.Close()
	synced := 0
	for _, state := range states {
		messages, err := db.GetFolderEmailCount(ctx, state.id)
		if err != nil {
			t.Fatalf("GetFolderEmailCount(%s) error = %v", state.remoteID, err)
		}
		if !state.fullySynced || state.syncError != "" || messages != 1 {
			t.Errorf("folder %s: synced=%v error=%q messages=%d, want synced with 1 message", state.remoteID, state.fullySynced, state.syncError, messages)
		}
		synced++
	}
	if synced != len(folders) {
		t.Fatalf("synced folders = %d, want %d", synced, len(folders))
	}
}
