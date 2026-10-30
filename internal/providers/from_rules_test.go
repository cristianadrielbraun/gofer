package providers

import (
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestAccountFromRule(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		smtpHost string
		domains  []string
		kind     models.FromRuleKind
		service  string
	}{
		{"gmail", ProviderGmail, "smtp.gmail.com", nil, models.FromListed, ""},
		{"jmap without catch-all", ProviderJMAP, "", nil, models.FromListed, ""},
		{"jmap with catch-all", ProviderJMAP, "", []string{"example.net"}, models.FromDomains, ""},
		{"outlook", ProviderOutlook, "", nil, models.FromUnknown, FromServiceOutlook},
		{"imap on gmail", ProviderIMAP, "SMTP.Gmail.com.", nil, models.FromUnknown, FromServiceGmail},
		{"imap on icloud", ProviderIMAP, ICloudSMTPHost, nil, models.FromUnknown, FromServiceICloud},
		{"imap on office365", ProviderIMAP, "smtp.office365.com", nil, models.FromUnknown, FromServiceOutlook},
		{"imap elsewhere", ProviderIMAP, "mail.example.org", nil, models.FromUnknown, ""},
	}
	for _, tc := range cases {
		rule := AccountFromRule(tc.provider, tc.smtpHost, tc.domains)
		if rule.Kind != tc.kind || rule.Service != tc.service {
			t.Errorf("%s: rule = %+v, want kind %d service %q", tc.name, rule, tc.kind, tc.service)
		}
	}
}

func TestFromRuleAccepts(t *testing.T) {
	listed := models.FromRule{Kind: models.FromListed}
	domains := models.FromRule{Kind: models.FromDomains, Domains: []string{"example.net", "shop.example.net"}}
	unknown := models.FromRule{Kind: models.FromUnknown, Service: FromServiceOutlook}
	cases := []struct {
		rule  models.FromRule
		email string
		want  bool
	}{
		{listed, "sales@example.org", false},
		{domains, "sales@EXAMPLE.net", true},
		{domains, "sales@shop.example.net", true},
		{domains, "sales@other.example.net", false},
		{domains, "sales@example.net.evil", false},
		{domains, "no-at-sign", false},
		{unknown, "anyone@example.com", true},
	}
	for _, tc := range cases {
		if got := tc.rule.Accepts(tc.email); got != tc.want {
			t.Errorf("%+v.Accepts(%q) = %v, want %v", tc.rule, tc.email, got, tc.want)
		}
	}
	if listed.TakesOtherAddresses() || !domains.TakesOtherAddresses() || !unknown.TakesOtherAddresses() {
		t.Error("TakesOtherAddresses is wrong")
	}
}
