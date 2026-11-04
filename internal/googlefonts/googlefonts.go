// GOFER_GOOGLE_FONTS.
package googlefonts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (

	EnvVar = "GOFER_GOOGLE_FONTS"

	CSSPath    = "/fonts/google/css"
	FilePrefix = "/fonts/google/file/"
	
	ListPath = "/api/fonts/google"

	metadataURL = "https://fonts.google.com/metadata/fonts"
	cssURL      = "https://fonts.googleapis.com/css2"
	fileOrigin  = "https://fonts.gstatic.com/"

	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36"

	listMaxAge   = 7 * 24 * time.Hour
	cssMaxAge    = 30 * 24 * time.Hour
	maxFamilies  = 4
	maxBodyBytes = 16 << 20
	maxFileBytes = 4 << 20
)

type Family struct {
	Name     string `json:"family"`
	Category string `json:"category"`
	weights  []int
}

var (
	ErrUnknownFamily = errors.New("googlefonts: unknown family")
	ErrBadPath = errors.New("googlefonts: not a font file path")

	familyNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ]{0,59}$`)
	filePathPattern   = regexp.MustCompile(`^s/[a-z0-9]+/v[0-9]+/[A-Za-z0-9_\-]+\.woff2$`)
	gstaticURLPattern = regexp.MustCompile(`https://fonts\.gstatic\.com/(s/[a-z0-9]+/v[0-9]+/[A-Za-z0-9_\-]+\.woff2)`)
)

func ValidName(name string) bool {
	return familyNamePattern.MatchString(name) && !strings.Contains(name, "  ") && strings.TrimSpace(name) == name
}

func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvVar))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func StylesheetURL(families ...string) string {
	values := url.Values{}
	seen := map[string]bool{}
	for _, family := range families {
		if family == "" || seen[family] {
			continue
		}
		seen[family] = true
		values.Add("family", family)
	}
	if len(values) == 0 {
		return ""
	}
	return CSSPath + "?" + values.Encode()
}

type Library struct {
	client *http.Client
	dir    string

	mu       sync.Mutex
	families map[string]Family
	list     []byte
	listAt   time.Time
	fetching map[string]*sync.Mutex
}

func New(dir string, client *http.Client) *Library {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Library{client: client, dir: dir, fetching: map[string]*sync.Mutex{}}
}

func CacheDir() string {
	db := os.Getenv("GOFER_DB_PATH")
	if db == "" {
		db = "data/gofer.db"
	}
	return filepath.Join(filepath.Dir(db), "google-fonts")
}

func (l *Library) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+ListPath, l.serveList)
	mux.HandleFunc("GET "+CSSPath, l.serveCSS)
	mux.HandleFunc("GET "+FilePrefix+"{path...}", l.serveFile)
	return mux
}

func (l *Library) serveList(w http.ResponseWriter, r *http.Request) {
	list, err := l.listJSON(r.Context())
	if err != nil {
		http.Error(w, "font list unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(list)
}

func (l *Library) serveCSS(w http.ResponseWriter, r *http.Request) {
	families := r.URL.Query()["family"]
	if len(families) == 0 || len(families) > maxFamilies {
		http.Error(w, "choose one to four families", http.StatusBadRequest)
		return
	}
	css, err := l.Stylesheet(r.Context(), families...)
	switch {
	case errors.Is(err, ErrUnknownFamily):
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, "stylesheet unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(css)
}

func (l *Library) serveFile(w http.ResponseWriter, r *http.Request) {
	data, err := l.File(r.Context(), r.PathValue("path"))
	switch {
	case errors.Is(err, ErrBadPath):
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, "font unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "font/woff2")

	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(data)
}

func (l *Library) Families(ctx context.Context) ([]Family, error) {
	families, err := l.loadFamilies(ctx)
	if err != nil {
		return nil, err
	}
	list := make([]Family, 0, len(families))
	for _, family := range families {
		list = append(list, family)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

func (l *Library) Known(ctx context.Context, name string) bool {
	families, err := l.loadFamilies(ctx)
	if err != nil {
		return false
	}
	_, ok := families[name]
	return ok
}

func (l *Library) listJSON(ctx context.Context) ([]byte, error) {
	if _, err := l.loadFamilies(ctx); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.list, nil
}

func (l *Library) loadFamilies(ctx context.Context) (map[string]Family, error) {
	l.mu.Lock()
	if l.families != nil && time.Since(l.listAt) < listMaxAge {
		families := l.families
		l.mu.Unlock()
		return families, nil
	}
	l.mu.Unlock()

	raw, err := l.cached(ctx, "metadata.json", listMaxAge, func(ctx context.Context) ([]byte, error) {
		return l.get(ctx, metadataURL, maxBodyBytes)
	})
	if err != nil {
		return nil, err
	}
	families, err := parseMetadata(raw)
	if err != nil {
		return nil, err
	}
	list := make([]Family, 0, len(families))
	for _, family := range families {
		list = append(list, family)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	encoded, err := json.Marshal(list)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.families, l.list, l.listAt = families, encoded, time.Now()
	l.mu.Unlock()
	return families, nil
}

func parseMetadata(raw []byte) (map[string]Family, error) {
	raw = bytes.TrimPrefix(bytes.TrimSpace(raw), []byte(")]}'"))
	var metadata struct {
		Families []struct {
			Family   string                     `json:"family"`
			Category string                     `json:"category"`
			Fonts    map[string]json.RawMessage `json:"fonts"`
		} `json:"familyMetadataList"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("googlefonts: read metadata: %w", err)
	}
	families := make(map[string]Family, len(metadata.Families))
	for _, entry := range metadata.Families {
		if !ValidName(entry.Family) {
			continue
		}
		family := Family{Name: entry.Family, Category: entry.Category}
		for style := range entry.Fonts {
			if weight, err := strconv.Atoi(style); err == nil {
				family.weights = append(family.weights, weight)
			}
		}
		sort.Ints(family.weights)
		families[family.Name] = family
	}
	if len(families) == 0 {
		return nil, errors.New("googlefonts: metadata lists no families")
	}
	return families, nil
}

var interfaceWeights = []int{400, 500, 600, 700}

func (f Family) query() string {
	var weights []string
	for _, want := range interfaceWeights {
		for _, have := range f.weights {
			if have == want {
				weights = append(weights, strconv.Itoa(want))
			}
		}
	}
	if len(weights) == 0 {
		return f.Name
	}
	return f.Name + ":wght@" + strings.Join(weights, ";")
}

func (l *Library) Stylesheet(ctx context.Context, names ...string) ([]byte, error) {
	families, err := l.loadFamilies(ctx)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	values := url.Values{}
	key := sha256.New()
	for _, name := range names {
		family, ok := families[name]
		if !ok {
			return nil, ErrUnknownFamily
		}
		values.Add("family", family.query())
		key.Write([]byte(family.query() + "\x00"))
	}
	values.Set("display", "swap")
	file := "css-" + hex.EncodeToString(key.Sum(nil)[:12]) + ".css"
	return l.cached(ctx, file, cssMaxAge, func(ctx context.Context) ([]byte, error) {
		css, err := l.get(ctx, cssURL+"?"+values.Encode(), maxBodyBytes)
		if err != nil {
			return nil, err
		}
		return RewriteCSS(css), nil
	})
}

// RewriteCSS points a stylesheet's font URLs to the local server, and removes @import and absolute URLs
func RewriteCSS(css []byte) []byte {
	css = gstaticURLPattern.ReplaceAll(css, []byte(FilePrefix+"$1"))
	var out bytes.Buffer
	for _, line := range bytes.SplitAfter(css, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if bytes.Contains(trimmed, []byte("://")) || bytes.HasPrefix(trimmed, []byte("@import")) {
			continue
		}
		out.Write(line)
	}
	return out.Bytes()
}

// File is a font file, fetched once and kept.
func (l *Library) File(ctx context.Context, path string) ([]byte, error) {
	if !filePathPattern.MatchString(path) || strings.Contains(path, "..") {
		return nil, ErrBadPath
	}
	name := "file-" + strings.ReplaceAll(path, "/", "_")
	return l.cached(ctx, name, 0, func(ctx context.Context) ([]byte, error) {
		data, err := l.get(ctx, fileOrigin+path, maxFileBytes)
		if err != nil {
			return nil, err
		}
		if !bytes.HasPrefix(data, []byte("wOF2")) {
			return nil, errors.New("googlefonts: not a woff2 file")
		}
		return data, nil
	})
}

func (l *Library) cached(ctx context.Context, name string, maxAge time.Duration, fetch func(context.Context) ([]byte, error)) ([]byte, error) {
	lock := l.lock(name)
	lock.Lock()
	defer lock.Unlock()
	path := filepath.Join(l.dir, name)
	info, statErr := os.Stat(path)
	if statErr == nil && (maxAge == 0 || time.Since(info.ModTime()) < maxAge) {
		if data, err := os.ReadFile(path); err == nil {
			return data, nil
		}
	}
	data, err := fetch(ctx)
	if err != nil {
		if statErr == nil {
			if stale, readErr := os.ReadFile(path); readErr == nil {
				return stale, nil
			}
		}
		return nil, err
	}
	if err := os.MkdirAll(l.dir, 0o750); err == nil {
		tmp := path + ".tmp"
		if os.WriteFile(tmp, data, 0o640) == nil {
			_ = os.Rename(tmp, path)
		}
	}
	return data, nil
}

func (l *Library) lock(name string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	lock, ok := l.fetching[name]
	if !ok {
		lock = &sync.Mutex{}
		l.fetching[name] = lock
	}
	return lock
}

func (l *Library) get(ctx context.Context, address string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	res, err := l.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("googlefonts: %s answered %s", req.URL.Host, res.Status)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("googlefonts: %s sent more than %d bytes", req.URL.Host, limit)
	}
	return data, nil
}
