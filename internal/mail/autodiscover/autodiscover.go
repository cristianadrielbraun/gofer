package autodiscover

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	mailtransport "github.com/cristianadrielbraun/gofer/internal/mail/transport"
	"golang.org/x/net/publicsuffix"
)

const (
	SourceProviderXML    = "provider_xml"
	SourceThunderbirdXML = "thunderbird_xml"
	SourceMXProviderXML  = "mx_provider_xml"
	SourceDNSSRV         = "dns_srv"
	SourceHeuristic      = "heuristic"

	maxConfigBytes = 1 << 20
)

type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type SRVResolver interface {
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
}

type MXResolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
}

type Options struct {
	HTTPClient              HTTPClient
	Resolver                SRVResolver
	MXResolver              MXResolver
	ProbeHeuristics         bool
	HeuristicProbeTimeout   time.Duration
	AllowHTTPDiscovery      func(domain string) bool
	AllowPlaintextTransport func(protocol, host string, port int) bool
}

type securityPolicy struct {
	allowHTTPDiscovery      func(domain string) bool
	allowPlaintextTransport func(protocol, host string, port int) bool
}

func (p securityPolicy) allowsHTTPDiscovery(domain string) bool {
	return p.allowHTTPDiscovery != nil && p.allowHTTPDiscovery(cleanHost(domain))
}

func (p securityPolicy) allowsPlaintextTransport(protocol, host string, port int) bool {
	return p.allowPlaintextTransport != nil && p.allowPlaintextTransport(strings.ToLower(strings.TrimSpace(protocol)), cleanHost(host), port)
}

type Candidate struct {
	Source       string   `json:"source"`
	Confidence   int      `json:"confidence"`
	IMAPHost     string   `json:"imap_host"`
	IMAPPort     int      `json:"imap_port"`
	IMAPTLSMode  string   `json:"imap_tls_mode"`
	SMTPHost     string   `json:"smtp_host"`
	SMTPPort     int      `json:"smtp_port"`
	SMTPTLSMode  string   `json:"smtp_tls_mode"`
	Username     string   `json:"username"`
	SMTPUsername string   `json:"smtp_username,omitempty"`
	AuthMethod   string   `json:"auth_method"`
	Provider     string   `json:"provider,omitempty"`
	Notes        []string `json:"notes,omitempty"`
}

type emailParts struct {
	Address   string
	LocalPart string
	Domain    string
}

func Discover(ctx context.Context, email string, opts Options) ([]Candidate, error) {
	parts, err := parseEmail(email)
	if err != nil {
		return nil, err
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = newDiscoveryHTTPClient()
	}
	if opts.Resolver == nil {
		opts.Resolver = net.DefaultResolver
	}
	if opts.MXResolver == nil {
		if resolver, ok := opts.Resolver.(MXResolver); ok {
			opts.MXResolver = resolver
		} else {
			opts.MXResolver = net.DefaultResolver
		}
	}
	if opts.HeuristicProbeTimeout == 0 {
		opts.HeuristicProbeTimeout = 1500 * time.Millisecond
	}
	policy := securityPolicy{
		allowHTTPDiscovery:      opts.AllowHTTPDiscovery,
		allowPlaintextTransport: opts.AllowPlaintextTransport,
	}

	var out []Candidate
	out = append(out, discoverConfigXML(ctx, opts.HTTPClient, parts, policy)...)
	out = append(out, discoverMXConfigXML(ctx, opts.HTTPClient, opts.MXResolver, parts, policy)...)
	out = append(out, discoverSRV(ctx, opts.Resolver, parts)...)
	out = append(out, heuristicCandidates(ctx, parts, opts.ProbeHeuristics, opts.HeuristicProbeTimeout)...)
	return dedupeCandidates(out, policy), nil
}

func newDiscoveryHTTPClient() *http.Client {
	return &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("autodiscovery stopped after too many redirects")
		}
		if len(via) == 0 || via[0].URL == nil || req.URL == nil {
			return fmt.Errorf("autodiscovery refused an invalid redirect")
		}
		initialURL := via[0].URL
		if initialURL.Scheme == "https" && req.URL.Scheme != "https" {
			return fmt.Errorf("autodiscovery refused HTTPS downgrade")
		}
		if initialURL.Scheme == "http" && req.URL.Scheme == "http" && !strings.EqualFold(initialURL.Hostname(), req.URL.Hostname()) {
			return fmt.Errorf("HTTP autodiscovery refused a cross-host redirect")
		}
		if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
			return fmt.Errorf("autodiscovery redirected to unsupported scheme %q", req.URL.Scheme)
		}
		return nil
	}}
}

func parseEmail(email string) (emailParts, error) {
	email = strings.TrimSpace(email)
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return emailParts{}, fmt.Errorf("enter a valid email address")
	}
	local := strings.TrimSpace(email[:at])
	domain := strings.ToLower(strings.Trim(strings.TrimSpace(email[at+1:]), "."))
	if local == "" || !validDomain(domain) {
		return emailParts{}, fmt.Errorf("enter a valid email domain")
	}
	return emailParts{Address: email, LocalPart: local, Domain: domain}, nil
}

func validDomain(domain string) bool {
	if domain == "" || len(domain) > 253 {
		return false
	}
	if strings.EqualFold(domain, "localhost") {
		return false
	}
	if _, err := netip.ParseAddr(domain); err == nil {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				continue
			}
			return false
		}
	}
	return true
}

type configLookup struct {
	domain                 string
	providerSource         string
	thunderbirdSource      string
	providerConfidence     int
	thunderbirdConfidence  int
	httpFallbackConfidence int
	notes                  []string
}

type configEndpoint struct {
	source     string
	rawURL     string
	confidence int
	notes      []string
}

func discoverConfigXML(ctx context.Context, client HTTPClient, parts emailParts, policy securityPolicy) []Candidate {
	return discoverConfigXMLForDomain(ctx, client, parts, configLookup{
		domain:                 parts.Domain,
		providerSource:         SourceProviderXML,
		thunderbirdSource:      SourceThunderbirdXML,
		providerConfidence:     90,
		thunderbirdConfidence:  82,
		httpFallbackConfidence: 66,
	}, policy)
}

func discoverConfigXMLForDomain(ctx context.Context, client HTTPClient, parts emailParts, lookup configLookup, policy securityPolicy) []Candidate {
	lookup.domain = strings.ToLower(strings.TrimSpace(lookup.domain))
	if !validDomain(lookup.domain) {
		return nil
	}
	secure := []configEndpoint{
		{
			source:     lookup.providerSource,
			rawURL:     "https://autoconfig." + lookup.domain + "/mail/config-v1.1.xml?emailaddress=" + url.QueryEscape(parts.Address),
			confidence: lookup.providerConfidence,
			notes:      lookup.notes,
		},
		{
			source:     lookup.providerSource,
			rawURL:     "https://" + lookup.domain + "/.well-known/autoconfig/mail/config-v1.1.xml?emailaddress=" + url.QueryEscape(parts.Address),
			confidence: lookup.providerConfidence - 1,
			notes:      lookup.notes,
		},
		{
			source:     lookup.thunderbirdSource,
			rawURL:     "https://autoconfig.thunderbird.net/v1.1/" + url.PathEscape(lookup.domain),
			confidence: lookup.thunderbirdConfidence,
			notes:      lookup.notes,
		},
	}
	out := fetchConfigEndpoints(ctx, client, parts, secure, policy)
	if len(out) > 0 {
		return out
	}
	if !policy.allowsHTTPDiscovery(lookup.domain) {
		return nil
	}

	httpNotes := appendNotes(lookup.notes, "Provider XML was fetched over HTTP fallback; verify before saving.")
	httpEndpoints := []configEndpoint{
		{
			source:     lookup.providerSource,
			rawURL:     "http://autoconfig." + lookup.domain + "/mail/config-v1.1.xml?emailaddress=" + url.QueryEscape(parts.Address),
			confidence: lookup.httpFallbackConfidence,
			notes:      httpNotes,
		},
		{
			source:     lookup.providerSource,
			rawURL:     "http://" + lookup.domain + "/.well-known/autoconfig/mail/config-v1.1.xml?emailaddress=" + url.QueryEscape(parts.Address),
			confidence: lookup.httpFallbackConfidence - 1,
			notes:      httpNotes,
		},
	}
	return fetchConfigEndpoints(ctx, client, parts, httpEndpoints, policy)
}

func fetchConfigEndpoints(ctx context.Context, client HTTPClient, parts emailParts, endpoints []configEndpoint, policy securityPolicy) []Candidate {
	if len(endpoints) == 0 {
		return nil
	}
	type result struct {
		index      int
		candidates []Candidate
	}
	results := make(chan result, len(endpoints))
	var wg sync.WaitGroup
	for index, endpoint := range endpoints {
		wg.Add(1)
		go func(index int, endpoint configEndpoint) {
			defer wg.Done()
			body, err := fetchXML(ctx, client, endpoint.rawURL)
			if err != nil {
				return
			}
			candidates, err := parseConfigXML(body, parts, endpoint.source, endpoint.confidence, endpoint.notes, policy)
			if err != nil {
				return
			}
			results <- result{index: index, candidates: candidates}
		}(index, endpoint)
	}
	wg.Wait()
	close(results)

	ordered := make([]result, 0, len(endpoints))
	for result := range results {
		ordered = append(ordered, result)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].index < ordered[j].index
	})
	out := make([]Candidate, 0)
	for _, result := range ordered {
		out = append(out, result.candidates...)
	}
	return out
}

func fetchXML(ctx context.Context, client HTTPClient, rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/xml,text/xml;q=0.9,*/*;q=0.1")
	req.Header.Set("User-Agent", "Gofer Mail Autodiscovery")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	initialURL, initialErr := url.Parse(rawURL)
	if initialErr != nil || resp.Request == nil || resp.Request.URL == nil {
		return nil, fmt.Errorf("invalid autodiscovery response URL")
	}
	finalURL := resp.Request.URL
	if finalURL.Scheme != "https" && finalURL.Scheme != "http" {
		return nil, fmt.Errorf("autodiscovery redirected to unsupported scheme %q", finalURL.Scheme)
	}
	if initialURL.Scheme == "https" && finalURL.Scheme != "https" {
		return nil, fmt.Errorf("autodiscovery refused HTTPS downgrade")
	}
	if initialURL.Scheme == "http" && finalURL.Scheme == "http" && !strings.EqualFold(initialURL.Hostname(), finalURL.Hostname()) {
		return nil, fmt.Errorf("HTTP autodiscovery refused a cross-host redirect")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("fetch %s: status %d", rawURL, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxConfigBytes))
}

func discoverMXConfigXML(ctx context.Context, client HTTPClient, resolver MXResolver, parts emailParts, policy securityPolicy) []Candidate {
	if resolver == nil {
		return nil
	}
	domains := mxProviderDomains(ctx, resolver, parts.Domain)
	var out []Candidate
	for _, domain := range domains {
		notes := []string{
			"Matched mail provider through MX records for " + parts.Domain + ".",
			"Using provider config domain " + domain + ".",
		}
		out = append(out, discoverConfigXMLForDomain(ctx, client, parts, configLookup{
			domain:                 domain,
			providerSource:         SourceMXProviderXML,
			thunderbirdSource:      SourceMXProviderXML,
			providerConfidence:     78,
			thunderbirdConfidence:  76,
			httpFallbackConfidence: 58,
			notes:                  notes,
		}, policy)...)
		if len(out) >= 12 {
			return out
		}
	}
	return out
}

func mxProviderDomains(ctx context.Context, resolver MXResolver, domain string) []string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	records, err := resolver.LookupMX(ctx, domain)
	if err != nil || len(records) == 0 {
		return nil
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Pref != records[j].Pref {
			return records[i].Pref < records[j].Pref
		}
		return cleanHost(records[i].Host) < cleanHost(records[j].Host)
	})

	seen := map[string]bool{domain: true}
	out := make([]string, 0, 4)
	for _, record := range records {
		for _, lookupDomain := range providerDomainsFromMXHost(cleanHost(record.Host)) {
			if lookupDomain == "" || seen[lookupDomain] || !validDomain(lookupDomain) {
				continue
			}
			seen[lookupDomain] = true
			out = append(out, lookupDomain)
			if len(out) >= 4 {
				return out
			}
		}
	}
	return out
}

func providerDomainsFromMXHost(host string) []string {
	host = cleanHost(host)
	if host == "" {
		return nil
	}
	var out []string
	for _, known := range knownMXConfigDomains {
		if host == known.suffix || strings.HasSuffix(host, "."+known.suffix) {
			out = append(out, known.domains...)
		}
	}

	base, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		base = lastLabelsDomain(host, 2)
	}
	if base != "" {
		out = append(out, base)
	}

	if base != "" && host != base {
		hostLabels := strings.Split(host, ".")
		baseLabels := strings.Split(base, ".")
		prefixLen := len(hostLabels) - len(baseLabels)
		if prefixLen > 0 {
			out = append(out, strings.Join(hostLabels[prefixLen-1:], "."))
		}
	}
	return compactStrings(out)
}

var knownMXConfigDomains = []struct {
	suffix  string
	domains []string
}{
	{suffix: "google.com", domains: []string{"gmail.com", "googlemail.com"}},
	{suffix: "googlemail.com", domains: []string{"gmail.com", "googlemail.com"}},
	{suffix: "protection.outlook.com", domains: []string{"office365.com", "outlook.com"}},
	{suffix: "outlook.com", domains: []string{"outlook.com", "office365.com"}},
	{suffix: "hotmail.com", domains: []string{"outlook.com"}},
	{suffix: "yahoodns.net", domains: []string{"yahoo.com"}},
	{suffix: "zoho.com", domains: []string{"zoho.com"}},
	{suffix: "zohomail.com", domains: []string{"zoho.com"}},
}

func lastLabelsDomain(host string, count int) string {
	labels := strings.Split(cleanHost(host), ".")
	if len(labels) < count {
		return ""
	}
	return strings.Join(labels[len(labels)-count:], ".")
}

type clientConfig struct {
	XMLName       xml.Name      `xml:"clientConfig"`
	EmailProvider emailProvider `xml:"emailProvider"`
}

type emailProvider struct {
	ID               string       `xml:"id,attr"`
	DisplayName      string       `xml:"displayName"`
	DisplayShortName string       `xml:"displayShortName"`
	IncomingServers  []mailServer `xml:"incomingServer"`
	OutgoingServers  []mailServer `xml:"outgoingServer"`
}

type mailServer struct {
	Type           string   `xml:"type,attr"`
	Hostname       string   `xml:"hostname"`
	Port           int      `xml:"port"`
	SocketType     string   `xml:"socketType"`
	Username       string   `xml:"username"`
	Authentication []string `xml:"authentication"`
}

func ParseConfigXML(data []byte, email string, source string) ([]Candidate, error) {
	parts, err := parseEmail(email)
	if err != nil {
		return nil, err
	}
	return parseConfigXML(data, parts, source, defaultConfigConfidence(source), nil, securityPolicy{})
}

func parseConfigXML(data []byte, parts emailParts, source string, confidence int, notes []string, policy securityPolicy) ([]Candidate, error) {
	var cfg clientConfig
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.Strict = false
	if err := decoder.Decode(&cfg); err != nil {
		return nil, err
	}
	if cfg.XMLName.Local != "clientConfig" {
		return nil, fmt.Errorf("not a clientConfig document")
	}

	var incoming []mailServer
	for _, server := range cfg.EmailProvider.IncomingServers {
		if strings.EqualFold(strings.TrimSpace(server.Type), "imap") {
			incoming = append(incoming, server)
		}
	}
	if len(incoming) == 0 || len(cfg.EmailProvider.OutgoingServers) == 0 {
		return nil, fmt.Errorf("no supported imap/smtp configuration")
	}
	if confidence == 0 {
		confidence = defaultConfigConfidence(source)
	}

	var out []Candidate
	for _, in := range incoming {
		for _, smtp := range cfg.EmailProvider.OutgoingServers {
			if !strings.EqualFold(strings.TrimSpace(smtp.Type), "smtp") {
				continue
			}
			candidate, ok := candidateFromXMLServers(in, smtp, parts, source, confidence, notes, policy)
			if !ok {
				continue
			}
			out = append(out, candidate)
			if len(out) >= 8 {
				return out, nil
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no supported secure mail configuration")
	}
	return out, nil
}

func defaultConfigConfidence(source string) int {
	switch source {
	case SourceThunderbirdXML:
		return 82
	case SourceMXProviderXML:
		return 76
	default:
		return 90
	}
}

func candidateFromXMLServers(imapServer, smtpServer mailServer, parts emailParts, source string, confidence int, baseNotes []string, policy securityPolicy) (Candidate, bool) {
	auth, notes, ok := chooseManualAuth(imapServer.Authentication, smtpServer.Authentication)
	if !ok {
		return Candidate{}, false
	}
	notes = appendNotes(baseNotes, notes...)
	imapHost := cleanHost(imapServer.Hostname)
	smtpHost := cleanHost(smtpServer.Hostname)
	if imapHost == "" || smtpHost == "" || imapServer.Port == 0 || smtpServer.Port == 0 {
		return Candidate{}, false
	}
	provider := ""
	if auth == "oauth2" {
		provider = knownOAuthProviderForHosts(imapHost, smtpHost)
		if provider == "" {
			return Candidate{}, false
		}
		notes = append(notes, "Use "+oauthProviderLabel(provider)+" sign-in for this configuration.")
	}
	imapTLS := socketTypeToTLSMode(imapServer.SocketType, imapServer.Port)
	smtpTLS := socketTypeToTLSMode(smtpServer.SocketType, smtpServer.Port)
	imapTLS, err := mailtransport.RequireTLSModeWithPlaintext("IMAP", imapTLS, policy.allowsPlaintextTransport("imap", imapHost, imapServer.Port))
	if err != nil {
		return Candidate{}, false
	}
	smtpTLS, err = mailtransport.RequireTLSModeWithPlaintext("SMTP", smtpTLS, policy.allowsPlaintextTransport("smtp", smtpHost, smtpServer.Port))
	if err != nil {
		return Candidate{}, false
	}
	if (imapTLS == mailtransport.TLSModePlaintext || smtpTLS == mailtransport.TLSModePlaintext) && auth != "plain" {
		return Candidate{}, false
	}
	username := expandPlaceholders(firstNonEmpty(imapServer.Username, "%EMAILADDRESS%"), parts)
	smtpUsername := expandPlaceholders(smtpServer.Username, parts)
	if smtpUsername == username {
		smtpUsername = ""
	}
	return Candidate{
		Source:       source,
		Confidence:   confidence,
		IMAPHost:     imapHost,
		IMAPPort:     imapServer.Port,
		IMAPTLSMode:  imapTLS,
		SMTPHost:     smtpHost,
		SMTPPort:     smtpServer.Port,
		SMTPTLSMode:  smtpTLS,
		Username:     username,
		SMTPUsername: smtpUsername,
		AuthMethod:   auth,
		Provider:     provider,
		Notes:        compactStrings(notes),
	}, true
}

func chooseManualAuth(authLists ...[]string) (string, []string, bool) {
	notes := []string{}
	allHavePlain := true
	allHaveOAuth := true
	for _, auths := range authLists {
		hasPlain := false
		hasOAuth := false
		for _, auth := range auths {
			switch strings.ToLower(strings.TrimSpace(auth)) {
			case "", "password-cleartext", "plain":
				hasPlain = true
			case "oauth2":
				hasOAuth = true
			}
		}
		if hasOAuth {
			notes = append(notes, "Provider advertises OAuth2; use provider sign-in when available.")
		}
		if !hasPlain {
			allHavePlain = false
		}
		if !hasOAuth {
			allHaveOAuth = false
		}
	}
	if allHavePlain {
		return "plain", compactStrings(notes), true
	}
	if allHaveOAuth {
		return "oauth2", compactStrings(notes), true
	}
	return "", compactStrings(notes), false
}

func knownOAuthProviderForHosts(hosts ...string) string {
	for _, host := range hosts {
		host = cleanHost(host)
		if host == "imap.gmail.com" || host == "smtp.gmail.com" || strings.HasSuffix(host, ".gmail.com") || strings.HasSuffix(host, ".googlemail.com") {
			return "gmail"
		}
		if host == "outlook.office365.com" || host == "smtp.office365.com" || host == "smtp-mail.outlook.com" || strings.HasSuffix(host, ".office365.com") || strings.HasSuffix(host, ".outlook.com") {
			return "outlook"
		}
	}
	return ""
}

func oauthProviderLabel(provider string) string {
	switch provider {
	case "gmail":
		return "Google"
	case "outlook":
		return "Microsoft"
	default:
		return "provider"
	}
}

func socketTypeToTLSMode(socketType string, port int) string {
	switch strings.ToUpper(strings.TrimSpace(socketType)) {
	case "SSL", "TLS":
		return "tls"
	case "STARTTLS":
		return "starttls"
	case "PLAIN", "NONE":
		return mailtransport.TLSModePlaintext
	}
	switch port {
	case 993, 995, 465:
		return "tls"
	case 143, 587:
		return "starttls"
	default:
		return "tls"
	}
}

func discoverSRV(ctx context.Context, resolver SRVResolver, parts emailParts) []Candidate {
	imaps := lookupService(ctx, resolver, "imaps", parts.Domain, "tls")
	imap := lookupService(ctx, resolver, "imap", parts.Domain, "starttls")
	submission := lookupService(ctx, resolver, "submission", parts.Domain, "starttls")
	incoming := append(imaps, imap...)
	if len(incoming) == 0 || len(submission) == 0 {
		return nil
	}
	sortEndpoints(incoming)
	sortEndpoints(submission)

	var out []Candidate
	for _, in := range incoming {
		for _, smtp := range submission {
			out = append(out, Candidate{
				Source:      SourceDNSSRV,
				Confidence:  72,
				IMAPHost:    in.Host,
				IMAPPort:    in.Port,
				IMAPTLSMode: in.TLSMode,
				SMTPHost:    smtp.Host,
				SMTPPort:    smtp.Port,
				SMTPTLSMode: smtp.TLSMode,
				Username:    parts.Address,
				AuthMethod:  "plain",
				Notes:       []string{"DNS SRV does not publish username format; using the full email address."},
			})
			if len(out) >= 8 {
				return out
			}
		}
	}
	return out
}

type endpoint struct {
	Host     string
	Port     int
	TLSMode  string
	Priority uint16
	Weight   uint16
}

func lookupService(ctx context.Context, resolver SRVResolver, service, domain, tlsMode string) []endpoint {
	_, records, err := resolver.LookupSRV(ctx, service, "tcp", domain)
	if err != nil || len(records) == 0 {
		return nil
	}
	out := make([]endpoint, 0, len(records))
	for _, record := range records {
		host := cleanHost(record.Target)
		if host == "" || record.Port == 0 {
			continue
		}
		out = append(out, endpoint{
			Host:     host,
			Port:     int(record.Port),
			TLSMode:  tlsMode,
			Priority: record.Priority,
			Weight:   record.Weight,
		})
	}
	return out
}

func heuristicCandidates(ctx context.Context, parts emailParts, probe bool, probeTimeout time.Duration) []Candidate {
	candidates := []Candidate{
		heuristicCandidate(parts, "imap."+parts.Domain, 993, "tls", "smtp."+parts.Domain, 587, "starttls", 52),
		heuristicCandidate(parts, "imap."+parts.Domain, 993, "tls", "smtp."+parts.Domain, 465, "tls", 50),
		heuristicCandidate(parts, "imap."+parts.Domain, 143, "starttls", "smtp."+parts.Domain, 587, "starttls", 47),
		heuristicCandidate(parts, "mail."+parts.Domain, 993, "tls", "smtp."+parts.Domain, 587, "starttls", 45),
		heuristicCandidate(parts, "mail."+parts.Domain, 993, "tls", "mail."+parts.Domain, 587, "starttls", 44),
		heuristicCandidate(parts, "mail."+parts.Domain, 143, "starttls", "smtp."+parts.Domain, 587, "starttls", 41),
		heuristicCandidate(parts, "mail."+parts.Domain, 143, "starttls", "mail."+parts.Domain, 587, "starttls", 40),
		heuristicCandidate(parts, parts.Domain, 993, "tls", "smtp."+parts.Domain, 587, "starttls", 37),
		heuristicCandidate(parts, parts.Domain, 143, "starttls", "smtp."+parts.Domain, 587, "starttls", 35),
		heuristicCandidate(parts, parts.Domain, 993, "tls", parts.Domain, 587, "starttls", 33),
		heuristicCandidate(parts, parts.Domain, 143, "starttls", parts.Domain, 587, "starttls", 31),
		heuristicCandidate(parts, "imap."+parts.Domain, 993, "tls", "smtp."+parts.Domain, 25, "starttls", 29),
	}
	if !probe {
		return candidates
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	type probeKey struct {
		protocol string
		host     string
		port     int
		tlsMode  string
	}
	type probeResult struct {
		key   probeKey
		probe protocolProbe
	}
	unique := make(map[probeKey]bool)
	for _, candidate := range candidates {
		unique[probeKey{protocol: "imap", host: candidate.IMAPHost, port: candidate.IMAPPort, tlsMode: candidate.IMAPTLSMode}] = true
		unique[probeKey{protocol: "smtp", host: candidate.SMTPHost, port: candidate.SMTPPort, tlsMode: candidate.SMTPTLSMode}] = true
	}

	results := make(chan probeResult, len(unique))
	var wg sync.WaitGroup
	for key := range unique {
		wg.Add(1)
		go func(key probeKey) {
			defer wg.Done()
			results <- probeResult{key: key, probe: probeMailEndpoint(ctx, key.protocol, key.host, key.port, key.tlsMode, probeTimeout)}
		}(key)
	}
	wg.Wait()
	close(results)

	probes := make(map[probeKey]protocolProbe, len(unique))
	for result := range results {
		probes[result.key] = result.probe
	}
	out := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		imapProbe := probes[probeKey{protocol: "imap", host: candidate.IMAPHost, port: candidate.IMAPPort, tlsMode: candidate.IMAPTLSMode}]
		smtpProbe := probes[probeKey{protocol: "smtp", host: candidate.SMTPHost, port: candidate.SMTPPort, tlsMode: candidate.SMTPTLSMode}]
		if imapProbe.ok && smtpProbe.ok {
			candidate.Notes = compactStrings(appendNotes(appendNotes(candidate.Notes, imapProbe.notes...), smtpProbe.notes...))
			out = append(out, candidate)
		}
	}
	return out
}

func heuristicCandidate(parts emailParts, imapHost string, imapPort int, imapTLS string, smtpHost string, smtpPort int, smtpTLS string, confidence int) Candidate {
	return Candidate{
		Source:      SourceHeuristic,
		Confidence:  confidence,
		IMAPHost:    imapHost,
		IMAPPort:    imapPort,
		IMAPTLSMode: imapTLS,
		SMTPHost:    smtpHost,
		SMTPPort:    smtpPort,
		SMTPTLSMode: smtpTLS,
		Username:    parts.Address,
		AuthMethod:  "plain",
		Notes:       []string{"Guessed from common mail hostnames; verify before saving."},
	}
}

type protocolProbe struct {
	ok    bool
	notes []string
}

func probeMailEndpoint(ctx context.Context, protocol, host string, port int, tlsMode string, timeout time.Duration) protocolProbe {
	secureTLSMode, err := mailtransport.RequireTLSMode(protocol, tlsMode)
	if err != nil {
		return protocolProbe{}
	}
	tlsMode = secureTLSMode
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	dialer := &net.Dialer{Timeout: timeout}
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	var conn net.Conn
	if tlsMode == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return protocolProbe{}
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = conn.SetDeadline(deadline)

	switch protocol {
	case "imap":
		return probeIMAP(conn, tlsMode == "starttls")
	case "smtp":
		return probeSMTP(conn, tlsMode == "starttls")
	default:
		return protocolProbe{}
	}
}

func probeIMAP(conn net.Conn, requireStartTLS bool) protocolProbe {
	reader := bufio.NewReader(conn)
	greeting, err := readProtocolLine(reader)
	if err != nil {
		return protocolProbe{}
	}
	if _, err := conn.Write([]byte("A001 CAPABILITY\r\n")); err != nil {
		return protocolProbe{}
	}
	capability, err := readIMAPTaggedResponse(reader, "A001", 12)
	if err != nil {
		return protocolProbe{}
	}
	upper := strings.ToUpper(greeting + "\n" + capability)
	if requireStartTLS && !strings.Contains(upper, "STARTTLS") {
		return protocolProbe{}
	}
	_, _ = conn.Write([]byte("A002 LOGOUT\r\n"))

	notes := []string{"IMAP capability probe succeeded."}
	if requireStartTLS {
		notes = append(notes, "IMAP server advertises STARTTLS.")
	}
	notes = append(notes, authNotesFromCapabilities("IMAP", upper)...)
	return protocolProbe{ok: true, notes: notes}
}

func probeSMTP(conn net.Conn, requireStartTLS bool) protocolProbe {
	reader := bufio.NewReader(conn)
	if _, err := readSMTPResponse(reader, 8); err != nil {
		return protocolProbe{}
	}
	if _, err := conn.Write([]byte("EHLO gofer.local\r\n")); err != nil {
		return protocolProbe{}
	}
	ehlo, err := readSMTPResponse(reader, 16)
	if err != nil {
		return protocolProbe{}
	}
	upper := strings.ToUpper(ehlo)
	if requireStartTLS && !strings.Contains(upper, "STARTTLS") {
		return protocolProbe{}
	}
	_, _ = conn.Write([]byte("QUIT\r\n"))

	notes := []string{"SMTP EHLO probe succeeded."}
	if requireStartTLS {
		notes = append(notes, "SMTP server advertises STARTTLS.")
	}
	notes = append(notes, authNotesFromCapabilities("SMTP", upper)...)
	return protocolProbe{ok: true, notes: notes}
}

func readProtocolLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(line) > 4096 {
		return "", fmt.Errorf("protocol line too long")
	}
	return line, nil
}

func readIMAPTaggedResponse(reader *bufio.Reader, tag string, maxLines int) (string, error) {
	var builder strings.Builder
	upperTag := strings.ToUpper(tag) + " "
	for i := 0; i < maxLines; i++ {
		line, err := readProtocolLine(reader)
		if err != nil {
			return "", err
		}
		builder.WriteString(line)
		if strings.HasPrefix(strings.ToUpper(line), upperTag) {
			return builder.String(), nil
		}
	}
	return "", fmt.Errorf("imap tagged response not found")
}

func readSMTPResponse(reader *bufio.Reader, maxLines int) (string, error) {
	var builder strings.Builder
	var code string
	for i := 0; i < maxLines; i++ {
		line, err := readProtocolLine(reader)
		if err != nil {
			return "", err
		}
		builder.WriteString(line)
		if len(line) < 4 {
			continue
		}
		if code == "" {
			code = line[:3]
		}
		if line[:3] == code && line[3] == ' ' {
			return builder.String(), nil
		}
	}
	return "", fmt.Errorf("smtp response not complete")
}

func authNotesFromCapabilities(protocol, upper string) []string {
	var notes []string
	if strings.Contains(upper, "OAUTHBEARER") || strings.Contains(upper, "XOAUTH2") || strings.Contains(upper, "AUTH=XOAUTH2") {
		notes = append(notes, protocol+" server advertises OAuth2.")
	}
	if strings.Contains(upper, "AUTH=PLAIN") || strings.Contains(upper, "AUTH PLAIN") || strings.Contains(upper, "AUTH=LOGIN") || strings.Contains(upper, "AUTH LOGIN") {
		notes = append(notes, protocol+" server advertises password auth.")
	}
	return notes
}

func endpointReachable(ctx context.Context, host string, port int, tlsMode string, timeout time.Duration) bool {
	secureTLSMode, err := mailtransport.RequireTLSMode("mail", tlsMode)
	if err != nil {
		return false
	}
	tlsMode = secureTLSMode
	if timeout <= 0 {
		timeout = 900 * time.Millisecond
	}
	dialer := &net.Dialer{Timeout: timeout}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if tlsMode == "tls" {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		})
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func expandPlaceholders(value string, parts emailParts) string {
	replacer := strings.NewReplacer(
		"%EMAILADDRESS%", parts.Address,
		"%EMAILLOCALPART%", parts.LocalPart,
		"%EMAILDOMAIN%", parts.Domain,
	)
	return replacer.Replace(strings.TrimSpace(value))
}

func cleanHost(host string) string {
	host = strings.TrimSpace(host)
	host = strings.TrimSuffix(host, ".")
	host = strings.ToLower(host)
	if host == "" {
		return ""
	}
	return host
}

func sortEndpoints(endpoints []endpoint) {
	sort.SliceStable(endpoints, func(i, j int) bool {
		if endpoints[i].Priority != endpoints[j].Priority {
			return endpoints[i].Priority < endpoints[j].Priority
		}
		if endpoints[i].Weight != endpoints[j].Weight {
			return endpoints[i].Weight > endpoints[j].Weight
		}
		return endpoints[i].Host < endpoints[j].Host
	})
}

func dedupeCandidates(candidates []Candidate, policy securityPolicy) []Candidate {
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Confidence > candidates[j].Confidence
	})
	seen := make(map[string]bool)
	out := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.IMAPHost == "" || candidate.SMTPHost == "" || candidate.Username == "" || candidate.AuthMethod == "" {
			continue
		}
		if _, err := mailtransport.RequireTLSModeWithPlaintext("IMAP", candidate.IMAPTLSMode, policy.allowsPlaintextTransport("imap", candidate.IMAPHost, candidate.IMAPPort)); err != nil {
			continue
		}
		if _, err := mailtransport.RequireTLSModeWithPlaintext("SMTP", candidate.SMTPTLSMode, policy.allowsPlaintextTransport("smtp", candidate.SMTPHost, candidate.SMTPPort)); err != nil {
			continue
		}
		key := strings.Join([]string{
			candidate.IMAPHost, strconv.Itoa(candidate.IMAPPort), candidate.IMAPTLSMode,
			candidate.SMTPHost, strconv.Itoa(candidate.SMTPPort), candidate.SMTPTLSMode,
			candidate.Username, candidate.SMTPUsername, candidate.AuthMethod,
		}, "|")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, candidate)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func appendNotes(base []string, values ...string) []string {
	out := append([]string{}, base...)
	out = append(out, values...)
	return out
}

func compactStrings(values []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
