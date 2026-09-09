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

func TestAdminSecurityVerificationReturnToAllowsOnlyKnownAdminPages(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{value: "/admin/users", want: "/admin/users"},
		{value: "/admin/users?reset_user=reset-target", want: "/admin/users?reset_user=reset-target"},
		{value: "/admin/users?reset_user="},
		{value: "/admin/users?reset_user=a&reset_user=b"},
		{value: "/admin/users?reset_user=a&next=/admin"},
		{value: "/admin/users?reset_user=%3Cscript%3E"},
		{value: "/admin/security", want: "/admin/security"},
		{value: "/admin/activity", want: "/admin/activity"},
		{value: "/admin/activity?page=2", want: "/admin/activity?page=2"},
		{value: "/admin/activity?page=02", want: "/admin/activity?page=2"},
		{value: "/admin/activity?filter=failures", want: "/admin/activity?filter=failures"},
		{value: "/admin/activity?page=02&filter=sessions", want: "/admin/activity?filter=sessions&page=2"},
		{value: "https://attacker.example/admin/users"},
		{value: "//attacker.example/admin/users"},
		{value: "/admin/users?next=https://attacker.example"},
		{value: "/admin/activity?page=0"},
		{value: "/admin/activity?filter="},
		{value: "/admin/activity?filter=unsupported"},
		{value: "/admin/activity?filter=failures&filter=sessions"},
		{value: "/admin/activity?page=1&private=true"},
		{value: "/settings/security"},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			if got := adminSecurityVerificationReturnTo(test.value); got != test.want {
				t.Fatalf("adminSecurityVerificationReturnTo(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}

func TestAdminUsersDialogVerifiesTOTPWithoutLeavingAdmin(t *testing.T) {
	manager, db, stack, sessionCookie, secret := completedSecuritySettingsStack(t)
	session, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || session == nil {
		t.Fatalf("load administrator session = %#v, %v", session, err)
	}
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		UPDATE sessions SET step_up_at = ? WHERE id = ?`, now.Add(-11*time.Minute), session.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `
		UPDATE totp_credentials SET last_accepted_step = ?
		WHERE user_id = ? AND enabled = 1 AND revoked_at IS NULL`, now.Unix()/30-1, session.UserID,
	); err != nil {
		t.Fatal(err)
	}

	pageRequest := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	pageRequest.AddCookie(sessionCookie)
	page := httptest.NewRecorder()
	stack.ServeHTTP(page, pageRequest)
	if page.Code != http.StatusOK {
		t.Fatalf("stale administrator users page = %d %q", page.Code, page.Body.String())
	}
	html := page.Body.String()
	for _, want := range []string{
		`data-admin-security-verification`, `data-admin-security-totp`,
		`action="/settings/security/step-up"`, `name="return_to" value="/admin/users"`,
		`src="/assets/js/passkey-authentication.js"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("stale administrator users page omitted %q: %q", want, html)
		}
	}
	proof := csrfProofFromForm(t, html, securityStepUpPath)
	postJSON := func(code, returnTo string, includeCSRF bool) *httptest.ResponseRecorder {
		values := url.Values{"code": {code}, "return_to": {returnTo}}
		if includeCSRF {
			values.Set(auth.CSRFFormFieldName, proof)
		}
		request := httptest.NewRequest(http.MethodPost, securityStepUpPath, strings.NewReader(values.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", "Admin verification dialog/1.0")
		request.RemoteAddr = "198.51.100.91:45124"
		request.AddCookie(sessionCookie)
		recorder := httptest.NewRecorder()
		stack.ServeHTTP(recorder, request)
		return recorder
	}

	withoutCSRF := postJSON("000000", "/admin/users", false)
	if withoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("administrator dialog without CSRF = %d %q", withoutCSRF.Code, withoutCSRF.Body.String())
	}
	validCode := setupHandlerTOTPCodeAt(t, secret, now)
	invalidCode := "000000"
	if invalidCode == validCode {
		invalidCode = "000001"
	}
	rejected := postJSON(invalidCode, "/admin/users", true)
	if rejected.Code != http.StatusUnprocessableEntity ||
		rejected.Header().Get("Content-Type") != "application/json" ||
		!strings.Contains(rejected.Body.String(), securityTOTPFailureMessage) {
		t.Fatalf("invalid administrator dialog code = %d headers:%v body:%q", rejected.Code, rejected.Header(), rejected.Body.String())
	}

	verified := postJSON(validCode, "/admin/users", true)
	if verified.Code != http.StatusOK || !strings.Contains(verified.Body.String(), `"redirect":"/admin/users"`) {
		t.Fatalf("administrator dialog verification = %d headers:%v body:%q", verified.Code, verified.Header(), verified.Body.String())
	}

	unlockedRequest := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	unlockedRequest.AddCookie(sessionCookie)
	unlocked := httptest.NewRecorder()
	stack.ServeHTTP(unlocked, unlockedRequest)
	if unlocked.Code != http.StatusOK || strings.Contains(unlocked.Body.String(), `data-admin-security-verification`) ||
		strings.Contains(unlocked.Body.String(), "Recent administrator verification required") {
		t.Fatalf("verified administrator users page remained locked = %d %q", unlocked.Code, unlocked.Body.String())
	}
}

func TestAdminPasswordResetResumesConfirmationAfterVerification(t *testing.T) {
	for _, action := range []struct{ key, path, dialog string }{
		{"reset_user", "/admin/users/resume-target/credential-reset", "admin-user-credential-reset-"},
		{"change_user", "/admin/users/resume-target/require-password-change", "admin-user-required-change-"},
	} {
		t.Run(action.key, func(t *testing.T) {
			manager, db, stack, cookie, secret := completedSecuritySettingsStack(t)
			now := time.Now().UTC()
			_, err := db.Write().ExecContext(t.Context(), `INSERT INTO users
		(id, username, username_normalized, name, status, auth_version, user_type, is_admin, created_at, updated_at)
		VALUES ('resume-target', 'resume-target', 'resume-target', 'Resume Target', 'active', 1, 'webmail', 0, ?, ?)`, now, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Write().ExecContext(t.Context(), `INSERT INTO password_credentials (user_id, password_hash) VALUES ('resume-target', 'fixture-password-hash')`); err != nil {
				t.Fatal(err)
			}
			session, err := manager.GetSessionByToken(t.Context(), cookie.Value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Write().ExecContext(t.Context(), `UPDATE sessions SET step_up_at = ? WHERE id = ?`, now.Add(-11*time.Minute), session.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Write().ExecContext(t.Context(), `UPDATE totp_credentials SET last_accepted_step = ? WHERE user_id = ?`, now.Unix()/30-1, session.UserID); err != nil {
				t.Fatal(err)
			}
			get := func(path string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodGet, path, nil)
				r.AddCookie(cookie)
				w := httptest.NewRecorder()
				stack.ServeHTTP(w, r)
				return w
			}
			continuation := "/admin/users?" + action.key + "=resume-target"
			page := get(continuation)
			if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `name="return_to" value="`+continuation+`"`) {
				t.Fatalf("missing reset continuation: %d %s", page.Code, page.Body.String())
			}
			stalePost := postSecuritySettings(t, stack, action.path, url.Values{
				auth.CSRFFormFieldName: {csrfProofFromForm(t, page.Body.String(), action.path)},
			}, cookie)
			if stalePost.Code != http.StatusForbidden || !strings.Contains(stalePost.Body.String(), `name="return_to" value="`+continuation+`"`) {
				t.Fatal("expired reset submission lost its target during verification")
			}
			values := url.Values{"code": {setupHandlerTOTPCodeAt(t, secret, now)}, "return_to": {continuation}, auth.CSRFFormFieldName: {csrfProofFromForm(t, page.Body.String(), securityStepUpPath)}}
			r := httptest.NewRequest(http.MethodPost, securityStepUpPath, strings.NewReader(values.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Accept", "application/json")
			r.AddCookie(cookie)
			verified := httptest.NewRecorder()
			stack.ServeHTTP(verified, r)
			if verified.Code != http.StatusOK || !strings.Contains(verified.Body.String(), `"redirect":"`+continuation+`"`) {
				t.Fatalf("verification lost continuation: %d %s", verified.Code, verified.Body.String())
			}
			resumed := get(continuation)
			html := resumed.Body.String()
			start := strings.Index(html, `id="`+action.dialog+`resume-target"`)
			if resumed.Code != http.StatusOK || start < 0 {
				t.Fatal("reset dialog missing after verification")
			}
			end := strings.Index(html[start:], ">")
			if !strings.Contains(html[start:start+end], `data-tui-dialog-open="true"`) {
				t.Fatal("reset confirmation did not reopen")
			}
			var tokens int
			if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens WHERE user_id = 'resume-target'`).Scan(&tokens); err != nil || tokens != 0 {
				t.Fatalf("verification issued a token: %d %v", tokens, err)
			}
			var required bool
			if err := db.Read().QueryRowContext(t.Context(), `SELECT must_change FROM password_credentials WHERE user_id = 'resume-target'`).Scan(&required); err != nil || required {
				t.Fatal("verification required a password change before confirmation")
			}
			for _, target := range []string{"missing-target", session.UserID} {
				unavailable := get("/admin/users?" + action.key + "=" + target)
				if strings.Contains(unavailable.Body.String(), `id="`+action.dialog+target+`"`) {
					t.Fatal("unavailable target received a reset dialog")
				}
			}
		})
	}
}
