package imap

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// startScriptedIMAPServer answers one connection with the given capability
// list and accepts any login. It plays servers go-imap's own server can't,
// such as ones without AUTH=PLAIN.
func startScriptedIMAPServer(t *testing.T, caps string) (*models.AccountConfig, *commandRecorder) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	recorder := &commandRecorder{}
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		_, _ = io.WriteString(conn, "* OK ready\r\n")
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
			case "STATUS":
				// With the space before the paren that 139.com sends.
				reply = "* STATUS \"INBOX\" (MESSAGES 11 UIDNEXT 12 UIDVALIDITY 1 UNSEEN 7 )\r\n" + reply
			}
			if _, err := io.WriteString(conn, reply); err != nil {
				return
			}
		}
	}()
	host, portString, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portString)
	return &models.AccountConfig{
		AccountID: "acc", IMAPHost: host, IMAPAllowPrivate: true, IMAPPort: port,
		IMAPTLSMode: "plaintext", IMAPAllowPlaintext: true,
		AuthMethod: "plain", Username: "user@example.com",
	}, recorder
}

func TestClientUsesLOGINWhenServerOffersNoAuthPlain(t *testing.T) {
	tests := []struct {
		name string
		caps string
		want string
	}{
		{name: "AUTH=PLAIN", caps: "IMAP4rev1 SASL-IR AUTH=PLAIN", want: "AUTHENTICATE"},
		{name: "only XOAUTH2, like 163.com", caps: "IMAP4rev1 SASL-IR AUTH=XOAUTH2", want: "LOGIN"},
		{name: "no mechanism, like 139.com", caps: "IMAP4rev1", want: "LOGIN"},
		{name: "LOGINDISABLED", caps: "IMAP4rev1 LOGINDISABLED", want: "AUTHENTICATE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, recorder := startScriptedIMAPServer(t, tt.caps)
			client, err := NewClient(context.Background(), cfg, "secret")
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			client.Close()

			var got []string
			for _, line := range strings.Split(recorder.String(), "\r\n") {
				if fields := strings.Fields(line); len(fields) > 1 && (fields[1] == "LOGIN" || fields[1] == "AUTHENTICATE") {
					got = append(got, fields[1])
				}
			}
			if len(got) != 1 || got[0] != tt.want {
				t.Fatalf("login commands = %v, want [%s]; client sent:\n%s", got, tt.want, recorder.String())
			}
		})
	}
}

func TestClientIdentifiesItselfWhenServerSupportsID(t *testing.T) {
	for _, tt := range []struct {
		caps string
		want bool
	}{
		{caps: "IMAP4rev1 ID", want: true},
		{caps: "IMAP4rev1", want: false},
	} {
		t.Run(tt.caps, func(t *testing.T) {
			cfg, recorder := startScriptedIMAPServer(t, tt.caps)
			client, err := NewClient(context.Background(), cfg, "secret")
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			client.Close()

			if got := strings.Contains(recorder.String(), ` ID ("name" "Gofer"`); got != tt.want {
				t.Fatalf("sent ID = %v, want %v; client sent:\n%s", got, tt.want, recorder.String())
			}
		})
	}
}
