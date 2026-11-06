package handler

import (
	"net/http"
	"net/url"
	"slices"
	"testing"
)

// An account's folders are put in order only by its owner, and only with
// that account's folders.
func TestFolderOrderSavesTheOwnersOrder(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO folders (id, account_id, remote_id, name, icon, role, selectable, sort_order) VALUES
		('order-inbox', 'victim-account', 'INBOX', 'Inbox', 'inbox', 'inbox', 1, 0),
		('order-trips', 'victim-account', 'Trips', 'Trips', 'folder', 'custom', 1, 100)`); err != nil {
		t.Fatal(err)
	}
	id := map[string]string{"id": "victim-account"}
	for _, tc := range []struct {
		name   string
		form   url.Values
		owner  bool
		status int
	}{
		{"foreign", url.Values{"folder_id": {"order-trips", "order-inbox"}}, false, http.StatusConflict},
		{"unknown folder", url.Values{"folder_id": {"order-trips", "order-gone"}}, true, http.StatusConflict},
		{"nothing", url.Values{}, true, http.StatusConflict},
		{"owner", url.Values{"folder_id": {"order-trips", "order-inbox"}}, true, http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := identityRequest(t, h, h.handleFolderOrder, http.MethodPost, "/", id, tc.form, tc.owner)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
	tree, err := db.AccountFolderTree(ctx, "victim-account")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, f := range tree {
		ids = append(ids, f.ID)
	}
	if want := []string{"order-trips", "order-inbox"}; !slices.Equal(ids, want) {
		t.Fatalf("folders = %v, want %v", ids, want)
	}
}

// The unified folders are put in order from the sidebar or Settings;
// Scheduled and unknown views are refused.
func TestUnifiedFolderOrderSavesTheOrder(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	for _, tc := range []struct {
		name   string
		form   url.Values
		status int
	}{
		{"scheduled", url.Values{"virtual_folder_id": {"scheduled", "inbox"}}, http.StatusConflict},
		{"unknown", url.Values{"virtual_folder_id": {"projects"}}, http.StatusConflict},
		{"unified folders", url.Values{"virtual_folder_id": {"trash", "inbox"}}, http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := identityRequest(t, h, h.handleUnifiedFolderOrder, http.MethodPost, "/", nil, tc.form, true)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
	settings := db.GetUISettings(t.Context(), "owner")
	if settings["virtual_folder_order:trash"] != "0" || settings["virtual_folder_order:inbox"] != "1" {
		t.Fatalf("settings = %v", settings)
	}
}
