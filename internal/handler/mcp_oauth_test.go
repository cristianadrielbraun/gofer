package handler

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	goferauth "github.com/cristianadrielbraun/gofer/internal/auth"
)

func oauthPKCE() (verifier, challenge string) {
	verifier = strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestMCPOAuthFlow(t *testing.T) {
	h, db, srv := mcpTestServer(t)
	_ = db
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	getJSON := func(path string, into any) int {
		t.Helper()
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_ = json.NewDecoder(resp.Body).Decode(into)
		return resp.StatusCode
	}
	postForm := func(path string, form url.Values) (int, map[string]any) {
		t.Helper()
		resp, err := client.PostForm(srv.URL+path, form)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// A request without a token says where to get one.
	resp, _ := client.Post(srv.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource/mcp") {
		t.Fatalf("401 = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	var resource map[string]any
	if getJSON("/.well-known/oauth-protected-resource/mcp", &resource) != 200 || resource["resource"] != "https://gofer.example/mcp" {
		t.Fatalf("resource metadata = %+v", resource)
	}
	var meta map[string]any
	if getJSON("/.well-known/oauth-authorization-server", &meta) != 200 || meta["token_endpoint"] != "https://gofer.example/oauth/token" {
		t.Fatalf("server metadata = %+v", meta)
	}

	// Register.
	reg, _ := http.Post(srv.URL+"/oauth/register", "application/json", strings.NewReader(`{"client_name":"Claude","redirect_uris":["https://claude.example/callback"]}`))
	var registered map[string]any
	_ = json.NewDecoder(reg.Body).Decode(&registered)
	reg.Body.Close()
	clientID, _ := registered["client_id"].(string)
	if reg.StatusCode != http.StatusCreated || clientID == "" {
		t.Fatalf("register = %d %+v", reg.StatusCode, registered)
	}
	if bad, _ := http.Post(srv.URL+"/oauth/register", "application/json", strings.NewReader(`{"redirect_uris":["javascript:alert(1)"]}`)); bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("a script redirect registered: %d", bad.StatusCode)
	}

	verifier, challenge := oauthPKCE()
	query := url.Values{"client_id": {clientID}, "redirect_uri": {"https://claude.example/callback"}, "response_type": {"code"},
		"state": {"xyz"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}, "scope": {"mail.read mail.send"}, "resource": {"https://gofer.example/mcp"}}
	user, err := h.auth.GetUserByID(t.Context(), "owner")
	if err != nil || user == nil {
		t.Fatal(err)
	}
	authorize := func(method string, values url.Values) *httptest.ResponseRecorder {
		var req *http.Request
		if method == http.MethodGet {
			req = httptest.NewRequest(method, "/oauth/authorize?"+values.Encode(), nil)
		} else {
			req = httptest.NewRequest(method, "/oauth/authorize", strings.NewReader(values.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req = req.WithContext(goferauth.ContextWithUser(req.Context(), user))
		rec := httptest.NewRecorder()
		h.handleOAuthAuthorize(rec, req)
		return rec
	}

	// The consent page offers what was asked, checked.
	page := authorize(http.MethodGet, query)
	checked := func(scope string) bool {
		re := regexp.MustCompile(`<input[^>]*data-mcp-consent-scope="` + regexp.QuoteMeta(scope) + `"[^>]*>`)
		// The attribute, not a checked: style in its class.
		return regexp.MustCompile(`\schecked[\s/>]`).MatchString(re.FindString(page.Body.String()))
	}
	if page.Code != 200 || !checked("mail.read") || !checked("mail.send") || checked("accounts.manage") {
		t.Fatalf("consent page = %d\n%s", page.Code, page.Body.String())
	}
	// A redirect it did not register is never followed.
	evil := url.Values{}
	for k, v := range query {
		evil[k] = v
	}
	evil.Set("redirect_uri", "https://evil.example/steal")
	if rec := authorize(http.MethodGet, evil); rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
		t.Fatalf("unregistered redirect = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// The user allows only reading.
	allow := url.Values{"decision": {"allow"}, "scope": {"mail.read"}}
	for k, v := range query {
		if k != "scope" {
			allow[k] = v
		}
	}
	rec := authorize(http.MethodPost, allow)
	location, _ := url.Parse(rec.Header().Get("Location"))
	code := location.Query().Get("code")
	if rec.Code != http.StatusFound || code == "" || location.Query().Get("state") != "xyz" || location.Host != "claude.example" {
		t.Fatalf("allow = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	exchange := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {"https://claude.example/callback"}, "client_id": {clientID}, "code_verifier": {strings.Repeat("w", 50)}}
	if status, out := postForm("/oauth/token", exchange); status != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier = %d %+v", status, out)
	}
	// A code is used once, even after a failed try; get another.
	rec = authorize(http.MethodPost, allow)
	location, _ = url.Parse(rec.Header().Get("Location"))
	exchange.Set("code", location.Query().Get("code"))
	exchange.Set("code_verifier", verifier)
	status, tokens := postForm("/oauth/token", exchange)
	access, _ := tokens["access_token"].(string)
	refresh, _ := tokens["refresh_token"].(string)
	if status != 200 || access == "" || refresh == "" || tokens["scope"] != "mail.read" {
		t.Fatalf("token = %d %+v", status, tokens)
	}
	if status, out := postForm("/oauth/token", exchange); status != 400 || out["error"] != "invalid_grant" {
		t.Fatalf("code used twice = %d %+v", status, out)
	}
	// A code that comes again revokes what it was traded for (RFC 6749
	// 4.1.2): someone else may have it.
	if _, err := db.VerifyMCPToken(t.Context(), access, time.Now()); err == nil {
		t.Fatal("the tokens of a code used twice still work")
	}
	rec = authorize(http.MethodPost, allow)
	location, _ = url.Parse(rec.Header().Get("Location"))
	exchange.Set("code", location.Query().Get("code"))
	status, tokens = postForm("/oauth/token", exchange)
	access, _ = tokens["access_token"].(string)
	refresh, _ = tokens["refresh_token"].(string)
	if status != 200 || access == "" {
		t.Fatalf("token after = %d %+v", status, tokens)
	}

	// The access token works with the scope chosen, and only it.
	session := mcpConnect(t, srv, access)
	accounts, r := mcpCall[mcpListAccountsOut](t, session, "list_accounts", nil)
	if r.IsError || len(accounts.Accounts) != 1 {
		t.Fatalf("list_accounts with OAuth = %+v %s", accounts, mcpResultText(r))
	}
	if _, r := mcpCall[map[string]any](t, session, "send_message", map[string]any{"to": []string{"a@example.net"}, "subject": "x", "body": "y"}); !r.IsError {
		t.Fatal("sent without the scope the user left out")
	}

	// Refresh turns the refresh token over.
	status, renewed := postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}})
	if status != 200 || renewed["refresh_token"] == refresh || renewed["access_token"] == "" {
		t.Fatalf("refresh = %d %+v", status, renewed)
	}
	if status, _ := postForm("/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}); status != 400 {
		t.Fatalf("old refresh token still works: %d", status)
	}

	// The app shows in Settings, and revoking it there signs it out.
	grants, err := db.ListOAuthGrants(t.Context(), "owner")
	if err != nil || len(grants) != 1 || grants[0].ClientName != "Claude" {
		t.Fatalf("grants = %+v %v", grants, err)
	}
	if err := db.RevokeOAuthGrant(t.Context(), "owner", grants[0].ID); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+renewed["access_token"].(string))
	req.Header.Set("Content-Type", "application/json")
	after, _ := http.DefaultClient.Do(req)
	body, _ := io.ReadAll(after.Body)
	after.Body.Close()
	if after.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked app still works: %d %s", after.StatusCode, body)
	}
}
