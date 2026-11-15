package handler

import (
	"fmt"
	"strings"
	"testing"
)

func TestMCPTokenLimitedToASharedMailbox(t *testing.T) {
	_, db, ticket, call := mcpWriteSessionFor(t, []string{mcpSharedMailboxKey("victim-account", "Support@Example.com")},
		MCPScopeMailRead, MCPScopeMailOrganize, MCPScopeMailSend, MCPScopeAccounts, MCPScopeSettings)
	ctx := t.Context()

	folders, isErr, text := call("list_folders", map[string]any{"account_id": "victim-account"})
	if isErr {
		t.Fatalf("list_folders: %s", text)
	}
	got := fmt.Sprint(folders["folders"])
	if strings.Contains(got, "own-inbox") || !strings.Contains(got, "support-inbox") {
		t.Fatalf("folders = %s", got)
	}
	if _, isErr, _ := call("list_messages", map[string]any{"folder_id": "own-inbox"}); !isErr {
		t.Fatal("listed a folder outside the token's mailboxes")
	}
	if _, isErr, text := call("read_message", map[string]any{"message_id": ticket}); isErr {
		t.Fatalf("read_message in the allowed mailbox: %s", text)
	}
	hello, _ := db.GetMessageLocalIDByInternetIDInternal(ctx, "victim-account", "<hello@example.com>")
	if _, isErr, _ := call("read_message", map[string]any{"message_id": fmt.Sprint(hello)}); !isErr {
		t.Fatal("read a message outside the token's mailboxes")
	}
	if _, isErr, _ := call("mark_messages", map[string]any{"message_ids": []string{fmt.Sprint(hello)}, "read": true}); !isErr {
		t.Fatal("changed a message outside the token's mailboxes")
	}
	if _, isErr, _ := call("move_messages", map[string]any{"message_ids": []string{ticket}, "folder_id": "own-inbox"}); !isErr {
		t.Fatal("moved mail into a folder outside the token's mailboxes")
	}
	search, isErr, text := call("search_messages", map[string]any{"query": "Hello"})
	if !isErr && strings.Contains(fmt.Sprint(search), "Hello there") {
		t.Fatalf("search found mail outside the token's mailboxes: %v", search)
	}

	// It sends only as the shared mailbox.
	if _, isErr, _ := call("send_message", map[string]any{"to": []string{"a@example.net"}, "subject": "x", "body": "y"}); !isErr {
		t.Fatal("sent as the person's own address by default")
	}
	if _, isErr, _ := call("send_message", map[string]any{"from": "owner@example.com", "to": []string{"a@example.net"}, "subject": "x", "body": "y"}); !isErr {
		t.Fatal("sent as an address outside the token's mailboxes")
	}
	out, isErr, text := call("reply_to_message", map[string]any{"message_id": ticket, "body": "On it."})
	if isErr || out["from"] != "support@example.com" {
		t.Fatalf("reply: %+v %s", out, text)
	}

	// Accounts and settings stay out of reach of a limited token.
	if _, isErr, text := call("get_settings", map[string]any{}); !isErr || !strings.Contains(text, "mailboxes") {
		t.Fatalf("get_settings with a limited token: %v %s", isErr, text)
	}
}

func TestMCPTokenLimitedToTheOwnMailbox(t *testing.T) {
	_, _, ticket, call := mcpWriteSessionFor(t, []string{mcpOwnMailboxKey("victim-account")}, MCPScopeMailRead, MCPScopeMailSend)
	if _, isErr, _ := call("read_message", map[string]any{"message_id": ticket}); !isErr {
		t.Fatal("read a shared mailbox's message")
	}
	if _, isErr, _ := call("send_message", map[string]any{"from": "support@example.com", "to": []string{"a@example.net"}, "subject": "x", "body": "y"}); !isErr {
		t.Fatal("sent as a shared mailbox the token may not reach")
	}
	if out, isErr, text := call("send_message", map[string]any{"to": []string{"a@example.net"}, "subject": "x", "body": "y"}); isErr || out["from"] != "owner@example.com" {
		t.Fatalf("send as own address: %+v %s", out, text)
	}
}

func TestMCPTurnsASharedMailboxOff(t *testing.T) {
	_, db, _, call := mcpWriteSession(t, MCPScopeAccounts, MCPScopeMailRead)
	out, isErr, text := call("set_shared_mailbox_enabled", map[string]any{"account_id": "victim-account", "email": "support@example.com", "enabled": false})
	if isErr || fmt.Sprint(out["shared_mailboxes_off"]) != "[support@example.com]" {
		t.Fatalf("turn off = %+v %s", out, text)
	}
	if off, _ := db.DisabledSharedMailboxes(t.Context(), "victim-account"); !off["support@example.com"] {
		t.Fatal("not turned off in storage")
	}
	folders, _, _ := call("list_folders", map[string]any{"account_id": "victim-account"})
	if strings.Contains(fmt.Sprint(folders["folders"]), "support-inbox") {
		t.Fatalf("a turned off shared mailbox's folders are listed: %v", folders["folders"])
	}
	if _, isErr, _ := call("set_shared_mailbox_enabled", map[string]any{"account_id": "victim-account", "email": "nobody@example.com", "enabled": false}); !isErr {
		t.Fatal("turned off a shared mailbox the account does not have")
	}
	if _, isErr, _ := call("set_shared_mailbox_enabled", map[string]any{"account_id": "attacker-account", "email": "support@example.com", "enabled": false}); !isErr {
		t.Fatal("changed another user's account")
	}
}
