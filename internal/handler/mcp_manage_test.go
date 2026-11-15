package handler

import (
	"github.com/cristianadrielbraun/gofer/internal/sharedmailbox"
	"strings"
	"testing"
	"time"

	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestMCPManageFoldersIdentitiesSettings(t *testing.T) {
	h, db, srv := mcpTestServer(t)
	h.syncer = mailpkg.NewSyncOrchestrator(db, h.accountStore, h.blobStore, nil)
	ctx := t.Context()
	// From addresses for shared mailboxes are made only once chosen.
	if err := db.MergeUISettings(ctx, "owner", map[string]string{sharedmailbox.KeyAutoIdentities: "true"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ApplySharedMailboxDiscovery(ctx, "victim-account", []string{"support@example.com"}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, secret, err := db.CreateMCPToken(ctx, storage.MCPTokenInput{UserID: "owner", Scopes: []string{MCPScopeSettings, MCPScopeMailRead, MCPScopeMailOrganize}})
	if err != nil {
		t.Fatal(err)
	}
	session := mcpConnect(t, srv, secret)
	call := func(tool string, args map[string]any) (map[string]any, bool, string) {
		t.Helper()
		out, r := mcpCall[map[string]any](t, session, tool, args)
		return out, r.IsError, mcpResultText(r)
	}

	if _, isErr, text := call("set_appearance", map[string]any{"folder_id": "own-inbox", "color": "#22c55e"}); isErr {
		t.Fatalf("set_appearance: %s", text)
	}
	if _, isErr, text := call("set_appearance", map[string]any{"folder_id": "support-inbox", "color": "#3b82f6", "icon": "heart"}); isErr {
		t.Fatalf("set_appearance with an icon: %s", text)
	}
	folders, _ := mcpCall[mcpListFoldersOut](t, session, "list_folders", map[string]any{"account_id": "victim-account"})
	for _, f := range folders.Folders {
		if f.ID == "own-inbox" && f.Color != "#22c55e" {
			t.Fatalf("color not saved: %+v", f)
		}
	}
	if _, isErr, _ := call("set_appearance", map[string]any{"folder_id": "attacker-inbox", "color": "#000000"}); !isErr {
		t.Fatal("changed another user's folder")
	}
	if _, isErr, _ := call("rename_folder", map[string]any{"folder_id": "attacker-inbox", "name": "x"}); !isErr {
		t.Fatal("renamed another user's folder")
	}
	if _, isErr, text := call("delete_folder", map[string]any{"folder_id": "own-inbox"}); !isErr || !strings.Contains(text, "confirm") {
		t.Fatalf("delete without confirm: %s", text)
	}
	if _, isErr, text := call("empty_folder", map[string]any{"folder_id": "own-inbox"}); !isErr || !strings.Contains(text, "confirm") {
		t.Fatalf("empty without confirm: %s", text)
	}

	out, isErr, text := call("save_identity", map[string]any{"account_id": "victim-account", "name": "Ana Sales", "email": "sales@example.com"})
	if isErr {
		t.Fatalf("save_identity: %s", text)
	}
	var salesID, supportID string
	for _, i := range out["identities"].([]any) {
		identity := i.(map[string]any)
		switch identity["email"] {
		case "sales@example.com":
			salesID = identity["id"].(string)
		case "support@example.com":
			supportID = identity["id"].(string)
			if identity["shared_mailbox"] != "support@example.com" || identity["state"] != "active" {
				t.Fatalf("shared identity = %+v", identity)
			}
		}
	}
	if salesID == "" || supportID == "" {
		t.Fatalf("identities = %+v", out)
	}
	if _, isErr, text := call("save_identity", map[string]any{"account_id": "victim-account", "email": "not an address"}); !isErr {
		t.Fatalf("saved a bad address: %s", text)
	}
	if out, isErr, text := call("set_default_identity", map[string]any{"account_id": "victim-account", "identity_id": salesID}); isErr || !strings.Contains(text, `"default":true`) {
		t.Fatalf("set_default_identity: %+v %s", out, text)
	}
	if _, isErr, text := call("set_shared_identity_enabled", map[string]any{"account_id": "victim-account", "identity_id": supportID, "enabled": false}); isErr || !strings.Contains(text, `"state":"disabled"`) {
		t.Fatalf("turn off shared identity: %s", text)
	}

	if _, isErr, text := call("save_signature", map[string]any{"name": "Work", "text_body": "Ana\nExample Ltd"}); isErr {
		t.Fatalf("save_signature: %s", text)
	}
	sigs, _ := mcpCall[mcpSignaturesOut](t, session, "list_signatures", nil)
	if len(sigs.Signatures) != 1 || sigs.Signatures[0].Name != "Work" {
		t.Fatalf("signatures = %+v", sigs)
	}

	settings, _ := mcpCall[mcpSettingsOut](t, session, "update_settings", map[string]any{"settings": map[string]string{"shared_mailboxes_show_address": "false"}, "sync_interval_minutes": 15})
	if settings.SharedMailboxes.ShowAddress || settings.SyncMinutes != 15 {
		t.Fatalf("settings = %+v", settings)
	}
	if got, _ := mcpCall[mcpSettingsOut](t, session, "get_settings", nil); got.Settings["shared_mailboxes_show_address"] != "false" {
		t.Fatalf("get_settings = %+v", got.Settings)
	}
}
