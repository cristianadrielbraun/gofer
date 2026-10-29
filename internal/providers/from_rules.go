package providers

import (
	"slices"
	"strings"
)

// FromRuleKind is how much Gofer knows about the From addresses a way of
// sending accepts.
type FromRuleKind int

const (
	// FromUnknown is a server that does not say which addresses it accepts:
	// any may be tried, and the server may refuse the message or send it from
	// the account's own address instead.
	FromUnknown FromRuleKind = iota
	// FromDomains accepts any address at Domains.
	FromDomains
	// FromListed accepts only the addresses the provider lists, which Gofer
	// already has as the account's identities.
	FromListed
)

// The services a FromUnknown rule may name, whose own lists of addresses
// decide what they accept.
const (
	FromServiceGmail   = "gmail"
	FromServiceICloud  = "icloud"
	FromServiceOutlook = "outlook"
)

// FromRule is what Gofer knows about the From addresses one way of sending
// accepts: an account's own, or an identity's own SMTP server.
type FromRule struct {
	Kind FromRuleKind
	// Domains are the domains a FromDomains rule accepts any address at.
	Domains []string
	// Service names, for a FromUnknown rule, the known service behind the
	// server, which accepts only the addresses set up there; empty for any
	// other server.
	Service string
}

// AccountFromRule returns the rule of an account's own way of sending: an
// account of provider sending through smtpHost, whose provider reported
// catchAllDomains as whole-domain identities. Gmail sends only from its "Send
// mail as" list, and a JMAP server only from its identities; Outlook and SMTP
// servers say nothing either way.
func AccountFromRule(provider, smtpHost string, catchAllDomains []string) FromRule {
	switch provider {
	case ProviderGmail:
		return FromRule{Kind: FromListed}
	case ProviderJMAP:
		if len(catchAllDomains) == 0 {
			return FromRule{Kind: FromListed}
		}
		return FromRule{Kind: FromDomains, Domains: catchAllDomains}
	case ProviderOutlook:
		return FromRule{Kind: FromUnknown, Service: FromServiceOutlook}
	}
	return SMTPFromRule(smtpHost)
}

// SMTPFromRule returns the rule of an SMTP server, naming the service behind
// the well-known ones.
func SMTPFromRule(host string) FromRule {
	switch strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".") {
	case "smtp.gmail.com":
		return FromRule{Kind: FromUnknown, Service: FromServiceGmail}
	case ICloudSMTPHost:
		return FromRule{Kind: FromUnknown, Service: FromServiceICloud}
	case "smtp.office365.com", "smtp-mail.outlook.com":
		return FromRule{Kind: FromUnknown, Service: FromServiceOutlook}
	}
	return FromRule{Kind: FromUnknown}
}

// TakesOtherAddresses reports whether the rule lets a message go out from an
// address that is not one of the provider's listed ones.
func (r FromRule) TakesOtherAddresses() bool {
	return r.Kind != FromListed
}

// Accepts reports whether email may be tried as the From of a message sent
// this way. A FromUnknown rule accepts every address, since only the server
// can tell.
func (r FromRule) Accepts(email string) bool {
	switch r.Kind {
	case FromListed:
		return false
	case FromDomains:
		at := strings.LastIndex(email, "@")
		return at >= 0 && slices.ContainsFunc(r.Domains, func(domain string) bool { return strings.EqualFold(domain, email[at+1:]) })
	}
	return true
}
