package handler

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/admin"
	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// addBulkPeople adds webmail users by ID, each with status.
func addBulkPeople(t *testing.T, db *storage.DB, status string, ids ...string) {
	t.Helper()
	now := time.Now().UTC()
	for _, id := range ids {
		if _, err := db.Write().ExecContext(t.Context(), `INSERT INTO users (id, username, username_normalized, name, status, auth_version, user_type, is_admin, created_at, updated_at)
			VALUES (?, ?, ?, '', ?, 1, 'webmail', 0, ?, ?)`, id, id, id, status, now, now); err != nil {
			t.Fatal(err)
		}
	}
}

// bulkDialog fetches a change's dialog the way the selection's menu does.
func bulkDialog(t *testing.T, stack http.Handler, cookie *http.Cookie, path string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("HX-Request", "true")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	stack.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "<html") {
		t.Fatalf("GET %s = %d %q", path, response.Code, response.Body.String())
	}
	return response.Body.String()
}

// bulkList returns the lines of a dialog's list marked with marker, each
// a person and what it says about them.
func bulkList(page, marker string) []string {
	start := strings.Index(page, marker)
	if start < 0 {
		return nil
	}
	section := page[start:]
	if end := strings.Index(section, "</section>"); end >= 0 {
		section = section[:end]
	}
	var lines []string
	for _, item := range bulkListItem.FindAllStringSubmatch(section, -1) {
		var words []string
		for _, span := range bulkListSpan.FindAllStringSubmatch(item[1], -1) {
			words = append(words, html.UnescapeString(strings.TrimSpace(span[1])))
		}
		lines = append(lines, strings.Join(words, " — "))
	}
	return lines
}

var (
	bulkListItem = regexp.MustCompile(`(?s)<li[^>]*>(.*?)</li>`)
	bulkListSpan = regexp.MustCompile(`(?s)<span[^>]*>([^<]*)</span>`)
)

func TestAdminUsersBulkDialogSaysWhomTheChangeLeavesOut(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	session, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || session == nil {
		t.Fatalf("administrator session = %#v, %v", session, err)
	}
	addBulkPeople(t, db, "active", "bulk-amber", "bulk-basil")
	addBulkPeople(t, db, "disabled", "bulk-cedar")
	addBulkPeople(t, db, "pending", "bulk-dune")

	page := getSecuritySettingsPath(t, stack, "/admin/users?q=bulk", sessionCookie).Body.String()
	for _, want := range []string{
		`data-admin-users-bulk`, `hx-get="/admin/users/bulk/disable"`, `hx-get="/admin/users/bulk/sign-out"`,
		`name="user" value="bulk-amber"`, `aria-label="Select bulk-amber"`, `aria-label="Select everyone on this page"`,
		`name="list" value="/admin/users?q=bulk"`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("Users page is missing %q", want)
		}
	}

	dialog := bulkDialog(t, stack, sessionCookie, "/admin/users/bulk/disable?user=bulk-amber&user=bulk-cedar&user="+session.UserID+"&user=bulk-dune&user=gone-person&user=bulk-amber&list="+url.QueryEscape("/admin/users?q=bulk"))
	if !strings.Contains(dialog, "Disable 1 person?") || !strings.Contains(dialog, `action="/admin/users/bulk/disable"`) || !strings.Contains(dialog, `name="list" value="/admin/users?q=bulk"`) {
		t.Fatalf("disable dialog = %q", dialog)
	}
	if got := bulkList(dialog, "data-admin-users-bulk-change"); strings.Join(got, "|") != "bulk-amber" {
		t.Fatalf("would change %q", got)
	}
	administrator, err := manager.GetUserByID(t.Context(), session.UserID)
	if err != nil || administrator == nil {
		t.Fatal(err)
	}
	wantLeftOut := []string{"bulk-cedar — Already disabled", administrator.Username + " — That's you", "bulk-dune — Hasn't accepted the invitation yet", "gone-person — No longer exists"}
	if got := bulkList(dialog, "data-admin-users-bulk-left-out"); strings.Join(got, "|") != strings.Join(wantLeftOut, "|") {
		t.Fatalf("left out %q, want %q", got, wantLeftOut)
	}
	if strings.Count(dialog, `name="user"`) != 1 || !strings.Contains(dialog, `name="user" value="bulk-amber"`) {
		t.Fatalf("the change would send other people: %q", dialog)
	}

	// Everyone the list matches, read from the list's own search.
	all := bulkDialog(t, stack, sessionCookie, "/admin/users/bulk/sign-out?all=1&q=bulk&status=active")
	if got := bulkList(all, "data-admin-users-bulk-change"); strings.Join(got, "|") != "bulk-amber|bulk-basil" || !strings.Contains(all, "Sign 2 people out everywhere?") {
		t.Fatalf("everyone matching = %q in %q", got, all)
	}
	if nothing := bulkDialog(t, stack, sessionCookie, "/admin/users/bulk/enable?user=bulk-amber"); !strings.Contains(nothing, "Nothing to change") ||
		strings.Contains(nothing, "<form") || !strings.Contains(nothing, "Already active") {
		t.Fatalf("nothing to change = %q", nothing)
	}
	// The address opens the dialog with the page.
	full := getSecuritySettingsPath(t, stack, "/admin/users/bulk/disable?user=bulk-amber", sessionCookie)
	if full.Code != http.StatusOK || !strings.Contains(full.Body.String(), "<html") || !strings.Contains(full.Body.String(), "Disable 1 person?") {
		t.Fatalf("dialog at its address = %d", full.Code)
	}
}

func TestAdminUsersBulkDialogRefusesMoreThanOneChangeTakes(t *testing.T) {
	_, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	ids := make([]string, 201)
	for i := range ids {
		ids[i] = fmt.Sprintf("crowd-%03d", i)
	}
	addBulkPeople(t, db, "active", ids...)
	if tooMany := bulkDialog(t, stack, sessionCookie, "/admin/users/bulk/disable?all=1&q=crowd"); !strings.Contains(tooMany, "Too many people") ||
		!strings.Contains(tooMany, "up to 200 people") || strings.Contains(tooMany, "<form") {
		t.Fatalf("too many matching = %q", tooMany)
	}
	// The list knows: selecting everyone isn't offered past the limit.
	page := getSecuritySettingsPath(t, stack, "/admin/users?q=crowd", sessionCookie).Body.String()
	if strings.Contains(page, "data-admin-users-bulk-select-matching") || !strings.Contains(page, "data-admin-users-bulk-limit-note") {
		t.Fatal("the list offered to select more people than a change takes")
	}
	narrowed := getSecuritySettingsPath(t, stack, "/admin/users?q=crowd-1", sessionCookie).Body.String()
	if !strings.Contains(narrowed, "Select all 100 matching") {
		t.Fatal("the list didn't offer everyone matching")
	}
}

func TestAdminUsersBulkChangeShowsWhatItDidToEachPerson(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	session, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || session == nil {
		t.Fatalf("administrator session = %#v, %v", session, err)
	}
	addBulkPeople(t, db, "active", "bulk-amber", "bulk-basil")
	addBulkPeople(t, db, "disabled", "bulk-cedar")
	if _, err := manager.CreateAuthenticatedSession(t.Context(), "bulk-amber", "Sample browser", auth.AuthenticationMethodPassword, auth.AssuranceLevelSingleFactor); err != nil {
		t.Fatal(err)
	}
	path := "/admin/users/bulk/disable"
	people := url.Values{"user": {"bulk-amber", "bulk-basil", "bulk-cedar", session.UserID}, "list": {"/admin/users?q=bulk"}}
	if refused := postSecuritySettings(t, stack, path, people, sessionCookie); refused.Code != http.StatusForbidden {
		t.Fatalf("change without CSRF = %d", refused.Code)
	}
	dialog := bulkDialog(t, stack, sessionCookie, path+"?user=bulk-amber&user=bulk-basil")
	people.Set(auth.CSRFFormFieldName, csrfProofFromForm(t, dialog, path))
	response := postSecuritySettings(t, stack, path, people, sessionCookie)
	result := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(result, "Disabled 2 people") ||
		!strings.Contains(result, `data-admin-users-list="/admin/users?q=bulk"`) {
		t.Fatalf("bulk disable = %d %q", response.Code, result)
	}
	if got := bulkList(result, "data-admin-users-bulk-changed"); strings.Join(got, "|") != "bulk-amber — 1 session ended|bulk-basil" {
		t.Fatalf("changed %q", got)
	}
	if got := bulkList(result, "data-admin-users-bulk-left-out"); len(got) != 2 || got[0] != "bulk-cedar — Already disabled" || !strings.HasSuffix(got[1], " — That's you") {
		t.Fatalf("left out %q", got)
	}
	for _, id := range []string{"bulk-amber", "bulk-basil"} {
		user, err := manager.GetUserByID(t.Context(), id)
		if err != nil || user == nil || user.Status != auth.UserStatusDisabled {
			t.Fatalf("%s = %#v, %v", id, user, err)
		}
	}
	page, err := audit.List(context.Background(), db.Read(), audit.Filter{Action: "user.disabled"}, 0, audit.MaxPageSize)
	if err != nil || len(page.Records) != 2 || page.Records[0].CorrelationID == "" || page.Records[0].CorrelationID != page.Records[1].CorrelationID {
		t.Fatalf("audit records = %+v, %v", page.Records, err)
	}

	// A change for nobody, or for an unknown change, is refused.
	if empty := postSecuritySettings(t, stack, path, url.Values{auth.CSRFFormFieldName: {people.Get(auth.CSRFFormFieldName)}}, sessionCookie); empty.Code != http.StatusBadRequest ||
		!strings.Contains(empty.Body.String(), "Choose who to change from the list") {
		t.Fatalf("change for nobody = %d", empty.Code)
	}
	if unknown := getSecuritySettingsPath(t, stack, "/admin/users/bulk/promote?user=bulk-amber", sessionCookie); unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown change = %d", unknown.Code)
	}
}

// The largest change, 200 people by ID, fits the form a change may send.
func TestAdminUsersBulkChangeTakesTwoHundredPeople(t *testing.T) {
	_, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	ids := make([]string, 201)
	now := time.Now().UTC()
	for i := range ids {
		ids[i] = fmt.Sprintf("0b5e2c4a-%04d-4c3e-9a1f-6d2b8e7f%04d", i, i)
		username := fmt.Sprintf("crowd-%03d", i)
		if _, err := db.Write().ExecContext(t.Context(), `INSERT INTO users (id, username, username_normalized, name, status, auth_version, user_type, is_admin, created_at, updated_at)
			VALUES (?, ?, ?, '', 'active', 1, 'webmail', 0, ?, ?)`, ids[i], username, username, now, now); err != nil {
			t.Fatal(err)
		}
	}
	path := "/admin/users/bulk/require-mfa"
	dialog := bulkDialog(t, stack, sessionCookie, path+"?user="+ids[0])
	proof := csrfProofFromForm(t, dialog, path)
	values := url.Values{auth.CSRFFormFieldName: {proof}, "user": ids[:200], "list": {"/admin/users?q=crowd&status=active&sort=newest"}}
	if size := len(values.Encode()); size <= 8<<10 {
		t.Fatalf("the form is %d bytes; this test needs it past the 8 KB other forms may send", size)
	}
	response := postSecuritySettings(t, stack, path, values, sessionCookie)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "2FA required of 200 people") {
		t.Fatalf("change to 200 people = %d %q", response.Code, response.Body.String())
	}
	values["user"] = ids
	if tooMany := postSecuritySettings(t, stack, path, values, sessionCookie); tooMany.Code != http.StatusBadRequest {
		t.Fatalf("change to 201 people = %d", tooMany.Code)
	}
}

func TestAdminUsersBulkChangeAsksAStaleSessionToVerify(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	session, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || session == nil {
		t.Fatalf("administrator session = %#v, %v", session, err)
	}
	addBulkPeople(t, db, "active", "bulk-amber")
	path := "/admin/users/bulk/sign-out"
	dialog := bulkDialog(t, stack, sessionCookie, path+"?user=bulk-amber")
	if _, err := db.Write().ExecContext(t.Context(), `UPDATE sessions SET step_up_at = ? WHERE id = ?`, time.Now().UTC().Add(-11*time.Minute), session.ID); err != nil {
		t.Fatal(err)
	}
	response := postSecuritySettings(t, stack, path, url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, dialog, path)}, "user": {"bulk-amber"},
	}, sessionCookie)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "Verify this administrator session before changing a user") ||
		!strings.Contains(response.Body.String(), "admin-security-verification-dialog") {
		t.Fatalf("stale bulk change = %d %q", response.Code, response.Body.String())
	}
	// Verifying leads back to the list: the change's address can't bring
	// back whom it named.
	if !strings.Contains(response.Body.String(), `name="return_to" value="/admin/users"`) {
		t.Fatalf("verification after a refused bulk change leads elsewhere: %q", response.Body.String())
	}
	// A dialog asked for by a stale session brings the verification along.
	stale := bulkDialog(t, stack, sessionCookie, path+"?user=bulk-amber")
	if !strings.Contains(stale, "data-step-up") || !strings.Contains(stale, "admin-security-verification-dialog") {
		t.Fatalf("stale dialog = %q", stale)
	}
}

func TestAdministratorCanSignSomeoneOutEverywhere(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	addBulkPeople(t, db, "active", "roaming-person")
	for _, agent := range []string{"Laptop browser", "Phone browser"} {
		if _, err := manager.CreateAuthenticatedSession(t.Context(), "roaming-person", agent, auth.AuthenticationMethodPassword, auth.AssuranceLevelSingleFactor); err != nil {
			t.Fatal(err)
		}
	}
	path := adminUserSignOutPath("roaming-person")
	list := getSecuritySettingsPath(t, stack, "/admin/users?q=roaming", sessionCookie).Body.String()
	if !strings.Contains(list, `href="`+path+`?list=`) || !strings.Contains(list, "Sign out everywhere") {
		t.Fatalf("the person's menu doesn't offer signing out: %q", list)
	}
	dialog := adminUserActionDialog(t, stack, sessionCookie, path)
	if !strings.Contains(dialog, "Sign roaming-person out everywhere?") || !strings.Contains(dialog, "They can sign in again straight away") {
		t.Fatalf("sign-out dialog = %q", dialog)
	}
	proof := csrfProofFromForm(t, dialog, path)
	signedOut := postSecuritySettings(t, stack, path, url.Values{auth.CSRFFormFieldName: {proof}}, sessionCookie)
	if flash, ok := adminFlashOf(t, signedOut); signedOut.Code != http.StatusSeeOther || !ok || flash.Message != "User signed out of 2 sessions." {
		t.Fatalf("sign out = %d %+v", signedOut.Code, flash)
	}
	again := postSecuritySettings(t, stack, path, url.Values{auth.CSRFFormFieldName: {proof}}, sessionCookie)
	if flash, ok := adminFlashOf(t, again); again.Code != http.StatusSeeOther || !ok || flash.Message != "The user wasn't signed in anywhere." {
		t.Fatalf("sign out again = %d %+v", again.Code, flash)
	}
	// Someone disabled is signed out already; the menu doesn't offer it.
	if _, err := db.Write().ExecContext(t.Context(), `UPDATE users SET status = 'disabled' WHERE id = 'roaming-person'`); err != nil {
		t.Fatal(err)
	}
	if list := getSecuritySettingsPath(t, stack, "/admin/users?q=roaming", sessionCookie).Body.String(); strings.Contains(list, path) {
		t.Fatal("a disabled person's menu offers signing out")
	}
	if refused := postSecuritySettings(t, stack, path, url.Values{auth.CSRFFormFieldName: {proof}}, sessionCookie); refused.Code != http.StatusBadRequest {
		t.Fatalf("signing out someone disabled = %d", refused.Code)
	}
}

// Every change to many people, and every reason one can leave someone out,
// has its words: the dialogs build their keys from the change and reason.
func TestAdminUsersBulkChangesHaveTheirWords(t *testing.T) {
	var keys []string
	for _, action := range admin.BulkActions {
		name := string(action)
		keys = append(keys, "admin.users.bulk.action."+name, "admin.users.bulk.detail."+name, "admin.users.bulk.unchanged."+name)
		for _, counted := range []string{"title", "button", "done"} {
			keys = append(keys, "admin.users.bulk."+counted+"."+name+".one", "admin.users.bulk."+counted+"."+name+".other")
		}
	}
	for _, reason := range []admin.Reason{
		admin.ReasonYou, admin.ReasonPending, admin.ReasonDeleting, admin.ReasonAdministrator, admin.ReasonNoPassword,
		admin.ReasonEveryoneRequired, admin.ReasonGone, admin.ReasonFollowUpFailed,
		admin.Reason(admin.CodeForbidden), admin.Reason(admin.CodeVerificationRequired), admin.Reason(admin.CodeInvalid),
		admin.Reason(admin.CodeUnavailable), admin.Reason(admin.CodeNeedsAuthenticator), admin.Reason(admin.CodeLastAdministrator),
		admin.Reason(admin.CodeInternal),
	} {
		keys = append(keys, "admin.users.bulk.reason."+string(reason))
	}
	for _, key := range keys {
		if !i18n.Has(key) {
			t.Errorf("%s is not in en.json", key)
		}
	}
}
