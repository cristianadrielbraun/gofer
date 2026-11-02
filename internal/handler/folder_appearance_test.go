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
