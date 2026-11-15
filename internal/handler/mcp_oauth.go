package handler

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	goferauth "github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/httpguard"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// Gofer is its own OAuth 2.1 authorization server for the MCP server, as the
// MCP authorization spec describes: a client finds it from the protected
// resource metadata (RFC 9728) and the server metadata (RFC 8414), registers
// itself (RFC 7591), sends the user to the authorization page, where they
// sign in to Gofer and choose what it may do, and trades the code, with its
// PKCE verifier, for an access token and a refresh token. The tokens act as
// the user with the scopes they chose, like a token made in Settings.

func (h *Handler) oauthBaseURL() string { return strings.TrimRight(h.auth.Config().BaseURL, "/") }

func (h *Handler) oauthResource() string { return h.oauthBaseURL() + httpguard.MCPPath }

func (h *Handler) registerMCPOAuthRoutes(mux *http.ServeMux) {
	if !h.mcpEnabled() {
		return
	}
	resource := auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               h.oauthResource(),
		AuthorizationServers:   []string{h.oauthBaseURL()},
		ScopesSupported:        MCPScopes,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Gofer",
	})
	for _, path := range []string{httpguard.OAuthProtectedResourcePath, httpguard.OAuthProtectedResourcePath + httpguard.MCPPath} {
		mux.Handle("GET "+path, oauthCORS(resource))
		mux.Handle("OPTIONS "+path, oauthCORS(resource))
	}
	for _, path := range []string{httpguard.OAuthServerMetadataPath, httpguard.OAuthServerMetadataPath + httpguard.MCPPath} {
		mux.Handle("GET "+path, oauthCORS(http.HandlerFunc(h.handleOAuthServerMetadata)))
		mux.Handle("OPTIONS "+path, oauthCORS(http.HandlerFunc(h.handleOAuthServerMetadata)))
	}
	mux.Handle("POST "+httpguard.OAuthRegisterPath, oauthCORS(http.HandlerFunc(h.handleOAuthRegister)))
	mux.Handle("OPTIONS "+httpguard.OAuthRegisterPath, oauthCORS(http.HandlerFunc(h.handleOAuthRegister)))
	mux.Handle("POST "+httpguard.OAuthTokenPath, oauthCORS(http.HandlerFunc(h.handleOAuthToken)))
	mux.Handle("OPTIONS "+httpguard.OAuthTokenPath, oauthCORS(http.HandlerFunc(h.handleOAuthToken)))
	mux.Handle("POST "+httpguard.OAuthRevokePath, oauthCORS(http.HandlerFunc(h.handleOAuthRevoke)))
	mux.Handle("OPTIONS "+httpguard.OAuthRevokePath, oauthCORS(http.HandlerFunc(h.handleOAuthRevoke)))
	mux.HandleFunc("GET "+httpguard.OAuthAuthorizePath, h.handleOAuthAuthorize)
	mux.HandleFunc("POST "+httpguard.OAuthAuthorizePath, h.handleOAuthAuthorize)
}

// oauthCORS lets a client running in a browser read these answers; they
// hold nothing a cookie would unlock.
func oauthCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) handleOAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	base := h.oauthBaseURL()
	writeOAuthJSON(w, http.StatusOK, &oauthex.AuthServerMeta{
		Issuer:                            base,
		AuthorizationEndpoint:             base + httpguard.OAuthAuthorizePath,
		TokenEndpoint:                     base + httpguard.OAuthTokenPath,
		RegistrationEndpoint:              base + httpguard.OAuthRegisterPath,
		RevocationEndpoint:                base + httpguard.OAuthRevokePath,
		ScopesSupported:                   MCPScopes,
		ResponseTypesSupported:            []string{"code"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported: []string{"none"},
		CodeChallengeMethodsSupported:     []string{"S256"},
	})
}

func writeOAuthJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	writeOAuthJSON(w, status, map[string]string{"error": code, "error_description": description})
}

// validOAuthRedirect accepts https addresses, http ones on the device
// itself, and an app's own scheme, never one that runs script.
func validOAuthRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Fragment != "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return u.Host != ""
	case "http":
		host := u.Hostname()
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	case "javascript", "data", "vbscript", "file", "blob":
		return false
	default:
		return true
	}
}

func (h *Handler) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	var meta oauthex.ClientRegistrationMetadata
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&meta); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "the registration is not valid JSON")
		return
	}
	if len(meta.RedirectURIs) == 0 || len(meta.RedirectURIs) > 10 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "give one to ten redirect_uris")
		return
	}
	for _, uri := range meta.RedirectURIs {
		if !validOAuthRedirect(uri) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris must be https, a loopback http address, or an app's own scheme")
			return
		}
	}
	name := strings.TrimSpace(meta.ClientName)
	if name == "" {
		name = "MCP client"
	}
	client, err := h.db.RegisterOAuthClient(r.Context(), name, meta.RedirectURIs)
	if err != nil {
		log.Printf("oauth: register client: %v", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the client could not be registered")
		return
	}
	writeOAuthJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  client.ID,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                client.Name,
		"redirect_uris":              client.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

// oauthAuthorizeRequest is what the authorization page is asked.
type oauthAuthorizeRequest struct {
	ClientID, RedirectURI, State, CodeChallenge, Method, Resource, Scope string
}

func oauthAuthorizeRequestFrom(v url.Values) oauthAuthorizeRequest {
	return oauthAuthorizeRequest{
		ClientID: v.Get("client_id"), RedirectURI: v.Get("redirect_uri"), State: v.Get("state"),
		CodeChallenge: v.Get("code_challenge"), Method: v.Get("code_challenge_method"),
		Resource: v.Get("resource"), Scope: v.Get("scope"),
	}
}

func (q oauthAuthorizeRequest) hidden() map[string]string {
	return map[string]string{
		"client_id": q.ClientID, "redirect_uri": q.RedirectURI, "state": q.State, "response_type": "code",
		"code_challenge": q.CodeChallenge, "code_challenge_method": q.Method, "resource": q.Resource,
	}
}

// oauthRedirect sends the browser back to the client with params.
func oauthRedirect(w http.ResponseWriter, r *http.Request, redirectURI string, params url.Values) {
	u, _ := url.Parse(redirectURI)
	query := u.Query()
	for key, values := range params {
		for _, v := range values {
			query.Add(key, v)
		}
	}
	u.RawQuery = query.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (h *Handler) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.invalid_form_data"), http.StatusBadRequest)
		return
	}
	source := r.URL.Query()
	if r.Method == http.MethodPost {
		source = r.PostForm
	}
	q := oauthAuthorizeRequestFrom(source)
	ctx := r.Context()
	user := goferauth.GetCurrentUser(ctx)
	page := views.MCPConsentData{Scopes: MCPScopes}
	fatal := func(message string) {
		page.FatalError = message
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusBadRequest)
		_ = views.MCPConsentPage(page).Render(ctx, w)
	}

	// Until the client and its redirect are known good, nothing is sent
	// back to it: an unknown address gets an error page, never a redirect.
	client, err := h.db.GetOAuthClient(ctx, q.ClientID)
	if err != nil {
		fatal(i18n.T(ctx, "mcp.consent.unknown_client"))
		return
	}
	if !slices.Contains(client.RedirectURIs, q.RedirectURI) {
		fatal(i18n.T(ctx, "mcp.consent.bad_redirect"))
		return
	}
	fail := func(code, description string) {
		oauthRedirect(w, r, q.RedirectURI, url.Values{"error": {code}, "error_description": {description}, "state": {q.State}, "iss": {h.oauthBaseURL()}})
	}
	if source.Get("response_type") != "code" {
		fail("unsupported_response_type", "only response_type=code is supported")
		return
	}
	if q.CodeChallenge == "" || q.Method != "S256" {
		fail("invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	if q.Resource != "" && strings.TrimRight(q.Resource, "/") != h.oauthResource() {
		fail("invalid_target", "the resource is not this server's MCP endpoint")
		return
	}
	if user == nil {
		fatal(i18n.T(ctx, "mcp.consent.not_signed_in"))
		return
	}

	if r.Method == http.MethodPost {
		if r.PostFormValue("decision") != "allow" {
			fail("access_denied", "the user did not allow it")
			return
		}
		scopes := validMCPScopes(r.PostForm["scope"])
		mailboxes, mailboxErr := h.mcpChosenMailboxes(ctx, user.ID, mcpFormAllMailboxes(r.PostForm), r.PostForm["mailbox"])
		switch {
		case len(scopes) == 0:
			page.Error = i18n.T(ctx, "settings.mcp.scope_required")
		case mailboxErr != nil:
			page.Error = i18n.T(ctx, "settings.mcp.mailbox_required")
		default:
			code, err := h.db.CreateOAuthCode(ctx, storage.OAuthCode{
				ClientID: client.ID, UserID: user.ID, RedirectURI: q.RedirectURI, Scopes: scopes, Mailboxes: mailboxes,
				CodeChallenge: q.CodeChallenge, Resource: q.Resource,
			})
			if err != nil {
				log.Printf("oauth: create code: %v", err)
				fail("server_error", "the authorization could not be saved")
				return
			}
			log.Printf("oauth: %s allowed %q (%s) %v", user.ID, client.Name, client.ID, scopes)
			oauthRedirect(w, r, q.RedirectURI, url.Values{"code": {code}, "state": {q.State}, "iss": {h.oauthBaseURL()}})
			return
		}
	}

	requested := map[string]bool{}
	for _, scope := range validMCPScopes(strings.Fields(q.Scope)) {
		requested[scope] = true
	}
	if len(requested) == 0 {
		// A client that asks for nothing in particular starts with reading.
		requested[MCPScopeMailRead] = true
	}
	page.ClientName, page.Requested, page.Hidden = client.Name, requested, q.hidden()
	if choices, err := h.mcpMailboxChoices(ctx, user.ID); err == nil {
		page.Mailboxes = choices
	}
	if u, err := url.Parse(q.RedirectURI); err == nil {
		page.RedirectTo = u.Scheme + "://" + u.Host
	}
	page.CSRF = goferauth.CSRFToken(ctx, http.MethodPost, httpguard.OAuthAuthorizePath)
	page.UserName = user.Name
	if page.UserName == "" {
		page.UserName = user.Username
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page must not be framed by the client's site.
	w.Header().Set("X-Frame-Options", "DENY")
	_ = views.MCPConsentPage(page).Render(ctx, w)
}

func pkceMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

func (h *Handler) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the request is not a form")
		return
	}
	ctx := r.Context()
	clientID := r.PostFormValue("client_id")
	client, err := h.db.GetOAuthClient(ctx, clientID)
	if err != nil {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "unknown client_id")
		return
	}
	now := time.Now().UTC()
	var tokens storage.OAuthTokens
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		code, err := h.db.ConsumeOAuthCode(ctx, r.PostFormValue("code"), client.ID, now)
		if errors.Is(err, storage.ErrOAuthInvalidGrant) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the code is unknown, used or expired")
			return
		}
		if err != nil {
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "the code could not be checked")
			return
		}
		if r.PostFormValue("redirect_uri") != code.RedirectURI {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match")
			return
		}
		if !pkceMatches(r.PostFormValue("code_verifier"), code.CodeChallenge) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the code_verifier does not match")
			return
		}
		if resource := r.PostFormValue("resource"); resource != "" && strings.TrimRight(resource, "/") != h.oauthResource() {
			writeOAuthError(w, http.StatusBadRequest, "invalid_target", "the resource is not this server's MCP endpoint")
			return
		}
		tokens, err = h.db.CreateOAuthGrantFromCode(ctx, client, code, now)
		if err != nil {
			log.Printf("oauth: create grant: %v", err)
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "the tokens could not be issued")
			return
		}
	case "refresh_token":
		tokens, err = h.db.RefreshOAuthGrant(ctx, r.PostFormValue("refresh_token"), client.ID, now)
		if errors.Is(err, storage.ErrOAuthInvalidGrant) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is unknown, expired or revoked")
			return
		}
		if err != nil {
			log.Printf("oauth: refresh: %v", err)
			writeOAuthError(w, http.StatusInternalServerError, "server_error", "the tokens could not be issued")
			return
		}
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
		return
	}
	writeOAuthJSON(w, http.StatusOK, map[string]any{
		"access_token":  tokens.AccessToken,
		"token_type":    "Bearer",
		"expires_in":    tokens.ExpiresIn,
		"refresh_token": tokens.RefreshToken,
		"scope":         strings.Join(tokens.Scopes, " "),
	})
}

func (h *Handler) handleOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the request is not a form")
		return
	}
	if err := h.db.RevokeOAuthToken(r.Context(), r.PostFormValue("token"), r.PostFormValue("client_id")); err != nil {
		log.Printf("oauth: revoke: %v", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the token could not be revoked")
		return
	}
	w.WriteHeader(http.StatusOK)
}
