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
	// ChoicePath serves ChoiceCSS, so Settings can show a new font choice
	// without a reload.
	ChoicePath = "/fonts/google/choice.css"
	// PreviewPath serves a family drawn only well enough to show its own
	// name, under PreviewPrefix plus its name, for the Settings font menus.
	PreviewPath   = "/fonts/google/preview.css"
	PreviewPrefix = "Gofer Preview "

	// BodySetting and HeadingSetting are the UI settings that hold the
	// chosen text and heading families; empty means the built-in font.
	BodySetting    = "body_font"
	HeadingSetting = "heading_font"

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
	// A stylesheet for a few letters points at /l/font?kit=…&skey=…&v=…,
	// which Gofer serves as l/<kit>/<skey>/<v>.woff2.
	subsetURLPattern  = regexp.MustCompile(`https://fonts\.gstatic\.com/l/font\?kit=([A-Za-z0-9_\-]+)&(?:amp;)?skey=([0-9a-f]+)&(?:amp;)?v=(v[0-9]+)`)
	subsetPathPattern = regexp.MustCompile(`^l/([A-Za-z0-9_\-]+)/([0-9a-f]+)/(v[0-9]+)\.woff2$`)
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

// ChoiceCSS loads the chosen families through Gofer and makes them the page's
// text and heading fonts, or is "" when the library is off or nothing is
// chosen. Express keeps its own font. Names that ValidName refuses are
// ignored, which also keeps them from breaking out of the CSS string.
func ChoiceCSS(body, heading string) string {
	if !Enabled() {
		return ""
	}
	if !ValidName(body) {
		body = ""
	}
	if !ValidName(heading) {
		heading = ""
	}
	href := StylesheetURL(body, heading)
	if href == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, `@import url(%q);:root:not([data-theme="express"]){`, href)
	if body != "" {
		family := fmt.Sprintf(`"%s","Nunito Sans",ui-sans-serif,system-ui,sans-serif`, body)
		fmt.Fprintf(&b, "--font-sans:%s;--default-font-family:%s;", family, family)
	}
	if heading != "" {
		fmt.Fprintf(&b, `--font-serif:"%s","Fraunces",ui-serif,Georgia,serif;`, heading)
	}
	b.WriteString("}")
	return b.String()
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
	mux.HandleFunc("GET "+ChoicePath, serveChoice)
	mux.HandleFunc("GET "+PreviewPath, l.servePreview)
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

func (l *Library) servePreview(w http.ResponseWriter, r *http.Request) {
	css, err := l.Preview(r.Context(), r.URL.Query().Get("family"))
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

func serveChoice(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, ChoiceCSS(query.Get("body"), query.Get("heading")))
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

// previewWeight is the weight a preview is drawn in: regular when the family
// has it, else its first, or 0 when the metadata lists none.
func (f Family) previewWeight() int {
	for _, weight := range f.weights {
		if weight == 400 {
			return 400
		}
	}
	if len(f.weights) > 0 {
		return f.weights[0]
	}
	return 0
}

// Preview is a stylesheet for the family with only the letters of its name,
// a few KB, named PreviewPrefix plus the family so it never stands in for the
// family itself on the page.
func (l *Library) Preview(ctx context.Context, name string) ([]byte, error) {
	families, err := l.loadFamilies(ctx)
	if err != nil {
		return nil, err
	}
	family, ok := families[name]
	if !ok {
		return nil, ErrUnknownFamily
	}
	query := family.Name
	if weight := family.previewWeight(); weight != 0 {
		query += ":wght@" + strconv.Itoa(weight)
	}
	values := url.Values{"family": {query}, "text": {family.Name}, "display": {"swap"}}
	sum := sha256.Sum256([]byte(query))
	file := "preview-" + hex.EncodeToString(sum[:12]) + ".css"
	return l.cached(ctx, file, cssMaxAge, func(ctx context.Context) ([]byte, error) {
		css, err := l.get(ctx, cssURL+"?"+values.Encode(), maxBodyBytes)
		if err != nil {
			return nil, err
		}
		css = RewriteCSS(css)
		return bytes.ReplaceAll(css, []byte("font-family: '"+family.Name+"'"), []byte("font-family: '"+PreviewPrefix+family.Name+"'")), nil
	})
}

// RewriteCSS points a stylesheet's font URLs to the local server, and removes @import and absolute URLs
func RewriteCSS(css []byte) []byte {
	css = gstaticURLPattern.ReplaceAll(css, []byte(FilePrefix+"$1"))
	css = subsetURLPattern.ReplaceAll(css, []byte(FilePrefix+"l/$1/$2/$3.woff2"))
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
	upstream := fileOrigin + path
	if subset := subsetPathPattern.FindStringSubmatch(path); subset != nil {
		upstream = fileOrigin + "l/font?" + url.Values{"kit": {subset[1]}, "skey": {subset[2]}, "v": {subset[3]}}.Encode()
	} else if !filePathPattern.MatchString(path) {
		return nil, ErrBadPath
	}
	if strings.Contains(path, "..") {
		return nil, ErrBadPath
	}
	name := "file-" + strings.ReplaceAll(path, "/", "_")
	return l.cached(ctx, name, 0, func(ctx context.Context) ([]byte, error) {
		data, err := l.get(ctx, upstream, maxFileBytes)
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
