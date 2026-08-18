package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

const passwordSecurityNewPassword = "a different excellent local passphrase"

func passwordSecurityStack(t *testing.T) (*Handler, *auth.Manager, *http.ServeMux, *auth.Session, *auth.Session) {
	t.Helper()
	handler, manager, _ := newLocalLoginHandler(t, auth.UserStatusActive, false, false, true, false)
	current, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "current browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatalf("CreateAuthenticatedSession(current) error = %v", err)
	}
	other, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "other browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatalf("CreateAuthenticatedSession(other) error = %v", err)
	}
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	return handler, manager, mux, current, other
}

func getPasswordSecurityPage(t *testing.T, manager *auth.Manager, mux *http.ServeMux, sessionToken string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/settings/security", nil)
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: sessionToken})
	recorder := httptest.NewRecorder()
	manager.Middleware(mux).ServeHTTP(recorder, request)
	return recorder
}

func TestPasswordSecurityPageRendersLocalAccessibleChangeForm(t *testing.T) {
	_, manager, mux, current, _ := passwordSecurityStack(t)
	if _, err := manager.DB().Write().ExecContext(t.Context(), `
		INSERT INTO accounts (id, user_id, provider, email_address)
		VALUES ('mailbox', 'person', 'imap', 'mailbox@example.net')`); err != nil {
		t.Fatal(err)
	}
	recorder := getPasswordSecurityPage(t, manager, mux, current.Token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("security settings status = %d body=%q", recorder.Code, recorder.Body.String())
	}
	html := recorder.Body.String()
	for _, want := range []string{
		`href="/settings/security"`,
		`data-local-login-identifiers`,
		`data-local-login-username>Person</dd>`,
		`data-local-login-email>Person@Example.com</dd>`,
		"separate from mailbox addresses and external sign-in identities",
		`action="/settings/security/password"`,
		`name="_csrf"`,
		`name="current_password"`,
		`autocomplete="current-password"`,
		`name="new_password"`,
		`name="confirm_password"`,
		`autocomplete="new-password"`,
		`minlength="15"`,
		"Changing your password signs out every other browser and device.",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("security settings missing %q", want)
		}
	}
	if strings.Contains(html, current.Token) {
		t.Fatal("security settings exposed the raw session bearer")
	}
	if strings.Contains(html, "mailbox@example.net") || strings.Contains(html, "person@example.com") {
		t.Fatal("security settings rendered a mailbox address or normalized lookup identifier as a local sign-in display value")
	}
	if strings.Contains(html, "fonts.googleapis.com") || strings.Contains(html, "fonts.gstatic.com") {
		t.Fatal("security settings requested a remote font")
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("security settings Cache-Control = %q", recorder.Header().Get("Cache-Control"))
	}
}

func TestPasswordChangeRequiresCSRFAndRotatesOnlyAfterValidCurrentPassword(t *testing.T) {
	_, manager, mux, current, other := passwordSecurityStack(t)
	stack := manager.Middleware(mux)
	form := url.Values{
		"current_password": {localLoginPassword},
		"new_password":     {passwordSecurityNewPassword},
		"confirm_password": {passwordSecurityNewPassword},
	}
	request := httptest.NewRequest(http.MethodPost, passwordChangePath, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: current.Token})
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("password change without CSRF = %d body=%q", recorder.Code, recorder.Body.String())
	}
	for _, session := range []*auth.Session{current, other} {
		if found, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || found == nil {
			t.Fatalf("session %q after CSRF rejection = %#v, %v", session.ID, found, err)
		}
	}

	page := getPasswordSecurityPage(t, manager, mux, current.Token)
	form.Set(auth.CSRFFormFieldName, csrfProofFromForm(t, page.Body.String(), passwordChangePath))
	request = httptest.NewRequest(http.MethodPost, passwordChangePath, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", "Password Settings Test/1.0")
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: current.Token})
	recorder = httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/settings/security?password_changed=1" {
		t.Fatalf("password change response = %d %q body=%q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	changedCookie := responseCookie(recorder, "gofer_session", true)
	if changedCookie == nil || !changedCookie.HttpOnly || !changedCookie.Secure || changedCookie.SameSite != http.SameSiteLaxMode || changedCookie.Value == current.Token {
		t.Fatalf("changed-password session cookie = %#v", changedCookie)
	}
	if found, err := manager.GetSessionByToken(t.Context(), current.Token); err != nil || found != nil {
		t.Fatalf("old current session after change = %#v, %v", found, err)
	}
	if found, err := manager.GetSessionByToken(t.Context(), other.Token); err != nil || found != nil {
		t.Fatalf("other session after change = %#v, %v", found, err)
	}
	changedSession, err := manager.GetSessionByToken(t.Context(), changedCookie.Value)
	if err != nil || changedSession == nil || changedSession.StepUpAt == nil || changedSession.StepUpMethod != auth.AuthenticationMethodPassword {
		t.Fatalf("changed session = %#v, %v", changedSession, err)
	}

	request = httptest.NewRequest(http.MethodGet, "/settings/security?password_changed=1", nil)
	request.AddCookie(changedCookie)
	recorder = httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `role="status"`) || !strings.Contains(recorder.Body.String(), "Password changed. Other signed-in devices were signed out.") {
		t.Fatalf("password change confirmation = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestPasswordChangeErrorsDoNotEchoCredentialsOrMutateSession(t *testing.T) {
	_, manager, mux, current, other := passwordSecurityStack(t)
	page := getPasswordSecurityPage(t, manager, mux, current.Token)
	proof := csrfProofFromForm(t, page.Body.String(), passwordChangePath)
	const submittedCurrent = "incorrect-current-password-secret"
	const submittedNew = "a unique replacement password secret"
	form := url.Values{
		auth.CSRFFormFieldName: {proof},
		"current_password":     {submittedCurrent},
		"new_password":         {submittedNew},
		"confirm_password":     {submittedNew},
	}
	request := httptest.NewRequest(http.MethodPost, passwordChangePath, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: current.Token})
	recorder := httptest.NewRecorder()
	manager.Middleware(mux).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), auth.ErrCurrentPasswordInvalid.Error()) || !strings.Contains(recorder.Body.String(), `role="alert"`) {
		t.Fatalf("invalid current password response = %d %q", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), submittedCurrent) || strings.Contains(recorder.Body.String(), submittedNew) {
		t.Fatal("password error response echoed submitted credential contents")
	}
	for _, session := range []*auth.Session{current, other} {
		if found, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || found == nil {
			t.Fatalf("session %q after password rejection = %#v, %v", session.ID, found, err)
		}
	}
}

func TestPasswordChangeRequiresStrongVerificationForAdministrator(t *testing.T) {
	handler, manager, _ := newLocalLoginHandler(t, auth.UserStatusActive, true, true, true, false)
	current, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "current browser",
		auth.AuthenticationMethodPassword, auth.AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	other, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "other browser",
		auth.AuthenticationMethodPassword, auth.AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	stack := manager.Middleware(mux)
	proof := csrfProofForSession(t, manager, current.Token, passwordChangePath)
	form := url.Values{
		auth.CSRFFormFieldName: {proof},
		"current_password":     {localLoginPassword},
		"new_password":         {passwordSecurityNewPassword},
		"confirm_password":     {passwordSecurityNewPassword},
	}
	post := func() *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, passwordChangePath, strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.AddCookie(&http.Cookie{Name: "gofer_session", Value: current.Token})
		recorder := httptest.NewRecorder()
		stack.ServeHTTP(recorder, request)
		return recorder
	}

	recorder := post()
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/settings/security?verification_required=1" {
		t.Fatalf("unverified administrator password change = %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	for _, session := range []*auth.Session{current, other} {
		if found, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || found == nil {
			t.Fatalf("session %q after verification rejection = %#v, %v", session.ID, found, err)
		}
	}
	if steppedUp, err := manager.RecordSessionStepUp(
		t.Context(), current.UserID, current.ID, auth.AuthenticationMethodTOTP,
	); err != nil || !steppedUp {
		t.Fatalf("RecordSessionStepUp() = %t, %v", steppedUp, err)
	}
	recorder = post()
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/settings/security?password_changed=1" {
		t.Fatalf("verified administrator password change = %d %q body=%q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	changedCookie := responseCookie(recorder, "gofer_session", true)
	if changedCookie == nil {
		t.Fatal("verified administrator password change did not rotate the session cookie")
	}
	changed, err := manager.GetSessionByToken(t.Context(), changedCookie.Value)
	if err != nil || changed == nil || changed.StepUpAt == nil || changed.StepUpMethod != auth.AuthenticationMethodTOTP {
		t.Fatalf("verified administrator changed session = %#v, %v", changed, err)
	}
}

func TestPasswordChangeRejectsOversizedFormBeforeMutation(t *testing.T) {
	_, manager, mux, current, other := passwordSecurityStack(t)
	page := getPasswordSecurityPage(t, manager, mux, current.Token)
	proof := csrfProofFromForm(t, page.Body.String(), passwordChangePath)
	form := url.Values{
		auth.CSRFFormFieldName: {proof},
		"current_password":     {localLoginPassword},
		"new_password":         {passwordSecurityNewPassword},
		"confirm_password":     {passwordSecurityNewPassword},
		"padding":              {strings.Repeat("x", passwordChangeFormMaximumBytes)},
	}
	request := httptest.NewRequest(http.MethodPost, passwordChangePath, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: "gofer_session", Value: current.Token})
	recorder := httptest.NewRecorder()
	manager.Middleware(mux).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("oversized password form = %d body=%q", recorder.Code, recorder.Body.String())
	}
	for _, session := range []*auth.Session{current, other} {
		if found, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || found == nil {
			t.Fatalf("session %q after oversized form = %#v, %v", session.ID, found, err)
		}
	}
}

func TestPasswordSecurityPageExplainsMissingLocalCredentialWithoutRenderingForm(t *testing.T) {
	handler, manager, db := newLocalLoginHandler(t, auth.UserStatusActive, false, false, false, false)
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (id, email, email_normalized, name, status, auth_version)
		VALUES ('federated', 'federated@example.com', 'federated@example.com', 'Federated', 'active', 1)`); err != nil {
		t.Fatalf("insert federated user: %v", err)
	}
	session, err := manager.CreateAuthenticatedSession(t.Context(), "federated", "browser", auth.AuthenticationMethodFederatedGoogle, auth.AssuranceLevelSingleFactor)
	if err != nil {
		t.Fatalf("CreateAuthenticatedSession() error = %v", err)
	}
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	recorder := getPasswordSecurityPage(t, manager, mux, session.Token)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "does not have a local password") || strings.Contains(recorder.Body.String(), `action="/settings/security/password"`) {
		t.Fatalf("federated-only security page = %d %q", recorder.Code, recorder.Body.String())
	}
}
