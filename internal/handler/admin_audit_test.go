package handler

import (
	"net/http"
	"net/url"
	"testing"

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
