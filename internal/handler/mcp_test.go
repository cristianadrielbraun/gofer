package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// mcpTestServer serves the MCP server of a handler with the owner's
// account, its folders and one message.
func mcpTestServer(t *testing.T) (*Handler, *storage.DB, *httptest.Server) {
	t.Helper()
	h, db := newAccountOwnershipTestHandler(t)
	h.auth = auth.NewManager(&auth.Config{Enabled: true, BaseURL: "https://gofer.example"}, db)
	ctx := t.Context()
	if err := db.UpsertFolders(ctx, []storage.UpsertFolderInput{
		{ID: "own-inbox", AccountID: "victim-account", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
		{ID: "support-inbox", AccountID: "victim-account", RemoteID: "Shared Folders/support@example.com/INBOX", Name: "INBOX", Role: "custom", Selectable: true, SharedMailbox: "support@example.com", SharedRole: "inbox"},
		{ID: "attacker-inbox", AccountID: "attacker-account", RemoteID: "INBOX", Name: "Inbox", Role: "inbox", Selectable: true},
	}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []storage.DraftMessageInput{
		{AccountID: "victim-account", FolderID: "own-inbox", InternetMessageID: "<hello@example.com>", FromName: "Customer", FromEmail: "customer@example.net", Subject: "Hello there", Snippet: "Can you help?"},
		{AccountID: "attacker-account", FolderID: "attacker-inbox", InternetMessageID: "<secret@example.com>", FromEmail: "x@example.net", Subject: "Not yours", Snippet: "private"},
	} {
		if _, err := db.SaveDraftMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	// Every route, as Gofer serves them, so a clash shows here.
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return h, db, srv
}

func mcpConnect(t *testing.T, srv *httptest.Server, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{token}, Timeout: 10 * time.Second},
		MaxRetries: -1, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func mcpCall[T any](t *testing.T, session *mcp.ClientSession, tool string, args map[string]any) (T, *mcp.CallToolResult) {
	t.Helper()
	var out T
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if !result.IsError && result.StructuredContent != nil {
		raw, _ := json.Marshal(result.StructuredContent)
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s output: %v", tool, err)
		}
	}
	return out, result
}

func mcpResultText(r *mcp.CallToolResult) string {
	var parts []string
	for _, c := range r.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	return strings.Join(parts, " ")
}

func TestMCPNeedsAValidToken(t *testing.T) {
	_, db, srv := mcpTestServer(t)
	for _, token := range []string{"", "gofer_mcp_unknown"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401", token, resp.StatusCode)
		}
	}
	// A revoked token stops working.
	tok, secret, _ := db.CreateMCPToken(t.Context(), storage.MCPTokenInput{UserID: "owner", Scopes: []string{MCPScopeMailRead}})
	_ = db.RevokeMCPToken(t.Context(), "owner", tok.ID)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	if _, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{secret}}, MaxRetries: -1, DisableStandaloneSSE: true}, nil); err == nil {
		t.Fatal("a revoked token connected")
	}
}

func TestMCPToolsNeedTheirScope(t *testing.T) {
	_, db, srv := mcpTestServer(t)
	_, secret, err := db.CreateMCPToken(t.Context(), storage.MCPTokenInput{UserID: "owner", Scopes: []string{MCPScopeSettings}})
	if err != nil {
		t.Fatal(err)
	}
	session := mcpConnect(t, srv, secret)
	_, result := mcpCall[mcpListAccountsOut](t, session, "list_accounts", nil)
	if !result.IsError || !strings.Contains(mcpResultText(result), MCPScopeMailRead) {
		t.Fatalf("list_accounts without mail.read = %+v", result)
	}
}

func TestMCPReadToolsSeeOnlyTheTokensUser(t *testing.T) {
	_, db, srv := mcpTestServer(t)
	_, secret, err := db.CreateMCPToken(t.Context(), storage.MCPTokenInput{UserID: "owner", Name: "Claude", Scopes: []string{MCPScopeMailRead}})
	if err != nil {
		t.Fatal(err)
	}
	session := mcpConnect(t, srv, secret)

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil || len(tools.Tools) < 6 {
		t.Fatalf("tools = %+v, %v", tools, err)
	}

	accounts, _ := mcpCall[mcpListAccountsOut](t, session, "list_accounts", nil)
	if len(accounts.Accounts) != 1 || accounts.Accounts[0].ID != "victim-account" {
		t.Fatalf("accounts = %+v", accounts)
	}
	if got := accounts.Accounts[0].SharedMailboxes; len(got) != 1 || got[0] != "support@example.com" {
		t.Fatalf("shared mailboxes = %v", got)
	}

	folders, _ := mcpCall[mcpListFoldersOut](t, session, "list_folders", nil)
	var shared *mcpFolder
	for i, f := range folders.Folders {
		if f.AccountID != "victim-account" {
			t.Fatalf("folder of another user: %+v", f)
		}
		if f.ID == "support-inbox" {
			shared = &folders.Folders[i]
		}
	}
	if shared == nil || shared.Role != "inbox" || shared.SharedMailbox != "support@example.com" || shared.Path != "support@example.com/Inbox" {
		t.Fatalf("shared folder = %+v", shared)
	}
	if _, r := mcpCall[mcpListFoldersOut](t, session, "list_folders", map[string]any{"account_id": "attacker-account"}); !r.IsError {
		t.Fatal("listed another user's account folders")
	}

	page, _ := mcpCall[mcpMessagePage](t, session, "list_messages", map[string]any{"folder_id": "own-inbox"})
	if len(page.Messages) != 1 || page.Messages[0].Subject != "Hello there" {
		t.Fatalf("messages = %+v", page)
	}
	if other, r := mcpCall[mcpMessagePage](t, session, "list_messages", map[string]any{"folder_id": "attacker-inbox"}); !r.IsError && len(other.Messages) != 0 {
		t.Fatalf("listed another user's messages: %+v", other)
	}

	found, _ := mcpCall[mcpMessagePage](t, session, "search_messages", map[string]any{"subject": "Hello"})
	if len(found.Messages) != 1 {
		t.Fatalf("search = %+v", found)
	}
	if none, _ := mcpCall[mcpMessagePage](t, session, "search_messages", map[string]any{"subject": "Not yours"}); len(none.Messages) != 0 {
		t.Fatalf("search found another user's message: %+v", none)
	}

	msg, r := mcpCall[mcpMessage](t, session, "read_message", map[string]any{"message_id": page.Messages[0].ID})
	if r.IsError || msg.Subject != "Hello there" || msg.From.Email != "customer@example.net" {
		t.Fatalf("read = %+v %s", msg, mcpResultText(r))
	}
	attackerID, _ := db.GetMessageLocalIDByInternetIDInternal(t.Context(), "attacker-account", "<secret@example.com>")
	if _, r := mcpCall[mcpMessage](t, session, "read_message", map[string]any{"message_id": attackerID}); !r.IsError {
		t.Fatal("read another user's message")
	}
}
