package notifications

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserSettingsTabsReadOwnedData(t *testing.T) {
	f := newUserSignatureFixture(t)
	for owner, zone := range map[string]string{"alice": "Europe/Prague", "bob": "America/New_York"} {
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			return db.MergeUISettings(t.Context(), owner, map[string]string{"timezone": zone})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.system.SetUISettings(t.Context(), "alice", map[string]string{"timezone": "central-sentinel"}); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		for _, tab := range []string{"accounts", "sync", "operations", "contacts", "appearance", "regional", "compose-display", "advanced"} {
			for _, partial := range []bool{false, true} {
				req := httptest.NewRequest("GET", "/settings/"+tab+"?user_id=other", nil)
				req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions[owner].Token})
				if partial {
					req.Header.Set("HX-Request", "true")
				}
				response := httptest.NewRecorder()
				f.http.ServeHTTP(response, req)
				body := response.Body.String()
				if response.Code != 200 || !strings.Contains(body, "data-settings-page") || strings.Contains(body, "central-sentinel") {
					t.Fatalf("%s %s partial=%t: %d", owner, tab, partial, response.Code)
				}
				if strings.Contains(body, "<!doctype html>") == partial {
					t.Fatalf("wrong full/partial layout for %s", tab)
				}
				other := "alice"
				if owner == other {
					other = "bob"
				}
				if strings.Contains(body, other+"@example.com") || strings.Contains(body, other+" signature") {
					t.Fatalf("foreign settings data in %s", tab)
				}
				if tab == "accounts" && !strings.Contains(body, owner+"@example.com") {
					t.Fatal("owned account missing")
				}
				if tab == "compose-display" && !strings.Contains(body, owner+" signature") {
					t.Fatal("owned signature missing")
				}
				if tab == "regional" {
					zone := "Europe/Prague"
					if owner == "bob" {
						zone = "America/New_York"
					}
					if !strings.Contains(body, `data-current-timezone="`+zone+`"`) {
						t.Fatal("owned timezone missing")
					}
				}
			}
		}
	}
	if response := f.request("alice", "GET", "/settings", ""); response.Code != 301 || response.Header().Get("Location") != "/settings/accounts" {
		t.Fatal("settings redirect changed", response.Code, response.Header())
	}
	if response := f.request("alice", "GET", "/settings/unknown", ""); response.Code != 404 {
		t.Fatal("unknown tab accepted", response.Code)
	}
}

func freshUserSecuritySession(t *testing.T, f *userStorageFixture, owner string) {
	t.Helper()
	session, err := f.auth.CreateAuthenticatedSession(t.Context(), owner, owner+" security browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelSingleFactor)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.auth.RecordSessionStepUp(t.Context(), owner, session.ID, auth.AuthenticationMethodPassword); err != nil || !ok {
		t.Fatal("record step up", ok, err)
	}
	f.sessions[owner] = session
}

func userSecurityProof(t *testing.T, f *userStorageFixture, owner, path string) string {
	t.Helper()
	var proof string
	req := httptest.NewRequest("GET", "/settings/security", nil)
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions[owner].Token})
	f.auth.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		proof = auth.CSRFToken(r.Context(), "POST", path)
	})).ServeHTTP(httptest.NewRecorder(), req)
	if proof == "" {
		t.Fatal("no authenticated CSRF proof")
	}
	return proof
}

func TestUserSettingsSecurityKeepsCentralAuthentication(t *testing.T) {
	f := newUserStorageFixture(t)
	if _, err := f.system.Write().ExecContext(t.Context(), `INSERT INTO password_credentials(user_id, password_hash) VALUES ('alice', 'presence-only-fixture')`); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"alice", "bob"} {
		freshUserSecuritySession(t, f, owner)
		if err := f.routing.WithUser(t.Context(), owner, func(db *storage.DB) error {
			return db.MergeUISettings(t.Context(), owner, map[string]string{"theme_style": "minimal"})
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.system.SetUISettings(t.Context(), owner, map[string]string{"theme_style": "classic"}); err != nil {
			t.Fatal(err)
		}
		response := f.request(owner, "GET", "/settings/security", "")
		body := response.Body.String()
		if response.Code != 200 || !strings.Contains(body, `data-theme="minimal"`) || strings.Contains(body, `data-theme="classic"`) || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("security preferences not owned", owner, response.Code)
		}
		if strings.Contains(body, `action="/settings/security/password"`) != (owner == "alice") {
			t.Fatal("credential presence did not come from central auth", owner)
		}
		if strings.Contains(body, f.sessions[owner].Token) {
			t.Fatal("raw session token rendered")
		}
	}
	other, err := f.auth.CreateAuthenticatedSession(t.Context(), "alice", "other browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelSingleFactor)
	if err != nil {
		t.Fatal(err)
	}
	path := "/settings/security/sessions/revoke-others"
	if response := f.request("alice", "POST", path, ""); response.Code != 403 {
		t.Fatal("mutation accepted without CSRF", response.Code)
	}
	if response := f.request("alice", "POST", path, "_csrf="+userSecurityProof(t, f, "bob", path)); response.Code != 403 {
		t.Fatal("foreign CSRF accepted", response.Code)
	}
	if response := f.request("alice", "POST", path, "_csrf="+userSecurityProof(t, f, "alice", path)); response.Code != 303 {
		t.Fatal("central session mutation failed", response.Code, response.Body.String())
	}
	if session, err := f.auth.GetSessionByToken(t.Context(), other.Token); err != nil || session != nil {
		t.Fatal("other central session not revoked", err)
	}
	if session, err := f.auth.GetSessionByToken(t.Context(), f.sessions["bob"].Token); err != nil || session == nil {
		t.Fatal("foreign central session changed", err)
	}
	if _, err := f.system.Write().ExecContext(t.Context(), `UPDATE sessions SET step_up_at=? WHERE id=?`, time.Now().UTC().Add(-time.Hour), f.sessions["alice"].ID); err != nil {
		t.Fatal(err)
	}
	response := f.request("alice", "GET", "/settings/security", "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), "Verify it’s you") || !strings.Contains(response.Body.String(), `data-theme="minimal"`) || strings.Contains(response.Body.String(), `action="/settings/security/password"`) {
		t.Fatal("stale session verification bypassed", response.Code)
	}
	if response := f.request("alice", "POST", path, "_csrf="+userSecurityProof(t, f, "alice", path)); response.Code != 303 || !strings.Contains(response.Header().Get("Location"), "verification_required=1") {
		t.Fatal("stale central mutation accepted", response.Code, response.Header())
	}
}

func TestUserSettingsMissingStoreDoesNotUseCentralPreferences(t *testing.T) {
	f := newUserStorageFixture(t)
	var path string
	if err := f.routing.WithUser(t.Context(), "alice", func(db *storage.DB) error { path = db.Path(); return nil }); err != nil {
		t.Fatal(err)
	}
	// The one-store cache closes Alice before the disposable file is moved.
	if err := f.routing.WithUser(t.Context(), "bob", func(*storage.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Rename(path+".saved", path); err != nil {
			t.Error(err)
		}
	})
	for url, status := range map[string]int{"/settings/appearance": 404, "/settings/security": 503} {
		if response := f.request("alice", "GET", url, ""); response.Code != status {
			t.Fatal("missing database rendered settings", url, response.Code)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing database was replaced", err)
	}
}

func TestUserSettingsBrowserWriteReleasesStore(t *testing.T) {
	f := newUserStorageFixture(t)
	for _, path := range []string{"/settings/regional", "/settings/security"} {
		t.Run(path, func(t *testing.T) {
			w := &blockedContactViewWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
			req := httptest.NewRequest("GET", path, nil)
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
			if err := f.routing.WithUser(ctx, "bob", func(*storage.DB) error { return nil }); err != nil {
				t.Fatal("browser retained database lease", err)
			}
		})
	}
}
