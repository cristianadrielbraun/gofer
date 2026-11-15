package sharedmailbox

import "testing"

func TestIdentityName(t *testing.T) {
	cases := []struct{ address, mode, custom, want string }{
		{"support@example.com", PrefixDomain, "", "Example Support"},
		{"support@abc.com.my", PrefixDomain, "", "ABC Support"},
		{"sales-test@abc.com.my", PrefixDomain, "", "ABC Sales Test"},
		{"ai@example.co.uk", PrefixDomain, "", "Example AI"},
		{"team@mail.example.org", PrefixDomain, "", "Example Team"},
		{"hello@acme-corp.io", PrefixDomain, "", "Acme Corp Hello"},
		{"support@example.com", PrefixNone, "", "Support"},
		{"support@example.com", PrefixCustom, "Help Desk", "Help Desk Support"},
		{"SUPPORT@Example.com", "", "", "Example Support"},
		{"support-team", PrefixDomain, "", "Support Team"},
	}
	for _, c := range cases {
		if got := IdentityName(c.address, c.mode, c.custom); got != c.want {
			t.Errorf("IdentityName(%q, %q, %q) = %q, want %q", c.address, c.mode, c.custom, got, c.want)
		}
	}
}

func TestSettingsFrom(t *testing.T) {
	if got := SettingsFrom(nil); got != DefaultSettings() {
		t.Fatalf("defaults = %+v", got)
	}
	got := SettingsFrom(map[string]string{KeyAutoIdentities: "false", KeyPrefixMode: PrefixCustom, KeyCustomPrefix: ""})
	if got.AutoIdentities || got.PrefixMode != PrefixDomain || !got.ShowAddress {
		t.Fatalf("settings = %+v", got)
	}
}
