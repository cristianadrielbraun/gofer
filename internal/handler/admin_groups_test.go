package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func TestGroupPagesCreateFillAndDeleteAGroup(t *testing.T) {
	_, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	addBulkPeople(t, db, "active", "ines.moreau", "pavel.arden")
	addBulkPeople(t, db, "disabled", "left.already")
	if _, err := db.Write().Exec(`UPDATE users SET deletion_pending = 1 WHERE id = 'left.already'`); err != nil {
		t.Fatal(err)
	}

	list := getSecuritySettingsPath(t, stack, views.AdminGroupsPath, sessionCookie)
	body := list.Body.String()
	if list.Code != http.StatusOK || !strings.Contains(body, "No groups yet") || !strings.Contains(body, `href="/admin/groups"`) {
		t.Fatalf("empty Groups page = %d %q", list.Code, body)
	}
	if refused := postSecuritySettings(t, stack, views.AdminGroupsPath, url.Values{"name": {"Field team"}}, sessionCookie); refused.Code != http.StatusForbidden {
		t.Fatalf("create without CSRF = %d", refused.Code)
	}
	csrf := csrfProofFromForm(t, body, views.AdminGroupsPath)
	created := postSecuritySettings(t, stack, views.AdminGroupsPath, url.Values{"name": {"Field team"}, "description": {"Out on the road"}, auth.CSRFFormFieldName: {csrf}}, sessionCookie)
	location := created.Header().Get("Location")
	if created.Code != http.StatusSeeOther || !strings.HasPrefix(location, views.AdminGroupsPath+"/") {
		t.Fatalf("create = %d %q %q", created.Code, location, created.Body.String())
	}
	groupID := strings.TrimPrefix(location, views.AdminGroupsPath+"/")

	taken := postSecuritySettings(t, stack, views.AdminGroupsPath, url.Values{"name": {"field TEAM"}, auth.CSRFFormFieldName: {csrf}}, sessionCookie)
	if taken.Code != http.StatusBadRequest || !strings.Contains(taken.Body.String(), "Another group already has this name.") ||
		!strings.Contains(taken.Body.String(), `value="field TEAM"`) {
		t.Fatalf("name taken = %d %q", taken.Code, taken.Body.String())
	}

	list = getSecuritySettingsPath(t, stack, views.AdminGroupsPath+"?q=road", sessionCookie)
	if body := list.Body.String(); !strings.Contains(body, "Field team") || !strings.Contains(body, "0 people") {
		t.Fatalf("searched list = %q", body)
	}
	if body := getSecuritySettingsPath(t, stack, views.AdminGroupsPath+"?q=night", sessionCookie).Body.String(); !strings.Contains(body, "No group matches the search.") {
		t.Fatalf("list with no match = %q", body)
	}

	// Only webmail users who aren't being deleted are offered.
	add := getSecuritySettingsPath(t, stack, views.AdminGroupAddPath(groupID, ""), sessionCookie)
	body = add.Body.String()
	if add.Code != http.StatusOK || !strings.Contains(body, `value="ines.moreau"`) || !strings.Contains(body, `value="pavel.arden"`) ||
		strings.Contains(body, `value="left.already"`) || strings.Count(body, "data-admin-group-candidate") != 2 {
		t.Fatalf("Add people dialog = %d %q", add.Code, body)
	}
	addAction := views.AdminGroupPath(groupID, "members")
	added := postSecuritySettings(t, stack, addAction, url.Values{"user_id": {"ines.moreau", "pavel.arden"}, auth.CSRFFormFieldName: {csrfProofFromForm(t, body, addAction)}}, sessionCookie)
	if added.Code != http.StatusSeeOther || added.Header().Get("Location") != views.AdminGroupPath(groupID, views.AdminGroupTabMembers) {
		t.Fatalf("add = %d %q %q", added.Code, added.Header().Get("Location"), added.Body.String())
	}
	if refused := postSecuritySettings(t, stack, addAction, url.Values{"user_id": {"left.already"}, auth.CSRFFormFieldName: {csrfProofFromForm(t, body, addAction)}}, sessionCookie); refused.Code != http.StatusConflict ||
		!strings.Contains(refused.Body.String(), "someone chosen can") {
		t.Fatalf("add someone being deleted = %d %q", refused.Code, refused.Body.String())
	}
	add = getSecuritySettingsPath(t, stack, views.AdminGroupAddPath(groupID, "ines"), sessionCookie)
	if body := add.Body.String(); !strings.Contains(body, "Nobody else matches the search.") {
		t.Fatalf("search for someone already in = %q", body)
	}

	members := getSecuritySettingsPath(t, stack, views.AdminGroupPath(groupID, views.AdminGroupTabMembers), sessionCookie)
	body = members.Body.String()
	if members.Code != http.StatusOK || strings.Count(body, "data-admin-group-member") != 2 || !strings.Contains(body, "2 people") {
		t.Fatalf("members = %d %q", members.Code, body)
	}
	removeAction := views.AdminGroupPath(groupID, "members/remove")
	if nobody := postSecuritySettings(t, stack, removeAction, url.Values{auth.CSRFFormFieldName: {csrfProofFromForm(t, body, removeAction)}}, sessionCookie); nobody.Code != http.StatusBadRequest ||
		!strings.Contains(nobody.Body.String(), "Choose someone first.") {
		t.Fatalf("remove nobody = %d %q", nobody.Code, nobody.Body.String())
	}
	removed := postSecuritySettings(t, stack, removeAction, url.Values{"user_id": {"pavel.arden"}, auth.CSRFFormFieldName: {csrfProofFromForm(t, body, removeAction)}}, sessionCookie)
	if removed.Code != http.StatusSeeOther {
		t.Fatalf("remove = %d %q", removed.Code, removed.Body.String())
	}

	settings := getSecuritySettingsPath(t, stack, views.AdminGroupPath(groupID, views.AdminGroupTabSettings), sessionCookie)
	body = settings.Body.String()
	settingsAction := views.AdminGroupPath(groupID, views.AdminGroupTabSettings)
	if settings.Code != http.StatusOK || !strings.Contains(body, `name="version" value="1"`) || !strings.Contains(body, "Out on the road") {
		t.Fatalf("settings = %d %q", settings.Code, body)
	}
	settingsCSRF := csrfProofFromForm(t, body, settingsAction)
	saved := postSecuritySettings(t, stack, settingsAction, url.Values{"name": {"Field crew"}, "description": {""}, "version": {"1"}, auth.CSRFFormFieldName: {settingsCSRF}}, sessionCookie)
	if saved.Code != http.StatusSeeOther {
		t.Fatalf("rename = %d %q", saved.Code, saved.Body.String())
	}
	stale := postSecuritySettings(t, stack, settingsAction, url.Values{"name": {"Field people"}, "version": {"1"}, auth.CSRFFormFieldName: {settingsCSRF}}, sessionCookie)
	if body := stale.Body.String(); stale.Code != http.StatusConflict || !strings.Contains(body, "This group changed while you had it open") ||
		!strings.Contains(body, `value="Field crew"`) || !strings.Contains(body, `name="version" value="2"`) {
		t.Fatalf("rename from an old page = %d %q", stale.Code, body)
	}

	confirm := getSecuritySettingsPath(t, stack, views.AdminGroupPath(groupID, "delete"), sessionCookie)
	body = confirm.Body.String()
	deleteAction := views.AdminGroupPath(groupID, "delete")
	if !strings.Contains(body, "1 person will be taken out of it.") || !strings.Contains(body, `data-tui-dialog-open="true"`) {
		t.Fatalf("delete confirmation = %q", body)
	}
	deleted := postSecuritySettings(t, stack, deleteAction, url.Values{"version": {"2"}, auth.CSRFFormFieldName: {csrfProofFromForm(t, body, deleteAction)}}, sessionCookie)
	if deleted.Code != http.StatusSeeOther || deleted.Header().Get("Location") != views.AdminGroupsPath {
		t.Fatalf("delete = %d %q %q", deleted.Code, deleted.Header().Get("Location"), deleted.Body.String())
	}
	if gone := getSecuritySettingsPath(t, stack, views.AdminGroupPath(groupID, views.AdminGroupTabMembers), sessionCookie); gone.Code != http.StatusNotFound {
		t.Fatalf("deleted group's page = %d", gone.Code)
	}

	var recorded int
	if err := db.Read().QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action LIKE 'group.%'`).Scan(&recorded); err != nil || recorded != 5 {
		t.Fatalf("group records = %d, %v", recorded, err)
	}
}

func TestVerificationLeadsBackToGroupPages(t *testing.T) {
	for value, want := range map[string]string{
		"/admin/groups":                "/admin/groups",
		"/admin/groups/g-1":            "/admin/groups/g-1",
		"/admin/groups/g-1/settings":   "/admin/groups/g-1/settings",
		"/admin/groups/g-1/add":        "/admin/groups/g-1/add",
		"/admin/groups/g-1/delete":     "/admin/groups/g-1/delete",
		"/admin/groups/g-1/members":    "",
		"/admin/groups/g-1/add?q=ines": "",
		"/admin/groups/g-1/settings/x": "",
		"//evil.example/admin/groups":  "",
	} {
		if got := adminSecurityVerificationReturnTo(value); got != want {
			t.Errorf("return to %q = %q, want %q", value, got, want)
		}
	}
}
