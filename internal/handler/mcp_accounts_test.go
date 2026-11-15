package handler

import (
	"strings"
	"testing"

	mailpkg "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestMCPManageAccounts(t *testing.T) {
	h, db, srv := mcpTestServer(t)
	h.syncer = mailpkg.NewSyncOrchestrator(db, h.accountStore, h.blobStore, nil)
	_, secret, err := db.CreateMCPToken(t.Context(), storage.MCPTokenInput{UserID: "owner", Scopes: []string{MCPScopeAccounts, MCPScopeMailRead}})
	if err != nil {
		t.Fatal(err)
	}
	session := mcpConnect(t, srv, secret)
	call := func(tool string, args map[string]any) (map[string]any, bool, string) {
		t.Helper()
		out, r := mcpCall[map[string]any](t, session, tool, args)
		return out, r.IsError, mcpResultText(r)
	}

	if out, isErr, text := call("add_account", map[string]any{"email": "new@example.com"}); !isErr || !strings.Contains(text, "password") {
		t.Fatalf("add without password: %+v %s", out, text)
	}
	out, isErr, text := call("add_account", map[string]any{
		"email": "new@example.com", "name": "Team", "password": "secret",
		"imap_host": "imap.example.com", "smtp_host": "smtp.example.com", "skip_test": true, "color": "#ff0000",
	})
	if isErr {
		t.Fatalf("add_account: %s", text)
	}
	account := out["account"].(map[string]any)
	id := account["id"].(string)
	if account["email"] != "new@example.com" || account["name"] != "Team" || account["color"] != "#ff0000" {
		t.Fatalf("added = %+v", account)
	}
	// The password is kept encrypted, never shown.
	if strings.Contains(text, "secret") {
		t.Fatal("the password came back")
	}
	if dup, isErr, _ := call("add_account", map[string]any{"email": "new@example.com", "password": "x", "imap_host": "imap.example.com", "smtp_host": "smtp.example.com", "skip_test": true}); !isErr {
		t.Fatalf("added the same mailbox twice: %+v", dup)
	}

	if out, isErr, text := call("update_account", map[string]any{"account_id": id, "name": "Team Mail", "imap_port": 1993}); isErr || out["name"] != "Team Mail" {
		t.Fatalf("update_account: %+v %s", out, text)
	}
	data, err := h.accountStore.GetEditData(t.Context(), id)
	if err != nil || data.IMAPPort != 1993 || data.IMAPHost != "imap.example.com" || data.SMTPHost != "smtp.example.com" {
		t.Fatalf("after update: %+v %v", data, err)
	}
	if out, isErr, text := call("set_account_services", map[string]any{"account_id": id, "mail": false}); isErr || out["mail_sync"] != false {
		t.Fatalf("set_account_services: %+v %s", out, text)
	}
	if _, isErr, text := call("set_account_color", map[string]any{"account_id": id, "color": "not a color"}); !isErr {
		t.Fatalf("bad color accepted: %s", text)
	}
	if _, isErr, _ := call("update_account", map[string]any{"account_id": "attacker-account", "name": "mine"}); !isErr {
		t.Fatal("changed another user's account")
	}
	if _, isErr, text := call("reorder_accounts", map[string]any{"account_ids": []string{id, "victim-account"}}); isErr {
		t.Fatalf("reorder: %s", text)
	}

	test, isErr, text := call("test_account_connection", map[string]any{"email": "new@example.com", "imap_host": "127.0.0.1", "imap_port": 1, "smtp_host": "127.0.0.1", "smtp_port": 1, "password": "x"})
	if isErr || test["ok"] != false {
		t.Fatalf("test against nothing: %+v %s", test, text)
	}

	if _, isErr, text := call("remove_account", map[string]any{"account_id": id}); !isErr || !strings.Contains(text, "confirm") {
		t.Fatalf("remove without confirm: %s", text)
	}
	if _, isErr, text := call("remove_account", map[string]any{"account_id": id, "confirm": true}); isErr {
		t.Fatalf("remove: %s", text)
	}
}
