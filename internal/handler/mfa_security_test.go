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

func completedSecuritySettingsStack(t *testing.T) (*auth.Manager, *storage.DB, http.Handler, *http.Cookie, string) {
	t.Helper()
	manager, db, stack, setupCookie := prepareHandlerSetupReview(t)
	state, err := manager.GetSetupOwnerState(t.Context(), setupCookie.Value, "https://gofer.example")
	if err != nil || state == nil || state.Draft == nil || state.Draft.TOTPSecret == "" {
		t.Fatalf("load setup security draft = %#v, %v", state, err)
	}
	secret := state.Draft.TOTPSecret
	completed := postSetupReview(stack, setupCookie, url.Values{"action": {"complete"}})
	if completed.Code != http.StatusSeeOther {
		t.Fatalf("complete setup = %d %q", completed.Code, completed.Body.String())
	}
	sessionCookie := responseCookie(completed, "gofer_session", true)
	if sessionCookie == nil {
		t.Fatal("completed setup omitted session cookie")
	}
	return manager, db, stack, sessionCookie, secret
}

func getSecuritySettings(t *testing.T, stack http.Handler, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return getSecuritySettingsPath(t, stack, "/settings/security", cookies...)
}

func getSecuritySettingsPath(t *testing.T, stack http.Handler, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	return recorder
}

func postSecuritySettings(t *testing.T, stack http.Handler, path string, values url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", "Security Settings Handler/1.0")
	request.RemoteAddr = "198.51.100.90:45123"
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	return recorder
}

func TestSecuritySettingsRendersManagedFactorsAndProtectsActionsWithCSRF(t *testing.T) {
	_, _, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	page := getSecuritySettings(t, stack, sessionCookie)
	if page.Code != http.StatusOK || page.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("security settings = %d headers:%v body:%q", page.Code, page.Header(), page.Body.String())
	}
	html := page.Body.String()
	for _, want := range []string{
		"Authenticator app", "Enrolled", "Recovery codes", "10 remaining",
		`action="/settings/security/totp/start"`,
		`action="/settings/security/recovery/start"`,
		"Add another strong authenticator before disabling this one.",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("security settings missing %q", want)
		}
	}
	if strings.Contains(html, `action="/settings/security/totp/disable"`) {
		t.Fatal("last required authenticator rendered an enabled disable action")
	}
	withoutCSRF := postSecuritySettings(t, stack, securityRecoveryStartPath, url.Values{}, sessionCookie)
	if withoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("recovery generation without CSRF = %d %q", withoutCSRF.Code, withoutCSRF.Body.String())
	}
	oversized := postSecuritySettings(t, stack, securityTOTPStartPath, url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, html, securityTOTPStartPath)},
		"padding":              {strings.Repeat("x", securityManagementFormBytes)},
	}, sessionCookie)
	if oversized.Code != http.StatusForbidden {
		t.Fatalf("oversized security form = %d %q", oversized.Code, oversized.Body.String())
	}
}

func TestSecuritySettingsReplacesTOTPThroughSessionBoundChallenge(t *testing.T) {
	manager, _, stack, sessionCookie, oldSecret := completedSecuritySettingsStack(t)
	page := getSecuritySettings(t, stack, sessionCookie)
	start := postSecuritySettings(t, stack, securityTOTPStartPath, url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, page.Body.String(), securityTOTPStartPath)},
	}, sessionCookie)
	if start.Code != http.StatusSeeOther || start.Header().Get("Location") != "/settings/security?totp_replacement=1" {
		t.Fatalf("start TOTP replacement = %d location:%q body:%q", start.Code, start.Header().Get("Location"), start.Body.String())
	}
	challengeCookie := responseCookie(start, "gofer_security_challenge", true)
	if challengeCookie == nil || !challengeCookie.HttpOnly || !challengeCookie.Secure ||
		challengeCookie.SameSite != http.SameSiteLaxMode || challengeCookie.Path != "/settings/security" {
		t.Fatalf("security challenge cookie = %#v", challengeCookie)
	}
	replacementPage := getSecuritySettings(t, stack, sessionCookie, challengeCookie)
	if replacementPage.Code != http.StatusOK {
		t.Fatalf("replacement page = %d %q", replacementPage.Code, replacementPage.Body.String())
	}
	for _, want := range []string{
		"Verify the replacement authenticator", "Manual setup key",
		`action="/settings/security/totp/confirm"`, "current authenticator remains active",
	} {
		if !strings.Contains(replacementPage.Body.String(), want) {
			t.Fatalf("replacement page missing %q", want)
		}
	}
	state, err := manager.GetTOTPReplacement(
		t.Context(), challengeCookie.Value, sessionCookie.Value, "https://gofer.example",
	)
	if err != nil || state == nil || state.Enrollment == nil {
		t.Fatalf("GetTOTPReplacement() = %#v, %v", state, err)
	}
	newSecret := strings.ReplaceAll(state.Enrollment.ManualKey, " ", "")
	code := setupHandlerTOTPCode(t, newSecret)
	confirm := postSecuritySettings(t, stack, securityTOTPConfirmPath, url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, replacementPage.Body.String(), securityTOTPConfirmPath)},
		"code":                 {code},
	}, sessionCookie, challengeCookie)
	if confirm.Code != http.StatusSeeOther || confirm.Header().Get("Location") != "/settings/security?totp_replaced=1" {
		t.Fatalf("confirm TOTP replacement = %d location:%q body:%q", confirm.Code, confirm.Header().Get("Location"), confirm.Body.String())
	}
	rotatedCookie := responseCookie(confirm, "gofer_session", true)
	if rotatedCookie == nil || rotatedCookie.Value == sessionCookie.Value {
		t.Fatalf("rotated security session cookie = %#v", rotatedCookie)
	}
	if cleared := responseCookie(confirm, "gofer_security_challenge", false); cleared == nil || cleared.MaxAge != -1 {
		t.Fatalf("cleared security challenge cookie = %#v", cleared)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value); err != nil || stored != nil {
		t.Fatalf("old session after TOTP replacement = %#v, %v", stored, err)
	}
	confirmationPage := getSecuritySettings(t, stack, rotatedCookie)
	if confirmationPage.Code != http.StatusOK || !strings.Contains(confirmationPage.Body.String(), "Authenticator app") {
		t.Fatalf("security page with rotated session = %d %q", confirmationPage.Code, confirmationPage.Body.String())
	}
	if strings.Contains(confirmationPage.Body.String(), oldSecret) || strings.Contains(confirmationPage.Body.String(), newSecret) {
		t.Fatal("security summary exposed an authenticator seed")
	}
}

func TestSecuritySettingsRecoveryBatchIsDisplayedOnceThenAcknowledged(t *testing.T) {
	manager, _, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	page := getSecuritySettings(t, stack, sessionCookie)
	generated := postSecuritySettings(t, stack, securityRecoveryStartPath, url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, page.Body.String(), securityRecoveryStartPath)},
	}, sessionCookie)
	if generated.Code != http.StatusOK || !strings.Contains(generated.Body.String(), "These plaintext codes are shown once") {
		t.Fatalf("generate recovery batch = %d %q", generated.Code, generated.Body.String())
	}
	challengeCookie := responseCookie(generated, "gofer_security_challenge", true)
	if challengeCookie == nil {
		t.Fatal("recovery generation omitted security challenge cookie")
	}
	state, err := manager.GetRecoveryCodeReplacement(
		t.Context(), challengeCookie.Value, sessionCookie.Value, "https://gofer.example",
	)
	if err != nil || state == nil || state.BatchID == "" {
		t.Fatalf("GetRecoveryCodeReplacement() = %#v, %v", state, err)
	}
	refreshed := getSecuritySettings(t, stack, sessionCookie, challengeCookie)
	if refreshed.Code != http.StatusOK || !strings.Contains(refreshed.Body.String(), "This pending batch was already displayed") {
		t.Fatalf("refreshed recovery batch = %d %q", refreshed.Code, refreshed.Body.String())
	}
	complete := postSecuritySettings(t, stack, securityRecoveryCompletePath, url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, refreshed.Body.String(), securityRecoveryCompletePath)},
		"batch_id":             {state.BatchID},
		"saved":                {"yes"},
	}, sessionCookie, challengeCookie)
	if complete.Code != http.StatusSeeOther || complete.Header().Get("Location") != "/settings/security?recovery_replaced=1" {
		t.Fatalf("complete recovery batch = %d location:%q body:%q", complete.Code, complete.Header().Get("Location"), complete.Body.String())
	}
	confirmation := getSecuritySettingsPath(t, stack, "/settings/security?recovery_replaced=1", sessionCookie)
	if confirmation.Code != http.StatusOK || !strings.Contains(confirmation.Body.String(), "previous unused codes no longer work") ||
		!strings.Contains(confirmation.Body.String(), "10 remaining") {
		t.Fatalf("recovery replacement confirmation = %d %q", confirmation.Code, confirmation.Body.String())
	}
}

func TestSecuritySettingsStaleSessionRequiresTOTPVerification(t *testing.T) {
	manager, db, stack, sessionCookie, secret := completedSecuritySettingsStack(t)
	session, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || session == nil {
		t.Fatalf("load security session = %#v, %v", session, err)
	}
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		UPDATE sessions SET step_up_at = ? WHERE id = ?`, time.Now().UTC().Add(-11*time.Minute), session.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `
		UPDATE totp_credentials SET last_accepted_step = ?
		WHERE user_id = ? AND enabled = 1 AND revoked_at IS NULL`, now.Unix()/30-1, session.UserID,
	); err != nil {
		t.Fatal(err)
	}
	page := getSecuritySettings(t, stack, sessionCookie)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Verify it’s you") ||
		strings.Contains(page.Body.String(), `action="/settings/security/recovery/start"`) {
		t.Fatalf("stale security page = %d %q", page.Code, page.Body.String())
	}
	stepUp := postSecuritySettings(t, stack, securityStepUpPath, url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, page.Body.String(), securityStepUpPath)},
		"code":                 {setupHandlerTOTPCodeAt(t, secret, now)},
	}, sessionCookie)
	if stepUp.Code != http.StatusSeeOther || stepUp.Header().Get("Location") != "/settings/security?verified=1" {
		t.Fatalf("security step-up = %d location:%q body:%q", stepUp.Code, stepUp.Header().Get("Location"), stepUp.Body.String())
	}
	verified := getSecuritySettingsPath(t, stack, "/settings/security?verified=1", sessionCookie)
	if verified.Code != http.StatusOK || !strings.Contains(verified.Body.String(), "Sensitive actions are available for ten minutes") {
		t.Fatalf("verified security page = %d %q", verified.Code, verified.Body.String())
	}
}
