package notifications

import (
	"net/url"
	"strings"
	"testing"
)

// On a managed installation, people's own proxies appear in their security
// settings only once the administrator allows them, are saved as theirs, and
// must have a public address.
func TestUserOwnProxiesInSecuritySettings(t *testing.T) {
	// The routes with the account repository, as a managed server has them.
	f := newUserStorageFixtureConfigured(t, true, nil)
	for _, owner := range []string{"alice", "bob"} {
		freshUserSecuritySession(t, f, owner)
	}
	page := f.request("alice", "GET", "/settings/security", "")
	if page.Code != 200 || strings.Contains(page.Body.String(), "data-own-proxies") {
		t.Fatalf("security settings = %d; want no own proxies before they are allowed", page.Code)
	}
	if err := f.system.SetPeopleMayAddOutboundProxies(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	page = f.request("alice", "GET", "/settings/security", "")
	if page.Code != 200 || !strings.Contains(page.Body.String(), "data-own-proxies") || !strings.Contains(page.Body.String(), `action="/settings/security/proxies"`) {
		t.Fatalf("security settings = %d; want the own proxies card", page.Code)
	}

	const path = "/settings/security/proxies"
	form := url.Values{"name": {"Home"}, "type": {"socks5"}, "host": {"127.0.0.1"}, "port": {"1080"}}
	form.Set("_csrf", userSecurityProof(t, f, "alice", path))
	rec := f.request("alice", "POST", path, form.Encode())
	if rec.Code != 303 || !strings.Contains(rec.Header().Get("Location"), "proxy_error=") {
		t.Fatalf("private own proxy = %d %q, want refused", rec.Code, rec.Header().Get("Location"))
	}
	form.Set("host", "8.8.8.8")
	rec = f.request("alice", "POST", path, form.Encode())
	if rec.Code != 303 || strings.Contains(rec.Header().Get("Location"), "proxy_error=") {
		t.Fatalf("public own proxy = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	own, err := f.system.ListOwnOutboundProxies(t.Context(), "alice")
	if err != nil || len(own) != 1 || own[0].Name != "Home" || own[0].IsDefault {
		t.Fatalf("alice's proxies = %+v, %v", own, err)
	}
	if theirs, _ := f.system.ListOwnOutboundProxies(t.Context(), "bob"); len(theirs) != 0 {
		t.Fatalf("bob's proxies = %+v", theirs)
	}
	if page := f.request("bob", "GET", "/settings/security", ""); strings.Contains(page.Body.String(), "8.8.8.8") {
		t.Fatal("bob's settings show alice's proxy")
	}
}
