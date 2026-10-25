package notifications

import (
	"bufio"
	"io"
	"net"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// acceptingSMTPLogins is an SMTP server that lets any login in and accepts
// every message.
func acceptingSMTPLogins(t *testing.T) int {
	port, _ := recordingSMTPSenders(t)
	return port
}

// recordingSMTPSenders is acceptingSMTPLogins, also returning the MAIL FROM
// of each message accepted so far.
func recordingSMTPSenders(t *testing.T) (int, func() []string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var mu sync.Mutex
	var senders []string
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
					case strings.HasPrefix(line, "MAIL FROM:"):
						mu.Lock()
						senders = append(senders, strings.TrimPrefix(line, "MAIL FROM:"))
						mu.Unlock()
						write("250 ok")
					case line == "DATA":
						write("354 go ahead")
						_, _ = io.ReadAll(r.DotReader())
						write("250 queued")
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
	return listener.Addr().(*net.TCPAddr).Port, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), senders...)
	}
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

func TestUserIdentitySendsThroughItsOwnServer(t *testing.T) {
	f, imap, smtp := newUserComposeFixture(t)
	port, senders := recordingSMTPSenders(t)
	if err := f.system.AddPlaintextTransportException(t.Context(), "smtp", "127.0.0.1", port, "administrator"); err != nil {
		t.Fatal(err)
	}
	if err := f.system.AddPrivateTargetException(t.Context(), "smtp", "127.0.0.1", port, "administrator"); err != nil {
		t.Fatal(err)
	}
	alice := f.accounts["alice"].ID
	form := url.Values{"name": {"Alice Shop"}, "email": {"alice-shop@example.com"}, "send_through": {"smtp"}, "smtp_host": {"127.0.0.1"}, "smtp_port": {strconv.Itoa(port)}, "smtp_tls_mode": {"plaintext"}, "smtp_username": {"alice-shop"}, "smtp_password": {"shop-secret"}}
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
	}); err != nil || shop.ID == "" {
		t.Fatalf("identity = %+v, %v", shop, err)
	}

	sent := f.compose(t, "alice", "/compose", url.Values{"identity_id": {shop.ID}, "to": {"recipient@example.com"}, "subject": {"From the shop"}, "body": {"Hello"}})
	waitUserIMAP(t, func() bool {
		return f.sendStatus(t, "alice", sent["send_id"].(string)).SentCopyStatus == storage.SentCopyComplete
	})
	if got := senders(); len(got) != 1 || got[0] != "<alice-shop@example.com>" {
		t.Fatalf("own server senders = %q", got)
	}
	if smtp.count("alice-shop") != 0 || smtp.count("alice") != 0 {
		t.Fatal("the account's server sent it too")
	}
	if copies := imap.rawCopies("alice", "Sent"); len(copies) != 1 || !strings.Contains(copies[0], `From: "Alice Shop" <alice-shop@example.com>`) {
		t.Fatalf("Sent copies = %q", copies)
	}
}

func TestUserGmailIdentitySendsThroughItsOwnServer(t *testing.T) {
	f, api := newProviderSendFixture(t, "gmail")
	port, senders := recordingSMTPSenders(t)
	if err := f.system.AddPlaintextTransportException(t.Context(), "smtp", "127.0.0.1", port, "administrator"); err != nil {
		t.Fatal(err)
	}
	if err := f.system.AddPrivateTargetException(t.Context(), "smtp", "127.0.0.1", port, "administrator"); err != nil {
		t.Fatal(err)
	}
	alice := f.accounts["alice"].ID
	form := url.Values{"name": {"Alice Shop"}, "email": {"alice-shop@example.com"}, "send_through": {"smtp"}, "smtp_host": {"127.0.0.1"}, "smtp_port": {strconv.Itoa(port)}, "smtp_tls_mode": {"plaintext"}, "smtp_username": {"alice-shop"}, "smtp_password": {"shop-secret"}}
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
	}); err != nil || shop.ID == "" {
		t.Fatalf("identity = %+v, %v", shop, err)
	}
	rec := f.request("alice", "POST", "/compose", url.Values{"account_id": {alice}, "identity_id": {shop.ID}, "to": {"recipient@example.test"}, "subject": {"From the shop"}, "body": {"Hello"}}.Encode())
	if rec.Code != 202 {
		t.Fatalf("compose = %d %s", rec.Code, rec.Body.String())
	}
	waitUserIMAP(t, func() bool { return len(senders()) == 1 })
	if got := senders(); got[0] != "<alice-shop@example.com>" || api.acceptedCount("alice") != 0 {
		t.Fatalf("own server senders = %q, Gmail sends = %d", got, api.acceptedCount("alice"))
	}
}
