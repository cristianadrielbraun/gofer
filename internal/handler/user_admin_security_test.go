package handler

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestOwnedAdminSecurityListsLocalTransportReferences(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='bob'; INSERT INTO accounts(id,user_id,email_address,imap_host,imap_port,imap_tls_mode) VALUES('central-only','alice','central-transport-secret@example.com','mail.lab.test',143,'plaintext')`); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.h.withUserDB(t.Context(), owner, func(db *storage.DB) error {
			_, err := db.Write().Exec(`UPDATE accounts SET imap_host=' Mail.Lab.Test ',imap_port=143,imap_tls_mode=' PlainText ',smtp_host='smtp.lab.test',smtp_port=25,smtp_tls_mode='plaintext'`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.system.Write().Exec(`UPDATE users SET status='disabled' WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []string{"imap", "smtp"} {
		host, port := "mail.lab.test", 143
		if protocol == "smtp" {
			host, port = "smtp.lab.test", 25
		}
		if err := f.system.AddPlaintextTransportException(t.Context(), protocol, host, port, "administrator"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.system.AddHTTPDiscoveryException(t.Context(), "lab.test", "administrator"); err != nil {
		t.Fatal(err)
	}
	ctx := auth.ContextWithUser(t.Context(), &auth.User{ID: "administrator", AuthVersion: 1})
	items, err := f.admin.mailSecurityExceptions(ctx)
	if err != nil || len(items) != 3 {
		t.Fatal(items, err)
	}
	for _, item := range items {
		if item.Kind == models.MailSecurityExceptionPlaintextTransport {
			if len(item.Accounts) != 2 || item.Accounts[0].ID != f.accounts["alice"].ID || item.Accounts[1].ID != f.accounts["bob"].ID {
				t.Fatal("affected local accounts", item)
			}
		} else if len(item.Accounts) != 0 {
			t.Fatal("nontransport policy gained accounts", item)
		}
		policy, err := f.system.GetMailSecurityPolicy(ctx, item.ID)
		if err != nil || policy == nil || len(policy.Accounts) != 0 {
			t.Fatal("policy metadata joined central mailboxes", policy, err)
		}
	}
	page := f.request("/admin/security")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "alice@example.com") || !strings.Contains(page.Body.String(), "bob@example.com") || strings.Contains(page.Body.String(), "central-transport-secret") {
		t.Fatal("security page", page.Code, page.Body.String())
	}
	if _, err := f.admin.mailSecurityExceptions(auth.ContextWithUser(t.Context(), &auth.User{ID: "alice", AuthVersion: 1})); !errors.Is(err, storage.ErrUserDiagnosticsAccess) {
		t.Fatal("private identity inspected transports", err)
	}
	if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	if err := f.h.withUserDB(t.Context(), "bob", func(db *storage.DB) error { _, err := db.Write().Exec(`UPDATE accounts SET is_deleting=1`); return err }); err != nil {
		t.Fatal(err)
	}
	items, err = f.admin.mailSecurityExceptions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Kind == models.MailSecurityExceptionPlaintextTransport && (len(item.Accounts) != 1 || item.Accounts[0].ID != f.accounts["alice"].ID) {
			t.Fatal("deleting account included", item)
		}
	}
}

func ownedSecurityPost(t *testing.T, f *ownedAdminDiagnosticFixture, page, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	form.Set(auth.CSRFFormFieldName, csrfProofFromForm(t, page, path))
	r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.session.Token})
	w := httptest.NewRecorder()
	f.http.ServeHTTP(w, r)
	return w
}

func TestOwnedAdminSecurityRestartAndNativeRevocation(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing-store-%t", missing), func(t *testing.T) {
			f := newOwnedAdminDiagnosticFixture(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close() })
			host, portText, _ := net.SplitHostPort(listener.Addr().String())
			port, _ := strconv.Atoi(portText)
			if err := f.system.AddPlaintextTransportException(t.Context(), "imap", host, port, "administrator"); err != nil {
				t.Fatal(err)
			}
			if err := f.system.AddPrivateTargetException(t.Context(), "imap", host, port, "administrator"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.system.Write().Exec(`UPDATE users SET status='active' WHERE id='bob'; UPDATE sessions SET step_up_at=CURRENT_TIMESTAMP,step_up_method='totp' WHERE user_id='administrator'`); err != nil {
				t.Fatal(err)
			}
			if err := f.h.userAccounts.UpdateAccount(t.Context(), "alice", f.accounts["alice"].ID, &models.CreateAccountRequest{Provider: "imap", EmailAddress: "alice@example.com", DisplayName: "alice", IMAPHost: host, IMAPPort: port, IMAPTLSMode: "plaintext", SMTPHost: "smtp.example.com", Username: "alice", AuthMethod: "plain"}); err != nil {
				t.Fatal(err)
			}
			if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error { _, err := db.Write().Exec(`UPDATE folders SET uid_validity=1`); return err }); err != nil {
				t.Fatal(err)
			}
			page := f.request("/admin/security")
			if page.Code != 200 {
				t.Fatal(page.Code, page.Body.String())
			}
			// Re-adding an existing permission restarts affected owned sessions.
			started := make(chan struct{})
			serviceDone := make(chan error, 1)
			go func() {
				serviceDone <- f.h.userIMAP.RunAccountService(t.Context(), "alice", f.accounts["alice"].ID, mail.AccountServiceContacts, time.Minute, func(ctx context.Context) error { close(started); <-ctx.Done(); return ctx.Err() })
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("owned service did not start")
			}
			post := ownedSecurityPost(t, f, page.Body.String(), "/admin/security/plaintext", url.Values{"protocol": {"imap"}, "host": {host}, "port": {portText}, "acknowledge": {"yes"}})
			if post.Code != 303 || strings.Contains(post.Header().Get("Location"), "error=") {
				t.Fatal("add permission", post.Code, post.Header())
			}
			select {
			case err := <-serviceDone:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("old session not canceled", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("add did not restart owned session")
			}
			connected, disconnected := make(chan struct{}), make(chan struct{})
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				fmt.Fprint(conn, "* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN SASL-IR] fixture ready\r\n")
				reader := bufio.NewReader(conn)
				if _, err := reader.ReadString('\n'); err != nil {
					return
				}
				close(connected)
				// Leave native authentication pending until cancellation closes TCP.
				for {
					if _, err := reader.ReadString('\n'); err != nil {
						close(disconnected)
						return
					}
				}
			}()
			t.Cleanup(func() { f.cancel(); listener.Close(); <-serverDone })
			bodyDone := make(chan error, 1)
			go func() { bodyDone <- f.h.userIMAP.RefetchBody(t.Context(), "alice", 1) }()
			select {
			case <-connected:
			case err := <-bodyDone:
				t.Fatal("native body returned before connecting", err)
			case <-time.After(5 * time.Second):
				t.Fatal("native IMAP did not connect")
			}
			bobStarted, bobRelease := make(chan struct{}), make(chan struct{})
			bobDone := make(chan error, 1)
			unlockBob := sync.OnceFunc(func() { close(bobRelease) })
			t.Cleanup(unlockBob)
			go func() {
				bobDone <- f.h.userIMAP.RunAccountService(t.Context(), "bob", f.accounts["bob"].ID, mail.AccountServiceCalendar, time.Minute, func(ctx context.Context) error {
					close(bobStarted)
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-bobRelease:
						return nil
					}
				})
			}()
			select {
			case <-bobStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("other owner held off by native wait")
			}
			policies, err := f.system.ListMailSecurityPolicies(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var id string
			for _, policy := range policies {
				if policy.Kind == models.MailSecurityExceptionPlaintextTransport {
					id = policy.ID
				}
			}
			if id == "" {
				t.Fatal("permission absent")
			}
			deletePath := "/admin/security/exceptions/" + id + "/delete"
			page = f.request("/admin/security")
			if page.Code != 200 {
				t.Fatal(page.Code, page.Body.String())
			}
			if missing {
				var path string
				if err := f.h.withUserDB(t.Context(), "bob", func(db *storage.DB) error { path = db.Path(); return nil }); err != nil {
					t.Fatal(err)
				}
				if err := f.h.withUserDB(t.Context(), "alice", func(*storage.DB) error { return nil }); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			post = ownedSecurityPost(t, f, page.Body.String(), deletePath, url.Values{})
			if post.Code != 303 || strings.Contains(post.Header().Get("Location"), "error=") {
				t.Fatal("revocation", post.Code, post.Header())
			}
			if allowed, err := f.system.IsPlaintextTransportAllowed(t.Context(), "imap", host, port); err != nil || allowed {
				t.Fatal("central permission survived", allowed, err)
			}
			select {
			case err := <-bodyDone:
				if err == nil {
					t.Fatal("revocation reported native success")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("native session survived revocation")
			}
			select {
			case <-disconnected:
			case <-time.After(5 * time.Second):
				t.Fatal("native connection was not closed")
			}
			if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error {
				body, err := db.GetEmailBodyForUser(t.Context(), "1", "alice")
				if err == nil && !strings.Contains(string(body), "alice private body") {
					return errors.New("session cancellation lost saved body")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if missing {
				select {
				case err := <-bobDone:
					if !errors.Is(err, context.Canceled) {
						t.Fatal("damaged-store fallback did not cancel sessions", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("fallback session survived")
				}
			} else {
				select {
				case err := <-bobDone:
					t.Fatal("unrelated owner canceled", err)
				default:
				}
				unlockBob()
				if err := <-bobDone; err != nil {
					t.Fatal(err)
				}
				// No replacement job is scheduled by revocation. Explicit retries
				// must reject the now-unapproved plaintext endpoint before dialing.
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				if err := f.h.userIMAP.RefetchBody(ctx, "alice", 1); err == nil {
					t.Fatal("new request bypassed revoked policy")
				}
			}
		})
	}
}

func TestOwnedAdminSecurityShutdownPreventsPolicyMutation(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	if _, err := f.system.Write().Exec(`UPDATE sessions SET step_up_at=CURRENT_TIMESTAMP,step_up_method='totp' WHERE user_id='administrator'`); err != nil {
		t.Fatal(err)
	}
	if err := f.system.AddPlaintextTransportException(t.Context(), "imap", "mail.lab.test", 143, "administrator"); err != nil {
		t.Fatal(err)
	}
	policies, err := f.system.ListMailSecurityPolicies(t.Context())
	if err != nil || len(policies) != 1 {
		t.Fatal(policies, err)
	}
	page := f.request("/admin/security")
	if page.Code != 200 {
		t.Fatal(page.Code, page.Body.String())
	}
	ctx, cancel := context.WithCancel(t.Context())
	f.h.userStorageContext = ctx
	cancel()
	path := "/admin/security/exceptions/" + policies[0].ID + "/delete"
	response := ownedSecurityPost(t, f, page.Body.String(), path, url.Values{})
	if response.Code == 303 && !strings.Contains(response.Header().Get("Location"), "error=") {
		t.Fatal("shutdown accepted revocation", response.Header())
	}
	if allowed, err := f.system.IsPlaintextTransportAllowed(t.Context(), "imap", "mail.lab.test", 143); err != nil || !allowed {
		t.Fatal("shutdown changed central policy", allowed, err)
	}
}
