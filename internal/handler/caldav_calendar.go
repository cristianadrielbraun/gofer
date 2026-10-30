package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
	"golang.org/x/net/publicsuffix"
)

func normalizeCalDAVBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("CalDAV server URL is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || !calDAVURLScheme(parsed.Scheme) {
		return "", fmt.Errorf("enter a valid CalDAV server URL starting with https://, or http:// for a server an administrator approved")
	}
	if parsed.User != nil || parsed.RawQuery != "" {
		return "", fmt.Errorf("do not include credentials or query parameters in the CalDAV server URL")
	}
	parsed.Fragment = ""
	return parsed.String(), nil
}

// calDAVAutodiscoveryCandidates lists the addresses to try: the given server
// addresses first, in order, then guesses from the identities' domains.
func calDAVAutodiscoveryCandidates(ctx context.Context, serverURLs []string, identities ...string) []string {
	candidates := make([]string, 0, 16)
	seenCandidates := make(map[string]bool)
	addCandidate := func(raw string) {
		normalized, err := normalizeCalDAVBaseURL(raw)
		if err != nil || seenCandidates[strings.ToLower(normalized)] {
			return
		}
		seenCandidates[strings.ToLower(normalized)] = true
		candidates = append(candidates, normalized)
	}
	for _, server := range serverURLs {
		if strings.TrimSpace(server) != "" {
			addCandidate(server)
		}
	}

	domains := make([]string, 0, len(identities)*2)
	seenDomains := make(map[string]bool)
	for _, identity := range identities {
		domain := calDAVDiscoveryDomain(identity)
		if ip, err := netip.ParseAddr(domain); err == nil {
			// Literal addresses are direct candidates, never DNS names. Keep
			// an explicit URL's scheme, port and path when one was supplied.
			if _, err := netip.ParseAddr(strings.Trim(strings.TrimSpace(identity), "[]")); err != nil && !strings.Contains(identity, "@") {
				addCandidate(identity)
			} else {
				host := ip.String()
				if ip.Is6() {
					host = "[" + host + "]"
				}
				addCandidate((&url.URL{Scheme: "https", Host: host}).String())
			}
			continue
		}
		for _, discoveryDomain := range calDAVDiscoveryDomains(domain) {
			key := strings.ToLower(discoveryDomain)
			if discoveryDomain == "" || seenDomains[key] {
				continue
			}
			seenDomains[key] = true
			domains = append(domains, discoveryDomain)
		}
	}

	for _, domain := range domains {
		select {
		case <-ctx.Done():
			return candidates
		default:
		}
		for _, candidate := range calDAVSRVCandidates(ctx, domain) {
			addCandidate(candidate)
		}
		for _, prefix := range []string{"cdav.", "caldav.", "dav.", "calendar."} {
			addCandidate("https://" + prefix + domain)
		}
		addCandidate("https://" + domain)
		if len(candidates) >= 20 {
			return candidates
		}
	}
	return candidates
}

func calDAVSRVCandidates(ctx context.Context, domain string) []string {
	lookupCtx, cancel := context.WithTimeout(ctx, cardDAVDiscoveryDNSTimeout)
	defer cancel()
	_, records, err := net.DefaultResolver.LookupSRV(lookupCtx, "caldavs", "tcp", domain)
	if err != nil {
		return nil
	}
	candidates := make([]string, 0, len(records))
	for _, record := range records {
		host := strings.Trim(strings.TrimSpace(record.Target), ".")
		if host == "" {
			continue
		}
		candidate := "https://" + host
		if record.Port != 443 {
			candidate = "https://" + net.JoinHostPort(host, strconv.Itoa(int(record.Port)))
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func calDAVDiscoveryDomain(candidate string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return ""
	}
	if ip, err := netip.ParseAddr(strings.Trim(candidate, "[]")); err == nil {
		return ip.String()
	}
	if strings.Contains(candidate, "@") && !strings.Contains(candidate, "://") {
		parts := strings.Split(candidate, "@")
		return strings.ToLower(strings.Trim(strings.TrimSpace(parts[len(parts)-1]), "."))
	}
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.Trim(strings.TrimSpace(parsed.Hostname()), "."))
}

func calDAVDiscoveryDomains(domain string) []string {
	domain = strings.ToLower(strings.Trim(strings.TrimSpace(domain), "."))
	if domain == "" {
		return nil
	}
	if _, err := netip.ParseAddr(domain); err == nil {
		return nil
	}
	domains := []string{domain}
	parts := strings.Split(domain, ".")
	if len(parts) > 2 {
		parent := strings.Join(parts[len(parts)-2:], ".")
		if parent != domain {
			domains = append(domains, parent)
		}
	}
	return domains
}

func discoverCalDAVCalendarsCandidates(ctx context.Context, candidates []string, username, password, userID, accountID string, create *davCreate) ([]storage.CalendarSource, string, error) {
	var lastErr error
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			lastErr = err
			break
		}
		normalized, err := normalizeCalDAVBaseURL(candidate)
		if err != nil {
			lastErr = err
			continue
		}
		calendars, err := discoverCalDAVCalendars(ctx, normalized, username, password, userID, accountID, create)
		if err == nil {
			return calendars, normalized, nil
		}
		if create != nil && create.attempted {
			return nil, "", err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no CalDAV server candidates could be derived from this account")
	}
	return nil, "", fmt.Errorf("URL autodiscover could not find a CalDAV server: %w", lastErr)
}

func calDAVWellKnownURL(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return baseURL
	}
	parsed.Path = "/.well-known/caldav"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

// discoverCalDAVCalendars lists the calendars of a CalDAV server, first
// making the one create asks for, when it isn't nil. A server that reports a
// calendar home with nothing in it has no calendars yet, which isn't an
// error, so one can be created there.
func discoverCalDAVCalendars(ctx context.Context, rawBaseURL, username, password, userID, accountID string, create *davCreate) ([]storage.CalendarSource, error) {
	baseURL, err := normalizeCalDAVBaseURL(rawBaseURL)
	if err != nil {
		return nil, err
	}

	endpoints := []string{calDAVWellKnownURL(baseURL), baseURL}
	var lastErr error
	emptyHome := false
	for _, endpoint := range endpoints {
		principalURL, home, requestErr := calDAVDiscoveryProperties(ctx, endpoint, username, password)
		if requestErr != nil {
			lastErr = requestErr
			continue
		}
		if home == "" && principalURL != "" {
			principalURL, requestErr = resolveCalDAVHref(endpoint, principalURL)
			if requestErr != nil {
				lastErr = requestErr
				continue
			}
			home, requestErr = calDAVCalendarHome(ctx, principalURL, username, password)
			if requestErr != nil {
				lastErr = requestErr
				continue
			}
			if home != "" {
				home, requestErr = resolveCalDAVHref(principalURL, home)
				if requestErr != nil {
					lastErr = requestErr
					continue
				}
			}
		} else if home != "" {
			home, requestErr = resolveCalDAVHref(endpoint, home)
			if requestErr != nil {
				lastErr = requestErr
				continue
			}
		}
		reported := home != ""
		if !reported {
			// Some servers use the configured endpoint itself as the calendar home.
			home = endpoint
		}
		if create != nil {
			// Only a home the server named is known to take new calendars.
			if !reported {
				lastErr = fmt.Errorf("%s did not say where new calendars go", endpoint)
				continue
			}
			create.attempted = true
			created, createErr := createCalDAVCalendar(ctx, home, username, password, create.Name)
			if createErr != nil {
				return nil, createErr
			}
			create.Created = created
			calendars, listErr := listCalDAVCalendars(ctx, home, username, password, userID, accountID)
			if listErr != nil {
				return nil, listErr
			}
			for i := range calendars {
				if davSameCollection(calendars[i].RemoteID, created) {
					calendars[i].IsSelected = true
				}
			}
			return calendars, nil
		}
		calendars, listErr := listCalDAVCalendars(ctx, home, username, password, userID, accountID)
		if listErr == nil && len(calendars) > 0 {
			return calendars, nil
		}
		if listErr != nil {
			lastErr = listErr
		} else {
			// A home the server named counts as an empty server; the endpoint
			// itself, listed only as a fallback, could be any WebDAV folder.
			emptyHome = emptyHome || reported
			lastErr = fmt.Errorf("no calendars were returned from %s", home)
		}
	}
	if emptyHome {
		return []storage.CalendarSource{}, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("could not discover CalDAV calendars: %w", lastErr)
	}
	return nil, fmt.Errorf("the CalDAV server did not return a calendar home or any calendars")
}

func calDAVDiscoveryProperties(ctx context.Context, endpoint, username, password string) (string, string, error) {
	body := `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:current-user-principal/><c:calendar-home-set/></d:prop></d:propfind>`
	multi, err := calDAVPropfind(ctx, endpoint, username, password, "0", body)
	if err != nil {
		return "", "", err
	}
	for _, response := range multi.Responses {
		prop := response.okProp()
		return strings.TrimSpace(prop.CurrentUserPrincipal.Href), strings.TrimSpace(prop.CalendarHomeSet.Href), nil
	}
	return "", "", nil
}

func calDAVCalendarHome(ctx context.Context, principalURL, username, password string) (string, error) {
	body := `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><c:calendar-home-set/></d:prop></d:propfind>`
	multi, err := calDAVPropfind(ctx, principalURL, username, password, "0", body)
	if err != nil {
		return "", err
	}
	for _, response := range multi.Responses {
		if home := strings.TrimSpace(response.okProp().CalendarHomeSet.Href); home != "" {
			return home, nil
		}
	}
	return "", nil
}

func listCalDAVCalendars(ctx context.Context, homeURL, username, password, userID, accountID string) ([]storage.CalendarSource, error) {
	body := `<?xml version="1.0" encoding="utf-8"?><d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav" xmlns:a="http://apple.com/ns/ical/"><d:prop><d:displayname/><d:resourcetype/><d:current-user-privilege-set/><c:calendar-description/><c:calendar-timezone/><a:calendar-color/></d:prop></d:propfind>`
	multi, err := calDAVPropfind(ctx, homeURL, username, password, "1", body)
	if err != nil {
		return nil, err
	}
	calendars := make([]storage.CalendarSource, 0, len(multi.Responses))
	seen := make(map[string]bool)
	for _, response := range multi.Responses {
		prop := response.okProp()
		if !prop.ResourceType.Calendar {
			continue
		}
		if strings.TrimSpace(response.Href) == "" {
			continue
		}
		remoteID, err := resolveCalDAVHref(homeURL, response.Href)
		if err != nil {
			return nil, err
		}
		if remoteID == "" || seen[remoteID] {
			continue
		}
		seen[remoteID] = true
		name := strings.TrimSpace(prop.DisplayName)
		if name == "" {
			name = calDAVCalendarName(remoteID)
		}
		isPrimary := len(calendars) == 0
		color := strings.TrimSpace(prop.CalendarColor)
		if len(color) == 9 {
			color = color[:7]
		}
		if !isCalendarHexColor(color) {
			color = ""
		}
		accessRole := "unknown"
		if prop.CurrentUserPrivileges != nil {
			accessRole = "reader"
			for _, privilege := range prop.CurrentUserPrivileges.Privileges {
				if privilege.All != nil || privilege.Write != nil || privilege.Bind != nil {
					accessRole = "writer"
					break
				}
			}
		}
		calendars = append(calendars, storage.CalendarSource{
			UserID:      userID,
			AccountID:   accountID,
			Provider:    storage.CalendarSourceProviderCalDAV,
			RemoteID:    remoteID,
			Name:        name,
			Description: strings.TrimSpace(prop.CalendarDescription),
			TimeZone:    calDAVCalendarTimeZone(prop.CalendarTimeZone),
			Color:       color,
			AccessRole:  accessRole,
			IsPrimary:   isPrimary,
			IsSelected:  isPrimary,
		})
	}
	return calendars, nil
}

func calDAVCalendarName(calendarURL string) string {
	parsed, err := url.Parse(calendarURL)
	if err != nil {
		return "Calendar"
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) == 0 || strings.TrimSpace(parts[len(parts)-1]) == "" {
		return "Calendar"
	}
	name, err := url.PathUnescape(parts[len(parts)-1])
	if err != nil || strings.TrimSpace(name) == "" {
		return "Calendar"
	}
	return name
}

func resolveCalDAVHref(baseURL, href string) (string, error) {
	resolved := absoluteDAVHref(baseURL, href)
	base, baseErr := url.Parse(baseURL)
	remote, remoteErr := url.Parse(resolved)
	if baseErr != nil || remoteErr != nil || remote.Host == "" || remote.Scheme == "" {
		return "", fmt.Errorf("CalDAV server returned an invalid resource URL")
	}
	if !sameCalDAVOrigin(base, remote) {
		return "", fmt.Errorf("CalDAV server returned a resource on a different origin")
	}
	return remote.String(), nil
}

// sameCalDAVOrigin reports whether remote is on the same CalDAV service as
// base, so it may be sent the same login. That is the same scheme, host and
// port, or, over HTTPS, another host of the same registered domain: some
// providers keep each person's calendars on a server of their own, as iCloud
// does with caldav.icloud.com and p63-caldav.icloud.com. The public suffix
// list keeps shared domains such as github.io from counting as one site.
func sameCalDAVOrigin(base, remote *url.URL) bool {
	if base == nil || remote == nil || !strings.EqualFold(base.Scheme, remote.Scheme) || davURLPort(base) != davURLPort(remote) {
		return false
	}
	baseHost := strings.TrimSuffix(strings.ToLower(base.Hostname()), ".")
	remoteHost := strings.TrimSuffix(strings.ToLower(remote.Hostname()), ".")
	if baseHost == remoteHost {
		return true
	}
	if !strings.EqualFold(base.Scheme, "https") {
		return false
	}
	baseSite, baseOK := davRegisteredDomain(baseHost)
	remoteSite, remoteOK := davRegisteredDomain(remoteHost)
	return baseOK && remoteOK && baseSite == remoteSite
}

// davRegisteredDomain is the domain a host is registered under, such as
// icloud.com for p63-caldav.icloud.com. IP addresses and names without a
// registered domain have none, so they only match themselves.
func davRegisteredDomain(host string) (string, bool) {
	if host == "" || net.ParseIP(host) != nil {
		return "", false
	}
	site, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil || site == "" {
		return "", false
	}
	return site, true
}

// davURLPort is a URL's port, with the scheme's default written out: iCloud
// writes :443 into the URLs it returns.
func davURLPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

func calDAVPropfind(ctx context.Context, endpoint, username, password, depth, body string) (davMultiStatus, error) {
	if err := calendarDiscoveryDAVGuard(ctx); err != nil {
		return davMultiStatus{}, err
	}
	req, err := newCardDAVRequest(ctx, "PROPFIND", endpoint, username, password, strings.NewReader(body))
	if err != nil {
		return davMultiStatus{}, err
	}
	req.Header.Set("Depth", depth)
	req.Header.Set("Content-Type", `application/xml; charset="utf-8"`)
	client := &http.Client{
		Transport: calDAVHTTPTransport,
		Timeout:   cardDAVDiscoveryHTTPTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if err := calendarDiscoveryDAVGuard(ctx); err != nil {
				return err
			}
			if len(via) >= 5 {
				return fmt.Errorf("too many CalDAV redirects")
			}
			if len(via) > 0 && !sameCalDAVOrigin(via[0].URL, req.URL) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return davMultiStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return davMultiStatus{}, calendarDiscoveryDAVFailure(ctx, calDAVDiscoveryError{status: resp.StatusCode,
			body: sanitizeProviderErrorBody(strings.TrimSpace(string(data))), retryAt: providerRetryAfter(resp)})
	}
	const bodyLimit = 32 << 20
	wire, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit+1))
	if err != nil {
		return davMultiStatus{}, err
	}
	if len(wire) > bodyLimit {
		return davMultiStatus{}, fmt.Errorf("CalDAV discovery response exceeds 32 MiB")
	}
	multi, err := decodeDAVMultiStatus(bytes.NewReader(wire))
	if err != nil {
		return davMultiStatus{}, fmt.Errorf("decode CalDAV response: %w", err)
	}
	return multi, nil
}

type calDAVDiscoveryError struct {
	status  int
	body    string
	retryAt time.Time
}

func (e calDAVDiscoveryError) Error() string {
	return fmt.Sprintf("CalDAV discovery returned %d: %s", e.status, e.body)
}
func (e calDAVDiscoveryError) RetryAfter() (time.Time, bool) { return e.retryAt, !e.retryAt.IsZero() }
