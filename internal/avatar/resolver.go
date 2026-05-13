package avatar

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	positiveTTL  = 7 * 24 * time.Hour
	negativeTTL  = 24 * time.Hour
	maxImageSize = 2 << 20
)

var gravatarHashPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type Image struct {
	Data        []byte
	ContentType string
	ExpiresAt   time.Time
	Source      string
	SourceURL   string
}

type Resolver struct {
	client *http.Client
	mu     sync.Mutex
	cache  map[string]cacheEntry
}

type cacheEntry struct {
	image   Image
	found   bool
	expires time.Time
}

func NewResolver() *Resolver {
	return &Resolver{
		client: &http.Client{Timeout: 4 * time.Second},
		cache:  make(map[string]cacheEntry),
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
	if r == nil {
		return Image{}, false, fmt.Errorf("avatar resolver is nil")
	}
	hash = strings.ToLower(strings.TrimSpace(hash))
	if !IsGravatarHash(hash) {
		return Image{}, false, nil
	}

	now := time.Now()
	r.mu.Lock()
	if entry, ok := r.cache[hash]; ok && now.Before(entry.expires) {
		r.mu.Unlock()
		return entry.image, entry.found, nil
	}
	r.mu.Unlock()

	image, found, err := r.fetchGravatar(ctx, hash)
	if err != nil {
		return Image{}, false, err
	}

	expires := now.Add(negativeTTL)
	if found {
		expires = now.Add(positiveTTL)
		image.ExpiresAt = expires
		image.Source = "gravatar"
	}

	r.mu.Lock()
	r.cache[hash] = cacheEntry{image: image, found: found, expires: expires}
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

func (r *Resolver) fetchGravatar(ctx context.Context, hash string) (Image, bool, error) {
	url := fmt.Sprintf("https://www.gravatar.com/avatar/%s?s=96&d=404&r=pg", hash)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
		return Image{}, false, fmt.Errorf("gravatar returned %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
		return Image{}, false, fmt.Errorf("gravatar returned non-image content type %q", contentType)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageSize+1))
	if err != nil {
		return Image{}, false, err
	}
	if len(data) > maxImageSize {
		return Image{}, false, fmt.Errorf("gravatar image exceeds %d bytes", maxImageSize)
	}

	return Image{Data: data, ContentType: contentType, Source: "gravatar", SourceURL: url}, true, nil
}

func (r *Resolver) fetchBIMI(ctx context.Context, domain string) (Image, bool, error) {
	records, err := net.DefaultResolver.LookupTXT(ctx, "default._bimi."+domain)
	if err != nil {
		if dnsErr, ok := err.(*net.DNSError); ok && (dnsErr.IsNotFound || dnsErr.IsTimeout) {
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
	if err := validateRemoteAvatarURL(ctx, rawURL); err != nil {
		return Image{}, false, err
	}
	client := *r.client
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return http.ErrUseLastResponse
		}
		return validateRemoteAvatarURL(req.Context(), req.URL.String())
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
	if !looksLikeSVG(data) {
		return Image{}, false, fmt.Errorf("bimi logo is not SVG")
	}

	return Image{Data: data, ContentType: "image/svg+xml", Source: "bimi", SourceURL: rawURL}, true, nil
}

func validateRemoteAvatarURL(ctx context.Context, rawURL string) error {
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
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
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
