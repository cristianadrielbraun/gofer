package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/audit"
	"github.com/cristianadrielbraun/gofer/internal/auth"
)

// Each mail security change the admin console makes leaves one record in
// the administration audit log, naming the administrator and the target.
func TestAdminMailSecurityChangesAreAudited(t *testing.T) {
	manager, db, stack, cookie, _ := completedSecuritySettingsStack(t)
	session, err := manager.GetSessionByToken(t.Context(), cookie.Value)
	if err != nil || session == nil {
		t.Fatalf("load administrator session = %#v, %v", session, err)
	}
	page := getSecuritySettingsPath(t, stack, "/admin/security", cookie)
	if page.Code != http.StatusOK {
		t.Fatalf("security page = %d", page.Code)
	}
	html := page.Body.String()
	for _, change := range []struct {
		path string
		form url.Values
	}{
		{"/admin/security/http-discovery", url.Values{"domain": {"mail.sample.test"}, "acknowledge": {"yes"}}},
		{"/admin/security/plaintext", url.Values{"protocol": {"imap"}, "host": {"imap.sample.test"}, "port": {"143"}, "acknowledge": {"yes"}}},
		{"/admin/security/private-target", url.Values{"protocol": {"http"}, "host": {"127.0.0.1"}, "port": {"8080"}, "acknowledge": {"yes"}}},
	} {
		change.form.Set(auth.CSRFFormFieldName, csrfProofFromForm(t, html, change.path))
		if response := postSecuritySettings(t, stack, change.path, change.form, cookie); response.Code != http.StatusSeeOther || adminFlashFailed(t, response) {
			t.Fatalf("%s = %d %q", change.path, response.Code, adminFlashMessage(t, response))
		}
	}
	exceptions, err := db.ListMailSecurityExceptions(t.Context())
	if err != nil || len(exceptions) != 3 {
		t.Fatalf("exceptions = %+v, %v", exceptions, err)
	}
	deletePath := "/admin/security/exceptions/" + exceptions[0].ID + "/delete"
	refreshed := getSecuritySettingsPath(t, stack, "/admin/security", cookie).Body.String()
	if response := postSecuritySettings(t, stack, deletePath, url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, refreshed, deletePath)},
	}, cookie); response.Code != http.StatusSeeOther || adminFlashFailed(t, response) {
		t.Fatalf("delete exception = %d", response.Code)
	}

	page2, err := audit.List(t.Context(), db.Read(), audit.Filter{Action: "security."}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	records := page2.Records
	if len(records) != 4 {
		t.Fatalf("security records = %+v", records)
	}
	// Newest first.
	removed, added := records[0], records[1:]
	if removed.Action != "security.exception_removed" || removed.Target.ID != exceptions[0].ID {
		t.Fatalf("removal record = %+v", removed)
	}
	labels := map[string]bool{}
	for _, record := range added {
		if record.Action != "security.exception_added" || record.Actor.ID != session.UserID || record.Actor.SessionID != session.ID ||
			record.Actor.Label == "" || record.Client.UserAgent == "" {
			t.Fatalf("addition record = %+v", record)
		}
		labels[record.Target.Label] = true
	}
	for _, label := range []string{"mail.sample.test", "imap.sample.test:143", "127.0.0.1:8080"} {
		if !labels[label] {
			t.Errorf("no record for %s: %v", label, labels)
		}
	}
	verified, err := audit.Verify(t.Context(), db.Read(), manager.AuditKey())
	if err != nil || !verified.OK() {
		t.Fatalf("audit chain = %+v, %v", verified, err)
	}
}

func TestAdminAuditActivityShowsAdministrativeChanges(t *testing.T) {
	manager, db, stack, cookie, _ := completedSecuritySettingsStack(t)
	page := getSecuritySettingsPath(t, stack, "/admin/security", cookie)
	form := url.Values{"domain": {"mail.sample.test"}, "acknowledge": {"yes"}}
	form.Set(auth.CSRFFormFieldName, csrfProofFromForm(t, page.Body.String(), "/admin/security/http-discovery"))
	if response := postSecuritySettings(t, stack, "/admin/security/http-discovery", form, cookie); response.Code != http.StatusSeeOther {
		t.Fatalf("add exception = %d", response.Code)
	}

	tab := getSecuritySettingsPath(t, stack, "/admin/activity/administration", cookie)
	html := tab.Body.String()
	if tab.Code != http.StatusOK || !strings.Contains(html, "Allowed HTTP discovery for mail.sample.test") ||
		!strings.Contains(html, "Security Settings Handler/1.0") ||
		!strings.Contains(html, `aria-current="page"`) || !strings.Contains(html, "Sign-in and security") {
		t.Fatalf("administration tab = %d %q", tab.Code, html)
	}
	people := getSecuritySettingsPath(t, stack, "/admin/activity/administration?kind=user", cookie)
	if people.Code != http.StatusOK || strings.Contains(people.Body.String(), "mail.sample.test") {
		t.Fatalf("people filter showed a mail security change: %d", people.Code)
	}
	if unknown := getSecuritySettingsPath(t, stack, "/admin/activity/administration?kind=everything&before=-3", cookie); unknown.Code != http.StatusOK ||
		!strings.Contains(unknown.Body.String(), "mail.sample.test") {
		t.Fatalf("unknown filter values aren't ignored: %d", unknown.Code)
	}

	session, err := manager.GetSessionByToken(t.Context(), cookie.Value)
	if err != nil || session == nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `UPDATE sessions SET step_up_at = ? WHERE id = ?`, time.Now().UTC().Add(-11*time.Minute), session.ID); err != nil {
		t.Fatal(err)
	}
	stale := getSecuritySettingsPath(t, stack, "/admin/activity/administration?kind=security", cookie)
	staleHTML := stale.Body.String()
	if stale.Code != http.StatusOK || !strings.Contains(staleHTML, "data-admin-audit-activity-locked") ||
		strings.Contains(staleHTML, "mail.sample.test") ||
		!strings.Contains(staleHTML, `name="return_to" value="/admin/activity/administration?kind=security"`) {
		t.Fatalf("stale administration tab = %d %q", stale.Code, staleHTML)
	}
}

func TestAdminAuditExportAndRetention(t *testing.T) {
	manager, db, stack, cookie, _ := completedSecuritySettingsStack(t)
	tab := getSecuritySettingsPath(t, stack, "/admin/activity/administration", cookie).Body.String()

	// Keep administrative changes for a year.
	retentionPath := "/admin/activity/administration/retention"
	if refused := postSecuritySettings(t, stack, retentionPath, url.Values{
		"days": {"30"}, auth.CSRFFormFieldName: {csrfProofFromForm(t, tab, retentionPath)},
	}, cookie); refused.Code != http.StatusSeeOther || refused.Header().Get("Location") != "/admin/activity/administration?retention=1" || !adminFlashFailed(t, refused) {
		t.Fatalf("too short a retention = %d %q", refused.Code, refused.Header().Get("Location"))
	}
	saved := postSecuritySettings(t, stack, retentionPath, url.Values{
		"days": {"365"}, auth.CSRFFormFieldName: {csrfProofFromForm(t, tab, retentionPath)},
	}, cookie)
	if saved.Code != http.StatusSeeOther || !strings.Contains(adminFlashMessage(t, saved), "365 days") {
		t.Fatalf("save retention = %d %q", saved.Code, adminFlashMessage(t, saved))
	}
	if days, err := manager.AdminAuditRetention(t.Context()); err != nil || days != 365 {
		t.Fatalf("retention = %d, %v", days, err)
	}

	request := httptest.NewRequest(http.MethodGet, "/admin/activity/administration/export?format=csv&kind=instance", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	stack.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/csv") ||
		!strings.Contains(response.Header().Get("Content-Disposition"), "attachment") ||
		!strings.HasPrefix(body, "seq,id,occurred_at") || !strings.Contains(body, "instance.audit_retention_changed") {
		t.Fatalf("export = %d %v %q", response.Code, response.Header(), body)
	}
	exported, err := audit.List(t.Context(), db.Read(), audit.Filter{Action: "audit.exported"}, 0, 10)
	if err != nil || len(exported.Records) != 1 || exported.Records[0].Metadata["action_filter"] != "instance." {
		t.Fatalf("export record = %+v, %v", exported.Records, err)
	}

	session, err := manager.GetSessionByToken(t.Context(), cookie.Value)
	if err != nil || session == nil {
		t.Fatal(err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `UPDATE sessions SET step_up_at = ? WHERE id = ?`, time.Now().UTC().Add(-11*time.Minute), session.ID); err != nil {
		t.Fatal(err)
	}
	stale := httptest.NewRecorder()
	staleRequest := httptest.NewRequest(http.MethodGet, "/admin/activity/administration/export?format=jsonl", nil)
	staleRequest.AddCookie(cookie)
	stack.ServeHTTP(stale, staleRequest)
	if stale.Code != http.StatusSeeOther || stale.Header().Get("Location") != "/admin/activity/administration?verification_required=1" || stale.Body.Len() > 200 {
		t.Fatalf("stale export = %d %q", stale.Code, stale.Header().Get("Location"))
	}
}

func TestRefusedAdministrativeChangesAreRecorded(t *testing.T) {
	manager, db, stack, cookie, _ := completedSecuritySettingsStack(t)
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (id, username, username_normalized, name, status, auth_version, user_type, is_admin, created_at, updated_at)
		VALUES ('kept-user', 'kept-user', 'kept-user', 'Kept User', 'active', 1, 'webmail', 0, ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	path := "/admin/users/kept-user/status"
	proof := csrfProofFromForm(t, getSecuritySettingsPath(t, stack, path, cookie).Body.String(), path)
	session, err := manager.GetSessionByToken(t.Context(), cookie.Value)
	if err != nil || session == nil {
		t.Fatal(err)
	}
	setStepUp := func(at time.Time) {
		t.Helper()
		if _, err := db.Write().ExecContext(t.Context(), `UPDATE sessions SET step_up_at = ? WHERE id = ?`, at, session.ID); err != nil {
			t.Fatal(err)
		}
	}
	setStepUp(now.Add(-11 * time.Minute))
	for range 2 {
		postSecuritySettings(t, stack, path, url.Values{"status": {"disabled"}, auth.CSRFFormFieldName: {proof}}, cookie)
	}
	var status string
	if err := db.Read().QueryRow(`SELECT status FROM users WHERE id = 'kept-user'`).Scan(&status); err != nil || status != "active" {
		t.Fatalf("a stale session disabled someone: %q, %v", status, err)
	}
	refused, err := audit.List(t.Context(), db.Read(), audit.Filter{Action: auth.AuditActionAccessDenied}, 0, 10)
	if err != nil || len(refused.Records) != 1 {
		t.Fatalf("refused attempts = %+v, %v", refused.Records, err)
	}
	record := refused.Records[0]
	if record.Outcome != audit.OutcomeDenied || record.Target.ID != "users.access" ||
		record.Metadata["reason"] != auth.DeniedReasonVerificationRequired || record.Actor.SessionID != session.ID ||
		!strings.Contains(record.Client.UserAgent, "Security Settings Handler") {
		t.Fatalf("refused attempt = %+v", record)
	}

	setStepUp(time.Now().UTC())
	overview := getSecuritySettingsPath(t, stack, "/admin/overview", cookie).Body.String()
	if !strings.Contains(overview, "1 refused attempt in the last 7 days") ||
		!strings.Contains(overview, `href="/admin/activity/administration?kind=access"`) {
		t.Fatalf("the overview doesn't point at the refused attempt: %q", overview)
	}
	tab := getSecuritySettingsPath(t, stack, "/admin/activity/administration?kind=access", cookie).Body.String()
	if !strings.Contains(tab, "Refused without a recent verification: enabling or disabling people") || !strings.Contains(tab, "Refused attempts") {
		t.Fatalf("the administration tab doesn't show the refused attempt: %q", tab)
	}
}
