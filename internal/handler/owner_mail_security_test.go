package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

// In personal mode the owner has no admin area, so the mail security
// exceptions live in their security settings, behind the same recent
// verification the admin page requires.
func TestPersonalOwnerManagesMailSecurityExceptions(t *testing.T) {
	handler, db := newAccountOwnershipTestHandler(t)
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (id, username, username_normalized, name, status, auth_version, user_type, is_admin, created_at, updated_at)
		VALUES ('default', 'owner-profile', 'owner-profile', 'Owner', 'active', 1, 'webmail', 0, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (id, initialized, owner_user_id, initialized_at) VALUES (1, 1, 'default', ?)
		ON CONFLICT(id) DO UPDATE SET initialized = 1, owner_user_id = 'default', initialized_at = excluded.initialized_at`, now); err != nil {
		t.Fatal(err)
	}
	manager := auth.NewManager(&auth.Config{Enabled: true, Mode: auth.ModePersonal, BaseURL: "https://gofer.example"}, db,
		auth.Dependencies{BucketHashKey: []byte("personal-mail-security-test-key!")})
	handler.auth = manager
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	stack := manager.Middleware(mux)

	session := func(verified bool) *auth.Session {
		t.Helper()
		s, err := manager.CreateAuthenticatedSession(t.Context(), "default", "owner browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelMultiFactor)
		if err != nil {
			t.Fatal(err)
		}
		if verified {
			if ok, err := manager.RecordSessionStepUp(t.Context(), s.UserID, s.ID, auth.AuthenticationMethodPassword); err != nil || !ok {
				t.Fatalf("RecordSessionStepUp = %t, %v", ok, err)
			}
		}
		return s
	}
	do := func(s *auth.Session, method, path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		var req *http.Request
		if method == http.MethodPost {
			req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		req.AddCookie(&http.Cookie{Name: "gofer_session", Value: s.Token})
		rec := httptest.NewRecorder()
		stack.ServeHTTP(rec, req)
		return rec
	}

	owner := session(true)
	page := do(owner, http.MethodGet, "/settings/security", nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `id="mail-security"`) {
		t.Fatalf("security settings = %d, want the Mail security card: %q", page.Code, page.Body.String())
	}
	if strings.Contains(page.Body.String(), `action="/admin/security`) {
		t.Fatal("personal settings post to the admin area")
	}
	const addPath = ownerMailSecurityBase + "/private-target"
	add := url.Values{"protocol": {"imap"}, "host": {"192.168.1.10"}, "port": {"993"}, "acknowledge": {"yes"}}
	add.Set(auth.CSRFFormFieldName, csrfProofFromForm(t, page.Body.String(), addPath))

	// The owner opened the page while verified, but the verification ran out
	// before they submitted: they are sent to verify again first.
	staleOwner := session(true)
	staleForm := url.Values{"protocol": {"imap"}, "host": {"192.168.1.10"}, "port": {"993"}, "acknowledge": {"yes"}}
	staleForm.Set(auth.CSRFFormFieldName, csrfProofFromForm(t, do(staleOwner, http.MethodGet, "/settings/security", nil).Body.String(), addPath))
	if _, err := db.Write().ExecContext(t.Context(), `UPDATE sessions SET step_up_at = ? WHERE id = ?`, now.Add(-time.Hour), staleOwner.ID); err != nil {
		t.Fatal(err)
	}
	if rec := do(staleOwner, http.MethodPost, addPath, staleForm); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings/security" {
		t.Fatalf("unverified add = %d %q, want a redirect to verify", rec.Code, rec.Header().Get("Location"))
	}
	if allowed, _ := db.IsPrivateTargetAllowed(t.Context(), "imap", "192.168.1.10", 993); allowed {
		t.Fatal("an unverified session added an exception")
	}

	rec := do(owner, http.MethodPost, addPath, add)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/settings/security?mail_notice=") || !strings.HasSuffix(rec.Header().Get("Location"), "#mail-security") {
		t.Fatalf("add = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if allowed, err := db.IsPrivateTargetAllowed(t.Context(), "imap", "192.168.1.10", 993); err != nil || !allowed {
		t.Fatalf("exception after add = %t, %v", allowed, err)
	}

	page = do(owner, http.MethodGet, rec.Header().Get("Location"), nil)
	if !strings.Contains(page.Body.String(), "IMAP private target is now allowed for 192.168.1.10:993.") || !strings.Contains(page.Body.String(), "192.168.1.10") {
		t.Fatalf("settings after add lost the notice or the exception: %q", page.Body.String())
	}
	exceptions, err := db.ListMailSecurityExceptions(t.Context())
	if err != nil || len(exceptions) != 1 {
		t.Fatalf("exceptions = %#v, %v", exceptions, err)
	}
	deletePath := ownerMailSecurityBase + "/exceptions/" + exceptions[0].ID + "/delete"
	remove := url.Values{auth.CSRFFormFieldName: {csrfProofFromForm(t, page.Body.String(), deletePath)}}
	if rec := do(owner, http.MethodPost, deletePath, remove); rec.Code != http.StatusSeeOther {
		t.Fatalf("revoke = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if allowed, _ := db.IsPrivateTargetAllowed(t.Context(), "imap", "192.168.1.10", 993); allowed {
		t.Fatal("exception survived revocation")
	}

	// The admin area stays closed in personal mode.
	if rec := do(owner, http.MethodGet, "/admin/security", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("admin security in personal mode = %d, want 404", rec.Code)
	}
}

// In managed mode, exceptions are an administrator's decision.
func TestOwnerMailSecurityRoutesAreClosedInManagedMode(t *testing.T) {
	handler, db := newAccountOwnershipTestHandler(t)
	handler.auth = auth.NewManager(&auth.Config{Enabled: true, Mode: auth.ModeManaged, BaseURL: "https://gofer.example"}, db)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, ownerMailSecurityBase+"/private-target", strings.NewReader("protocol=imap&host=10.0.0.5&port=993&acknowledge=yes"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handler.ownerMailSecurity(handler.handleAddPrivateTargetException)(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("owner mail security route in managed mode = %d, want 404", rec.Code)
	}
	if allowed, _ := db.IsPrivateTargetAllowed(t.Context(), "imap", "10.0.0.5", 993); allowed {
		t.Fatal("added an exception through the owner route in managed mode")
	}
	if data, err := handler.ownerMailSecurityData(req); err != nil || data != nil {
		t.Fatalf("managed settings would show the card: %#v, %v", data, err)
	}
}

// Without sign-in, the Security tab has no sign-in methods or sessions to
// show, so it holds only the owner's Mail security card.
func TestOpenModeSecurityTabManagesMailSecurity(t *testing.T) {
	handler, db := newAccountOwnershipTestHandler(t)
	manager := auth.NewManager(&auth.Config{Enabled: false, Mode: auth.ModeOpen, BaseURL: "http://localhost"}, db)
	handler.auth = manager
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	stack := manager.Middleware(mux)

	page := httptest.NewRecorder()
	stack.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/settings/security", nil))
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, `id="mail-security"`) || !strings.Contains(body, "Sign-in settings appear only when Gofer runs with sign-in.") {
		t.Fatalf("open mode security tab = %d %q", page.Code, body)
	}
	for _, signIn := range []string{"Passkeys", "Recovery codes", "Your security activity", "Verify it"} {
		if strings.Contains(body, signIn) {
			t.Fatalf("open mode security tab shows sign-in section %q", signIn)
		}
	}

	const addPath = ownerMailSecurityBase + "/private-target"
	// Open mode has no session to bind a CSRF proof to; like the admin area
	// there, its forms rely on the same-origin check every POST passes.
	form := url.Values{"protocol": {"smtp"}, "host": {"127.0.0.1"}, "port": {"2525"}, "acknowledge": {"yes"}}
	req := httptest.NewRequest(http.MethodPost, addPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/settings/security?mail_notice=") {
		t.Fatalf("add = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if allowed, err := db.IsPrivateTargetAllowed(t.Context(), "smtp", "127.0.0.1", 2525); err != nil || !allowed {
		t.Fatalf("exception after add = %t, %v", allowed, err)
	}
}

// Personal mode's owner finds the diagnostics in Settings too, and their
// event stream carries what they follow; a managed instance's people have
// no diagnostics of their own.
func TestOwnerDiagnosticsInPersonalModeOnly(t *testing.T) {
	handler, db := newAccountOwnershipTestHandler(t)
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (id, username, username_normalized, name, status, auth_version, user_type, is_admin, created_at, updated_at)
		VALUES ('default', 'owner-profile', 'owner-profile', 'Owner', 'active', 1, 'webmail', 0, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (id, initialized, owner_user_id, initialized_at) VALUES (1, 1, 'default', ?)
		ON CONFLICT(id) DO UPDATE SET initialized = 1, owner_user_id = 'default', initialized_at = excluded.initialized_at`, now); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []auth.Mode{auth.ModePersonal, auth.ModeManaged} {
		manager := auth.NewManager(&auth.Config{Enabled: true, Mode: mode, BaseURL: "https://gofer.example"}, db,
			auth.Dependencies{BucketHashKey: []byte("personal-mail-security-test-key!")})
		handler.auth = manager
		mux := http.NewServeMux()
		handler.RegisterRoutes(mux)
		stack := manager.Middleware(mux)
		session, err := manager.CreateAuthenticatedSession(t.Context(), "default", "owner browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelMultiFactor)
		if err != nil {
			t.Fatal(err)
		}
		get := func(path string) *httptest.ResponseRecorder {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.AddCookie(&http.Cookie{Name: "gofer_session", Value: session.Token})
			response := httptest.NewRecorder()
			stack.ServeHTTP(response, request)
			return response
		}
		page, data := get("/settings/diagnostics/contacts"), get("/api/diagnostics/labels/status")
		if mode == auth.ModePersonal {
			if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `data-diagnostics-api="/api/diagnostics"`) || data.Code != http.StatusOK {
				t.Fatalf("personal owner's diagnostics = %d, %d %q", page.Code, data.Code, page.Body.String())
			}
			if !strings.Contains(get("/settings/accounts").Body.String(), `data-settings-sidebar-value="diagnostics"`) {
				t.Fatal("personal mode's Settings doesn't offer the diagnostics")
			}
			continue
		}
		if page.Code != http.StatusNotFound || data.Code != http.StatusNotFound {
			t.Fatalf("managed webmail user's diagnostics = %d, %d", page.Code, data.Code)
		}
	}
}
