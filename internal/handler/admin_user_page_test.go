package handler

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
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

	// Without a store per person, accounts are read from the one database.
	if mailboxes := getSecuritySettingsPath(t, stack, "/admin/users/page-person/mailboxes", sessionCookie).Body.String(); !strings.Contains(mailboxes, "They haven&#39;t added a mail account yet.") {
		t.Fatalf("mailboxes tab = %q", mailboxes)
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
		{value: "/admin/users/person-1/sessions", want: "/admin/users/person-1/sessions"},
		{value: "/admin/users/person-1/mailboxes", want: "/admin/users/person-1/mailboxes"},
		{value: "/admin/users/person-1/activity", want: "/admin/users/person-1/activity"},
		{value: "/admin/users/person-1/settings"},
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

func TestAdminUserSessionsTabSignsOutOneSession(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	addBulkPeople(t, db, "active", "session-person")
	for _, agent := range []string{
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1",
	} {
		if _, err := manager.CreateAuthenticatedSession(t.Context(), "session-person", agent, auth.AuthenticationMethodPassword, auth.AssuranceLevelSingleFactor); err != nil {
			t.Fatal(err)
		}
	}
	tab := "/admin/users/session-person/sessions"
	page := getSecuritySettingsPath(t, stack, tab, sessionCookie)
	body := page.Body.String()
	if page.Code != http.StatusOK || strings.Count(body, "data-admin-user-session>") != 2 || !strings.Contains(body, "Chrome on Linux") ||
		!strings.Contains(body, "Where they&#39;re signed in") || !strings.Contains(body, "Password") {
		t.Fatalf("sessions tab = %d %q", page.Code, body)
	}
	actions := regexp.MustCompile(`action="(/admin/users/session-person/sessions/[A-Za-z0-9_-]{43}/sign-out)"`).FindAllStringSubmatch(body, -1)
	if len(actions) != 2 {
		t.Fatalf("sign-out forms = %q", actions)
	}
	signOut := actions[0][1]
	response := postSecuritySettings(t, stack, signOut, url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, body, signOut)}, "list": {tab},
	}, sessionCookie)
	if flash, ok := adminFlashOf(t, response); response.Code != http.StatusSeeOther || response.Header().Get("Location") != tab ||
		!ok || !strings.HasPrefix(flash.Message, "Signed out of ") {
		t.Fatalf("sign out one session = %d %q %+v", response.Code, response.Header().Get("Location"), flash)
	}
	if after := getSecuritySettingsPath(t, stack, tab, sessionCookie).Body.String(); strings.Count(after, "data-admin-user-session>") != 1 {
		t.Fatalf("sessions after signing one out = %q", after)
	}
	again := postSecuritySettings(t, stack, signOut, url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, body, signOut)}, "list": {tab},
	}, sessionCookie)
	if flash, ok := adminFlashOf(t, again); again.Code != http.StatusSeeOther || !ok || flash.Message != "That session has already ended." {
		t.Fatalf("sign out an ended session = %d %+v", again.Code, flash)
	}
}

func TestAdminUserPageShowsWhatItsChangesIssue(t *testing.T) {
	_, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	addBulkPeople(t, db, "active", "reset-person")
	if _, err := db.Write().ExecContext(t.Context(), `INSERT INTO password_credentials (user_id, password_hash) VALUES ('reset-person', 'hash')`); err != nil {
		t.Fatal(err)
	}
	pagePath := "/admin/users/reset-person/sign-in"
	resetPath := adminUserCredentialResetPath("reset-person")
	// The dialog's own address, opened from the page, draws the page behind.
	behind := getSecuritySettingsPath(t, stack, resetPath+"?list="+url.QueryEscape(pagePath), sessionCookie).Body.String()
	if !strings.Contains(behind, "data-admin-user-page") || strings.Contains(behind, "data-admin-users-results") ||
		!strings.Contains(behind, `name="list" value="`+pagePath+`"`) {
		t.Fatalf("dialog at its address from the page = %q", behind)
	}
	issued := postSecuritySettings(t, stack, resetPath, url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, behind, resetPath)}, "list": {pagePath},
	}, sessionCookie)
	body := issued.Body.String()
	if issued.Code != http.StatusCreated || !strings.Contains(body, "data-admin-user-page") || !strings.Contains(body, "How they sign in") ||
		!strings.Contains(body, "/account/redeem") {
		t.Fatalf("reset token from the page = %d %q", issued.Code, body)
	}
}

func TestAdminUserActivityTabShowsWhatHappenedToThem(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	addBulkPeople(t, db, "active", "watched-person", "other-person")
	for _, id := range []string{"watched-person", "other-person"} {
		if _, err := manager.CreateAuthenticatedSession(t.Context(), id, "Browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelSingleFactor); err != nil {
			t.Fatal(err)
		}
		path := adminUserSignOutPath(id)
		dialog := adminUserActionDialog(t, stack, sessionCookie, path)
		if response := postSecuritySettings(t, stack, path, url.Values{auth.CSRFFormFieldName: {csrfProofFromForm(t, dialog, path)}}, sessionCookie); response.Code != http.StatusSeeOther {
			t.Fatalf("sign %s out = %d", id, response.Code)
		}
	}
	tab := "/admin/users/watched-person/activity"
	body := getSecuritySettingsPath(t, stack, tab, sessionCookie).Body.String()
	if !strings.Contains(body, "Signed watched-person out everywhere") || strings.Contains(body, "Signed other-person out everywhere") ||
		!strings.Contains(body, "Changes made to them") || strings.Contains(body, "Changes they made") ||
		!strings.Contains(body, `data-audit-action="user.signed_out"`) {
		t.Fatalf("activity tab = %q", body)
	}
	// An administrator's page also lists the changes they made.
	session, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || session == nil {
		t.Fatalf("administrator session = %#v, %v", session, err)
	}
	own := getSecuritySettingsPath(t, stack, "/admin/users/"+session.UserID+"/activity", sessionCookie).Body.String()
	if !strings.Contains(own, "Changes they made") || !strings.Contains(own, "Signed other-person out everywhere") {
		t.Fatalf("administrator's activity tab = %q", own)
	}

	// A stale session sees the tab locked; nothing is read, so no refusal
	// is recorded.
	if _, err := db.Write().ExecContext(t.Context(), `UPDATE sessions SET step_up_at = ? WHERE id = ?`, time.Now().UTC().Add(-11*time.Minute), session.ID); err != nil {
		t.Fatal(err)
	}
	locked := getSecuritySettingsPath(t, stack, tab, sessionCookie).Body.String()
	if !strings.Contains(locked, "data-admin-user-activity-locked") || strings.Contains(locked, "Signed watched-person out everywhere") {
		t.Fatalf("stale activity tab = %q", locked)
	}
	if denied, _ := audit.List(t.Context(), db.Read(), audit.Filter{Action: "access.denied"}, 0, 10); len(denied.Records) != 0 {
		t.Fatalf("viewing a locked tab recorded refusals: %+v", denied.Records)
	}
}
