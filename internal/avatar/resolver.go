package avatar

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/publicsuffix"
)

const (
	positiveTTL  = 7 * 24 * time.Hour
	negativeTTL  = 24 * time.Hour
	maxImageSize = 2 << 20
	maxHTMLSize  = 256 << 10
	maxRedirects = 5
)

var gravatarHashPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var svgEventAttrPattern = regexp.MustCompile(`(?i)[[:space:]]on[a-z]+[[:space:]]*=`)

type Image struct {
	Data        []byte
	ContentType string
	ExpiresAt   time.Time
	Source      string
	SourceURL   string
}

type Resolver struct {
	client       *http.Client
	lookupTXT    func(context.Context, string) ([]string, error)
	lookupIPAddr func(context.Context, string) ([]net.IPAddr, error)
	mu           sync.Mutex
	cache        map[string]cacheEntry
	inFlight     map[string]*inFlightLookup
}

type cacheEntry struct {
	image   Image
	found   bool
	expires time.Time
}

type inFlightLookup struct {
	done  chan struct{}
	image Image
	found bool
	err   error
}

func NewResolver() *Resolver {
	return &Resolver{
		client:       &http.Client{Timeout: 4 * time.Second},
		lookupTXT:    net.DefaultResolver.LookupTXT,
		lookupIPAddr: net.DefaultResolver.LookupIPAddr,
		cache:        make(map[string]cacheEntry),
		inFlight:     make(map[string]*inFlightLookup),
	}
}

func (r *Resolver) ClearCache() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.cache = make(map[string]cacheEntry)
	r.mu.Unlock()
}

func GravatarHash(email string) string {
	normalized := strings.ToLower(strings.TrimSpace(email))
	if normalized == "" || !strings.Contains(normalized, "@") {
		return ""
	}
	sum := md5.Sum([]byte(normalized))
	return hex.EncodeToString(sum[:])
}

func EmailDomain(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return ""
	}
	domain := strings.Trim(strings.TrimSpace(email[at+1:]), ".")
	if domain == "" || !strings.Contains(domain, ".") {
		return ""
	}
	return domain
}

func IsPublicMailboxDomain(domain string) bool {
	domain = strings.ToLower(strings.Trim(strings.TrimSpace(domain), "."))
	if domain == "" {
		return false
	}
	publicMailDomains := map[string]struct{}{
		"aol.com":        {},
		"duck.com":       {},
		"fastmail.com":   {},
		"gmail.com":      {},
		"gmx.com":        {},
		"googlemail.com": {},
		"hey.com":        {},
		"hotmail.com":    {},
		"icloud.com":     {},
		"live.com":       {},
		"mac.com":        {},
		"mail.com":       {},
		"me.com":         {},
		"msn.com":        {},
		"outlook.com":    {},
		"pm.me":          {},
		"proton.me":      {},
		"protonmail.com": {},
		"yahoo.com":      {},
		"yandex.com":     {},
		"zoho.com":       {},
	}
	_, ok := publicMailDomains[domain]
	return ok
}

func ParseBIMILogoURL(records []string) string {
	for _, record := range records {
		if !strings.Contains(strings.ToUpper(record), "BIMI1") {
			continue
		}
		parts := strings.Split(record, ";")
		valid := false
		logoURL := ""
		for _, part := range parts {
			key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok {
				continue
			}
			key = strings.ToLower(strings.TrimSpace(key))
			value = strings.TrimSpace(value)
			switch key {
			case "v":
				valid = strings.EqualFold(value, "BIMI1")
			case "l":
				logoURL = value
			}
		}
		if valid && logoURL != "" {
			return logoURL
		}
	}
	return ""
}

func IsGravatarHash(hash string) bool {
	return gravatarHashPattern.MatchString(strings.ToLower(strings.TrimSpace(hash)))
}

func (r *Resolver) ResolveGravatar(ctx context.Context, hash string) (Image, bool, error) {
	return r.resolveHashedAvatar(ctx, "gravatar", hash, r.fetchGravatar)
}

func (r *Resolver) ResolveLibravatar(ctx context.Context, hash string) (Image, bool, error) {
	return r.resolveHashedAvatar(ctx, "libravatar", hash, r.fetchLibravatar)
}

func (r *Resolver) resolveHashedAvatar(ctx context.Context, source, hash string, fetch func(context.Context, string) (Image, bool, error)) (Image, bool, error) {
	if r == nil {
		return Image{}, false, fmt.Errorf("avatar resolver is nil")
	}
	hash = strings.ToLower(strings.TrimSpace(hash))
	if !IsGravatarHash(hash) {
		return Image{}, false, nil
	}

	cacheKey := source + ":" + hash
	now := time.Now()
	r.mu.Lock()
	if entry, ok := r.cache[cacheKey]; ok && now.Before(entry.expires) {
		r.mu.Unlock()
		return entry.image, entry.found, nil
	}
	r.mu.Unlock()

	image, found, err := fetch(ctx, hash)
	if err != nil {
		return Image{}, false, err
	}

	expires := now.Add(negativeTTL)
	if found {
		expires = now.Add(positiveTTL)
		image.ExpiresAt = expires
		image.Source = source
	}

	r.mu.Lock()
	r.cache[cacheKey] = cacheEntry{image: image, found: found, expires: expires}
	r.mu.Unlock()

	return image, found, nil
}

func (r *Resolver) ResolveBIMI(ctx context.Context, email string) (Image, bool, error) {
	if r == nil {
		return Image{}, false, fmt.Errorf("avatar resolver is nil")
	}
	domain := EmailDomain(email)
	if domain == "" || IsPublicMailboxDomain(domain) {
		return Image{}, false, nil
	}

	cacheKey := "bimi:" + domain
	now := time.Now()
	r.mu.Lock()
	if entry, ok := r.cache[cacheKey]; ok && now.Before(entry.expires) {
		r.mu.Unlock()
		return entry.image, entry.found, nil
	}
	r.mu.Unlock()

	image, found, err := r.fetchBIMI(ctx, domain)
	if err != nil {
		return Image{}, false, err
	}

	expires := now.Add(negativeTTL)
	if found {
		expires = now.Add(positiveTTL)
		image.ExpiresAt = expires
		image.Source = "bimi"
	}

	r.mu.Lock()
	r.cache[cacheKey] = cacheEntry{image: image, found: found, expires: expires}
	r.mu.Unlock()

	return image, found, nil
}

func (r *Resolver) ResolveDomainIcon(ctx context.Context, email string) (Image, bool, error) {
	if r == nil {
		return Image{}, false, fmt.Errorf("avatar resolver is nil")
	}
	domain := EmailDomain(email)
	if domain == "" || IsPublicMailboxDomain(domain) {
		return Image{}, false, nil
	}

	cacheKey := "domain_icon:" + domain
	now := time.Now()
	r.mu.Lock()
	if entry, ok := r.cache[cacheKey]; ok && now.Before(entry.expires) {
		r.mu.Unlock()
		return entry.image, entry.found, nil
	}
	r.mu.Unlock()

	image, found, err := r.fetchDomainIconDeduped(ctx, cacheKey, domain)
	if err != nil {
		return Image{}, false, err
	}

	expires := now.Add(negativeTTL)
	if found {
		expires = now.Add(positiveTTL)
		image.ExpiresAt = expires
		image.Source = "domain_icon"
	}

	r.mu.Lock()
	r.cache[cacheKey] = cacheEntry{image: image, found: found, expires: expires}
	r.mu.Unlock()

	return image, found, nil
}

func (r *Resolver) fetchDomainIconDeduped(ctx context.Context, cacheKey, domain string) (Image, bool, error) {
	r.mu.Lock()
	if call := r.inFlight[cacheKey]; call != nil {
		done := call.done
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return Image{}, false, ctx.Err()
		case <-done:
			return call.image, call.found, call.err
		}
	}
	call := &inFlightLookup{done: make(chan struct{})}
	r.inFlight[cacheKey] = call
	r.mu.Unlock()

	call.image, call.found, call.err = r.fetchDomainIcon(ctx, domain)
	r.mu.Lock()
	delete(r.inFlight, cacheKey)
	close(call.done)
	r.mu.Unlock()
	return call.image, call.found, call.err
}

func (r *Resolver) fetchGravatar(ctx context.Context, hash string) (Image, bool, error) {
	url := fmt.Sprintf("https://www.gravatar.com/avatar/%s?s=96&d=404&r=pg", hash)
	return r.fetchHashedAvatar(ctx, "gravatar", url)
}

func (r *Resolver) fetchLibravatar(ctx context.Context, hash string) (Image, bool, error) {
	url := fmt.Sprintf("https://seccdn.libravatar.org/avatar/%s?s=96&d=404", hash)
	return r.fetchHashedAvatar(ctx, "libravatar", url)
}

func (r *Resolver) fetchHashedAvatar(ctx context.Context, source, rawURL string) (Image, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Image{}, false, err
	}
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,image/gif;q=0.8,*/*;q=0.5")
	req.Header.Set("User-Agent", "GoferMail/1.0")

	resp, err := r.client.Do(req)
	if err != nil {
		return Image{}, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return Image{}, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Image{}, false, fmt.Errorf("%s returned %d", source, resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		return Image{}, false, fmt.Errorf("%s returned non-image content type %q", source, contentType)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageSize+1))
	if err != nil {
		return Image{}, false, err
	}
	if len(data) > maxImageSize {
		return Image{}, false, fmt.Errorf("%s image exceeds %d bytes", source, maxImageSize)
	}

	return Image{Data: data, ContentType: contentType, Source: source, SourceURL: rawURL}, true, nil
}

func (r *Resolver) fetchBIMI(ctx context.Context, domain string) (Image, bool, error) {
	records, err := r.lookupTXT(ctx, "default._bimi."+domain)
	if err != nil {
		if dnsErr, ok := err.(*net.DNSError); ok && dnsErr.IsNotFound {
			return Image{}, false, nil
		}
		return Image{}, false, err
	}
	logoURL := ParseBIMILogoURL(records)
	if logoURL == "" {
		return Image{}, false, nil
	}
	return r.fetchBIMILogo(ctx, logoURL)
}

func (r *Resolver) fetchBIMILogo(ctx context.Context, rawURL string) (Image, bool, error) {
	if err := r.validateRemoteAvatarURL(ctx, rawURL); err != nil {
		return Image{}, false, err
	}
	client := *r.client
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return http.ErrUseLastResponse
		}
		return r.validateRemoteAvatarURL(req.Context(), req.URL.String())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Image{}, false, err
	}
	req.Header.Set("Accept", "image/svg+xml")
	req.Header.Set("User-Agent", "GoferMail/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return Image{}, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return Image{}, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Image{}, false, fmt.Errorf("bimi logo returned %d", resp.StatusCode)
	}

	contentType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if contentType != "image/svg+xml" && contentType != "application/svg+xml" {
		return Image{}, false, fmt.Errorf("bimi logo returned unsupported content type %q", contentType)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageSize+1))
	if err != nil {
		return Image{}, false, err
	}
	if len(data) > maxImageSize {
		return Image{}, false, fmt.Errorf("bimi logo exceeds %d bytes", maxImageSize)
	}
	if !isSafeSVG(data) {
		return Image{}, false, fmt.Errorf("bimi logo is not safe SVG")
	}

	return Image{Data: data, ContentType: "image/svg+xml", Source: "bimi", SourceURL: rawURL}, true, nil
}

func (r *Resolver) fetchDomainIcon(ctx context.Context, domain string) (Image, bool, error) {
	image, found, err := r.fetchDomainIconForDomain(ctx, domain)
	if err == nil || found || !isRemoteConnectionError(err) {
		return image, found, err
	}

	root, rootErr := registrableDomain(domain)
	if rootErr != nil || root == "" || root == domain {
		return image, found, err
	}
	return r.fetchDomainIconForDomain(ctx, root)
}

func (r *Resolver) fetchDomainIconForDomain(ctx context.Context, domain string) (Image, bool, error) {
	candidates, discoverErr := r.discoverDomainIconURLs(ctx, domain)
	candidates = append(candidates, domainIconFallbackURLs(domain)...)

	var lastErr error
	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		if candidate = strings.TrimSpace(candidate); candidate == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		image, found, err := r.fetchRemoteAvatarImage(ctx, "domain_icon", candidate)
		if err == nil && found {
			return image, true, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return Image{}, false, lastErr
	}
	if discoverErr != nil {
		return Image{}, false, discoverErr
	}
	return Image{}, false, nil
}

func domainIconFallbackURLs(domain string) []string {
	base := "https://" + domain
	return []string{
		base + "/favicon.ico",
		base + "/favicon.svg",
		base + "/favicon.png",
		base + "/apple-touch-icon.png",
		base + "/apple-touch-icon-precomposed.png",
		base + "/favicon-32x32.png",
		base + "/favicon-16x16.png",
	}
}

func (r *Resolver) discoverDomainIconURLs(ctx context.Context, domain string) ([]string, error) {
	rawURL := "https://" + domain + "/"
	if err := r.validateRemoteAvatarURL(ctx, rawURL); err != nil {
		return nil, err
	}
	client := *r.client
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return http.ErrUseLastResponse
		}
		return r.validateRemoteAvatarURL(req.Context(), req.URL.String())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.2")
	req.Header.Set("User-Agent", "GoferMail/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("domain homepage returned %d", resp.StatusCode)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if contentType != "" && contentType != "text/html" && contentType != "application/xhtml+xml" {
		return nil, nil
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHTMLSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxHTMLSize {
		return nil, fmt.Errorf("domain homepage exceeds %d bytes", maxHTMLSize)
	}
	baseURL := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		baseURL = resp.Request.URL.String()
	}
	return parseIconLinks(baseURL, data), nil
}

func (r *Resolver) fetchRemoteAvatarImage(ctx context.Context, source, rawURL string) (Image, bool, error) {
	if err := r.validateRemoteAvatarURL(ctx, rawURL); err != nil {
		return Image{}, false, err
	}
	client := *r.client
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return http.ErrUseLastResponse
		}
		return r.validateRemoteAvatarURL(req.Context(), req.URL.String())
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return Image{}, false, err
	}
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,image/gif,image/svg+xml,image/x-icon,*/*;q=0.2")
	req.Header.Set("User-Agent", "GoferMail/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return Image{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Image{}, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Image{}, false, fmt.Errorf("%s returned %d", source, resp.StatusCode)
	}

	contentType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if !strings.HasPrefix(contentType, "image/") {
		return Image{}, false, fmt.Errorf("%s returned non-image content type %q", source, contentType)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageSize+1))
	if err != nil {
		return Image{}, false, err
	}
	if len(data) > maxImageSize {
		return Image{}, false, fmt.Errorf("%s image exceeds %d bytes", source, maxImageSize)
	}
	if (contentType == "image/svg+xml" || contentType == "application/svg+xml") && !isSafeSVG(data) {
		return Image{}, false, fmt.Errorf("%s image is not safe SVG", source)
	}
	sourceURL := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		sourceURL = resp.Request.URL.String()
	}
	return Image{Data: data, ContentType: contentType, Source: source, SourceURL: sourceURL}, true, nil
}

func registrableDomain(domain string) (string, error) {
	domain = strings.ToLower(strings.Trim(strings.TrimSpace(domain), "."))
	if domain == "" {
		return "", nil
	}
	return publicsuffix.EffectiveTLDPlusOne(domain)
}

func isRemoteConnectionError(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

func (r *Resolver) validateRemoteAvatarURL(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if u.Scheme != "https" {
		return fmt.Errorf("avatar url must use https")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("avatar url missing host")
	}
	ips, err := r.lookupIPAddr(ctx, u.Hostname())
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return fmt.Errorf("avatar url host has no addresses")
	}
	for _, ip := range ips {
		if isPrivateIP(ip.IP) {
			return fmt.Errorf("avatar url resolves to private address")
		}
	}
	return nil
}

func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	return false
}

func parseIconLinks(baseURL string, data []byte) []string {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil
	}
	tokenizer := html.NewTokenizer(strings.NewReader(string(data)))
	icons := []string{}
	seen := map[string]struct{}{}
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return icons
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			if !strings.EqualFold(token.Data, "link") {
				continue
			}
			rel := ""
			href := ""
			for _, attr := range token.Attr {
				switch strings.ToLower(attr.Key) {
				case "rel":
					rel = attr.Val
				case "href":
					href = attr.Val
				}
			}
			if !linkRelHasIcon(rel) || strings.TrimSpace(href) == "" {
				continue
			}
			parsed, err := url.Parse(strings.TrimSpace(href))
			if err != nil {
				continue
			}
			resolved := base.ResolveReference(parsed)
			if resolved.Scheme != "https" || resolved.Hostname() == "" {
				continue
			}
			raw := resolved.String()
			if _, ok := seen[raw]; ok {
				continue
			}
			icons = append(icons, raw)
			seen[raw] = struct{}{}
			if len(icons) >= 5 {
				return icons
			}
		}
	}
}

func linkRelHasIcon(rel string) bool {
	for _, part := range strings.Fields(strings.ToLower(rel)) {
		if part == "icon" || part == "shortcut" || part == "apple-touch-icon" || part == "mask-icon" {
			return true
		}
	}
	return false
}

func looksLikeSVG(data []byte) bool {
	s := strings.TrimPrefix(strings.TrimSpace(string(data)), "\ufeff")
	for {
		lower := strings.ToLower(strings.TrimSpace(s))
		switch {
		case strings.HasPrefix(lower, "<?xml") || strings.HasPrefix(lower, "<?"):
			idx := strings.Index(lower, "?>")
			if idx < 0 {
				return false
			}
			s = strings.TrimSpace(s[idx+2:])
		case strings.HasPrefix(lower, "<!--"):
			idx := strings.Index(lower, "-->")
			if idx < 0 {
				return false
			}
			s = strings.TrimSpace(s[idx+3:])
		case strings.HasPrefix(lower, "<!doctype"):
			idx := strings.Index(lower, ">")
			if idx < 0 {
				return false
			}
			s = strings.TrimSpace(s[idx+1:])
		default:
			return strings.HasPrefix(lower, "<svg")
		}
	}
}

func isSafeSVG(data []byte) bool {
	if !looksLikeSVG(data) {
		return false
	}
	lower := strings.ToLower(string(data))
	blocked := []string{
		"<script",
		"<foreignobject",
		"<iframe",
		"<object",
		"<embed",
		"<image",
		"javascript:",
		"data:text/html",
		"data:application/xhtml",
		"url(http:",
		"url(https:",
	}
	for _, token := range blocked {
		if strings.Contains(lower, token) {
			return false
		}
	}
	return !svgEventAttrPattern.Match(data)
}
