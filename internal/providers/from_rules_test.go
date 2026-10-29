package providers

import "testing"

func TestAccountFromRule(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		smtpHost string
		domains  []string
		kind     FromRuleKind
		service  string
	}{
		{"gmail", ProviderGmail, "smtp.gmail.com", nil, FromListed, ""},
		{"jmap without catch-all", ProviderJMAP, "", nil, FromListed, ""},
		{"jmap with catch-all", ProviderJMAP, "", []string{"example.net"}, FromDomains, ""},
		{"outlook", ProviderOutlook, "", nil, FromUnknown, FromServiceOutlook},
		{"imap on gmail", ProviderIMAP, "SMTP.Gmail.com.", nil, FromUnknown, FromServiceGmail},
		{"imap on icloud", ProviderIMAP, ICloudSMTPHost, nil, FromUnknown, FromServiceICloud},
		{"imap on office365", ProviderIMAP, "smtp.office365.com", nil, FromUnknown, FromServiceOutlook},
		{"imap elsewhere", ProviderIMAP, "mail.example.org", nil, FromUnknown, ""},
	}
	for _, tc := range cases {
		rule := AccountFromRule(tc.provider, tc.smtpHost, tc.domains)
		if rule.Kind != tc.kind || rule.Service != tc.service {
			t.Errorf("%s: rule = %+v, want kind %d service %q", tc.name, rule, tc.kind, tc.service)
		}
	}
}

func TestFromRuleAccepts(t *testing.T) {
	listed := FromRule{Kind: FromListed}
	domains := FromRule{Kind: FromDomains, Domains: []string{"example.net", "shop.example.net"}}
	unknown := FromRule{Kind: FromUnknown, Service: FromServiceOutlook}
	cases := []struct {
		rule  FromRule
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
