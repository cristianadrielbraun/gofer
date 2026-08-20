package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
)

type handlerGoogleIDTokenVerifier struct {
	nonce   string
	subject string
	email   string
}

func (verifier *handlerGoogleIDTokenVerifier) Verify(context.Context, string) (*auth.GoogleIDTokenClaims, error) {
	return &auth.GoogleIDTokenClaims{
		Subject: verifier.subject, Nonce: verifier.nonce,
		Email: verifier.email, EmailVerified: true,
	}, nil
}

func googleIdentityLinkHandlerStack(
	t *testing.T,
) (*auth.Manager, *storage.DB, http.Handler, *http.Cookie, *handlerGoogleIDTokenVerifier) {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse Google token exchange: %v", err)
		}
		if r.Form.Get("code") != "authorization-code" || r.Form.Get("code_verifier") == "" {
			t.Errorf("Google token exchange form = %q", r.Form.Encode())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"access","token_type":"Bearer","id_token":"signed-id-token"}`)
	}))
	t.Cleanup(provider.Close)

	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, username, username_normalized, name, status, auth_version, created_at, updated_at
		) VALUES ('person', 'person', 'person', 'Person', 'active', 1, ?, ?);
		INSERT INTO password_credentials (user_id, password_hash, created_at, changed_at)
		VALUES ('person', 'password-hash', ?, ?)`,
		now, now, now, now,
	); err != nil {
		t.Fatalf("insert Google-link user: %v", err)
	}
	verifier := &handlerGoogleIDTokenVerifier{subject: "google-subject", email: "person@gmail.example"}
	manager := auth.NewManager(&auth.Config{
		Enabled: true, BaseURL: "https://gofer.example", SecureCookies: true,
		GoogleLoginClient: &oauth2.Config{
			ClientID: "client-id", ClientSecret: "client-secret",
			RedirectURL: "https://gofer.example/auth/google/callback",
			Scopes:      []string{"openid", "email", "profile"},
			Endpoint: oauth2.Endpoint{
				AuthURL: "https://accounts.example/authorize", TokenURL: provider.URL,
			},
		},
	}, db, auth.Dependencies{
		BucketHashKey:         []byte("google-link-handler-key-32-bytes"),
		GoogleIDTokenVerifier: verifier,
	})
	session, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Security Browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatalf("create Google-link session: %v", err)
	}
	if steppedUp, err := manager.RecordSessionStepUp(
		t.Context(), session.UserID, session.ID, auth.AuthenticationMethodPassword,
	); err != nil || !steppedUp {
		t.Fatalf("step up Google-link session = %t, %v", steppedUp, err)
	}
	handler := &Handler{db: db, auth: manager}
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	return manager, db, manager.Middleware(mux), &http.Cookie{Name: "gofer_session", Value: session.Token}, verifier
}

func startGoogleIdentityLinkHandlerFlow(
	t *testing.T,
	manager *auth.Manager,
	stack http.Handler,
	sessionCookie *http.Cookie,
	verifier *handlerGoogleIDTokenVerifier,
) (*http.Cookie, string) {
	t.Helper()
	page := getSecuritySettingsPath(t, stack, "/settings/security", sessionCookie)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `action="`+securityGoogleIdentityLinkPath+`"`) {
		t.Fatalf("Google identity settings = %d %q", page.Code, page.Body.String())
	}
	withoutCSRF := postSecuritySettings(t, stack, securityGoogleIdentityLinkPath, url.Values{}, sessionCookie)
	if withoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("Google identity link without CSRF = %d %q", withoutCSRF.Code, withoutCSRF.Body.String())
	}
	started := postSecuritySettings(t, stack, securityGoogleIdentityLinkPath, url.Values{
		auth.CSRFFormFieldName: {csrfProofForSession(t, manager, sessionCookie.Value, securityGoogleIdentityLinkPath)},
	}, sessionCookie)
	if started.Code != http.StatusTemporaryRedirect {
		t.Fatalf("start Google identity link = %d %q", started.Code, started.Body.String())
	}
	location, err := url.Parse(started.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Google identity authorization: %v", err)
	}
	state, nonce := location.Query().Get("state"), location.Query().Get("nonce")
	if state == "" || nonce == "" || location.Query().Get("code_challenge") == "" {
		t.Fatalf("Google identity authorization query = %q", location.RawQuery)
	}
	verifier.nonce = nonce
	preAuthCookie := responseCookie(started, "gofer_pre_auth", true)
	if preAuthCookie == nil || preAuthCookie.Value != state || preAuthCookie.Path != "/" {
		t.Fatalf("Google identity-link pre-authentication cookie = %#v", preAuthCookie)
	}
	return preAuthCookie, state
}

func TestGoogleIdentityLinkRouteRequiresCSRFAndRendersConnectedIdentity(t *testing.T) {
	manager, db, stack, sessionCookie, verifier := googleIdentityLinkHandlerStack(t)
	preAuthCookie, state := startGoogleIdentityLinkHandlerFlow(t, manager, stack, sessionCookie, verifier)

	callback := googleCallbackRequest(
		"/auth/google/callback?state=" + url.QueryEscape(state) + "&code=authorization-code",
	)
	callback.AddCookie(sessionCookie)
	callback.AddCookie(preAuthCookie)
	completed := httptest.NewRecorder()
	stack.ServeHTTP(completed, callback)
	if completed.Code != http.StatusSeeOther || completed.Header().Get("Location") != "/settings/security?google_linked=1" {
		t.Fatalf("complete Google identity link = %d %q body:%q", completed.Code, completed.Header().Get("Location"), completed.Body.String())
	}
	assertPreAuthCookieCleared(t, completed)

	confirmation := getSecuritySettingsPath(t, stack, "/settings/security?google_linked=1", sessionCookie)
	for _, want := range []string{
		"Google sign-in connected.", "person@gmail.example", "Connected",
		"For Gofer sign-in only", "does not connect a Gmail or Outlook mailbox",
		`action="` + securityGoogleIdentityLinkPath + `"`,
	} {
		if confirmation.Code != http.StatusOK || !strings.Contains(confirmation.Body.String(), want) {
			t.Fatalf("connected identity page missing %q: %d %q", want, confirmation.Code, confirmation.Body.String())
		}
	}
	var userID, subject string
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT user_id, subject FROM auth_identities WHERE provider = 'google'`,
	).Scan(&userID, &subject); err != nil {
		t.Fatalf("read linked identity: %v", err)
	}
	if userID != "person" || subject != "google-subject" || strings.Contains(confirmation.Body.String(), subject) {
		t.Fatalf("linked identity = user:%q subject:%q rendered-subject:%t", userID, subject, strings.Contains(confirmation.Body.String(), subject))
	}
}

func TestGoogleIdentityUnlinkRouteRequiresCSRFRotatesSessionAndPreservesGmailMailbox(t *testing.T) {
	manager, db, stack, sessionCookie, verifier := googleIdentityLinkHandlerStack(t)
	preAuthCookie, state := startGoogleIdentityLinkHandlerFlow(t, manager, stack, sessionCookie, verifier)
	callback := googleCallbackRequest(
		"/auth/google/callback?state=" + url.QueryEscape(state) + "&code=authorization-code",
	)
	callback.AddCookie(sessionCookie)
	callback.AddCookie(preAuthCookie)
	linked := httptest.NewRecorder()
	stack.ServeHTTP(linked, callback)
	if linked.Code != http.StatusSeeOther || linked.Header().Get("Location") != "/settings/security?google_linked=1" {
		t.Fatalf("complete identity link before unlink = %d %q", linked.Code, linked.Header().Get("Location"))
	}
	var identityID string
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT id FROM auth_identities WHERE user_id = 'person' AND provider = 'google'`,
	).Scan(&identityID); err != nil {
		t.Fatalf("read linked identity ID: %v", err)
	}
	unlinkPath := securityGoogleIdentityUnlinkPath(identityID)
	page := getSecuritySettingsPath(t, stack, "/settings/security", sessionCookie)
	for _, want := range []string{`action="` + unlinkPath + `"`, "Disconnect", "For Gofer sign-in only", "does not connect a Gmail or Outlook mailbox"} {
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), want) {
			t.Fatalf("unlink settings missing %q: %d %q", want, page.Code, page.Body.String())
		}
	}
	withoutCSRF := postSecuritySettings(t, stack, unlinkPath, url.Values{}, sessionCookie)
	if withoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("Google identity unlink without CSRF = %d %q", withoutCSRF.Code, withoutCSRF.Body.String())
	}
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO accounts (id, user_id, provider, provider_account_id, email_address)
		VALUES ('gmail-mailbox', 'person', 'gmail', 'mailbox-subject', 'mailbox@gmail.example');
		INSERT INTO oauth_accounts (
			id, account_id, provider, provider_account_id,
			access_token_ciphertext, refresh_token_ciphertext, key_version, scopes
		) VALUES (
			'gmail-mailbox-oauth', 'gmail-mailbox', 'google', 'mailbox-subject',
			x'01020304', x'05060708', 1, 'mail.read contacts.read'
		);`); err != nil {
		t.Fatalf("insert independent Gmail mailbox: %v", err)
	}

	unlinked := postSecuritySettings(t, stack, unlinkPath, url.Values{
		auth.CSRFFormFieldName: {csrfProofForSession(t, manager, sessionCookie.Value, unlinkPath)},
	}, sessionCookie)
	if unlinked.Code != http.StatusSeeOther || unlinked.Header().Get("Location") != "/settings/security?google_unlinked=1" {
		t.Fatalf("Google identity unlink = %d %q body:%q", unlinked.Code, unlinked.Header().Get("Location"), unlinked.Body.String())
	}
	rotatedCookie := responseCookie(unlinked, "gofer_session", true)
	if rotatedCookie == nil || rotatedCookie.Value == "" || rotatedCookie.Value == sessionCookie.Value {
		t.Fatalf("rotated identity-unlink session cookie = %#v", rotatedCookie)
	}
	if old := getSecuritySettingsPath(t, stack, "/settings/security", sessionCookie); old.Code != http.StatusSeeOther || old.Header().Get("Location") != "/login" {
		t.Fatalf("old session after identity unlink = %d %q", old.Code, old.Header().Get("Location"))
	}
	confirmation := getSecuritySettingsPath(t, stack, "/settings/security?google_unlinked=1", rotatedCookie)
	if confirmation.Code != http.StatusOK ||
		!strings.Contains(confirmation.Body.String(), "Google sign-in disconnected.") ||
		strings.Contains(confirmation.Body.String(), "person@gmail.example") ||
		strings.Contains(confirmation.Body.String(), `action="`+unlinkPath+`"`) {
		t.Fatalf("identity-unlink confirmation = %d %q", confirmation.Code, confirmation.Body.String())
	}
	var identities, accounts, oauthAccounts int
	var accessCiphertext, refreshCiphertext []byte
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_identities`).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM accounts WHERE id = 'gmail-mailbox'`).Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*), MIN(access_token_ciphertext), MIN(refresh_token_ciphertext)
		FROM oauth_accounts WHERE id = 'gmail-mailbox-oauth'`,
	).Scan(&oauthAccounts, &accessCiphertext, &refreshCiphertext); err != nil {
		t.Fatal(err)
	}
	if identities != 0 || accounts != 1 || oauthAccounts != 1 ||
		string(accessCiphertext) != "\x01\x02\x03\x04" || string(refreshCiphertext) != "\x05\x06\x07\x08" {
		t.Fatalf(
			"handler unlink boundary = identities:%d accounts:%d oauth:%d access:%x refresh:%x",
			identities, accounts, oauthAccounts, accessCiphertext, refreshCiphertext,
		)
	}
}

func TestGoogleIdentityUnlinkRouteProtectsLastSignInMethod(t *testing.T) {
	manager, db, stack, sessionCookie, verifier := googleIdentityLinkHandlerStack(t)
	preAuthCookie, state := startGoogleIdentityLinkHandlerFlow(t, manager, stack, sessionCookie, verifier)
	callback := googleCallbackRequest(
		"/auth/google/callback?state=" + url.QueryEscape(state) + "&code=authorization-code",
	)
	callback.AddCookie(sessionCookie)
	callback.AddCookie(preAuthCookie)
	linked := httptest.NewRecorder()
	stack.ServeHTTP(linked, callback)
	if linked.Code != http.StatusSeeOther {
		t.Fatalf("complete identity link before protected unlink = %d", linked.Code)
	}
	var identityID string
	if err := db.Read().QueryRowContext(t.Context(), `SELECT id FROM auth_identities WHERE user_id = 'person'`).Scan(&identityID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `DELETE FROM password_credentials WHERE user_id = 'person'`); err != nil {
		t.Fatal(err)
	}
	unlinkPath := securityGoogleIdentityUnlinkPath(identityID)
	page := getSecuritySettingsPath(t, stack, "/settings/security", sessionCookie)
	if page.Code != http.StatusOK || strings.Contains(page.Body.String(), `action="`+unlinkPath+`"`) ||
		!strings.Contains(page.Body.String(), "Add another usable sign-in method") {
		t.Fatalf("protected identity settings = %d %q", page.Code, page.Body.String())
	}
	blocked := postSecuritySettings(t, stack, unlinkPath, url.Values{
		auth.CSRFFormFieldName: {csrfProofForSession(t, manager, sessionCookie.Value, unlinkPath)},
	}, sessionCookie)
	if blocked.Code != http.StatusConflict || !strings.Contains(blocked.Body.String(), "Add another usable sign-in method") {
		t.Fatalf("protected identity unlink = %d %q", blocked.Code, blocked.Body.String())
	}
	var identities int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_identities`).Scan(&identities); err != nil || identities != 1 {
		t.Fatalf("identity after protected handler unlink = %d, %v", identities, err)
	}
}

func TestGoogleIdentityLinkConflictReturnsGenericSettingsFailure(t *testing.T) {
	manager, db, stack, sessionCookie, verifier := googleIdentityLinkHandlerStack(t)
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, username, username_normalized, name, status, auth_version, created_at, updated_at
		) VALUES ('owner', 'owner', 'owner', 'Owner', 'active', 1, ?, ?);
		INSERT INTO auth_identities (
			id, user_id, provider, issuer, subject, email, email_verified, created_at, linked_at
		) VALUES ('owner-google', 'owner', 'google', 'https://accounts.google.com',
		          'google-subject', 'owner@example.com', 1, ?, ?);`,
		now, now, now, now,
	); err != nil {
		t.Fatalf("insert conflicting Google identity: %v", err)
	}
	preAuthCookie, state := startGoogleIdentityLinkHandlerFlow(t, manager, stack, sessionCookie, verifier)
	callback := googleCallbackRequest(
		"/auth/google/callback?state=" + url.QueryEscape(state) + "&code=authorization-code",
	)
	callback.AddCookie(sessionCookie)
	callback.AddCookie(preAuthCookie)
	completed := httptest.NewRecorder()
	stack.ServeHTTP(completed, callback)
	if completed.Code != http.StatusSeeOther || completed.Header().Get("Location") != "/settings/security?google_link_failed=1" {
		t.Fatalf("conflicting Google identity link = %d %q", completed.Code, completed.Header().Get("Location"))
	}
	if strings.Contains(completed.Body.String(), "owner@example.com") || strings.Contains(completed.Header().Get("Location"), "owner") {
		t.Fatal("Google identity conflict exposed owning-account details")
	}
	failed := getSecuritySettingsPath(t, stack, completed.Header().Get("Location"), sessionCookie)
	if failed.Code != http.StatusOK || !strings.Contains(failed.Body.String(), "may already belong to another Gofer account") ||
		strings.Contains(failed.Body.String(), "owner@example.com") {
		t.Fatalf("generic Google identity conflict page = %d %q", failed.Code, failed.Body.String())
	}
}
