package avatar

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
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

func GravatarHash(email string) string {
	normalized := strings.ToLower(strings.TrimSpace(email))
	if normalized == "" || !strings.Contains(normalized, "@") {
		return ""
	}
	sum := md5.Sum([]byte(normalized))
	return hex.EncodeToString(sum[:])
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
	}

	r.mu.Lock()
	r.cache[hash] = cacheEntry{image: image, found: found, expires: expires}
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

	return Image{Data: data, ContentType: contentType}, true, nil
}
