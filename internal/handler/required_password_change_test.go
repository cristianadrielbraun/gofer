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

func TestForcedPasswordChangeRestrictsAccessUntilCompleted(t *testing.T) {
	manager, db, stack, adminCookie, _ := completedSecuritySettingsStack(t)
	hash, err := auth.HashPassword(localLoginPassword)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `INSERT INTO users
 (id, username, username_normalized, name, status, auth_version, user_type, is_admin, created_at, updated_at)
 VALUES ('forced-user', 'forced-user', 'forced-user', 'Forced User', 'active', 1, 'webmail', 0, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `INSERT INTO password_credentials (user_id, password_hash) VALUES ('forced-user', ?)`, hash); err != nil {
		t.Fatal(err)
	}
	oldSession, err := manager.CreateAuthenticatedSession(t.Context(), "forced-user", "old browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelSingleFactor)
	if err != nil {
		t.Fatal(err)
	}
	adminPage := getSecuritySettingsPath(t, stack, "/admin/users", adminCookie)
	path := "/admin/users/forced-user/require-password-change"
	if !strings.Contains(adminPage.Body.String(), "Require password change at next login") || strings.Contains(adminPage.Body.String(), `name="require_password_change"`) {
		t.Fatal("missing separate password-change action or retained combined control")
	}
	proof := csrfProofFromForm(t, adminPage.Body.String(), path)
	withoutProof := postSecuritySettings(t, stack, path, url.Values{}, adminCookie)
	if withoutProof.Code != http.StatusForbidden {
		t.Fatal("required-change action accepted missing CSRF")
	}
	issued := postSecuritySettings(t, stack, path, url.Values{auth.CSRFFormFieldName: {proof}}, adminCookie)
	if issued.Code != http.StatusSeeOther {
		t.Fatalf("issue required change: %d %s", issued.Code, issued.Body.String())
	}
	if found, err := manager.GetSessionByToken(t.Context(), oldSession.Token); err != nil || found != nil {
		t.Fatal("existing session survived required change")
	}
	var metadata string
	if err := db.Read().QueryRowContext(t.Context(), `SELECT metadata_json FROM auth_events WHERE subject_user_id = 'forced-user' AND event_type = ?`, auth.AuthEventPasswordChangeRequired).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(metadata, localLoginPassword) || strings.Contains(metadata, hash) {
		t.Fatal("incorrect or sensitive issuance metadata")
	}
	var tokenCount int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens WHERE user_id = 'forced-user'`).Scan(&tokenCount); err != nil || tokenCount != 0 {
		t.Fatal("required-change action generated a reset token")
	}
	resultPage := getSecuritySettingsPath(t, stack, issued.Header().Get("Location"), adminCookie)
	if !strings.Contains(resultPage.Body.String(), "Password change required") || strings.Contains(resultPage.Body.String(), `id="admin-user-credential-reset-result-dialog"`) {
		t.Fatal("required-change result displayed a token dialog or omitted status")
	}
	var stored string
	if err := db.Read().QueryRowContext(t.Context(), `SELECT password_hash FROM password_credentials WHERE user_id = 'forced-user'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if matched, _, err := auth.VerifyPassword(stored, localLoginPassword); err != nil || !matched {
		t.Fatalf("stored password verification failed: match=%v err=%v", matched, err)
	}
	login := postSecuritySettings(t, stack, "/login", url.Values{"identifier": {"forced-user"}, "password": {localLoginPassword}})
	var cookie *http.Cookie
	for _, c := range login.Result().Cookies() {
		if c.Name == "gofer_session" && c.Value != "" {
			cookie = c
		}
	}
	if login.Code != http.StatusSeeOther || cookie == nil {
		t.Fatalf("old password did not enter restricted flow: %d %s", login.Code, login.Body.String())
	}
	session, err := manager.GetSessionByToken(t.Context(), cookie.Value)
	if err != nil || session == nil || !session.PasswordChangeRequired {
		t.Fatal("login session is not restricted")
	}
	// A non-password authentication method must not remove the requirement.
	alternate, err := manager.CreateAuthenticatedSession(t.Context(), "forced-user", "passkey browser", auth.AuthenticationMethodPasskey, auth.AssuranceLevelPhishingResistant)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []*http.Cookie{cookie, {Name: cookie.Name, Value: alternate.Token}} {
		for _, tc := range []struct {
			path, accept string
			status       int
		}{
			{"/", "", http.StatusSeeOther}, {"/settings/security", "", http.StatusSeeOther},
			{"/api/accounts", "", http.StatusForbidden}, {"/events", "text/event-stream", http.StatusForbidden},
		} {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.AddCookie(c)
			req.Header.Set("Accept", tc.accept)
			w := httptest.NewRecorder()
			stack.ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("restricted %s: %d %s", tc.path, w.Code, w.Body.String())
			}
			if tc.status == http.StatusSeeOther && w.Header().Get("Location") != auth.RequiredPasswordChangePath {
				t.Fatal("wrong restricted redirect")
			}
		}
	}
	altCookie := &http.Cookie{Name: cookie.Name, Value: alternate.Token}
	altPage := getSecuritySettingsPath(t, stack, auth.RequiredPasswordChangePath, altCookie)
	loggedOut := postSecuritySettings(t, stack, "/auth/logout", url.Values{auth.CSRFFormFieldName: {csrfProofFromForm(t, altPage.Body.String(), "/auth/logout")}}, altCookie)
	if loggedOut.Code != http.StatusSeeOther {
		t.Fatalf("restricted logout failed: %d", loggedOut.Code)
	}
	page := getSecuritySettingsPath(t, stack, auth.RequiredPasswordChangePath, cookie)
	if page.Code != http.StatusOK || page.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("restricted page: %d", page.Code)
	}
	for _, secret := range []string{localLoginPassword, hash, cookie.Value} {
		if strings.Contains(page.Body.String(), secret) {
			t.Fatal("restricted page leaked credentials")
		}
	}
	changeProof := csrfProofFromForm(t, page.Body.String(), auth.RequiredPasswordChangePath)
	newPassword := "a different required password for webmail"
	submit := func(current, password, confirm, csrf string) *httptest.ResponseRecorder {
		return postSecuritySettings(t, stack, auth.RequiredPasswordChangePath, url.Values{"current_password": {current}, "new_password": {password}, "confirm_password": {confirm}, auth.CSRFFormFieldName: {csrf}}, cookie)
	}
	if w := submit(localLoginPassword, newPassword, newPassword, ""); w.Code != http.StatusForbidden {
		t.Fatal("forced change accepted missing CSRF")
	}
	for _, fields := range [][3]string{{"wrong secret", newPassword, newPassword}, {localLoginPassword, localLoginPassword, localLoginPassword}, {localLoginPassword, "short", "short"}, {localLoginPassword, newPassword, "mismatch"}} {
		if w := submit(fields[0], fields[1], fields[2], changeProof); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid change: %d %s", w.Code, w.Body.String())
		}
	}
	done := submit(localLoginPassword, newPassword, newPassword, changeProof)
	if done.Code != http.StatusSeeOther || done.Header().Get("Location") != "/" {
		t.Fatalf("complete required change: %d %s", done.Code, done.Body.String())
	}
	var rotated *http.Cookie
	for _, c := range done.Result().Cookies() {
		if c.Name == cookie.Name && c.Value != "" {
			rotated = c
		}
	}
	if rotated == nil {
		t.Fatal("missing rotated session")
	}
	full, err := manager.GetSessionByToken(t.Context(), rotated.Value)
	if err != nil || full == nil || full.PasswordChangeRequired {
		t.Fatal("new session remained restricted")
	}
	if found, err := manager.GetSessionByToken(t.Context(), alternate.Token); err != nil || found != nil {
		t.Fatal("parallel session survived password change")
	}
	var remaining int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens WHERE user_id = 'forced-user' AND used_at IS NULL AND revoked_at IS NULL`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("reset link survived forced change")
	}
	if w := getSecuritySettingsPath(t, stack, "/settings/security", rotated); w.Code != http.StatusOK {
		t.Fatalf("new session lacks ordinary access: %d", w.Code)
	}
	if _, err := manager.AuthenticatePassword(t.Context(), auth.PasswordLoginOptions{Identifier: "forced-user", Password: localLoginPassword, Source: "test"}); err == nil {
		t.Fatal("old password still accepted")
	}
}
