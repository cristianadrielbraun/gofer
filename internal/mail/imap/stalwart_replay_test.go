package imap

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// startReplayIMAPServer answers like the server a transcript in testdata was
// taken from: its capabilities, its NAMESPACE reply and its folder listing,
// to any LIST. It accepts any sign-in.
func startReplayIMAPServer(t *testing.T, transcript string) (*models.AccountConfig, *commandRecorder) {
	t.Helper()
	data, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	var caps, namespace string
	var list []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "CAPABILITY "):
			caps = strings.TrimPrefix(line, "CAPABILITY ")
		case strings.HasPrefix(line, "* NAMESPACE "):
			namespace = line
		case strings.HasPrefix(line, "* LIST "):
			list = append(list, line)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	recorder := &commandRecorder{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				_, _ = io.WriteString(conn, "* OK [CAPABILITY "+caps+"] ready\r\n")
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					_, _ = recorder.write([]byte(line))
					tag, command, _ := strings.Cut(line, " ")
					reply := tag + " OK done\r\n"
					switch name, _, _ := strings.Cut(strings.TrimSpace(command), " "); strings.ToUpper(name) {
					case "CAPABILITY":
						reply = "* CAPABILITY " + caps + "\r\n" + reply
					case "ID":
						reply = "* ID NIL\r\n" + reply
					case "NAMESPACE":
						reply = namespace + "\r\n" + reply
					case "LIST":
						reply = strings.Join(list, "\r\n") + "\r\n" + reply
					case "LOGOUT":
						_, _ = io.WriteString(conn, "* BYE\r\n"+reply)
						return
					}
					if _, err := io.WriteString(conn, reply); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	host, portString, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portString)
	return &models.AccountConfig{
		AccountID: "acc", IMAPHost: host, IMAPAllowPrivate: true, IMAPPort: port,
		IMAPTLSMode: "plaintext", IMAPAllowPlaintext: true,
		AuthMethod: "plain", Username: "gofer-test@example.com",
	}, recorder
}

// Stalwart lists the mailboxes of the groups a person is in under its "other
// users" namespace, "Shared Folders", with the same special folders as the
// person's own. Its NAMESPACE support comes with IMAP4rev2 rather than its
// own capability.
func TestListFoldersPlacesStalwartSharedMailboxes(t *testing.T) {
	cfg, recorder := startReplayIMAPServer(t, "testdata/stalwart-shared-folders.txt")
	client, err := NewClient(context.Background(), cfg, "secret")
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.Close()
	folders, err := client.ListFolders(context.Background())
	if err != nil {
		t.Fatalf("ListFolders() error = %v", err)
	}
	if !client.SharedMailboxesComplete() {
		t.Fatalf("listing not taken as complete; client sent:\n%s", recorder.String())
	}

	type placed struct{ role, mailbox, sharedRole string }
	got := map[string]placed{}
	for _, f := range folders {
		got[f.Name] = placed{f.Role, f.SharedMailbox, f.SharedRole}
	}
	want := map[string]placed{
		"INBOX":          {"inbox", "", ""},
		"Sent Items":     {"sent", "", ""},
		"Drafts":         {"drafts", "", ""},
		"Junk Mail":      {"junk", "", ""},
		"Deleted Items":  {"trash", "", ""},
		"Shared Folders": {"custom", "", SharedRoleNamespace},
	}
	for _, mailbox := range []string{"sales-test@example.com", "marketing-test@example.com", "support-test@example.com"} {
		base := "Shared Folders/" + mailbox
		want[base] = placed{"custom", mailbox, SharedRoleRoot}
		want[base+"/INBOX"] = placed{"custom", mailbox, "inbox"}
		want[base+"/Sent Items"] = placed{"custom", mailbox, "sent"}
		want[base+"/Drafts"] = placed{"custom", mailbox, "drafts"}
		want[base+"/Junk Mail"] = placed{"custom", mailbox, "junk"}
		want[base+"/Deleted Items"] = placed{"custom", mailbox, "trash"}
	}
	for name, w := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("%q was not listed", name)
		} else if g != w {
			t.Errorf("%q = %+v, want %+v", name, g, w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("listed %d folders, want %d", len(got), len(want))
	}
}
