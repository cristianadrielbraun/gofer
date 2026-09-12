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
	"github.com/cristianadrielbraun/gofer/internal/httpguard"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/oauth2"
)

const enrollmentRedemptionTestPassword = "an excellent redeemed passphrase"

type enrollmentGoogleVerifier struct {
	nonce string
}

func (verifier *enrollmentGoogleVerifier) Verify(context.Context, string) (*auth.GoogleIDTokenClaims, error) {
	return &auth.GoogleIDTokenClaims{
		Subject: "invited-google-subject", Nonce: verifier.nonce,
		Email: "different-google-address@example.com", EmailVerified: true,
	}, nil
}

func enrollmentRedemptionStack(t *testing.T, status auth.UserStatus, purpose auth.EnrollmentTokenPurpose) (*auth.Manager, *storage.DB, http.Handler, *auth.EnrollmentToken) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := auth.NewManager(&auth.Config{
		Enabled: true, BaseURL: "https://gofer.example", SecureCookies: true,
	}, db, auth.Dependencies{BucketHashKey: []byte("redemption-handler-key-32-bytes!")})
	now := time.Now().UTC().Add(-time.Minute)
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, username, username_normalized, name,
			status, auth_version, user_type, is_admin, created_at, updated_at
		) VALUES
			('admin', 'admin', 'admin', 'Admin', 'active', 1, 'management', 1, ?, ?),
			('person', 'person', 'person', 'Person', ?, 1, 'webmail', 0, ?, ?)`,
		now, now, status, now, now,
	); err != nil {
		t.Fatalf("insert redemption users: %v", err)
	}
	adminSession, err := manager.CreateAuthenticatedSession(t.Context(), "admin", "admin browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelMultiFactor)
	if err != nil {
		t.Fatalf("CreateAuthenticatedSession(admin) error = %v", err)
	}
	if steppedUp, err := manager.RecordSessionStepUp(t.Context(), "admin", adminSession.ID, auth.AuthenticationMethodTOTP); err != nil || !steppedUp {
		t.Fatalf("RecordSessionStepUp(admin) = %t, %v", steppedUp, err)
	}
	token, err := manager.IssueEnrollmentToken(t.Context(), auth.IssueEnrollmentTokenOptions{
		UserID: "person", CreatedBy: "admin", ActorSessionID: adminSession.ID, Purpose: purpose,
	})
	if err != nil {
		t.Fatalf("IssueEnrollmentToken() error = %v", err)
	}
	handler := &Handler{db: db, auth: manager}
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	return manager, db, manager.Middleware(mux), token
}

func postPasswordToken(stack http.Handler, path, token, password, confirmation string) *httptest.ResponseRecorder {
	form := url.Values{
		"token":            {token},
		"new_password":     {password},
		"confirm_password": {confirmation},
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://gofer.example")
	request.Header.Set("User-Agent", "Redemption Handler Test/1.0")
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	return recorder
}

func TestInvitationEnrollmentRouteIsPublicLocalAndNoStore(t *testing.T) {
	_, _, stack, token := enrollmentRedemptionStack(t, auth.UserStatusPending, auth.EnrollmentTokenPurposeEnrollment)
	request := httptest.NewRequest(http.MethodGet, invitationEnrollmentPath, nil)
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("redemption page = %d %q", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, required := range []string{
		`action="/account/enroll"`, `name="token"`, `type="password"`, "Invitation token",
		`autocomplete="one-time-code"`, `name="new_password"`,
		`name="confirm_password"`, `autocomplete="new-password"`,
		`minlength="15"`, "never included in the page URL",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("redemption page missing %q", required)
		}
	}
	if strings.Contains(body, token.Token) || strings.Contains(body, "fonts.googleapis.com") || strings.Contains(body, "fonts.gstatic.com") ||
		strings.Contains(body, invitationGoogleEnrollmentPath) || strings.Contains(body, "Gmail mailbox") {
		t.Fatal("invitation page exposed a token or requested remote or unavailable federated content")
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Referrer-Policy") != "no-referrer" || recorder.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("redemption security headers = cache:%q referrer:%q robots:%q", recorder.Header().Get("Cache-Control"), recorder.Header().Get("Referrer-Policy"), recorder.Header().Get("X-Robots-Tag"))
	}
}

func TestCredentialRedemptionRouteContainsOnlyPasswordResetContent(t *testing.T) {
	_, _, stack, token := enrollmentRedemptionStack(t, auth.UserStatusActive, auth.EnrollmentTokenPurposeCredentialReset)
	request := httptest.NewRequest(http.MethodGet, credentialRedemptionPath, nil)
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("credential redemption page = %d %q", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, required := range []string{
		`action="/account/redeem"`, "Reset your Gofer password", "Reset token",
		`name="new_password"`, `name="confirm_password"`, "Reset password",
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("credential redemption page missing %q", required)
		}
	}
	for _, forbidden := range []string{token.Token, "/account/enroll", "Invitation", "Google", "Gmail"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("credential redemption page contains %q", forbidden)
		}
	}
	if recorder.Header().Get("Cache-Control") != "no-store" || recorder.Header().Get("Referrer-Policy") != "no-referrer" || recorder.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("credential redemption security headers = cache:%q referrer:%q robots:%q", recorder.Header().Get("Cache-Control"), recorder.Header().Get("Referrer-Policy"), recorder.Header().Get("X-Robots-Tag"))
	}
}

func TestPasswordTokenRoutesRejectTheOtherPurposeWithoutMutation(t *testing.T) {
	tests := []struct {
		name         string
		status       auth.UserStatus
		tokenPurpose auth.EnrollmentTokenPurpose
		path         string
		message      string
	}{
		{name: "invitation on reset route", status: auth.UserStatusPending, tokenPurpose: auth.EnrollmentTokenPurposeEnrollment, path: credentialRedemptionPath, message: credentialRedemptionFailureMessage},
		{name: "reset on invitation route", status: auth.UserStatusActive, tokenPurpose: auth.EnrollmentTokenPurposeCredentialReset, path: invitationEnrollmentPath, message: invitationEnrollmentFailureMessage},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, db, stack, token := enrollmentRedemptionStack(t, test.status, test.tokenPurpose)
			recorder := postPasswordToken(stack, test.path, token.Token, enrollmentRedemptionTestPassword, enrollmentRedemptionTestPassword)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), test.message) {
				t.Fatalf("wrong-purpose response = %d %q", recorder.Code, recorder.Body.String())
			}
			var used, credentials int
			if err := db.Read().QueryRowContext(t.Context(), `SELECT used_at IS NOT NULL FROM user_enrollment_tokens WHERE id = ?`, token.ID).Scan(&used); err != nil {
				t.Fatal(err)
			}
			if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM password_credentials WHERE user_id = 'person'`).Scan(&credentials); err != nil {
				t.Fatal(err)
			}
			if used != 0 || credentials != 0 {
				t.Fatalf("wrong-purpose mutation = used:%d credentials:%d", used, credentials)
			}
		})
	}
}

func TestGoogleInvitationEnrollmentCompletesThroughPublicHandlerWithoutCreatingMailbox(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"application-access-token","refresh_token":"application-refresh-token","token_type":"Bearer","id_token":"signed-id-token"}`)
	}))
	defer provider.Close()

	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	verifier := &enrollmentGoogleVerifier{}
	manager := auth.NewManager(&auth.Config{
		Enabled: true, BaseURL: "https://gofer.example", SecureCookies: true,
		GoogleLoginClient: &oauth2.Config{
			ClientID: "application-login-client", ClientSecret: "application-login-secret",
			RedirectURL: "https://gofer.example/auth/google/login/callback",
			Scopes:      []string{"openid", "email", "profile"},
			Endpoint: oauth2.Endpoint{
				AuthURL: "https://accounts.example/authorize", TokenURL: provider.URL,
			},
		},
	}, db, auth.Dependencies{
		BucketHashKey: []byte("redemption-handler-key-32-bytes!"), GoogleIDTokenVerifier: verifier,
	})
	now := time.Now().UTC().Add(-time.Minute)
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, username, username_normalized, name,
			status, auth_version, user_type, is_admin, created_at, updated_at
		) VALUES
			('admin', 'admin', 'admin', 'Admin', 'active', 1, 'management', 1, ?, ?),
			('invitee', 'invitee', 'invitee', 'Invitee', 'pending', 1, 'webmail', 0, ?, ?)`,
		now, now, now, now,
	); err != nil {
		t.Fatal(err)
	}
	adminSession, err := manager.CreateAuthenticatedSession(t.Context(), "admin", "admin browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelMultiFactor)
	if err != nil {
		t.Fatal(err)
	}
	if steppedUp, err := manager.RecordSessionStepUp(t.Context(), "admin", adminSession.ID, auth.AuthenticationMethodTOTP); err != nil || !steppedUp {
		t.Fatalf("administrator step-up = %t, %v", steppedUp, err)
	}
	invitation, err := manager.IssueEnrollmentToken(t.Context(), auth.IssueEnrollmentTokenOptions{
		UserID: "invitee", CreatedBy: "admin", ActorSessionID: adminSession.ID,
		Purpose: auth.EnrollmentTokenPurposeEnrollment,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db, auth: manager}
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	stack := manager.Middleware(mux)

	pageRequest := httptest.NewRequest(http.MethodGet, invitationEnrollmentPath, nil)
	pageRecorder := httptest.NewRecorder()
	stack.ServeHTTP(pageRecorder, pageRequest)
	if pageRecorder.Code != http.StatusOK || !strings.Contains(pageRecorder.Body.String(), "does not connect your Gmail mailbox") {
		t.Fatalf("Google invitation page = %d %q", pageRecorder.Code, pageRecorder.Body.String())
	}

	form := url.Values{"token": {invitation.Token}}
	startRequest := httptest.NewRequest(http.MethodPost, invitationGoogleEnrollmentPath, strings.NewReader(form.Encode()))
	startRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	startRequest.Header.Set("Origin", "https://gofer.example")
	startRecorder := httptest.NewRecorder()
	stack.ServeHTTP(startRecorder, startRequest)
	if startRecorder.Code != http.StatusSeeOther {
		t.Fatalf("Google invitation start = %d %q", startRecorder.Code, startRecorder.Body.String())
	}
	authorizationURL, err := url.Parse(startRecorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := authorizationURL.Query().Get("state")
	verifier.nonce = authorizationURL.Query().Get("nonce")
	if state == "" || verifier.nonce == "" || authorizationURL.Query().Get("scope") != "openid email profile" {
		t.Fatalf("Google invitation authorization URL = %q", authorizationURL.String())
	}
	var preAuthCookie *http.Cookie
	for _, cookie := range startRecorder.Result().Cookies() {
		if cookie.Name == "gofer_pre_auth" && cookie.MaxAge > 0 {
			preAuthCookie = cookie
		}
	}
	if preAuthCookie == nil || preAuthCookie.Value != state {
		t.Fatalf("Google invitation pre-auth cookie = %#v", preAuthCookie)
	}
	deniedRequest := httptest.NewRequest(
		http.MethodGet, "/auth/google/login/callback?state="+url.QueryEscape(state)+"&error="+url.QueryEscape("private provider detail"), nil,
	)
	deniedRequest.Host = "gofer.example"
	deniedRequest.AddCookie(preAuthCookie)
	deniedRecorder := httptest.NewRecorder()
	stack.ServeHTTP(deniedRecorder, deniedRequest)
	if deniedRecorder.Code != http.StatusSeeOther || deniedRecorder.Header().Get("Location") != invitationEnrollmentPath+"?google_failed=1" ||
		strings.Contains(deniedRecorder.Header().Get("Location"), "private") || strings.Contains(deniedRecorder.Body.String(), "private provider detail") {
		t.Fatalf("denied Google invitation callback = %d %q body=%q", deniedRecorder.Code, deniedRecorder.Header().Get("Location"), deniedRecorder.Body.String())
	}
	var invitationUsed int
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT used_at IS NOT NULL FROM user_enrollment_tokens WHERE id = ?`, invitation.ID,
	).Scan(&invitationUsed); err != nil || invitationUsed != 0 {
		t.Fatalf("invitation after provider denial = used:%d error:%v", invitationUsed, err)
	}

	startRequest = httptest.NewRequest(http.MethodPost, invitationGoogleEnrollmentPath, strings.NewReader(form.Encode()))
	startRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	startRequest.Header.Set("Origin", "https://gofer.example")
	startRecorder = httptest.NewRecorder()
	stack.ServeHTTP(startRecorder, startRequest)
	if startRecorder.Code != http.StatusSeeOther {
		t.Fatalf("restarted Google invitation = %d %q", startRecorder.Code, startRecorder.Body.String())
	}
	authorizationURL, err = url.Parse(startRecorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state = authorizationURL.Query().Get("state")
	verifier.nonce = authorizationURL.Query().Get("nonce")
	preAuthCookie = nil
	for _, cookie := range startRecorder.Result().Cookies() {
		if cookie.Name == "gofer_pre_auth" && cookie.MaxAge > 0 {
			preAuthCookie = cookie
		}
	}
	if state == "" || verifier.nonce == "" || preAuthCookie == nil || preAuthCookie.Value != state {
		t.Fatalf("restarted Google invitation state = state:%q nonce:%q cookie:%#v", state, verifier.nonce, preAuthCookie)
	}

	callbackRequest := httptest.NewRequest(
		http.MethodGet, "/auth/google/login/callback?state="+url.QueryEscape(state)+"&code=authorization-code", nil,
	)
	callbackRequest.Host = "gofer.example"
	callbackRequest.Header.Set("User-Agent", "Invitation Browser/1.0")
	callbackRequest.AddCookie(preAuthCookie)
	callbackRecorder := httptest.NewRecorder()
	stack.ServeHTTP(callbackRecorder, callbackRequest)
	if callbackRecorder.Code != http.StatusSeeOther || callbackRecorder.Header().Get("Location") != "/" {
		t.Fatalf("Google invitation callback = %d %q body=%q", callbackRecorder.Code, callbackRecorder.Header().Get("Location"), callbackRecorder.Body.String())
	}
	var sessionCookie *http.Cookie
	for _, cookie := range callbackRecorder.Result().Cookies() {
		if cookie.Name == "gofer_session" && cookie.MaxAge > 0 {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil {
		t.Fatal("Google invitation callback omitted authenticated session cookie")
	}
	session, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || session == nil || session.UserID != "invitee" || session.AuthenticationMethod != auth.AuthenticationMethodFederatedGoogle {
		t.Fatalf("Google invitation session = %#v, %v", session, err)
	}
	for table, want := range map[string]int{"accounts": 0, "oauth_accounts": 0, "auth_identities": 1} {
		var count int
		if err := db.Read().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("%s rows = %d, want %d", table, count, want)
		}
	}
}

func TestEnrollmentRedemptionCompletesThroughPublicStackAndClearsAuthCookies(t *testing.T) {
	_, db, stack, token := enrollmentRedemptionStack(t, auth.UserStatusPending, auth.EnrollmentTokenPurposeEnrollment)
	recorder := postPasswordToken(stack, invitationEnrollmentPath, token.Token, enrollmentRedemptionTestPassword, enrollmentRedemptionTestPassword)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != invitationEnrollmentCompletePath {
		t.Fatalf("successful redemption = %d %q body=%q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	for _, cookieName := range []string{"gofer_session", "gofer_pre_auth"} {
		cookie := responseCookie(recorder, cookieName, false)
		if cookie == nil || cookie.MaxAge != -1 {
			t.Fatalf("cleared %s cookie = %#v", cookieName, cookie)
		}
	}
	var status auth.UserStatus
	var passwordHash string
	var used int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT status FROM users WHERE id = 'person'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT password_hash FROM password_credentials WHERE user_id = 'person'`).Scan(&passwordHash); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT used_at IS NOT NULL FROM user_enrollment_tokens WHERE id = ?`, token.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	matches, _, err := auth.VerifyPassword(passwordHash, enrollmentRedemptionTestPassword)
	if err != nil || !matches || status != auth.UserStatusActive || used != 1 {
		t.Fatalf("redeemed state = status:%q matches:%t used:%d error:%v", status, matches, used, err)
	}

	request := httptest.NewRequest(http.MethodGet, invitationEnrollmentCompletePath, nil)
	recorder = httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `role="status"`) || !strings.Contains(recorder.Body.String(), `href="/login"`) || strings.Contains(recorder.Body.String(), token.Token) {
		t.Fatalf("completion page = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestCredentialRedemptionCompletesThroughResetRoute(t *testing.T) {
	_, db, stack, token := enrollmentRedemptionStack(t, auth.UserStatusActive, auth.EnrollmentTokenPurposeCredentialReset)
	recorder := postPasswordToken(stack, credentialRedemptionPath, token.Token, enrollmentRedemptionTestPassword, enrollmentRedemptionTestPassword)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != credentialRedemptionCompletePath {
		t.Fatalf("successful credential redemption = %d %q body=%q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}

	var status auth.UserStatus
	var authVersion int64
	var passwordHash string
	var used int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT status, auth_version FROM users WHERE id = 'person'`).Scan(&status, &authVersion); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT password_hash FROM password_credentials WHERE user_id = 'person'`).Scan(&passwordHash); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT used_at IS NOT NULL FROM user_enrollment_tokens WHERE id = ?`, token.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	matches, _, err := auth.VerifyPassword(passwordHash, enrollmentRedemptionTestPassword)
	if err != nil || !matches || status != auth.UserStatusActive || authVersion != 2 || used != 1 {
		t.Fatalf("credential redemption state = status:%q version:%d matches:%t used:%d error:%v", status, authVersion, matches, used, err)
	}

	request := httptest.NewRequest(http.MethodGet, credentialRedemptionCompletePath, nil)
	recorder = httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Password reset") || strings.Contains(recorder.Body.String(), "Invitation accepted") {
		t.Fatalf("credential redemption completion = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestEnrollmentRedemptionKeepsFactorlessUserPendingUnderGlobalMFA(t *testing.T) {
	_, db, stack, token := enrollmentRedemptionStack(t, auth.UserStatusPending, auth.EnrollmentTokenPurposeEnrollment)
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (id, initialized, owner_user_id, mfa_policy)
		VALUES (1, 1, 'admin', 'all_users')`); err != nil {
		t.Fatal(err)
	}

	recorder := postPasswordToken(stack, invitationEnrollmentPath, token.Token, enrollmentRedemptionTestPassword, enrollmentRedemptionTestPassword)
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), enrollmentRedemptionMFARequiredMessage) {
		t.Fatalf("factorless global-MFA redemption = %d %q", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), token.Token) || strings.Contains(recorder.Body.String(), enrollmentRedemptionTestPassword) {
		t.Fatal("factorless redemption response echoed submitted credentials")
	}
	var status auth.UserStatus
	var used, credentials int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT status FROM users WHERE id = 'person'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT used_at IS NOT NULL FROM user_enrollment_tokens WHERE id = ?`, token.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM password_credentials WHERE user_id = 'person'`).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if status != auth.UserStatusPending || used != 0 || credentials != 0 {
		t.Fatalf("blocked redemption mutation = status:%q used:%d credentials:%d", status, used, credentials)
	}
}

func TestEnrollmentRedemptionFailuresDoNotRevealTokenStateOrEchoSecrets(t *testing.T) {
	type testCase struct {
		name  string
		alter func(*storage.DB, *auth.EnrollmentToken)
	}
	tests := []testCase{
		{name: "unknown"},
		{name: "expired", alter: func(db *storage.DB, token *auth.EnrollmentToken) {
			_, _ = db.Write().Exec(`UPDATE user_enrollment_tokens SET expires_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Minute), token.ID)
		}},
		{name: "revoked", alter: func(db *storage.DB, token *auth.EnrollmentToken) {
			_, _ = db.Write().Exec(`UPDATE user_enrollment_tokens SET revoked_at = ? WHERE id = ?`, time.Now().UTC(), token.ID)
		}},
	}
	var referenceBody string
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, db, stack, token := enrollmentRedemptionStack(t, auth.UserStatusPending, auth.EnrollmentTokenPurposeEnrollment)
			if test.alter != nil {
				test.alter(db, token)
			}
			supplied := token.Token
			if test.name == "unknown" {
				supplied = "unknown-private-token"
			}
			recorder := postPasswordToken(stack, invitationEnrollmentPath, supplied, enrollmentRedemptionTestPassword, enrollmentRedemptionTestPassword)
			if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), invitationEnrollmentFailureMessage) {
				t.Fatalf("generic redemption failure = %d %q", recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), supplied) || strings.Contains(recorder.Body.String(), enrollmentRedemptionTestPassword) {
				t.Fatal("redemption failure echoed submitted credentials")
			}
			if referenceBody == "" {
				referenceBody = recorder.Body.String()
			} else if recorder.Body.String() != referenceBody {
				t.Fatal("redemption failure body differs by token state")
			}
		})
	}
}

func TestEnrollmentRedemptionValidatesConfirmationPolicyAndFormSizeBeforeMutation(t *testing.T) {
	_, db, stack, token := enrollmentRedemptionStack(t, auth.UserStatusPending, auth.EnrollmentTokenPurposeEnrollment)
	for _, test := range []struct {
		name         string
		password     string
		confirmation string
		message      string
	}{
		{name: "mismatch", password: enrollmentRedemptionTestPassword, confirmation: "a different confirmation passphrase", message: "fields do not match"},
		{name: "policy", password: "short", confirmation: "short", message: auth.ErrPasswordTooShort.Error()},
	} {
		recorder := postPasswordToken(stack, invitationEnrollmentPath, token.Token, test.password, test.confirmation)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), test.message) || strings.Contains(recorder.Body.String(), token.Token) || strings.Contains(recorder.Body.String(), test.password) {
			t.Fatalf("%s response = %d %q", test.name, recorder.Code, recorder.Body.String())
		}
	}

	request := httptest.NewRequest(http.MethodPost, invitationEnrollmentPath, strings.NewReader(strings.Repeat("x", enrollmentRedemptionFormMaximumBytes+1)))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), invitationEnrollmentFailureMessage) {
		t.Fatalf("oversized redemption form = %d %q", recorder.Code, recorder.Body.String())
	}

	var used, credentials int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT used_at IS NOT NULL FROM user_enrollment_tokens WHERE id = ?`, token.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM password_credentials WHERE user_id = 'person'`).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if used != 0 || credentials != 0 {
		t.Fatalf("rejected form mutation = used:%d credentials:%d", used, credentials)
	}
}

func TestEnrollmentRedemptionPostRemainsProtectedByCanonicalOriginGuard(t *testing.T) {
	_, db, stack, token := enrollmentRedemptionStack(t, auth.UserStatusPending, auth.EnrollmentTokenPurposeEnrollment)
	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "https://gofer.example")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	guard, err := httpguard.LoadConfig()
	if err != nil {
		t.Fatalf("httpguard.LoadConfig() error = %v", err)
	}
	form := url.Values{
		"token":            {token.Token},
		"new_password":     {enrollmentRedemptionTestPassword},
		"confirm_password": {enrollmentRedemptionTestPassword},
	}
	request := httptest.NewRequest(http.MethodPost, invitationEnrollmentPath, strings.NewReader(form.Encode()))
	request.Host = "gofer.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://attacker.example")
	recorder := httptest.NewRecorder()
	guard.Middleware(stack).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "cross-origin request blocked") {
		t.Fatalf("cross-origin redemption = %d %q", recorder.Code, recorder.Body.String())
	}
	var used, credentials int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT used_at IS NOT NULL FROM user_enrollment_tokens WHERE id = ?`, token.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM password_credentials WHERE user_id = 'person'`).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if used != 0 || credentials != 0 {
		t.Fatalf("cross-origin redemption mutation = used:%d credentials:%d", used, credentials)
	}
}

func TestEnrollmentRedemptionAcceptsPrivacyBrowserNullOriginWithSameOriginMetadata(t *testing.T) {
	_, db, stack, token := enrollmentRedemptionStack(t, auth.UserStatusPending, auth.EnrollmentTokenPurposeEnrollment)
	t.Setenv("GOFER_ADDR", "127.0.0.1:8090")
	t.Setenv("GOFER_BASE_URL", "https://gofer.example")
	t.Setenv("GOFER_ALLOW_UNAUTHENTICATED_REMOTE", "")
	guard, err := httpguard.LoadConfig()
	if err != nil {
		t.Fatalf("httpguard.LoadConfig() error = %v", err)
	}
	form := url.Values{
		"token":            {token.Token},
		"new_password":     {enrollmentRedemptionTestPassword},
		"confirm_password": {enrollmentRedemptionTestPassword},
	}
	request := httptest.NewRequest(http.MethodPost, invitationEnrollmentPath, strings.NewReader(form.Encode()))
	request.Host = "gofer.example"
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "null")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	recorder := httptest.NewRecorder()
	guard.Middleware(stack).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != invitationEnrollmentCompletePath {
		t.Fatalf("null-origin same-origin redemption = %d %q body=%q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	var used int
	var passwordHash string
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT used_at IS NOT NULL FROM user_enrollment_tokens WHERE id = ?`, token.ID,
	).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT password_hash FROM password_credentials WHERE user_id = 'person'`,
	).Scan(&passwordHash); err != nil {
		t.Fatal(err)
	}
	matches, _, err := auth.VerifyPassword(passwordHash, enrollmentRedemptionTestPassword)
	if err != nil || used != 1 || !matches {
		t.Fatalf("null-origin redemption state = used:%d matches:%t error:%v", used, matches, err)
	}
}
