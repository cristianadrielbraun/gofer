package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func TestAdminSecurityVerificationReturnToAllowsOnlyKnownAdminPages(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{value: "/admin/users", want: "/admin/users"},
		{value: "/admin/users/reset-target/credential-reset", want: "/admin/users/reset-target/credential-reset"},
		{value: "/admin/users/reset-target/require-password-change", want: "/admin/users/reset-target/require-password-change"},
		{value: "/admin/users/target_2/mfa-policy", want: "/admin/users/target_2/mfa-policy"},
		{value: "/admin/users/target/status", want: "/admin/users/target/status"},
		{value: "/admin/users/target/delete", want: "/admin/users/target/delete"},
		{value: "/admin/users/invitations/AAAAAAAAAAAAAAAAAAAAA-bbbbbbbbbbbbbbbbbbbb_/revoke", want: "/admin/users/invitations/AAAAAAAAAAAAAAAAAAAAA-bbbbbbbbbbbbbbbbbbbb_/revoke"},
		{value: "/admin/users/invitations/AAAAAAAAAAAAAAAAAAAAA-bbbbbbbbbbbbbbbbbbbb_/rotate", want: "/admin/users/invitations/AAAAAAAAAAAAAAAAAAAAA-bbbbbbbbbbbbbbbbbbbb_/rotate"},
		{value: "/admin/users/invitations/short/rotate"},
		{value: "/admin/users/invitations/AAAAAAAAAAAAAAAAAAAAA-bbbbbbbbbbbbbbbbbbbb_/delete"},
		{value: "/admin/users/target/credential-reset?next=/admin"},
		{value: "/admin/users/target/unknown"},
		{value: "/admin/users/a/b/status"},
		{value: "/admin/users/%3Cscript%3E/status"},
		{value: "/admin/users//status"},
		{value: "/admin/users?reset_user=reset-target"},
		{value: "/admin/activity/administration", want: "/admin/activity/administration"},
		{value: "/admin/activity/administration?kind=security", want: "/admin/activity/administration?kind=security"},
		{value: "/admin/activity/administration?before=40&kind=user", want: "/admin/activity/administration?before=40&kind=user"},
		{value: "/admin/activity/administration?kind=everything"},
		{value: "/admin/activity/administration?before=-1"},
		{value: "/admin/activity/administration?before=04"},
		{value: "/admin/activity/administration?kind=user&next=/admin"},
		{value: "/admin/activity/administration?kind=user&kind=security"},
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
			// A dialog opened while the session was fresh.
			proof := csrfProofFromForm(t, adminUserActionDialog(t, stack, cookie, action.path), action.path)
			if _, err := db.Write().ExecContext(t.Context(), `UPDATE sessions SET step_up_at = ? WHERE id = ?`, now.Add(-11*time.Minute), session.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Write().ExecContext(t.Context(), `UPDATE totp_credentials SET last_accepted_step = ? WHERE user_id = ?`, now.Unix()/30-1, session.UserID); err != nil {
				t.Fatal(err)
			}
			get := func(path string, htmx bool) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodGet, path, nil)
				if htmx {
					r.Header.Set("HX-Request", "true")
				}
				r.AddCookie(cookie)
				w := httptest.NewRecorder()
				stack.ServeHTTP(w, r)
				return w
			}
			old := get("/admin/users?"+action.key+"=resume-target", false)
			if old.Code != http.StatusFound || old.Header().Get("Location") != action.path {
				t.Fatalf("old continuation address = %d %q", old.Code, old.Header().Get("Location"))
			}
			// From the menu, a stale session gets the dialog, and verification
			// to send it with when the page has none.
			fetched := get(action.path, true)
			if fetched.Code != http.StatusOK || !strings.Contains(fetched.Body.String(), `id="`+action.dialog+`resume-target"`) ||
				!strings.Contains(fetched.Body.String(), `action="`+action.path+`" data-step-up`) ||
				!strings.Contains(fetched.Body.String(), `id="admin-security-verification-dialog"`) ||
				!strings.Contains(fetched.Body.String(), `name="return_to" value="`+action.path+`"`) {
				t.Fatalf("stale dialog fetch = %d %q", fetched.Code, fetched.Body.String())
			}
			withVerification := httptest.NewRequest(http.MethodGet, action.path, nil)
			withVerification.Header.Set("HX-Request", "true")
			withVerification.Header.Set(views.AdminVerificationOnPageHeader, "true")
			withVerification.AddCookie(cookie)
			onPage := httptest.NewRecorder()
			stack.ServeHTTP(onPage, withVerification)
			if onPage.Code != http.StatusOK || strings.Contains(onPage.Body.String(), `id="admin-security-verification-dialog"`) {
				t.Fatalf("dialog fetched by a page with verification = %d %q", onPage.Code, onPage.Body.String())
			}
			// Opened at its address, the page shows the dialog, and its form
			// asks for verification when sent.
			page := get(action.path, false)
			if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `name="return_to" value="`+action.path+`"`) ||
				!strings.Contains(page.Body.String(), `id="`+action.dialog+`resume-target" data-tui-dialog data-tui-dialog-open="true"`) ||
				strings.Contains(page.Body.String(), `id="admin-security-verification-dialog" data-tui-dialog data-tui-dialog-open="true"`) {
				t.Fatalf("stale action address = %d %s", page.Code, page.Body.String())
			}
			stalePost := postSecuritySettings(t, stack, action.path, url.Values{auth.CSRFFormFieldName: {proof}}, cookie)
			if stalePost.Code != http.StatusForbidden || !strings.Contains(stalePost.Body.String(), `name="return_to" value="`+action.path+`"`) {
				t.Fatal("expired reset submission lost its target during verification")
			}
			values := url.Values{"code": {setupHandlerTOTPCodeAt(t, secret, now)}, "return_to": {action.path}, auth.CSRFFormFieldName: {csrfProofFromForm(t, page.Body.String(), securityStepUpPath)}}
			r := httptest.NewRequest(http.MethodPost, securityStepUpPath, strings.NewReader(values.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Accept", "application/json")
			r.AddCookie(cookie)
			verified := httptest.NewRecorder()
			stack.ServeHTTP(verified, r)
			if verified.Code != http.StatusOK || !strings.Contains(verified.Body.String(), `"redirect":"`+action.path+`"`) {
				t.Fatalf("verification lost continuation: %d %s", verified.Code, verified.Body.String())
			}
			resumed := get(action.path, false)
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
				path := strings.Replace(action.path, "resume-target", target, 1)
				unavailable := get(path, false)
				if unavailable.Code != http.StatusSeeOther || unavailable.Header().Get("Location") != "/admin/users" ||
					strings.Contains(unavailable.Body.String(), `id="`+action.dialog+target+`"`) || !adminFlashFailed(t, unavailable) {
					t.Fatalf("unavailable target %q = %d %q", target, unavailable.Code, unavailable.Body.String())
				}
			}
		})
	}
}
