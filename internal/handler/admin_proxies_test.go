package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// In personal mode the owner saves proxies in their security settings,
// behind the same recent verification as the exceptions, and picks one for
// each account from the account's menu.
func TestPersonalOwnerManagesOutboundProxies(t *testing.T) {
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
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO accounts (id, user_id, provider, email_address, display_name, imap_host, imap_port, imap_tls_mode, smtp_host, smtp_port, smtp_tls_mode, username)
		VALUES ('owner-mail', 'default', 'imap', 'robin@example.test', 'Robin', 'imap.example.test', 993, 'tls', 'smtp.example.test', 465, 'tls', 'robin@example.test')`); err != nil {
		t.Fatal(err)
	}
	manager := auth.NewManager(&auth.Config{Enabled: true, Mode: auth.ModePersonal, BaseURL: "https://gofer.example"}, db,
		auth.Dependencies{BucketHashKey: []byte("personal-proxy-settings-test-key")})
	handler.auth = manager
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	stack := manager.Middleware(mux)
	owner, err := manager.CreateAuthenticatedSession(t.Context(), "default", "owner browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelMultiFactor)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := manager.RecordSessionStepUp(t.Context(), owner.UserID, owner.ID, auth.AuthenticationMethodPassword); err != nil || !ok {
		t.Fatalf("RecordSessionStepUp = %t, %v", ok, err)
	}
	do := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		var req *http.Request
		if method == http.MethodPost {
			req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		} else {
			req = httptest.NewRequest(method, path, nil)
		}
		req.AddCookie(&http.Cookie{Name: "gofer_session", Value: owner.Token})
		rec := httptest.NewRecorder()
		stack.ServeHTTP(rec, req)
		return rec
	}

	page := do(http.MethodGet, "/settings/security", nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `id="outbound-proxies"`) {
		t.Fatalf("security settings = %d, want the proxies panel", page.Code)
	}
	const savePath = ownerMailSecurityBase + "/proxies"
	add := url.Values{"name": {"Office"}, "type": {"socks5"}, "host": {"proxy.example.test"}, "port": {"1080"},
		"username": {"robin"}, "password": {"s3cret"}, "default": {"yes"}}
	add.Set(auth.CSRFFormFieldName, csrfProofFromForm(t, page.Body.String(), savePath))
	rec := do(http.MethodPost, savePath, add)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "mail_notice=") {
		t.Fatalf("add = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	proxies, err := handler.accountStore.ListOutboundProxies(t.Context())
	if err != nil || len(proxies) != 1 || !proxies[0].IsDefault || proxies[0].Type != "socks5" {
		t.Fatalf("proxies = %+v, %v", proxies, err)
	}
	id := proxies[0].ID

	page = do(http.MethodGet, rec.Header().Get("Location"), nil)
	body := page.Body.String()
	if !strings.Contains(body, "socks5://proxy.example.test:1080") || strings.Contains(body, "s3cret") {
		t.Fatal("the settings page doesn't list the proxy, or shows its password")
	}

	// Editing without a password keeps the saved one; a bad type is refused.
	edit := url.Values{"id": {id}, "name": {"Office"}, "type": {"http"}, "host": {"proxy.example.test"}, "port": {"3128"}, "username": {"robin"}, "default": {"yes"}}
	edit.Set(auth.CSRFFormFieldName, csrfProofFromForm(t, body, savePath))
	if rec := do(http.MethodPost, savePath, edit); rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "mail_error=") {
		t.Fatalf("edit = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	proxy, err := handler.accountStore.ResolveProxyChoice(t.Context(), "", id)
	if err != nil || proxy.Type != "http" || proxy.Port != 3128 || proxy.Password != "s3cret" {
		t.Fatalf("edited proxy = %+v, %v", proxy, err)
	}
	bad := url.Values{"name": {"Bad"}, "type": {"https"}, "host": {"proxy.example.test"}, "port": {"443"}}
	bad.Set(auth.CSRFFormFieldName, csrfProofFromForm(t, body, savePath))
	if rec := do(http.MethodPost, savePath, bad); !strings.Contains(rec.Header().Get("Location"), "mail_error=") {
		t.Fatalf("an https proxy was accepted: %q", rec.Header().Get("Location"))
	}

	// The account's dialog offers the proxy and saves the choice.
	ownerContext := func(r *http.Request) *http.Request {
		return r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "default", Username: "owner-profile"}))
	}
	dialog := httptest.NewRecorder()
	handler.handleManageAccountProxy(dialog, ownerContext(withPath(httptest.NewRequest(http.MethodGet, "/api/accounts/owner-mail/proxy/manage", nil), "id", "owner-mail")))
	if dialog.Code != http.StatusOK || !strings.Contains(dialog.Body.String(), "Default (Office)") || !strings.Contains(dialog.Body.String(), "Office") {
		t.Fatalf("account proxy dialog = %d %q", dialog.Code, dialog.Body.String())
	}
	save := httptest.NewRequest(http.MethodPost, "/api/accounts/owner-mail/proxy", strings.NewReader(url.Values{"proxy": {storage.AccountProxyDirect}}.Encode()))
	save.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	saved := httptest.NewRecorder()
	handler.handleSaveAccountProxy(saved, ownerContext(withPath(save, "id", "owner-mail")))
	if choice, err := db.GetAccountProxyChoice(t.Context(), "owner-mail"); err != nil || choice != storage.AccountProxyDirect || !strings.Contains(saved.Body.String(), "Saved.") {
		t.Fatalf("saved choice = %q, %v; body %q", choice, err, saved.Body.String())
	}
	unknown := httptest.NewRequest(http.MethodPost, "/api/accounts/owner-mail/proxy", strings.NewReader(url.Values{"proxy": {"no-such-proxy"}}.Encode()))
	unknown.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	refused := httptest.NewRecorder()
	handler.handleSaveAccountProxy(refused, ownerContext(withPath(unknown, "id", "owner-mail")))
	if choice, _ := db.GetAccountProxyChoice(t.Context(), "owner-mail"); choice != storage.AccountProxyDirect || !strings.Contains(refused.Body.String(), "no longer exists") {
		t.Fatalf("an unknown proxy was saved: %q; body %q", choice, refused.Body.String())
	}
	// Someone else's account has no proxy dialog.
	other := httptest.NewRecorder()
	handler.handleManageAccountProxy(other, ownerContext(withPath(httptest.NewRequest(http.MethodGet, "/api/accounts/attacker-account/proxy/manage", nil), "id", "attacker-account")))
	if other.Code != http.StatusNotFound {
		t.Fatalf("another person's account proxy dialog = %d", other.Code)
	}

	// Deleting the proxy leaves accounts on the default, now direct.
	deletePath := ownerMailSecurityBase + "/proxies/" + id + "/delete"
	remove := url.Values{auth.CSRFFormFieldName: {csrfProofFromForm(t, body, deletePath)}}
	if rec := do(http.MethodPost, deletePath, remove); rec.Code != http.StatusSeeOther || strings.Contains(rec.Header().Get("Location"), "mail_error=") {
		t.Fatalf("delete = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if proxies, _ := handler.accountStore.ListOutboundProxies(t.Context()); len(proxies) != 0 {
		t.Fatalf("proxies after delete = %+v", proxies)
	}
}

func withPath(r *http.Request, key, value string) *http.Request {
	r.SetPathValue(key, value)
	return r
}
