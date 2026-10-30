package views

import (
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
func composeFromRoutes(acc models.Account) []composeFromRoute {
	routes := []composeFromRoute{{
		Key: "account:" + acc.ID, AccountID: acc.ID,
		Label: acc.Email, Detail: composeAccountWayName(acc),
		Rule: acc.FromRule, Reason: composeFromRuleReason(acc),
		Note: composeFromRuleNote(acc.FromRule, acc.Email),
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
			Label: identity.SMTPHost, Detail: "Server of " + identity.Email,
			Rule: rule, Note: composeFromRuleNote(rule, identity.Email),
		})
	}
	return routes
}

// composeHasOneTimeFrom reports whether any account has a way to send a
// one-time From.
func composeHasOneTimeFrom(accounts []models.Account) bool {
	for _, acc := range accounts {
		for _, route := range composeFromRoutes(acc) {
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

func composeAccountWayName(acc models.Account) string {
	switch acc.Provider {
	case providers.ProviderGmail:
		return "Gmail"
	case providers.ProviderOutlook:
		return "Outlook"
	case providers.ProviderJMAP:
		return "The account's JMAP server"
	}
	return "The account's SMTP server"
}

// composeFromRuleReason says why the account's own way can't send a one-time
// From, or is empty when it can.
func composeFromRuleReason(acc models.Account) string {
	if acc.FromRule.TakesOtherAddresses() {
		return ""
	}
	if acc.Provider == providers.ProviderGmail {
		return `Gmail sends only from the addresses in its "Send mail as" settings`
	}
	return "The server sends only from its own identities"
}

// composeFromRuleNote is what the compose form says about a way of sending
// with rule, which falls back to own when it won't use the address.
func composeFromRuleNote(rule models.FromRule, own string) string {
	switch rule.Kind {
	case models.FromDomains:
		return "Sends from any address at " + strings.Join(rule.Domains, ", ") + "."
	case models.FromListed:
		return ""
	}
	switch rule.Service {
	case providers.FromServiceOutlook:
		return "Outlook doesn't say which addresses this account may send as. Without permission for this address, it refuses the message."
	case providers.FromServiceGmail:
		return `Gmail sends only from the addresses in its "Send mail as" settings, and sends from ` + own + " instead of any other."
	case providers.FromServiceICloud:
		return "iCloud sends only from this account's iCloud addresses and aliases, and may refuse any other."
	}
	return "This server doesn't say which addresses it accepts. It may refuse the message, or send it from " + own + " instead."
}
