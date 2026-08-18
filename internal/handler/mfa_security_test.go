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
		"Passkeys", "Add passkey", `data-passkey-registration`,
		`src="/assets/js/passkey-registration.js"`,
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

func TestSecuritySettingsStartsAndRejectsPasskeyRegistrationThroughBoundJSONEndpoints(t *testing.T) {
	_, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	page := getSecuritySettings(t, stack, sessionCookie)
	startProof := csrfProofForSession(t, auth.NewManager(&auth.Config{Enabled: true}, db), sessionCookie.Value, securityPasskeyStartPath)
	started := postSecuritySettings(t, stack, securityPasskeyStartPath, url.Values{
		auth.CSRFFormFieldName: {startProof},
		"name":                 {"Work laptop"},
	}, sessionCookie)
	if started.Code != http.StatusOK || started.Header().Get("Content-Type") != "application/json" ||
		started.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("start passkey registration = %d headers:%v body:%q", started.Code, started.Header(), started.Body.String())
	}
	for _, want := range []string{
		`"name":"Gofer"`, `"id":"gofer.example"`, `"residentKey":"preferred"`,
		`"userVerification":"required"`, `"attestation":"none"`,
	} {
		if !strings.Contains(started.Body.String(), want) {
			t.Fatalf("passkey creation options missing %q: %s", want, started.Body.String())
		}
	}
	challengeCookie := responseCookie(started, "gofer_security_challenge", true)
	if challengeCookie == nil || !challengeCookie.HttpOnly || !challengeCookie.Secure ||
		challengeCookie.Path != "/settings/security" || challengeCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("passkey challenge cookie = %#v", challengeCookie)
	}
	finishRequest := httptest.NewRequest(http.MethodPost, securityPasskeyFinishPath, strings.NewReader(`{}`))
	finishRequest.Header.Set("Content-Type", "application/json")
	finishRequest.Header.Set(auth.CSRFHeaderName, csrfProofForSession(t, auth.NewManager(&auth.Config{Enabled: true}, db), sessionCookie.Value, securityPasskeyFinishPath))
	finishRequest.AddCookie(sessionCookie)
	finishRequest.AddCookie(challengeCookie)
	finishRecorder := httptest.NewRecorder()
	stack.ServeHTTP(finishRecorder, finishRequest)
	if finishRecorder.Code != http.StatusUnprocessableEntity || !strings.Contains(finishRecorder.Body.String(), "not accepted") {
		t.Fatalf("invalid passkey finish = %d %q", finishRecorder.Code, finishRecorder.Body.String())
	}
	var activeChallenges, credentials int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges WHERE consumed_at IS NULL`).Scan(&activeChallenges); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM webauthn_credentials`).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if activeChallenges != 0 || credentials != 0 {
		t.Fatalf("rejected passkey finish mutated state: challenges=%d credentials=%d", activeChallenges, credentials)
	}
	if !strings.Contains(page.Body.String(), `data-start-csrf="`) || !strings.Contains(page.Body.String(), `data-finish-csrf="`) {
		t.Fatal("passkey registration form omitted endpoint-bound CSRF proofs")
	}
}

func TestSecuritySettingsListsAndRemovesOwnedPasskey(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	session, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || session == nil {
		t.Fatalf("load security session = %#v, %v", session, err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO webauthn_credentials (
			id, user_id, credential_id, public_key, name, attachment, transports, rp_id
		) VALUES ('settings-passkey', ?, x'0102', x'0304', 'Existing security key',
		          'cross-platform', '["usb"]', 'gofer.example')`, session.UserID,
	); err != nil {
		t.Fatalf("insert settings passkey: %v", err)
	}
	page := getSecuritySettings(t, stack, sessionCookie)
	removePath := "/settings/security/passkeys/settings-passkey/remove"
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Existing security key") ||
		!strings.Contains(page.Body.String(), `action="`+removePath+`"`) {
		t.Fatalf("passkey settings page = %d %q", page.Code, page.Body.String())
	}
	removed := postSecuritySettings(t, stack, removePath, url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, page.Body.String(), removePath)},
	}, sessionCookie)
	if removed.Code != http.StatusSeeOther || removed.Header().Get("Location") != "/settings/security?passkey_removed=1" {
		t.Fatalf("remove passkey = %d location:%q body:%q", removed.Code, removed.Header().Get("Location"), removed.Body.String())
	}
	rotatedCookie := responseCookie(removed, "gofer_session", true)
	if rotatedCookie == nil || rotatedCookie.Value == sessionCookie.Value {
		t.Fatalf("passkey removal session cookie = %#v", rotatedCookie)
	}
	var revoked bool
	if err := db.Read().QueryRow(`SELECT revoked_at IS NOT NULL FROM webauthn_credentials WHERE id = 'settings-passkey'`).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("settings passkey revoked=%t err=%v", revoked, err)
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
	state, err := manager.GetTOTPManagement(
		t.Context(), challengeCookie.Value, sessionCookie.Value, "https://gofer.example",
	)
	if err != nil || state == nil || state.Enrollment == nil {
		t.Fatalf("GetTOTPManagement() = %#v, %v", state, err)
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

func TestSecuritySettingsEnrollsFirstAuthenticatorThroughSessionBoundChallenge(t *testing.T) {
	manager, db, stack, sessionCookie, oldSecret := completedSecuritySettingsStack(t)
	session, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || session == nil {
		t.Fatalf("load enrollment session = %#v, %v", session, err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `
		UPDATE users SET is_admin = 0, mfa_required = 0 WHERE id = ?;
		DELETE FROM recovery_codes WHERE user_id = ?;
		DELETE FROM totp_credentials WHERE user_id = ?`,
		session.UserID, session.UserID, session.UserID,
	); err != nil {
		t.Fatal(err)
	}
	page := getSecuritySettings(t, stack, sessionCookie)
	for _, want := range []string{
		"Not enrolled", "Set up authenticator", `action="/settings/security/totp/start"`,
	} {
		if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), want) {
			t.Fatalf("unenrolled security page missing %q: %d %q", want, page.Code, page.Body.String())
		}
	}
	if strings.Contains(page.Body.String(), "Replace authenticator") {
		t.Fatal("unenrolled security page rendered replacement action")
	}
	start := postSecuritySettings(t, stack, securityTOTPStartPath, url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, page.Body.String(), securityTOTPStartPath)},
	}, sessionCookie)
	if start.Code != http.StatusSeeOther || start.Header().Get("Location") != "/settings/security?totp_enrollment=1" {
		t.Fatalf("start TOTP enrollment = %d location:%q body:%q", start.Code, start.Header().Get("Location"), start.Body.String())
	}
	challengeCookie := responseCookie(start, "gofer_security_challenge", true)
	if challengeCookie == nil || !challengeCookie.HttpOnly || !challengeCookie.Secure ||
		challengeCookie.SameSite != http.SameSiteLaxMode || challengeCookie.Path != "/settings/security" {
		t.Fatalf("TOTP enrollment challenge cookie = %#v", challengeCookie)
	}
	enrollmentPage := getSecuritySettings(t, stack, sessionCookie, challengeCookie)
	for _, want := range []string{
		"Verify the new authenticator", "authenticator is not enabled until the code is verified",
		`alt="QR code containing the new Gofer authenticator key"`, "Enable authenticator",
		`action="/settings/security/totp/confirm"`,
	} {
		if enrollmentPage.Code != http.StatusOK || !strings.Contains(enrollmentPage.Body.String(), want) {
			t.Fatalf("TOTP enrollment page missing %q: %d %q", want, enrollmentPage.Code, enrollmentPage.Body.String())
		}
	}
	if strings.Contains(enrollmentPage.Body.String(), "current authenticator remains active") {
		t.Fatal("first-time TOTP enrollment claimed an existing authenticator remains active")
	}
	state, err := manager.GetTOTPManagement(
		t.Context(), challengeCookie.Value, sessionCookie.Value, "https://gofer.example",
	)
	if err != nil || state == nil || state.Enrollment == nil || state.IsReplacement {
		t.Fatalf("GetTOTPManagement() enrollment = %#v, %v", state, err)
	}
	newSecret := strings.ReplaceAll(state.Enrollment.ManualKey, " ", "")
	confirm := postSecuritySettings(t, stack, securityTOTPConfirmPath, url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, enrollmentPage.Body.String(), securityTOTPConfirmPath)},
		"code":                 {setupHandlerTOTPCode(t, newSecret)},
	}, sessionCookie, challengeCookie)
	if confirm.Code != http.StatusSeeOther || confirm.Header().Get("Location") != "/settings/security?totp_enrolled=1" {
		t.Fatalf("confirm TOTP enrollment = %d location:%q body:%q", confirm.Code, confirm.Header().Get("Location"), confirm.Body.String())
	}
	rotatedCookie := responseCookie(confirm, "gofer_session", true)
	if rotatedCookie == nil || rotatedCookie.Value == sessionCookie.Value {
		t.Fatalf("rotated TOTP enrollment session cookie = %#v", rotatedCookie)
	}
	confirmationPage := getSecuritySettingsPath(t, stack, "/settings/security?totp_enrolled=1", rotatedCookie)
	for _, want := range []string{
		"Authenticator enabled", "Generate and save recovery codes next", "Enrolled",
		"0 remaining", "Generate new recovery codes",
	} {
		if confirmationPage.Code != http.StatusOK || !strings.Contains(confirmationPage.Body.String(), want) {
			t.Fatalf("TOTP enrollment confirmation missing %q: %d %q", want, confirmationPage.Code, confirmationPage.Body.String())
		}
	}
	if strings.Contains(confirmationPage.Body.String(), oldSecret) || strings.Contains(confirmationPage.Body.String(), newSecret) {
		t.Fatal("TOTP enrollment confirmation exposed an authenticator seed")
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
