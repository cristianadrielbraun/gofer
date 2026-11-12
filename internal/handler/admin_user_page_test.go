package handler

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

func TestAdminUserPageShowsSomeonesAccountAndSignIn(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	addBulkPeople(t, db, "active", "page-person")
	if _, err := db.Write().ExecContext(t.Context(), `INSERT INTO recovery_codes (id, user_id, batch_id, code_hash) VALUES ('page-code', 'page-person', 'batch', 'hash')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `INSERT INTO auth_identities (id, user_id, provider, issuer, subject, email)
		VALUES ('page-identity', 'page-person', 'microsoft', 'https://login.microsoftonline.com/tenant/v2.0', 'subject', 'page.person@example.test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateAuthenticatedSession(t.Context(), "page-person", "Desk browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelSingleFactor); err != nil {
		t.Fatal(err)
	}

	// The list links each person to their page, keeping the list to go
	// back to.
	list := getSecuritySettingsPath(t, stack, "/admin/users?q=page", sessionCookie).Body.String()
	pagePath := "/admin/users/page-person?list=" + url.QueryEscape("/admin/users?q=page")
	if !strings.Contains(list, `href="`+html.EscapeString(pagePath)+`"`) {
		t.Fatalf("the list doesn't link to the person's page: %q", list)
	}

	overview := getSecuritySettingsPath(t, stack, pagePath, sessionCookie)
	body := overview.Body.String()
	if overview.Code != http.StatusOK || !strings.Contains(body, "data-admin-user-page") || !strings.Contains(body, "page-person") ||
		!strings.Contains(body, `href="/admin/users?q=page"`) || !strings.Contains(body, `aria-current="page"`) ||
		!strings.Contains(body, "Overview") || !strings.Contains(body, "Account") {
		t.Fatalf("overview = %d %q", overview.Code, body)
	}
	// The actions menu leads back to this page after a change.
	signOut := adminUserSignOutPath("page-person")
	if !strings.Contains(body, `href="`+signOut+`?list=`+html.EscapeString(url.QueryEscape(pagePath))+`"`) {
		t.Fatalf("the page's menu doesn't come back to it: %q", body)
	}

	signIn := getSecuritySettingsPath(t, stack, "/admin/users/page-person/sign-in", sessionCookie)
	body = signIn.Body.String()
	for _, want := range []string{"How they sign in", "1 left", "On 1 browser or device", "Microsoft", "page.person@example.test", "Not set up"} {
		if !strings.Contains(body, want) {
			t.Fatalf("sign-in tab lacks %q: %q", want, body)
		}
	}

	// A change made from the page returns to it.
	request := httptest.NewRequest(http.MethodGet, signOut+"?list="+url.QueryEscape(pagePath), nil)
	request.Header.Set("HX-Request", "true")
	request.AddCookie(sessionCookie)
	opened := httptest.NewRecorder()
	stack.ServeHTTP(opened, request)
	dialog := opened.Body.String()
	if opened.Code != http.StatusOK || !strings.Contains(dialog, `name="list" value="`+html.EscapeString(pagePath)+`"`) {
		t.Fatalf("dialog from the page = %d %q", opened.Code, dialog)
	}
	signedOut := postSecuritySettings(t, stack, signOut, url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, dialog, signOut)}, "list": {pagePath},
	}, sessionCookie)
	if signedOut.Code != http.StatusSeeOther || signedOut.Header().Get("Location") != pagePath {
		t.Fatalf("sign out from the page = %d %q", signedOut.Code, signedOut.Header().Get("Location"))
	}

	// Someone who isn't there goes back to the list, with a word why.
	gone := getSecuritySettingsPath(t, stack, "/admin/users/nobody-here", sessionCookie)
	if flash, ok := adminFlashOf(t, gone); gone.Code != http.StatusSeeOther || gone.Header().Get("Location") != "/admin/users" || !ok || flash.Message != "That person is no longer here." {
		t.Fatalf("unknown person = %d %q %+v", gone.Code, gone.Header().Get("Location"), flash)
	}
}

func TestAdminUserPageAddressesAreRebuilt(t *testing.T) {
	for _, test := range []struct{ value, want string }{
		{value: "/admin/users/person-1", want: "/admin/users/person-1"},
		{value: "/admin/users/person-1/sign-in", want: "/admin/users/person-1/sign-in"},
		{value: "/admin/users/person-1?list=%2Fadmin%2Fusers%3Fq%3Dana", want: "/admin/users/person-1?list=%2Fadmin%2Fusers%3Fq%3Dana"},
		{value: "/admin/users/person-1/sessions"},
		{value: "/admin/users/person-1?next=/admin"},
		{value: "/admin/users/person-1?list=https://attacker.example"},
		{value: "/admin/users/person-1?list=%2Fadmin%2Fusers%2Fperson-2"},
		{value: "/admin/users/export"},
		{value: "/admin/users/bulk"},
		{value: "/admin/users/%3Cscript%3E"},
	} {
		parsed, err := url.Parse(test.value)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := adminUserPageAddress(parsed); got != test.want || ok != (test.want != "") {
			t.Errorf("adminUserPageAddress(%q) = %q, %v, want %q", test.value, got, ok, test.want)
		}
		if got := adminSecurityVerificationReturnTo(test.value); got != test.want {
			t.Errorf("adminSecurityVerificationReturnTo(%q) = %q, want %q", test.value, got, test.want)
		}
	}
}
