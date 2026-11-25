package handler

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
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

// A folder hidden from the sidebar stays in the Folders dialog, dimmed with
// a mark, and its menu shows it again. A folder inside it is marked too,
// with nothing to hide, and Inbox under it can't be hidden at all. The same
// goes for a Gmail label.
func TestFoldersDialogShowsHiddenFolders(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()
	if _, err := db.Write().ExecContext(ctx, `
		INSERT INTO folders (id, account_id, parent_id, remote_id, name, role, selectable, sort_order) VALUES
		('f-inbox', 'victim-account', NULL, 'INBOX', 'Inbox', 'inbox', 1, 0),
		('f-trips', 'victim-account', NULL, 'Trips', 'Trips', 'custom', 1, 1),
		('f-coast', 'victim-account', 'f-trips', 'Trips/Coast', 'Coast', 'custom', 1, 2),
		('f-plans', 'victim-account', NULL, 'Plans', 'Plans', 'custom', 1, 3)`); err != nil {
		t.Fatalf("insert folders: %v", err)
	}
	account := map[string]string{"id": "victim-account"}
	hide := func(handler http.HandlerFunc, kind, id string) {
		t.Helper()
		rec := identityRequest(t, h, handler, http.MethodPost, "/api/"+kind+"/"+id+"/visibility", map[string]string{"id": id}, url.Values{"visible": {"false"}}, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("hide %s = %d %q", id, rec.Code, rec.Body.String())
		}
	}
	hide(h.handleSaveFolderVisibility, "folders", "f-trips")

	rec := identityRequest(t, h, h.handleManageFolders, http.MethodGet, "/", account, nil, true)
	body := rec.Body.String()
	actions := folderMenuActions(body)
	for _, want := range []string{"show-folder f-trips", "hide-folder f-plans"} {
		if !actions[want] {
			t.Errorf("menus lack %s: %v", want, actions)
		}
	}
	for _, unwanted := range []string{"hide-folder f-trips", "hide-folder f-coast", "show-folder f-coast", "hide-folder f-inbox", "show-folder f-plans"} {
		if actions[unwanted] {
			t.Errorf("menus have %s", unwanted)
		}
	}
	if got := strings.Count(body, "data-folder-hidden-mark"); got != 2 {
		t.Errorf("hidden marks = %d, want 2 (Trips and Coast)", got)
	}
	if !strings.Contains(body, `aria-label="Hidden from the sidebar with the folder it&#39;s in"`) {
		t.Errorf("Coast doesn't say why it's hidden: %q", body)
	}

	if _, err := db.Write().ExecContext(ctx, `UPDATE accounts SET provider = 'gmail' WHERE id = 'victim-account'`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertLabels(ctx, []storage.LabelInput{
		{AccountID: "victim-account", Name: "Receipts", ProviderID: "Label_1", ProviderType: storage.LabelProviderGmail},
		{AccountID: "victim-account", Name: "Travel", ProviderID: "Label_2", ProviderType: storage.LabelProviderGmail},
	}); err != nil {
		t.Fatal(err)
	}
	labels, err := db.ProviderLabels(ctx, "victim-account", storage.LabelProviderGmail)
	if err != nil || len(labels) != 2 {
		t.Fatalf("labels = %v, %v", labels, err)
	}
	ids := map[string]string{}
	for _, label := range labels {
		ids[label.Name] = label.ID
	}
	hide(h.handleSaveTagVisibility, "tags", ids["Receipts"])

	rec = identityRequest(t, h, h.handleManageFolders, http.MethodGet, "/", account, nil, true)
	body = rec.Body.String()
	actions = folderMenuActions(body)
	if !actions["show-tag "+ids["Receipts"]] || !actions["hide-tag "+ids["Travel"]] || actions["hide-tag "+ids["Receipts"]] {
		t.Errorf("label menus = %v", actions)
	}
	if got := strings.Count(body, "data-folder-hidden-mark"); got != 1 {
		t.Errorf("hidden marks = %d, want 1 (Receipts)", got)
	}
}

// Any of the owner's tags can be hidden, an IMAP keyword as well as a Gmail
// label, and a tag of all accounts on its own; another user's can't.
func TestTagVisibilityHTTP(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()
	if err := db.UpsertLabels(ctx, []storage.LabelInput{
		{AccountID: "victim-account", Name: "Clients", ProviderID: "Clients", ProviderType: "imap_keyword"},
		{AccountID: "attacker-account", Name: "Receipts", ProviderID: "Label_1", ProviderType: storage.LabelProviderGmail},
	}); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	rows, err := db.Read().QueryContext(ctx, `SELECT account_id, id FROM labels`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var accountID, id string
		if err := rows.Scan(&accountID, &id); err != nil {
			t.Fatal(err)
		}
		ids[accountID] = id
	}
	rows.Close()

	post := func(handler http.HandlerFunc, id string) int {
		t.Helper()
		return identityRequest(t, h, handler, http.MethodPost, "/visibility", map[string]string{"id": id}, url.Values{"visible": {"false"}}, true).Code
	}
	if code := post(h.handleSaveTagVisibility, ids["victim-account"]); code != http.StatusOK {
		t.Fatalf("hide the IMAP keyword = %d", code)
	}
	if code := post(h.handleSaveUnifiedTagVisibility, models.UnifiedTagID("Clients")); code != http.StatusOK {
		t.Fatalf("hide the unified Clients = %d", code)
	}
	for name, code := range map[string]int{
		"another user's tag":         post(h.handleSaveTagVisibility, ids["attacker-account"]),
		"another user's unified tag": post(h.handleSaveUnifiedTagVisibility, models.UnifiedTagID("Receipts")),
		"an unknown tag":             post(h.handleSaveTagVisibility, "missing"),
	} {
		if code != http.StatusNotFound {
			t.Errorf("%s: HTTP %d, want 404", name, code)
		}
	}
	var keys []string
	rows, err = db.Read().QueryContext(ctx, `SELECT key FROM app_settings WHERE key LIKE '%tag_visible:%' ORDER BY key`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	rows.Close()
	want := []string{"tag_visible:" + ids["victim-account"], "unified_tag_visible:" + models.UnifiedTagID("Clients")}
	if strings.Join(keys, " ") != strings.Join(want, " ") {
		t.Fatalf("settings = %v, want %v", keys, want)
	}
}
