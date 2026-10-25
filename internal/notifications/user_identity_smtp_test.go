package notifications

import (
	"bufio"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// acceptingSMTPLogins is an SMTP server that lets any login in.
func acceptingSMTPLogins(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				r := textproto.NewReader(bufio.NewReader(conn))
				write := func(s string) { _, _ = io.WriteString(conn, s+"\r\n") }
				write("220 fixture ready")
				for {
					line, err := r.ReadLine()
					if err != nil {
						return
					}
					switch {
					case strings.HasPrefix(line, "EHLO "):
						write("250-fixture\r\n250 AUTH PLAIN")
					case strings.HasPrefix(line, "AUTH PLAIN "):
						write("235 authenticated")
					case line == "QUIT":
						write("221 bye")
						return
					default:
						write("250 ok")
					}
				}
			}(conn)
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func TestUserIdentityOwnSMTPServer(t *testing.T) {
	f, _, _ := newUserComposeFixture(t)
	port := acceptingSMTPLogins(t)
	if err := f.system.AddPlaintextTransportException(t.Context(), "smtp", "127.0.0.1", port, "administrator"); err != nil {
		t.Fatal(err)
	}
	if err := f.system.AddPrivateTargetException(t.Context(), "smtp", "127.0.0.1", port, "administrator"); err != nil {
		t.Fatal(err)
	}
	alice := f.accounts["alice"].ID
	form := url.Values{"name": {"Alice Shop"}, "email": {"alice-shop@example.com"}, "send_through": {"smtp"}, "smtp_host": {"127.0.0.1"}, "smtp_port": {strconv.Itoa(port)}, "smtp_tls_mode": {"plaintext"}, "smtp_username": {"alice-shop"}, "smtp_password": {"shop-secret"}}

	if body := f.request("alice", "POST", "/api/accounts/"+alice+"/identities/smtp-test", form.Encode()).Body.String(); !strings.Contains(body, `data-account-identity-smtp-test-result="ok"`) {
		t.Fatalf("test = %q", body)
	}
	if rec := f.request("bob", "POST", "/api/accounts/"+alice+"/identities/smtp-test", form.Encode()); rec.Code != 404 {
		t.Fatalf("bob tests alice's account = %d", rec.Code)
	}
	if body := f.request("alice", "POST", "/api/accounts/"+alice+"/identities", form.Encode()).Body.String(); strings.Contains(body, "data-account-identity-error") {
		t.Fatalf("save = %q", body)
	}
	var shop models.Identity
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
		list, err := db.ListAccountIdentities(t.Context(), "alice", alice)
		if len(list) == 1 {
			shop = list[0]
		}
		return err
	}); err != nil || !shop.HasOwnSMTP() || !shop.HasSMTPPassword || shop.SMTPUsername != "alice-shop" {
		t.Fatalf("saved = %+v, %v", shop, err)
	}
	// The stored password tests the server, without it being sent back.
	form.Set("id", shop.ID)
	form.Del("smtp_password")
	if body := f.request("alice", "POST", "/api/accounts/"+alice+"/identities/smtp-test", form.Encode()).Body.String(); !strings.Contains(body, `data-account-identity-smtp-test-result="ok"`) {
		t.Fatalf("test with the stored password = %q", body)
	}
	if body := f.request("alice", "GET", "/api/accounts/"+alice+"/identities/panel?edit="+shop.ID, "").Body.String(); strings.Contains(body, "shop-secret") || !strings.Contains(body, "Saved; type to replace") {
		t.Fatalf("edit form = %q", body)
	}
}
