package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestFolderAppearanceOwnershipValidationAndPersistence(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO folders (id, account_id, remote_id, name, icon, role, selectable) VALUES
		('color-inbox', 'victim-account', 'INBOX', 'Inbox', 'inbox', 'inbox', 1)`); err != nil {
		t.Fatal(err)
	}
	id := map[string]string{"id": "color-inbox"}
	get := func(owner bool) int {
		return identityRequest(t, h, h.handleFolderAppearance, http.MethodGet, "/", id, nil, owner).Code
	}
	if got := get(false); got != http.StatusNotFound {
		t.Fatalf("foreign GET = %d", got)
	}
	if got := get(true); got != http.StatusOK {
		t.Fatalf("owner GET = %d", got)
	}
	for _, tc := range []struct {
		name   string
		form   url.Values
		owner  bool
		status int
	}{
		{"foreign write", url.Values{"color": {"#3b82f6"}}, false, http.StatusNotFound},
		{"CSS injection", url.Values{"color": {"red;background:url(https://example.com)"}}, true, http.StatusBadRequest},
		{"missing color", url.Values{}, true, http.StatusBadRequest},
		{"duplicate color", url.Values{"color": {"#3b82f6", "#ef4444"}}, true, http.StatusBadRequest},
		{"role icon override", url.Values{"color": {"#3b82f6"}, "icon": {"folder"}}, true, http.StatusBadRequest},
		{"valid color", url.Values{"color": {"#3B82F6"}}, true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := identityRequest(t, h, h.handleSaveFolderAppearance, http.MethodPost, "/", id, tc.form, tc.owner)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
	tree, err := db.AccountFolderTree(ctx, "victim-account")
	if err != nil || len(tree) != 1 || tree[0].Color != "#3b82f6" || tree[0].Icon != "inbox" {
		t.Fatalf("saved tree = %+v, %v", tree, err)
	}
	rec := identityRequest(t, h, h.handleFolderAppearance, http.MethodGet, "/", id, nil, true)
	if !strings.Contains(rec.Body.String(), `value="#3b82f6" checked`) || !strings.Contains(rec.Body.String(), "fixed by its role") {
		t.Fatalf("saved editor = %s", rec.Body.String())
	}
	rec = identityRequest(t, h, h.handleSaveFolderAppearance, http.MethodPost, "/", id, url.Values{"color": {""}}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body.String())
	}
	tree, err = db.AccountFolderTree(ctx, "victim-account")
	if err != nil || tree[0].Color != "" || tree[0].Icon != "inbox" {
		t.Fatalf("reset tree = %+v, %v", tree, err)
	}
	if _, err := db.Write().ExecContext(ctx, `UPDATE accounts SET is_deleting=1 WHERE id='victim-account'`); err != nil {
		t.Fatal(err)
	}
	if got := get(true); got != http.StatusNotFound {
		t.Fatalf("deleting account GET = %d", got)
	}
}

func TestCustomFolderIconPickerAndRoleProtection(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO folders (id, account_id, remote_id, name, icon, role, selectable) VALUES
		('icon-projects', 'victim-account', 'Projects', 'Projects', 'folder', 'custom', 1),
		('icon-inbox', 'victim-account', 'INBOX', 'Inbox', 'inbox', 'inbox', 1)`); err != nil {
		t.Fatal(err)
	}
	id := map[string]string{"id": "icon-projects"}
	get := func() string {
		rec := identityRequest(t, h, h.handleFolderAppearance, http.MethodGet, "/", id, nil, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if body := get(); strings.Count(body, `name="icon"`) != 18 || !strings.Contains(body, `aria-label="Default icon"`) {
		t.Fatalf("custom picker = %s", body)
	}
	for _, tc := range []struct {
		name   string
		form   url.Values
		owner  bool
		status int
	}{
		{"unknown icon", url.Values{"color": {"#3b82f6"}, "icon": {"missing-icon"}}, true, http.StatusBadRequest},
		{"SVG injection", url.Values{"color": {"#3b82f6"}, "icon": {`<svg onload="alert(1)">`}}, true, http.StatusBadRequest},
		{"duplicate icon", url.Values{"color": {"#3b82f6"}, "icon": {"heart", "flag"}}, true, http.StatusBadRequest},
		{"foreign icon write", url.Values{"color": {"#3b82f6"}, "icon": {"heart"}}, false, http.StatusNotFound},
		{"custom icon", url.Values{"color": {"#3b82f6"}, "icon": {"heart"}}, true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := identityRequest(t, h, h.handleSaveFolderAppearance, http.MethodPost, "/", id, tc.form, tc.owner)
			if rec.Code != tc.status {
				t.Fatalf("POST = %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
	if body := get(); !strings.Contains(body, `value="heart" checked`) || !strings.Contains(body, `data-folder-customized`) {
		t.Fatalf("saved picker = %s", body)
	}
	// Older color-only clients leave the saved icon in place.
	rec := identityRequest(t, h, h.handleSaveFolderAppearance, http.MethodPost, "/", id, url.Values{"color": {"#ef4444"}}, true)
	if rec.Code != http.StatusOK || !strings.Contains(get(), `value="heart" checked`) {
		t.Fatalf("color-only update lost icon: %d", rec.Code)
	}
	// A role can change while the editor is open. Refuse both changes together.
	if _, err := db.Write().ExecContext(ctx, `UPDATE folders SET role='archive', icon='archive' WHERE id='icon-projects'`); err != nil {
		t.Fatal(err)
	}
	rec = identityRequest(t, h, h.handleSaveFolderAppearance, http.MethodPost, "/", id, url.Values{"color": {"#3b82f6"}, "icon": {"flag"}}, true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("new role override = %d", rec.Code)
	}
	body := get()
	if strings.Contains(body, `name="icon"`) || strings.Contains(body, `data-folder-customized`) || !strings.Contains(body, `value="#ef4444" checked`) {
		t.Fatalf("role change must hide override and keep color: %s", body)
	}
	// Even an empty override is refused for an immutable role icon.
	id["id"] = "icon-inbox"
	for _, icon := range []string{"heart", ""} {
		rec := identityRequest(t, h, h.handleSaveFolderAppearance, http.MethodPost, "/", id, url.Values{"color": {""}, "icon": {icon}}, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("role icon %q = %d", icon, rec.Code)
		}
	}
	// Restoring the custom role makes Reset available again.
	id["id"] = "icon-projects"
	if _, err := db.Write().ExecContext(ctx, `UPDATE folders SET role='custom', icon='folder' WHERE id='icon-projects'`); err != nil {
		t.Fatal(err)
	}
	rec = identityRequest(t, h, h.handleSaveFolderAppearance, http.MethodPost, "/", id, url.Values{"color": {""}, "icon": {""}}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d: %s", rec.Code, rec.Body.String())
	}
	if body := get(); strings.Contains(body, `data-folder-customized`) || !strings.Contains(body, `value="" checked`) {
		t.Fatalf("reset picker = %s", body)
	}
}

func TestFolderIconStylesPersistForCustomAndRoleFolders(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO folders (id, account_id, remote_id, name, icon, role, selectable) VALUES
		('styled-custom', 'victim-account', 'Work', 'Work', 'folder', 'custom', 1),
		('styled-inbox', 'victim-account', 'INBOX', 'Inbox', 'inbox', 'inbox', 1)`); err != nil {
		t.Fatal(err)
	}
	id := map[string]string{"id": "styled-inbox"}
	for _, tc := range []struct {
		name   string
		form   url.Values
		owner  bool
		status int
	}{
		{"invalid style", url.Values{"color": {"#3b82f6"}, "style": {"solid"}}, true, http.StatusBadRequest},
		{"empty style", url.Values{"color": {""}, "style": {""}}, true, http.StatusBadRequest},
		{"duplicate style", url.Values{"color": {""}, "style": {"filled", "outlined"}}, true, http.StatusBadRequest},
		{"foreign style", url.Values{"color": {""}, "style": {"filled"}}, false, http.StatusNotFound},
		{"role filled style", url.Values{"color": {"#3b82f6"}, "style": {"filled"}}, true, http.StatusOK},
		{"role symbol override", url.Values{"color": {"#ef4444"}, "style": {"outlined"}, "icon": {"heart"}}, true, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := identityRequest(t, h, h.handleSaveFolderAppearance, http.MethodPost, "/", id, tc.form, tc.owner)
			if rec.Code != tc.status {
				t.Fatalf("POST = %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
	get := func() string {
		rec := identityRequest(t, h, h.handleFolderAppearance, http.MethodGet, "/", id, nil, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if body := get(); !strings.Contains(body, `value="filled" checked`) || !strings.Contains(body, `data-folder-filled-symbol="inbox"`) || strings.Contains(body, `name="icon"`) || !strings.Contains(body, `value="#3b82f6" checked`) {
		t.Fatalf("role appearance = %s", body)
	}
	// Color-only clients preserve the selected style.
	rec := identityRequest(t, h, h.handleSaveFolderAppearance, http.MethodPost, "/", id, url.Values{"color": {"#ef4444"}}, true)
	if rec.Code != http.StatusOK || !strings.Contains(get(), `value="filled" checked`) {
		t.Fatal("color-only update lost style")
	}
	id["id"] = "styled-custom"
	rec = identityRequest(t, h, h.handleSaveFolderAppearance, http.MethodPost, "/", id, url.Values{"color": {"#3b82f6"}, "icon": {"camera"}, "style": {"filled"}}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("custom filled = %d: %s", rec.Code, rec.Body.String())
	}
	if body := get(); !strings.Contains(body, `value="filled" checked`) || !strings.Contains(body, `value="camera" checked`) {
		t.Fatalf("custom appearance = %s", body)
	}
	rec = identityRequest(t, h, h.handleSaveFolderAppearance, http.MethodPost, "/", id, url.Values{"color": {""}, "icon": {""}, "style": {"outlined"}}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset = %d", rec.Code)
	}
	if body := get(); !strings.Contains(body, `value="outlined" checked`) || strings.Contains(body, `data-folder-customized`) {
		t.Fatalf("reset appearance = %s", body)
	}
}
