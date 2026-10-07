package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type ownedAdminDiagnosticFixture struct {
	*userContactPushFixture
	admin   *Handler
	manager *auth.Manager
	session *auth.Session
	http    http.Handler
}

type diagnosticBrowserWriter struct {
	*httptest.ResponseRecorder
	entered, release chan struct{}
	once             sync.Once
}

func (w *diagnosticBrowserWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	return w.ResponseRecorder.Write(data)
}

func newOwnedAdminDiagnosticFixture(t *testing.T) *ownedAdminDiagnosticFixture {
	t.Helper()
	f := &ownedAdminDiagnosticFixture{userContactPushFixture: newOwnedMessageContentFixture(t)}
	f.h.userStorageContext = t.Context()
	if _, err := f.system.Write().Exec(`INSERT INTO users(id,username,username_normalized,user_type,is_admin) VALUES('administrator','administrator','administrator','management',1),('unused','unused','unused','webmail',0)`); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := f.h.withUserDB(t.Context(), owner, func(db *storage.DB) error {
			if _, err := db.SaveContact(t.Context(), owner, models.Contact{ID: "same-contact", Name: owner, Email: owner + "@test.invalid"}); err != nil {
				return err
			}
			if owner == "bob" {
				if _, err := db.SaveContact(t.Context(), owner, models.Contact{ID: "second-contact", Name: "Bob second", Email: "second@test.invalid"}); err != nil {
					return err
				}
			}
			if _, err := db.Write().Exec(`UPDATE messages SET subject=? WHERE id=1`, owner+" private subject"); err != nil {
				return err
			}
			_, err := db.Write().Exec(`INSERT INTO message_mutations(id,account_id,message_id,folder_id,provider_type,kind,target_value,status,attempt_count,last_error,next_attempt_at) VALUES('same-operation',?,1,'inbox','imap','read',1,'failed',2,'access_token=private-diagnostic-secret',?)`, f.accounts[owner].ID, time.Now().Add(time.Hour))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.system.SaveContact(t.Context(), "alice", models.Contact{ID: "central-sentinel", Name: "Central secret", Email: "central@invalid.test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.system.Write().Exec(`UPDATE users SET username='Alice.Current',username_normalized='alice.current' WHERE id='alice'; UPDATE users SET status='disabled' WHERE id='bob'`); err != nil {
		t.Fatal(err)
	}
	f.manager = auth.NewManager(&auth.Config{Enabled: true, Mode: auth.ModeManaged, BaseURL: "http://localhost:8090"}, f.system)
	var err error
	f.session, err = f.manager.CreateAuthenticatedSession(t.Context(), "administrator", "diagnostic browser", auth.AuthenticationMethodTOTP, auth.AssuranceLevelMultiFactor)
	if err != nil {
		t.Fatal(err)
	}
	f.admin = New(f.system, nil, mail.NewSyncOrchestrator(f.system, nil, nil, nil), f.h.blobStore, f.manager, "")
	f.admin.ownedMailbox = f.h
	mux := http.NewServeMux()
	// Exercise the existing admin wrappers and HTTP handlers. Other admin data
	// paths and deletion remain outside this diagnostics checkpoint's coverage.
	f.admin.registerAdministrationRoutes(mux)
	f.http = f.manager.Middleware(mux)
	return f
}

func (f *ownedAdminDiagnosticFixture) request(path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.session.Token})
	w := httptest.NewRecorder()
	f.http.ServeHTTP(w, r)
	return w
}

func TestOwnedAdminDiagnosticsAggregateAndSelectOwners(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	for _, selected := range []string{"", "alice", "bob"} {
		query := ""
		if selected != "" {
			query = "?user_id=" + selected
		}
		contacts := f.request("/api/admin/contacts/status" + query)
		var contact models.ContactAdminStatus
		if contacts.Code != 200 || json.Unmarshal(contacts.Body.Bytes(), &contact) != nil {
			t.Fatal("contact diagnostic response", contacts.Code, contacts.Body.String())
		}
		want := map[string]int{"": 3, "alice": 1, "bob": 2}[selected]
		if contact.Total != want || contact.Manual != want || len(contact.AccountSync) != map[string]int{"": 2, "alice": 1, "bob": 1}[selected] {
			t.Fatal("contact diagnostic totals", selected, contact)
		}
		for _, account := range contact.AccountSync {
			if account.AccountID == f.accounts["alice"].ID && account.OwnerUsername != "Alice.Current" {
				t.Fatal("local profile stub overrode central username", account)
			}
		}
		labels := f.request("/api/admin/labels/status" + query)
		var label models.LabelAdminStatus
		if labels.Code != 200 || json.Unmarshal(labels.Body.Bytes(), &label) != nil || label.Totals.TotalMessages != map[string]int{"": 2, "alice": 1, "bob": 1}[selected] {
			t.Fatal("label diagnostic totals", selected, labels.Code, labels.Body.String())
		}
		operations := f.request("/api/admin/mail-operations/status" + query)
		var operation models.MailOperationsAdminStatus
		if operations.Code != 200 || json.Unmarshal(operations.Body.Bytes(), &operation) != nil || operation.Total != map[string]int{"": 2, "alice": 1, "bob": 1}[selected] || operation.ActionRequired != operation.Total {
			t.Fatal("operation diagnostic totals", selected, operations.Code, operations.Body.String())
		}
		if len(operation.Health.MessageMutations) != 1 || operation.Health.MessageMutations[0].Count != operation.Total {
			t.Fatal("operation health was not merged", operation.Health)
		}
		for _, body := range []string{contacts.Body.String(), labels.Body.String(), operations.Body.String()} {
			for _, forbidden := range []string{"central-sentinel", "Central secret", "central@invalid.test", "private-diagnostic-secret", "private subject"} {
				if strings.Contains(body, forbidden) {
					t.Fatal("private/central sentinel in diagnostics", forbidden)
				}
			}
		}
		for _, path := range []string{"/admin/contacts", "/admin/labels", "/admin/operations"} {
			response := f.request(path + query)
			if response.Code != 200 {
				t.Fatal("admin page", path, response.Code, response.Body.String())
			}
		}
	}
	if response := f.request("/api/admin/contacts/status?user_id=administrator"); response.Code != 404 {
		t.Fatal("management identity treated as webmail owner", response.Code)
	}
	if err := f.h.userStorage.WithUser(t.Context(), "bob", func(*storage.DB) error { return nil }); err == nil {
		t.Fatal("disabled owner gained private access")
	}
}

func TestOwnedAdminDiagnosticsBrowserReleasesSingleStore(t *testing.T) {
	f := newOwnedAdminDiagnosticFixture(t)
	w := &diagnosticBrowserWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	r := httptest.NewRequest("GET", "/api/admin/labels/status?user_id=alice", nil)
	r.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.session.Token})
	done := make(chan struct{})
	go func() { defer close(done); f.http.ServeHTTP(w, r) }()
	unlock := sync.OnceFunc(func() { close(w.release) })
	t.Cleanup(func() { unlock(); <-done })
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("diagnostic response did not reach browser")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := f.h.userStorage.ReadUserDiagnostics(ctx, storage.DiagnosticsActor{ID: "administrator", AuthVersion: 1}, "bob", storage.UserDiagnosticsContacts); err != nil {
		t.Fatal("browser retained the only database slot", err)
	}
}

func TestOwnedAdminDiagnosticsMissingStoreAndRootCancellation(t *testing.T) {
	for _, action := range []string{"missing", "root"} {
		t.Run(action, func(t *testing.T) {
			f := newOwnedAdminDiagnosticFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			f.h.userStorageContext = ctx
			if action == "root" {
				cancel()
			} else {
				var path string
				if err := f.h.withUserDB(t.Context(), "alice", func(db *storage.DB) error { path = db.Path(); return nil }); err != nil {
					t.Fatal(err)
				}
				// Bob is disabled but administrative reads still evict Alice.
				if _, err := f.h.userStorage.ReadUserDiagnostics(t.Context(), storage.DiagnosticsActor{ID: "administrator", AuthVersion: 1}, "bob", storage.UserDiagnosticsContacts); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			response := f.request("/api/admin/contacts/status?user_id=alice")
			if response.Code != 500 || strings.Contains(response.Body.String(), "central@invalid.test") {
				t.Fatal("failed diagnostic used central mailbox data", response.Code, response.Body.String())
			}
		})
	}
}
