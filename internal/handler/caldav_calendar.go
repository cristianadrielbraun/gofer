package handler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/storage"
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
	if err != nil || parsed.Host == "" || parsed.Scheme != "https" {
		return "", fmt.Errorf("enter a valid https CalDAV server URL to protect the mailbox credentials")
	}
	if parsed.User != nil || parsed.RawQuery != "" {
		return "", fmt.Errorf("do not include credentials or query parameters in the CalDAV server URL")
	}
	parsed.Fragment = ""
	return parsed.String(), nil
}

func calDAVAutodiscoveryCandidates(ctx context.Context, preferredURL string, identities ...string) []string {
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
	addCandidate(preferredURL)

	domains := make([]string, 0, len(identities)*2)
	seenDomains := make(map[string]bool)
	for _, identity := range identities {
		domain := calDAVDiscoveryDomain(identity)
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

func discoverCalDAVCalendarsCandidates(ctx context.Context, candidates []string, username, password, userID, accountID string) ([]storage.CalendarSource, string, error) {
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
		calendars, err := discoverCalDAVCalendars(ctx, normalized, username, password, userID, accountID)
		if err == nil {
			return calendars, normalized, nil
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

func discoverCalDAVCalendars(ctx context.Context, rawBaseURL, username, password, userID, accountID string) ([]storage.CalendarSource, error) {
	baseURL, err := normalizeCalDAVBaseURL(rawBaseURL)
	if err != nil {
		return nil, err
	}

	endpoints := []string{calDAVWellKnownURL(baseURL), baseURL}
	var lastErr error
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
		if home == "" {
			// Some servers use the configured endpoint itself as the calendar home.
			home = endpoint
		}
		calendars, listErr := listCalDAVCalendars(ctx, home, username, password, userID, accountID)
		if listErr == nil && len(calendars) > 0 {
			return calendars, nil
		}
		if listErr != nil {
			lastErr = listErr
		} else {
			lastErr = fmt.Errorf("no calendars were returned from %s", home)
		}
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
	if !strings.EqualFold(base.Scheme, remote.Scheme) || !strings.EqualFold(base.Host, remote.Host) {
		return "", fmt.Errorf("CalDAV server returned a resource on a different origin")
	}
	return remote.String(), nil
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
		Timeout: cardDAVDiscoveryHTTPTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if err := calendarDiscoveryDAVGuard(ctx); err != nil {
				return err
			}
			if len(via) >= 5 {
				return fmt.Errorf("too many CalDAV redirects")
			}
			if len(via) > 0 && (!strings.EqualFold(via[0].URL.Scheme, req.URL.Scheme) || !strings.EqualFold(via[0].URL.Host, req.URL.Host)) {
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
