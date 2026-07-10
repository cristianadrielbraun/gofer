package autodiscover

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

type fakeResolver map[string][]*net.SRV

func (r fakeResolver) LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error) {
	return "", r[service+"."+proto+"."+name], nil
}

func (r fakeResolver) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	return nil, nil
}

type fakeMXResolver map[string][]*net.MX

func (r fakeMXResolver) LookupMX(ctx context.Context, name string) ([]*net.MX, error) {
	return r[name], nil
}

type fakeHTTPClient map[string]string

func (c fakeHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if body, ok := c[req.URL.String()]; ok {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusNotFound,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("not found")),
	}, nil
}

func TestParseConfigXMLMapsIMAPSMTPSettings(t *testing.T) {
	xml := []byte(`<?xml version="1.0"?>
<clientConfig version="1.1">
  <emailProvider id="example.com">
    <incomingServer type="imap">
      <hostname>imap.example.com</hostname>
      <port>993</port>
      <socketType>SSL</socketType>
      <username>%EMAILADDRESS%</username>
      <authentication>OAuth2</authentication>
      <authentication>password-cleartext</authentication>
    </incomingServer>
    <outgoingServer type="smtp">
      <hostname>smtp.example.com</hostname>
      <port>587</port>
      <socketType>STARTTLS</socketType>
      <username>%EMAILLOCALPART%</username>
      <authentication>password-cleartext</authentication>
    </outgoingServer>
  </emailProvider>
</clientConfig>`)

	candidates, err := ParseConfigXML(xml, "me@example.com", SourceProviderXML)
	if err != nil {
		t.Fatalf("ParseConfigXML: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("len(candidates) = %d, want 1", len(candidates))
	}
	got := candidates[0]
	if got.IMAPHost != "imap.example.com" || got.IMAPPort != 993 || got.IMAPTLSMode != "tls" {
		t.Fatalf("imap settings = %#v", got)
	}
	if got.SMTPHost != "smtp.example.com" || got.SMTPPort != 587 || got.SMTPTLSMode != "starttls" {
		t.Fatalf("smtp settings = %#v", got)
	}
	if got.Username != "me@example.com" {
		t.Fatalf("Username = %q, want full email", got.Username)
	}
	if got.SMTPUsername != "me" {
		t.Fatalf("SMTPUsername = %q, want local part", got.SMTPUsername)
	}
	if got.AuthMethod != "plain" {
		t.Fatalf("AuthMethod = %q, want plain", got.AuthMethod)
	}
	if len(got.Notes) == 0 {
		t.Fatalf("expected OAuth note")
	}
}

func TestParseConfigXMLDropsUnencryptedMailServers(t *testing.T) {
	xml := []byte(`<?xml version="1.0"?>
<clientConfig version="1.1">
  <emailProvider id="example.com">
    <incomingServer type="imap">
      <hostname>imap.example.com</hostname>
      <port>143</port>
      <socketType>plain</socketType>
      <username>%EMAILADDRESS%</username>
      <authentication>password-cleartext</authentication>
    </incomingServer>
    <outgoingServer type="smtp">
      <hostname>smtp.example.com</hostname>
      <port>25</port>
      <socketType>plain</socketType>
      <username>%EMAILADDRESS%</username>
      <authentication>password-cleartext</authentication>
    </outgoingServer>
  </emailProvider>
</clientConfig>`)

	candidates, err := ParseConfigXML(xml, "me@example.com", SourceProviderXML)
	if err == nil {
		t.Fatalf("ParseConfigXML() candidates = %#v, want plaintext configuration rejected", candidates)
	}
}

func TestParseConfigXMLReturnsKnownOAuthProviderCandidate(t *testing.T) {
	xml := []byte(`<?xml version="1.0"?>
<clientConfig version="1.1">
  <emailProvider id="office365.com">
    <incomingServer type="imap">
      <hostname>outlook.office365.com</hostname>
      <port>993</port>
      <socketType>SSL</socketType>
      <username>%EMAILADDRESS%</username>
      <authentication>OAuth2</authentication>
    </incomingServer>
    <outgoingServer type="smtp">
      <hostname>smtp.office365.com</hostname>
      <port>587</port>
      <socketType>STARTTLS</socketType>
      <username>%EMAILADDRESS%</username>
      <authentication>OAuth2</authentication>
    </outgoingServer>
  </emailProvider>
</clientConfig>`)

	candidates, err := ParseConfigXML(xml, "me@example.com", SourceThunderbirdXML)
	if err != nil {
		t.Fatalf("ParseConfigXML: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("len(candidates) = %d, want 1", len(candidates))
	}
	got := candidates[0]
	if got.AuthMethod != "oauth2" || got.Provider != "outlook" {
		t.Fatalf("OAuth candidate = %#v", got)
	}
	if len(got.Notes) == 0 {
		t.Fatalf("expected OAuth note")
	}
}

func TestMXProviderDomainsPreferKnownHostedProvider(t *testing.T) {
	resolver := fakeMXResolver{
		"example.com": {
			{Host: "aspmx.l.google.com.", Pref: 10},
		},
	}

	domains := mxProviderDomains(context.Background(), resolver, "example.com")
	if len(domains) == 0 {
		t.Fatalf("expected MX provider domains")
	}
	if domains[0] != "gmail.com" {
		t.Fatalf("first MX provider domain = %q, want gmail.com; all domains: %#v", domains[0], domains)
	}
}

func TestDiscoverMXConfigXMLUsesProviderDomain(t *testing.T) {
	parts := emailParts{Address: "me@example.com", LocalPart: "me", Domain: "example.com"}
	resolver := fakeMXResolver{
		"example.com": {
			{Host: "aspmx.l.google.com.", Pref: 10},
		},
	}
	client := fakeHTTPClient{
		"https://autoconfig.thunderbird.net/v1.1/gmail.com": testConfigXML("imap.gmail.com", "smtp.gmail.com"),
	}

	candidates := discoverMXConfigXML(context.Background(), client, resolver, parts)
	if len(candidates) == 0 {
		t.Fatalf("expected MX XML candidates")
	}
	got := candidates[0]
	if got.Source != SourceMXProviderXML {
		t.Fatalf("Source = %q, want %q", got.Source, SourceMXProviderXML)
	}
	if got.IMAPHost != "imap.gmail.com" || got.SMTPHost != "smtp.gmail.com" {
		t.Fatalf("candidate = %#v", got)
	}
	if !strings.Contains(strings.Join(got.Notes, " "), "MX records") {
		t.Fatalf("expected MX note, got %#v", got.Notes)
	}
}

func TestDiscoverConfigXMLFallsBackToHTTP(t *testing.T) {
	parts := emailParts{Address: "me@example.com", LocalPart: "me", Domain: "example.com"}
	client := fakeHTTPClient{
		"http://autoconfig.example.com/mail/config-v1.1.xml?emailaddress=me%40example.com": testConfigXML("imap.example.com", "smtp.example.com"),
	}

	candidates := discoverConfigXMLForDomain(context.Background(), client, parts, configLookup{
		domain:                 "example.com",
		providerSource:         SourceProviderXML,
		thunderbirdSource:      SourceThunderbirdXML,
		providerConfidence:     90,
		thunderbirdConfidence:  82,
		httpFallbackConfidence: 66,
	})
	if len(candidates) == 0 {
		t.Fatalf("expected HTTP fallback candidates")
	}
	got := candidates[0]
	if got.Confidence != 66 {
		t.Fatalf("Confidence = %d, want 66", got.Confidence)
	}
	if !strings.Contains(strings.Join(got.Notes, " "), "HTTP fallback") {
		t.Fatalf("expected HTTP fallback note, got %#v", got.Notes)
	}
}

func TestDiscoverSRVBuildsCandidate(t *testing.T) {
	resolver := fakeResolver{
		"imaps.tcp.example.com": {
			{Target: "imap-low.example.com.", Port: 993, Priority: 20, Weight: 1},
			{Target: "imap.example.com.", Port: 993, Priority: 10, Weight: 1},
		},
		"submission.tcp.example.com": {
			{Target: "smtp.example.com.", Port: 587, Priority: 10, Weight: 1},
		},
	}
	parts := emailParts{Address: "me@example.com", LocalPart: "me", Domain: "example.com"}

	candidates := discoverSRV(context.Background(), resolver, parts)
	if len(candidates) == 0 {
		t.Fatalf("expected SRV candidates")
	}
	got := candidates[0]
	if got.Source != SourceDNSSRV {
		t.Fatalf("Source = %q, want %q", got.Source, SourceDNSSRV)
	}
	if got.IMAPHost != "imap.example.com" || got.IMAPTLSMode != "tls" {
		t.Fatalf("first SRV candidate = %#v", got)
	}
	if got.SMTPHost != "smtp.example.com" || got.SMTPPort != 587 || got.SMTPTLSMode != "starttls" {
		t.Fatalf("smtp SRV candidate = %#v", got)
	}
}

func TestHeuristicCandidatesWithoutProbe(t *testing.T) {
	parts := emailParts{Address: "me@example.com", LocalPart: "me", Domain: "example.com"}

	candidates := heuristicCandidates(context.Background(), parts, false, 0)
	if len(candidates) != 12 {
		t.Fatalf("len(candidates) = %d, want 12", len(candidates))
	}
	got := candidates[0]
	if got.Source != SourceHeuristic || got.IMAPHost != "imap.example.com" || got.SMTPHost != "smtp.example.com" {
		t.Fatalf("first heuristic candidate = %#v", got)
	}
}

func testConfigXML(imapHost, smtpHost string) string {
	return `<?xml version="1.0"?>
<clientConfig version="1.1">
  <emailProvider id="example.com">
    <incomingServer type="imap">
      <hostname>` + imapHost + `</hostname>
      <port>993</port>
      <socketType>SSL</socketType>
      <username>%EMAILADDRESS%</username>
      <authentication>password-cleartext</authentication>
    </incomingServer>
    <outgoingServer type="smtp">
      <hostname>` + smtpHost + `</hostname>
      <port>587</port>
      <socketType>STARTTLS</socketType>
      <username>%EMAILADDRESS%</username>
      <authentication>password-cleartext</authentication>
    </outgoingServer>
  </emailProvider>
</clientConfig>`
}
