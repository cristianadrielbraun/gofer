package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestTagAppearanceAcrossProviders(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()
	if _, err := db.Write().ExecContext(ctx, `INSERT INTO accounts(id,user_id,provider,email_address) VALUES
	('gmail-owner','owner','gmail','gmail@example.com'),('outlook-owner','owner','outlook','outlook@example.com'),
	('jmap-owner','owner','jmap','jmap@example.com'),('local-owner','owner','local','')`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ account, provider string }{
		{"victim-account", storage.LabelProviderKeyword}, {"gmail-owner", storage.LabelProviderGmail},
		{"outlook-owner", storage.LabelProviderOutlook}, {"jmap-owner", storage.LabelProviderKeyword}, {"local-owner", storage.LabelProviderLocal},
	} {
		t.Run(tc.account, func(t *testing.T) {
			providerID := "work"
			if tc.provider == storage.LabelProviderLocal {
				providerID = ""
			}
			label, err := db.EnsureLabel(ctx, storage.LabelInput{AccountID: tc.account, Name: "Work", Color: "provider-badge-color", ProviderType: tc.provider, ProviderID: providerID})
			if err != nil {
				t.Fatal(err)
			}
			id := map[string]string{"id": label.ID}
			rec := identityRequest(t, h, h.handleSaveTagAppearance, http.MethodPost, "/", id, url.Values{"color": {"#3B82F6"}, "style": {"filled"}}, true)
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"tag":true`) {
				t.Fatalf("save = %d: %s", rec.Code, rec.Body.String())
			}
			rec = identityRequest(t, h, h.handleTagAppearance, http.MethodGet, "/", id, nil, true)
			body := rec.Body.String()
			if rec.Code != http.StatusOK || !strings.Contains(body, `value="#3b82f6" checked`) || !strings.Contains(body, `value="filled" checked`) || !strings.Contains(body, `data-tag-appearance`) || strings.Contains(body, `name="icon"`) {
				t.Fatalf("editor = %d: %s", rec.Code, body)
			}
			beforeSync, err := db.TagAppearanceForUser(ctx, "owner", label.ID, false)
			if err != nil || beforeSync.Color != label.Color {
				t.Fatalf("personal appearance changed provider color: %+v, %v", beforeSync, err)
			}
			if err := db.UpsertLabels(ctx, []storage.LabelInput{{AccountID: tc.account, Name: "Work", Color: "updated-provider-color", ProviderType: tc.provider, ProviderID: providerID}}); err != nil {
				t.Fatal(err)
			}
			got, err := db.TagAppearanceForUser(ctx, "owner", label.ID, false)
			if err != nil || got.AppearanceColor != "#3b82f6" || got.IconStyle != "filled" {
				t.Fatalf("sync changed personal appearance: %+v, %v", got, err)
			}
			// Both the sidebar/account model and Gmail management must load overrides.
			accounts, err := db.GetAccounts(ctx, "owner")
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, a := range accounts {
				for _, l := range a.Labels {
					if l.ID == label.ID {
						found = l.AppearanceColor == "#3b82f6" && l.IconStyle == "filled"
					}
				}
			}
			if !found {
				t.Fatal("account sidebar lost tag appearance")
			}
			if tc.provider == storage.LabelProviderGmail {
				labels, err := db.ProviderLabels(ctx, tc.account, tc.provider)
				if err != nil || len(labels) != 1 || labels[0].AppearanceColor != "#3b82f6" {
					t.Fatalf("Gmail manager lost appearance: %+v, %v", labels, err)
				}
			}
		})
	}
	unified := models.UnifiedTagID("Work")
	id := map[string]string{"id": unified}
	rec := identityRequest(t, h, h.handleUnifiedTagAppearance, http.MethodGet, "/", id, nil, true)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `value="#3b82f6" checked`) {
		t.Fatal("unified tag inherited an account's override")
	}
	rec = identityRequest(t, h, h.handleSaveUnifiedTagAppearance, http.MethodPost, "/", id, url.Values{"color": {"#8b5cf6"}, "style": {"filled"}}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("unified save = %d: %s", rec.Code, rec.Body.String())
	}
	rec = identityRequest(t, h, h.handleUnifiedTagAppearance, http.MethodGet, "/", id, nil, true)
	if !strings.Contains(rec.Body.String(), `value="#8b5cf6" checked`) || !strings.Contains(rec.Body.String(), `data-tag-appearance-unified`) {
		t.Fatalf("unified editor = %s", rec.Body.String())
	}
	if got := models.UnifiedTagAppearance(models.Label{Name: " work "}, db.GetUISettings(ctx, "owner")); got.AppearanceColor != "#8b5cf6" {
		t.Fatalf("unified snapshot = %+v", got)
	}
}

func TestTagAppearanceOwnershipAndFixedSymbol(t *testing.T) {
	h, db := newAccountOwnershipTestHandler(t)
	ctx := t.Context()
	labels := []storage.LabelInput{
		{ID: "tag-own", AccountID: "victim-account", Name: "Work", ProviderType: storage.LabelProviderLocal},
		{ID: "tag-other", AccountID: "attacker-account", Name: "Private", ProviderType: storage.LabelProviderKeyword, ProviderID: "private"},
		{ID: "tag-system", AccountID: "victim-account", Name: "System", ProviderType: storage.LabelProviderGmail, ProviderID: "system", IsSystem: true},
		{ID: "tag-junk", AccountID: "victim-account", Name: "Junk", ProviderType: storage.LabelProviderKeyword, ProviderID: "$junk"},
	}
	if err := db.UpsertLabels(ctx, labels); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"tag-other", "tag-system", "tag-junk", "missing"} {
		values := map[string]string{"id": id}
		for _, save := range []bool{false, true} {
			handler := h.handleTagAppearance
			method := http.MethodGet
			var form url.Values
			if save {
				handler = h.handleSaveTagAppearance
				method = http.MethodPost
				form = url.Values{"color": {"#ef4444"}}
			}
			rec := identityRequest(t, h, handler, method, "/", values, form, true)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("unavailable tag %q = %d", id, rec.Code)
			}
		}
	}
	for _, name := range []string{"Private", "System", "Junk", "missing"} {
		rec := identityRequest(t, h, h.handleSaveUnifiedTagAppearance, http.MethodPost, "/", map[string]string{"id": models.UnifiedTagID(name)}, url.Values{"color": {"#ef4444"}}, true)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("unavailable unified tag %q = %d", name, rec.Code)
		}
	}
	id := map[string]string{"id": "tag-own"}
	for _, form := range []url.Values{
		{"color": {"#3b82f6"}, "icon": {"heart"}}, {"color": {"#3b82f6"}, "icon": {""}},
		{"color": {"red;fill:red"}}, {"color": {""}, "style": {"other"}}, {"color": {""}, "user_id": {"attacker"}},
	} {
		rec := identityRequest(t, h, h.handleSaveTagAppearance, http.MethodPost, "/", id, form, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid tag update = %d", rec.Code)
		}
	}
	for _, form := range []url.Values{{"color": {"#3b82f6"}, "style": {"filled"}}, {"color": {"#ef4444"}}, {"color": {""}, "style": {"outlined"}}} {
		rec := identityRequest(t, h, h.handleSaveTagAppearance, http.MethodPost, "/", id, form, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("tag update = %d: %s", rec.Code, rec.Body.String())
		}
	}
	got, err := db.TagAppearanceForUser(ctx, "owner", "tag-own", false)
	if err != nil || got.AppearanceColor != "" || got.IconStyle != "outlined" {
		t.Fatalf("reset = %+v, %v", got, err)
	}
	if _, err := db.Write().ExecContext(ctx, `UPDATE accounts SET is_deleting=1 WHERE id='victim-account'`); err != nil {
		t.Fatal(err)
	}
	rec := identityRequest(t, h, h.handleTagAppearance, http.MethodGet, "/", id, nil, true)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleting account = %d", rec.Code)
	}
}
