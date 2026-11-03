package views

import (
	"context"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
)

// composeFromRoute is one way a one-time From can be sent: an account's own,
// or the own SMTP server of one of its identities.
type composeFromRoute struct {
	// Key is the route's value in the Send via list.
	Key        string
	AccountID  string
	IdentityID string
	Label      string
	Detail     string
	Rule       models.FromRule
	// Reason says why the route can't send a one-time From; it is empty for
	// one that can.
	Reason string
	// Note is what the compose form says under the route: a warning for a
	// server that may refuse the address, or the domains it takes.
	Note string
}

func (r composeFromRoute) Warns() bool {
	return r.Rule.Kind == models.FromUnknown
}

func (r composeFromRoute) Domains() string {
	return strings.Join(r.Rule.Domains, " ")
}

// composeFromRoutes lists the ways a one-time From of acc can be sent: the
// account's own, then each further SMTP server its identities send through,
// once per server and login.
func composeFromRoutes(ctx context.Context, acc models.Account) []composeFromRoute {
	routes := []composeFromRoute{{
		Key: "account:" + acc.ID, AccountID: acc.ID,
		Label: acc.Email, Detail: composeAccountWayName(ctx, acc),
		Rule: acc.FromRule, Reason: composeFromRuleReason(ctx, acc),
		Note: composeFromRuleNote(ctx, acc.FromRule, acc.Email),
	}}
	seen := map[string]bool{}
	for _, identity := range acc.Identities {
		if !identity.HasOwnSMTP() {
			continue
		}
		server := strings.ToLower(strings.TrimSpace(identity.SMTPHost)) + ":" + strconv.Itoa(identity.SMTPPort) + ":" + strings.TrimSpace(identity.SMTPUsername)
		if seen[server] {
			continue
		}
		seen[server] = true
		rule := providers.SMTPFromRule(identity.SMTPHost)
		routes = append(routes, composeFromRoute{
			Key: "identity:" + identity.ID, AccountID: acc.ID, IdentityID: identity.ID,
			Label: identity.SMTPHost, Detail: i18n.T(ctx, "compose.from.server_of", "email", identity.Email),
			Rule: rule, Note: composeFromRuleNote(ctx, rule, identity.Email),
		})
	}
	return routes
}

// composeHasOneTimeFrom reports whether any account has a way to send a
// one-time From.
func composeHasOneTimeFrom(ctx context.Context, accounts []models.Account) bool {
	for _, acc := range accounts {
		for _, route := range composeFromRoutes(ctx, acc) {
			if route.Reason == "" {
				return true
			}
		}
	}
	return false
}

func composeFromKindName(kind models.FromRuleKind) string {
	switch kind {
	case models.FromDomains:
		return "domains"
	case models.FromListed:
		return "listed"
	}
	return "unknown"
}

func composeAccountWayName(ctx context.Context, acc models.Account) string {
	switch acc.Provider {
	case providers.ProviderGmail:
		return "Gmail"
	case providers.ProviderOutlook:
		return "Outlook"
	case providers.ProviderJMAP:
		return i18n.T(ctx, "compose.from.jmap_server")
	}
	return i18n.T(ctx, "compose.from.smtp_server")
}

// composeFromRuleReason says why the account's own way can't send a one-time
// From, or is empty when it can.
func composeFromRuleReason(ctx context.Context, acc models.Account) string {
	if acc.FromRule.TakesOtherAddresses() {
		return ""
	}
	if acc.Provider == providers.ProviderGmail {
		return i18n.T(ctx, "compose.from.gmail_reason")
	}
	return i18n.T(ctx, "compose.from.own_identities")
}

// composeFromRuleNote is what the compose form says about a way of sending
// with rule, which falls back to own when it won't use the address.
func composeFromRuleNote(ctx context.Context, rule models.FromRule, own string) string {
	switch rule.Kind {
	case models.FromDomains:
		return i18n.T(ctx, "compose.from.note.domains", "domains", strings.Join(rule.Domains, ", "))
	case models.FromListed:
		return ""
	}
	switch rule.Service {
	case providers.FromServiceOutlook:
		return i18n.T(ctx, "compose.from.note.outlook")
	case providers.FromServiceGmail:
		return i18n.T(ctx, "compose.from.note.gmail", "email", own)
	case providers.FromServiceICloud:
		return i18n.T(ctx, "compose.from.note.icloud")
	}
	return i18n.T(ctx, "compose.from.note.unknown", "email", own)
}
