package providers

import (
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

// The services a FromUnknown rule may name, whose own lists of addresses
// decide what they accept.
const (
	FromServiceGmail   = "gmail"
	FromServiceICloud  = "icloud"
	FromServiceOutlook = "outlook"
)

// AccountFromRule returns the rule of an account's own way of sending: an
// account of provider sending through smtpHost, whose provider reported
// catchAllDomains as whole-domain identities. Gmail sends only from its "Send
// mail as" list, and a JMAP server only from its identities; Outlook and SMTP
// servers say nothing either way.
func AccountFromRule(provider, smtpHost string, catchAllDomains []string) models.FromRule {
	switch provider {
	case ProviderGmail:
		return models.FromRule{Kind: models.FromListed}
	case ProviderJMAP:
		if len(catchAllDomains) == 0 {
			return models.FromRule{Kind: models.FromListed}
		}
		return models.FromRule{Kind: models.FromDomains, Domains: catchAllDomains}
	case ProviderOutlook:
		return models.FromRule{Kind: models.FromUnknown, Service: FromServiceOutlook}
	}
	return SMTPFromRule(smtpHost)
}

// SMTPFromRule returns the rule of an SMTP server, naming the service behind
// the well-known ones.
func SMTPFromRule(host string) models.FromRule {
	switch strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".") {
	case "smtp.gmail.com":
		return models.FromRule{Kind: models.FromUnknown, Service: FromServiceGmail}
	case ICloudSMTPHost:
		return models.FromRule{Kind: models.FromUnknown, Service: FromServiceICloud}
	case "smtp.office365.com", "smtp-mail.outlook.com":
		return models.FromRule{Kind: models.FromUnknown, Service: FromServiceOutlook}
	}
	return models.FromRule{Kind: models.FromUnknown}
}
