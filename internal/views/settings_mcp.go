package views

import (
	"context"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

// MCPSettingsData is what Settings > Advanced > AI access shows.
type MCPSettingsData struct {
	Enabled  bool
	Endpoint string
	Scopes   []string
	// Mailboxes are the user's mailboxes, to limit a new token to some.
	Mailboxes []MCPMailboxChoice
	Tokens    []MCPTokenRow
	// Grants are the apps let in through OAuth.
	Grants []MCPGrantRow
	// NewToken is a token just made, shown this once.
	NewToken string
	Error    string
}

// MCPMailboxChoice is one mailbox a token can be limited to: an account's
// own mail or a shared mailbox it opens.
type MCPMailboxChoice struct {
	Key     string
	Label   string
	Account string
	Shared  bool
}

// MCPGrantRow is one app let in through OAuth.
type MCPGrantRow struct {
	ID         string
	ClientName string
	Scopes     []string
	// Mailboxes names the mailboxes it may reach; none for all.
	Mailboxes  []string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// MCPTokenRow is one token in the list.
type MCPTokenRow struct {
	ID         string
	Name       string
	Scopes     []string
	Mailboxes  []string
	Kind       string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	Revoked    bool
	Expired    bool
}

// mcpMailboxSummary says which mailboxes a token or app may reach.
func mcpMailboxSummary(ctx context.Context, labels []string) string {
	if len(labels) == 0 {
		return i18n.T(ctx, "settings.mcp.mailboxes_all")
	}
	return strings.Join(labels, ", ")
}

func mcpScopeLabel(ctx context.Context, scope string) string {
	return i18n.T(ctx, "settings.mcp.scope."+scope)
}

func mcpScopeDescription(ctx context.Context, scope string) string {
	return i18n.T(ctx, "settings.mcp.scope."+scope+".description")
}

// mcpScopeDefault says which scopes a new token starts with: reading mail
// only, so that sending, deleting and changing accounts are chosen on purpose.
func mcpScopeDefault(scope string) bool { return scope == "mail.read" }

func mcpDate(ctx context.Context, t time.Time) string { return i18n.Dates(ctx).Date(t) }

func mcpTokenStatus(ctx context.Context, row MCPTokenRow) string {
	switch {
	case row.Revoked:
		return i18n.T(ctx, "settings.mcp.revoked")
	case row.Expired:
		return i18n.T(ctx, "settings.mcp.expired")
	case row.LastUsedAt != nil:
		return i18n.T(ctx, "settings.mcp.last_used", "date", mcpDate(ctx, *row.LastUsedAt))
	default:
		return i18n.T(ctx, "settings.mcp.never_used")
	}
}
