package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestVirtualFolderAppearanceIsolationAndValidation(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()
	// A real folder can have the same ID without sharing appearance preferences.
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO folders (id, account_id, remote_id, name, icon, role, selectable)
		VALUES ('inbox', 'victim-account', 'INBOX', 'Personal inbox', 'inbox', 'inbox', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFolderColor(ctx, "owner", "inbox", "#ef4444"); err != nil {
		t.Fatal(err)
	}
	id := map[string]string{"id": "inbox"}
	post := func(form url.Values, owner bool) int {
		return identityRequest(t, h, h.handleSaveVirtualFolderAppearance, http.MethodPost, "/", id, form, owner).Code
	}
	for _, tc := range []struct {
		name   string
		form   url.Values
		status int
	}{
		{"icon change", url.Values{"color": {"#3b82f6"}, "icon": {"heart"}}, http.StatusBadRequest},
		{"icon reset", url.Values{"color": {"#3b82f6"}, "icon": {""}}, http.StatusBadRequest},
		{"foreign owner", url.Values{"color": {"#3b82f6"}, "user_id": {"attacker"}}, http.StatusBadRequest},
		{"invalid color", url.Values{"color": {"red;background:red"}}, http.StatusBadRequest},
		{"duplicate style", url.Values{"color": {""}, "style": {"outlined", "filled"}}, http.StatusBadRequest},
		{"valid appearance", url.Values{"color": {"#3B82F6"}, "style": {"filled"}}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := post(tc.form, true); got != tc.status {
				t.Fatalf("POST = %d, want %d", got, tc.status)
			}
		})
	}
	get := func(owner bool) string {
		rec := identityRequest(t, h, h.handleVirtualFolderAppearance, http.MethodGet, "/", id, nil, owner)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if body := get(true); !strings.Contains(body, `value="#3b82f6" checked`) || !strings.Contains(body, `value="filled" checked`) || !strings.Contains(body, `data-folder-appearance-virtual`) || strings.Contains(body, `name="icon"`) {
		t.Fatalf("virtual editor = %s", body)
	}
	if body := get(false); strings.Contains(body, `value="#3b82f6" checked`) || strings.Contains(body, `value="filled" checked`) {
		t.Fatalf("other user inherited appearance: %s", body)
	}
	// Each user can style the same virtual view without touching the other user's view.
	if got := post(url.Values{"color": {"#8b5cf6"}, "style": {"outlined"}}, false); got != http.StatusOK {
		t.Fatalf("other user's preference = %d", got)
	}
	if body := get(true); !strings.Contains(body, `value="#3b82f6" checked`) || !strings.Contains(body, `value="filled" checked`) {
		t.Fatal("other user's write changed owner's appearance")
	}
	folders, err := db.AccountFolderTree(ctx, "victim-account")
	if err != nil || len(folders) != 1 || folders[0].Color != "#ef4444" || folders[0].IsVirtual || folders[0].Icon != "inbox" {
		t.Fatalf("real folder changed: %+v, %v", folders, err)
	}
	// Partial updates preserve the style; Reset restores default color and style.
	if got := post(url.Values{"color": {"#16a34a"}}, true); got != http.StatusOK || !strings.Contains(get(true), `value="filled" checked`) {
		t.Fatal("color-only update lost style")
	}
	if got := post(url.Values{"color": {""}, "style": {"outlined"}}, true); got != http.StatusOK {
		t.Fatalf("reset = %d", got)
	}
	if body := get(true); !strings.Contains(body, `value="outlined" checked`) || !strings.Contains(body, `value="" checked`) {
		t.Fatalf("reset = %s", body)
	}
	for _, unknown := range []string{"custom", "starred-victim-account", "unknown"} {
		id["id"] = unknown
		if got := post(url.Values{"color": {""}}, true); got != http.StatusNotFound {
			t.Fatalf("unknown %q POST = %d", unknown, got)
		}
		rec := identityRequest(t, h, h.handleVirtualFolderAppearance, http.MethodGet, "/", id, nil, true)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("unknown %q GET = %d", unknown, rec.Code)
		}
	}
}

func TestAllVirtualViewsHaveFixedSymbols(t *testing.T) {
	h, _ := newAccountOwnershipTestHandler(t)
	for _, folder := range models.VirtualFolders() {
		id := map[string]string{"id": folder.ID}
		rec := identityRequest(t, h, h.handleSaveVirtualFolderAppearance, http.MethodPost, "/", id, url.Values{"color": {"#3b82f6"}, "style": {"filled"}}, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("save %s = %d: %s", folder.ID, rec.Code, rec.Body.String())
		}
		rec = identityRequest(t, h, h.handleVirtualFolderAppearance, http.MethodGet, "/", id, nil, true)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `value="filled" checked`) || strings.Contains(rec.Body.String(), `name="icon"`) {
			t.Fatalf("editor %s = %d: %s", folder.ID, rec.Code, rec.Body.String())
		}
	}
}
