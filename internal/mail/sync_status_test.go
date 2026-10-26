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
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// startCountingIMAPServer serves an in-memory mailbox through a proxy that
// counts SELECT and EXAMINE commands, the expensive part of a folder poll.
func startCountingIMAPServer(t *testing.T, caps goimap.CapSet, folders []string) (proxy, direct string, selects *atomic.Int32) {
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
		Caps:         caps,
	})
	serverListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(serverListener) }()
	t.Cleanup(func() { _ = server.Close() })

	selects = &atomic.Int32{}
	proxyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
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
			go func() { _, _ = io.Copy(down, up); _ = down.Close() }()
			go func() {
				defer up.Close()
				defer down.Close()
				reader := bufio.NewReader(down)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if fields := strings.Fields(line); len(fields) >= 2 && (strings.EqualFold(fields[1], "SELECT") || strings.EqualFold(fields[1], "EXAMINE")) {
						selects.Add(1)
					}
					if _, err := io.WriteString(up, line); err != nil {
						return
					}
				}
			}()
		}
	}()
	return proxyListener.Addr().String(), serverListener.Addr().String(), selects
}

// mailboxClient changes the mailbox the way another mail program would.
func mailboxClient(t *testing.T, addr string) *imapclient.Client {
	t.Helper()
	client, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Login("user@example.com", "secret").Wait(); err != nil {
		t.Fatal(err)
	}
	return client
}

func appendTestMessage(t *testing.T, client *imapclient.Client, folder string, n int, flags ...goimap.Flag) {
	t.Helper()
	raw := fmt.Sprintf("From: sender@example.com\r\nTo: user@example.com\r\nMessage-ID: <status-%d@example.com>\r\nSubject: Message %d\r\nDate: Thu, 01 Oct 2026 09:%02d:00 +0000\r\n\r\nBody %d\r\n", n, n, n%60, n)
	cmd := client.Append(folder, int64(len(raw)), &goimap.AppendOptions{Flags: flags})
	if _, err := cmd.Write([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestIMAPPollSkipsUnchangedFoldersAndNoticesChanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps goimap.CapSet
	}{
		{"status with folder list", goimap.CapSet{goimap.CapIMAP4rev1: {}, goimap.CapIMAP4rev2: {}}},
		{"status per folder", goimap.CapSet{goimap.CapIMAP4rev1: {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			folders := []string{"INBOX", "Archive", "Projects"}
			proxy, direct, selects := startCountingIMAPServer(t, tc.caps, folders)
			other := mailboxClient(t, direct)
			for i, folder := range folders {
				appendTestMessage(t, other, folder, i+1)
			}
			appendTestMessage(t, other, "INBOX", 4, goimap.FlagSeen)

			host, portString, _ := net.SplitHostPort(proxy)
			port, _ := strconv.Atoi(portString)
			db := newLabelSyncTestDB(t)
			if err := db.AddPlaintextTransportException(ctx, "imap", host, port, "default"); err != nil {
				t.Fatal(err)
			}
			if err := db.AddPrivateTargetException(ctx, "imap", host, port, "default"); err != nil {
				t.Fatal(err)
			}
			accounts, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
			if err != nil {
				t.Fatal(err)
			}
			account, err := accounts.CreateAccount(ctx, "default", &models.CreateAccountRequest{
				EmailAddress: "user@example.com", IMAPHost: host, IMAPPort: port, IMAPTLSMode: "plaintext",
				SMTPHost: "smtp.example.com", Username: "user@example.com", Password: "secret",
			})
			if err != nil {
				t.Fatal(err)
			}
			orchestrator := NewSyncOrchestrator(db, accounts, nil, nil)
			poll := func() int32 {
				t.Helper()
				before := selects.Load()
				if err := orchestrator.syncAccount(ctx, account.ID, true); err != nil {
					t.Fatalf("poll: %v", err)
				}
				return selects.Load() - before
			}
			inbox := storage.FolderIDForIdentity(account.ID, "imap", "INBOX")
			readState := func(subject string) (found, read, starred bool) {
				t.Helper()
				err := db.Read().QueryRow(`SELECT mfs.is_read, mfs.is_starred FROM message_folder_state mfs
					JOIN messages m ON m.id = mfs.message_id
					WHERE mfs.folder_id = ? AND m.subject = ? AND mfs.is_deleted = 0`, inbox, subject).Scan(&read, &starred)
				return err == nil, read, starred
			}

			poll() // Download everything.
			poll() // Without CONDSTORE every flag is read once per process.
			if n := poll(); n != 0 {
				t.Fatalf("unchanged mailbox: poll selected %d folders, want none", n)
			}

			appendTestMessage(t, other, "INBOX", 5)
			if n := poll(); n == 0 {
				t.Fatal("new mail: poll selected no folder")
			}
			if found, _, _ := readState("Message 5"); !found {
				t.Fatal("new mail not downloaded")
			}
			if n := poll(); n != 0 {
				t.Fatalf("after new mail was synced: poll selected %d folders, want none", n)
			}

			// Read elsewhere: the unread count changes.
			if _, err := other.Select("INBOX", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			if err := other.Store(goimap.UIDSetNum(1), &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Flags: []goimap.Flag{goimap.FlagSeen}}, nil).Close(); err != nil {
				t.Fatal(err)
			}
			poll()
			if _, read, _ := readState("Message 1"); !read {
				t.Fatal("message read elsewhere still unread")
			}

			// Starred elsewhere: invisible to STATUS without CONDSTORE, so it
			// waits for the next sweep of every flag.
			if err := other.Store(goimap.UIDSetNum(1), &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Flags: []goimap.Flag{goimap.FlagFlagged}}, nil).Close(); err != nil {
				t.Fatal(err)
			}
			poll()
			imapFlagSweeps.Store(inbox, time.Now().Add(-2*imapFlagSweepInterval))
			poll()
			if _, _, starred := readState("Message 1"); !starred {
				t.Fatal("message starred elsewhere not starred after the flag sweep")
			}

			// Deleted elsewhere: the message count changes.
			if err := other.Store(goimap.UIDSetNum(2), &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Flags: []goimap.Flag{goimap.FlagDeleted}}, nil).Close(); err != nil {
				t.Fatal(err)
			}
			if err := other.Expunge().Close(); err != nil {
				t.Fatal(err)
			}
			if err := other.Unselect().Wait(); err != nil {
				t.Fatal(err)
			}
			poll()
			if found, _, _ := readState("Message 4"); found {
				t.Fatal("message deleted elsewhere is still listed")
			}
			if n := poll(); n != 0 {
				t.Fatalf("after the deletion was synced: poll selected %d folders, want none", n)
			}

			// One message arrives as another unread one is deleted: the counts
			// match, only UIDNEXT tells.
			appendTestMessage(t, other, "INBOX", 6)
			if _, err := other.Select("INBOX", nil).Wait(); err != nil {
				t.Fatal(err)
			}
			// INBOX UIDs: 1 Message 1, 2 Message 4 (deleted above), 3 Message 5.
			if err := other.Store(goimap.UIDSetNum(3), &goimap.StoreFlags{Op: goimap.StoreFlagsAdd, Flags: []goimap.Flag{goimap.FlagDeleted}}, nil).Close(); err != nil {
				t.Fatal(err)
			}
			if err := other.Expunge().Close(); err != nil {
				t.Fatal(err)
			}
			if err := other.Unselect().Wait(); err != nil {
				t.Fatal(err)
			}
			poll()
			if found, _, _ := readState("Message 6"); !found {
				t.Fatal("mail arriving alongside a deletion was not downloaded")
			}
			if found, _, _ := readState("Message 5"); found {
				t.Fatal("message deleted alongside new mail is still listed")
			}

			var uidNext uint32
			if err := db.Read().QueryRow(`SELECT COALESCE(uid_next, 0) FROM folders WHERE id = ?`, inbox).Scan(&uidNext); err != nil || uidNext == 0 {
				t.Fatalf("UIDNEXT not recorded: %d %v", uidNext, err)
			}
		})
	}
}
