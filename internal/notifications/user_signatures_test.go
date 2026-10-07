package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/handler"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func newUserSignatureFixture(t *testing.T) *userStorageFixture {
	t.Helper()
	noop := func(context.Context, string) error { return nil }
	f := newUserStorageFixture(t, handler.UserAccountHooks{Created: noop, Updated: noop, Cleanup: noop})
	for _, owner := range []string{"alice", "bob"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			_, err := db.SaveSignature(t.Context(), owner, models.Signature{ID: "same-signature", Name: owner + " signature", TextBody: owner + " private body"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestUserSignaturesHTTPDefaultEditingAndIsolation(t *testing.T) {
	f := newUserSignatureFixture(t)
	if _, err := f.system.SaveSignature(t.Context(), "alice", models.Signature{ID: "central-only", Name: "central sentinel", TextBody: "central secret"}); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		account := f.accounts[owner].ID
		form := url.Values{"new_signature_id": {"same-signature"}, "new_enabled": {"true"}, "reply_signature_id": {"same-signature"}, "reply_enabled": {"true"}, "reply_placement": {"after"}}
		if response := f.request(owner, "POST", "/api/accounts/"+account+"/signature-settings", form.Encode()); response.Code != 200 {
			t.Fatal("save defaults", owner, response.Code, response.Body.String())
		}
		for _, path := range []string{"/api/accounts/" + account + "/signatures?mode=reply", "/api/accounts/" + account + "/signatures/manage", "/api/settings/signatures/manage"} {
			response := f.request(owner, "GET", path, "")
			if response.Code != 200 || !strings.Contains(response.Body.String(), owner+" signature") || strings.Contains(response.Body.String(), "central secret") {
				t.Fatal("signature view", owner, path, response.Code, response.Body.String())
			}
			other := "alice"
			if owner == other {
				other = "bob"
			}
			if strings.Contains(response.Body.String(), other+" private body") || strings.Contains(response.Body.String(), other+" signature") {
				t.Fatal("foreign signature rendered")
			}
			if strings.Contains(path, "?mode=") {
				var result struct {
					Default  *models.Signature               `json:"default_signature"`
					Settings models.AccountSignatureSettings `json:"settings"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Default == nil || result.Default.TextBody != owner+" private body" || result.Settings.ReplyPlacement != "after" {
					t.Fatal("compose default lost", result, err)
				}
			}
		}
	}
	for _, path := range []string{"/api/accounts/" + f.accounts["bob"].ID + "/signatures", "/api/accounts/" + f.accounts["bob"].ID + "/signatures/manage"} {
		if response := f.request("alice", "GET", path, ""); response.Code != 404 {
			t.Fatal("foreign account read", response.Code)
		}
	}
	if response := f.request("alice", "POST", "/api/accounts/"+f.accounts["bob"].ID+"/signature-settings", "new_signature_id=same-signature&new_enabled=true"); response.Code != 404 {
		t.Fatal("foreign account default accepted", response.Code)
	}
	if response := f.request("alice", "POST", "/api/accounts/"+f.accounts["alice"].ID+"/signature-settings", "new_signature_id=central-only&new_enabled=true"); response.Code != 404 {
		t.Fatal("central signature adopted", response.Code)
	}
	form := url.Values{"id": {"same-signature"}, "name": {"Alice edited"}, "html_body": {"<b>local body</b><script>alert(1)</script>"}}
	response := f.request("alice", "POST", "/api/signatures", form.Encode())
	var edited models.Signature
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &edited) != nil || strings.Contains(edited.HTMLBody, "<script>") {
		t.Fatal("save/sanitize signature", response.Code, response.Body.String())
	}
	if response := f.request("alice", "DELETE", "/api/signatures/central-only", ""); response.Code != 404 {
		t.Fatal("central deletion routed", response.Code)
	}
	if response := f.request("alice", "DELETE", "/api/signatures/same-signature", ""); response.Code != 200 {
		t.Fatal("delete local signature", response.Code, response.Body.String())
	}
	if err := f.routing.WithUser(t.Context(), "bob", func(db *storage.DB) error {
		sig, err := db.GetSignature(t.Context(), "bob", "same-signature")
		if err == nil && sig.TextBody != "bob private body" {
			t.Fatal("Alice changed Bob signature")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.system.GetSignature(t.Context(), "alice", "central-only"); err != nil {
		t.Fatal("central sentinel removed", err)
	}
}

func TestUserSignaturesHTTPBrowserWriteReleasesStore(t *testing.T) {
	f := newUserSignatureFixture(t)
	w := &blockedContactViewWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	req := httptest.NewRequest("GET", "/api/settings/signatures/manage", nil)
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	done := make(chan struct{})
	go func() { defer close(done); f.http.ServeHTTP(w, req) }()
	t.Cleanup(func() { close(w.release); <-done })
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("render did not reach browser")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := f.routing.WithUser(ctx, "bob", func(db *storage.DB) error { _, err := db.GetSignature(ctx, "bob", "same-signature"); return err }); err != nil {
		t.Fatal("browser retained database lease", err)
	}
}

func TestUserSignaturesHTTPWriterWaitRejectsDisabledOwner(t *testing.T) {
	for _, action := range []string{"save", "delete", "settings"} {
		t.Run(action, func(t *testing.T) {
			f := newUserSignatureFixture(t)
			var db *storage.DB
			ready, release, writerDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			unlock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unlock)
			go func() {
				writerDone <- f.routing.WithUser(t.Context(), "alice", func(local *storage.DB) error {
					db = local
					tx, err := db.Write().BeginTx(t.Context(), nil)
					if err != nil {
						return err
					}
					defer tx.Rollback()
					close(ready)
					select {
					case <-release:
					case <-t.Context().Done():
					}
					return nil
				})
			}()
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				t.Fatal("writer not acquired")
			}
			method, path, body := "POST", "/api/signatures", "id=same-signature&name=changed&text_body=changed"
			switch action {
			case "delete":
				method, path, body = "DELETE", "/api/signatures/same-signature", ""
			case "settings":
				path, body = "/api/accounts/"+f.accounts["alice"].ID+"/signature-settings", "new_signature_id=same-signature&new_enabled=true"
			}
			response := make(chan *httptest.ResponseRecorder, 1)
			go func() { response <- f.request("alice", method, path, body) }()
			waitUserIMAP(t, func() bool { return db.Write().Stats().WaitCount > 0 })
			if _, err := f.system.Write().Exec("UPDATE users SET status='disabled' WHERE id='alice'"); err != nil {
				t.Fatal(err)
			}
			unlock()
			if err := <-writerDone; err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-response:
				if r.Code == 200 {
					t.Fatal("disabled owner write accepted")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("signature request did not finish")
			}
			if _, err := f.system.Write().Exec("UPDATE users SET status='active' WHERE id='alice'"); err != nil {
				t.Fatal(err)
			}
			if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				sig, err := db.GetSignature(t.Context(), "alice", "same-signature")
				if err != nil {
					return err
				}
				if sig.Name != "alice signature" || sig.TextBody != "alice private body" {
					t.Fatal("rejected write changed signature", sig)
				}
				settings, err := db.GetAccountSignatureSettings(t.Context(), "alice", f.accounts["alice"].ID)
				if err == nil && settings.NewEnabled {
					t.Fatal("rejected write changed defaults")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserSignaturesGuardFailureRollsBackBodyAndDefaults(t *testing.T) {
	f := newUserSignatureFixture(t)
	for _, action := range []string{"save", "delete", "settings"} {
		t.Run(action, func(t *testing.T) {
			err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error {
				checks := 0
				guard := func(*sql.Tx) error {
					checks++
					if checks == 2 {
						if _, err := f.system.Write().Exec("UPDATE users SET status='disabled' WHERE id='alice'"); err != nil {
							return err
						}
					}
					return f.routing.ValidateUser(t.Context(), "alice")
				}
				var err error
				switch action {
				case "save":
					_, err = db.SaveSignatureGuarded(t.Context(), "alice", models.Signature{ID: "same-signature", Name: "changed", TextBody: "changed"}, guard)
				case "delete":
					err = db.DeleteSignatureGuarded(t.Context(), "alice", "same-signature", guard)
				case "settings":
					err = db.SaveAccountSignatureSettingsGuarded(t.Context(), "alice", models.AccountSignatureSettings{AccountID: f.accounts["alice"].ID, NewEnabled: true, NewSignatureID: "same-signature"}, guard)
				}
				if !errors.Is(err, storage.ErrUserStoreOwner) || checks != 2 {
					t.Fatal("late ownership change not rejected", err, checks)
				}
				if _, err := f.system.Write().Exec("UPDATE users SET status='active' WHERE id='alice'"); err != nil {
					return err
				}
				sig, err := db.GetSignature(t.Context(), "alice", "same-signature")
				if err != nil {
					return err
				}
				if sig.Name != "alice signature" || sig.TextBody != "alice private body" {
					t.Fatal("late rejection did not roll back signature", sig)
				}
				settings, err := db.GetAccountSignatureSettings(t.Context(), "alice", f.accounts["alice"].ID)
				if err == nil && settings.NewEnabled {
					t.Fatal("late rejection did not roll back defaults")
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
