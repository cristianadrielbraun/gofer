package handler

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

func TestFolderVisibilityHTTPPersistence(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()

	_, err := db.Write().ExecContext(ctx, `
        INSERT INTO folders
            (id, account_id, remote_id, name, icon, role, selectable)
        VALUES
            ('vis-projects', 'victim-account', 'Projects',
             'Projects', 'folder', 'custom', 1),
            ('vis-inbox', 'victim-account', 'INBOX',
             'Inbox', 'inbox', 'inbox', 1)
    `)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		visible string
		hidden  bool
	}{
		{"false", true},
		{"true", false},
	} {
		rec := identityRequest(
			t, h, h.handleSaveFolderVisibility,
			http.MethodPost,
			"/api/folders/vis-projects/visibility",
			map[string]string{"id": "vis-projects"},
			url.Values{"visible": {tc.visible}},
			true,
		)

		if rec.Code != http.StatusOK {
			t.Fatalf("visible=%s: HTTP %d: %s",
				tc.visible, rec.Code, rec.Body.String())
		}

		var response map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}

		if response["id"] != "vis-projects" ||
			response["account_id"] != "victim-account" ||
			response["visible"] != tc.visible {
			t.Fatalf("unexpected JSON: %+v", response)
		}

		tree, err := db.AccountFolderTree(ctx, "victim-account")
		if err != nil {
			t.Fatal(err)
		}

		found := false
		for _, folder := range tree {
			if folder.ID == "vis-projects" {
				found = true
				if folder.Hidden != tc.hidden {
					t.Fatalf("Hidden=%v, expected %v",
						folder.Hidden, tc.hidden)
				}
			}
		}
		if !found {
			t.Fatal("Projects folder missing")
		}
	}
}

func TestFolderVisibilityHTTPRestrictions(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()

	_, err := db.Write().ExecContext(ctx, `
        INSERT INTO folders
            (id, account_id, remote_id, name, icon, role, selectable)
        VALUES
            ('vis-target', 'victim-account', 'Projects',
             'Projects', 'folder', 'custom', 1),
            ('vis-inbox', 'victim-account', 'INBOX',
             'Inbox', 'inbox', 'inbox', 1),
            ('vis-trash', 'victim-account', 'Trash',
             'Trash', 'trash', 'trash', 1),
            ('vis-outbox', 'victim-account', 'Outbox',
             'Outbox', 'outbox', 'outbox', 1),
            ('vis-locked', 'victim-account', 'Locked',
             'Locked', 'folder', 'custom', 0),
            ('vis-other', 'attacker-account', 'Other',
             'Other', 'folder', 'custom', 1)
    `)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		folderID string
		form     url.Values
		owner    bool
		status   int
	}{
		{
			"missing value", "vis-target",
			url.Values{}, true, 400,
		},
		{
			"invalid value", "vis-target",
			url.Values{"visible": {"yes"}}, true, 400,
		},
		{
			"duplicate value", "vis-target",
			url.Values{"visible": {"true", "false"}}, true, 400,
		},
		{
			"extra parameter", "vis-target",
			url.Values{
				"visible": {"false"},
				"other":   {"x"},
			}, true, 400,
		},
		{
			"foreign user", "vis-target",
			url.Values{"visible": {"false"}}, false, 404,
		},
		{
			"foreign account", "vis-other",
			url.Values{"visible": {"false"}}, true, 404,
		},
		{
			"unknown folder", "vis-missing",
			url.Values{"visible": {"false"}}, true, 404,
		},
		{
			"protected inbox", "vis-inbox",
			url.Values{"visible": {"false"}}, true, 404,
		},
		{
			"protected trash", "vis-trash",
			url.Values{"visible": {"false"}}, true, 404,
		},
		{
			"protected outbox", "vis-outbox",
			url.Values{"visible": {"false"}}, true, 404,
		},
		{
			"unselectable folder", "vis-locked",
			url.Values{"visible": {"false"}}, true, 404,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := identityRequest(
				t, h, h.handleSaveFolderVisibility,
				http.MethodPost,
				"/api/folders/"+tc.folderID+"/visibility",
				map[string]string{"id": tc.folderID},
				tc.form,
				tc.owner,
			)

			if rec.Code != tc.status {
				t.Fatalf(
					"HTTP %d, expected %d: %s",
					rec.Code, tc.status, rec.Body.String(),
				)
			}
		})
	}

	// Rejected requests must not modify stored preferences.
	var count int
	err = db.Read().QueryRowContext(ctx, `
        SELECT COUNT(*)
        FROM app_settings
        WHERE key LIKE 'folder_visible:%'
    `).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rejected operations wrote %d preferences", count)
	}
}
