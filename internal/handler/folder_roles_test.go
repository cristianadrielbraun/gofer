package handler

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestFolderRoleRoutes(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO folders (id, account_id, remote_id, name, role, selectable) VALUES
		('f-inbox', 'victim-account', 'INBOX', 'Inbox', 'inbox', 1),
		('f-sent', 'victim-account', 'Sent', 'Sent', 'sent', 1),
		('f-bin', 'victim-account', 'Corbeille', 'Corbeille', 'custom', 1),
		('f-old', 'victim-account', 'Old deleted', 'Old deleted', 'custom', 1),
		('f-other', 'attacker-account', 'Corbeille', 'Corbeille', 'custom', 1)`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	account := map[string]string{"id": "victim-account"}

	rec := identityRequest(t, h, h.handleManageFolderRoles, http.MethodGet, "/api/accounts/victim-account/folder-roles/manage", account, nil, true)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "folder-roles-dialog") || !strings.Contains(body, "Detected: Sent") || strings.Count(body, "data-folder-role-missing") != 4 {
		t.Fatalf("manage = %d %q", rec.Code, body)
	}
	// The inbox is shown but cannot be given to another folder.
	if strings.Contains(body, `value="f-inbox"`) || !strings.Contains(body, `value="f-bin"`) || strings.Contains(body, "data-folder-roles-sync") {
		t.Fatalf("manage folders = %q", body)
	}

	rec = identityRequest(t, h, h.handleSaveFolderRoles, http.MethodPost, "/api/accounts/victim-account/folder-roles", account, url.Values{"role_trash": {"f-bin"}, "role_archive": {"f-old"}, "role_sent": {""}}, true)
	body = rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "data-folder-roles-saved") || !strings.Contains(body, "/api/mail/sync/accounts/victim-account") || strings.Count(body, "Chosen by you") != 2 {
		t.Fatalf("save = %d %q", rec.Code, body)
	}
	// The account's alerts are swapped in place, still naming Drafts and Spam.
	if strings.Count(body, `hx-swap-oob="true"`) != 2 || !strings.Contains(body, `id="account-missing-folder-roles-victim-account"`) || !strings.Contains(body, `id="folder-roles-menu-alert-victim-account"`) || !strings.Contains(body, "Couldn&#39;t find the Drafts and Spam folders.") {
		t.Fatalf("save alerts = %q", body)
	}
	if got, _ := db.FolderRoleChoices(ctx, "victim-account"); !maps.Equal(got, map[string]string{"Corbeille": "trash", "Old deleted": "archive"}) {
		t.Fatalf("choices = %v", got)
	}

	rec = identityRequest(t, h, h.handleSaveFolderRoles, http.MethodPost, "/api/accounts/victim-account/folder-roles", account, url.Values{"role_trash": {"f-old"}, "role_archive": {"f-old"}}, true)
	body = rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "data-folder-roles-error") || strings.Contains(body, "data-folder-roles-sync") {
		t.Fatalf("refused save = %d %q", rec.Code, body)
	}
	if got, _ := db.FolderRoleChoices(ctx, "victim-account"); !maps.Equal(got, map[string]string{"Corbeille": "trash", "Old deleted": "archive"}) {
		t.Fatalf("choices after refused save = %v", got)
	}

	// Another's folder can't be chosen, and another's account can't be reached.
	rec = identityRequest(t, h, h.handleSaveFolderRoles, http.MethodPost, "/", account, url.Values{"role_trash": {"f-other"}}, true)
	if !strings.Contains(rec.Body.String(), "data-folder-roles-error") {
		t.Fatalf("foreign folder = %d %q", rec.Code, rec.Body.String())
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"manage": identityRequest(t, h, h.handleManageFolderRoles, http.MethodGet, "/", account, nil, false),
		"save":   identityRequest(t, h, h.handleSaveFolderRoles, http.MethodPost, "/", account, url.Values{"role_trash": {"f-bin"}}, false),
	} {
		if rec.Code != http.StatusNotFound {
			t.Errorf("foreign %s = %d %q, want 404", name, rec.Code, rec.Body.String())
		}
	}

	// Creating a folder for a role says why it failed: this account's server
	// can't be reached.
	rec = identityRequest(t, h, h.handleCreateFolderRoleFolder, http.MethodPost, "/api/accounts/victim-account/folder-roles/create?role=junk", account, nil, true)
	body = rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "Couldn&#39;t create the folder") || strings.Contains(body, "data-folder-roles-sync") {
		t.Fatalf("create = %d %q", rec.Code, body)
	}
	if rec = identityRequest(t, h, h.handleCreateFolderRoleFolder, http.MethodPost, "/api/accounts/victim-account/folder-roles/create?role=inbox", account, nil, true); rec.Code != http.StatusBadRequest {
		t.Fatalf("create inbox = %d, want 400", rec.Code)
	}
	if rec = identityRequest(t, h, h.handleCreateFolderRoleFolder, http.MethodPost, "/api/accounts/victim-account/folder-roles/create?role=trash", account, nil, false); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign create = %d, want 404", rec.Code)
	}

	// Only IMAP accounts have the dialog.
	if _, err := db.Write().ExecContext(ctx, `UPDATE accounts SET provider = 'gmail' WHERE id = 'victim-account'`); err != nil {
		t.Fatal(err)
	}
	rec = identityRequest(t, h, h.handleManageFolderRoles, http.MethodGet, "/", account, nil, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("gmail manage = %d, want 404", rec.Code)
	}
	if rec = identityRequest(t, h, h.handleCreateFolderRoleFolder, http.MethodPost, "/?role=trash", account, nil, true); rec.Code != http.StatusNotFound {
		t.Fatalf("gmail create = %d, want 404", rec.Code)
	}
}
