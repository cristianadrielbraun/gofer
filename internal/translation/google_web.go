package translation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	stdhtml "html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	ProviderGoogleWebBasic  = "google_web_basic"
	ProviderGoogleWebLegacy = "google_web"

	defaultGoogleWebAuthURL      = "https://translate.googleapis.com/_/translate_http/_/js/k=translate_http.tr.en_US.YusFYy3P_ro.O/am=AAg/d=1/exm=el_conf/ed=1/rs=AN8SPfq1Hb8iJRleQqQc8zhdzXmF9E56eQ/m=el_main"
	defaultGoogleWebTranslateURL = "https://translate-pa.googleapis.com/v1/translateHtml"

	maxInputRunes = 50000
	chunkRunes    = 1200
	batchSize     = 16
)

var (
	googleAPIKeyRE      = regexp.MustCompile(`(?i)['"]x-goog-api-key['"]\s*:\s*['"]([A-Za-z0-9_-]{20,80})['"]`)
	googleOriginalTagRE = regexp.MustCompile(`(?is)<i\b[^>]*>.*?</i>`)
	googleBreakTagRE    = regexp.MustCompile(`(?i)<\s*/?\s*(?:pre|p|div|br|li|tr|h[1-6])\b[^>]*>`)
	googleAnyTagRE      = regexp.MustCompile(`(?s)<[^>]*>`)
	googleManySpacesRE  = regexp.MustCompile(`[ \t]{2,}`)
	googleManyNewlineRE = regexp.MustCompile(`\n{3,}`)
)

type Provider interface {
	TranslateText(ctx context.Context, sourceLanguage, targetLanguage, text string) (Result, error)
}

type TextBatchProvider interface {
	TranslateTexts(ctx context.Context, sourceLanguage, targetLanguage string, texts []string) (TextsResult, error)
}

type Result struct {
	Provider         string
	Text             string
	DetectedLanguage string
	TargetLanguage   string
}

type TextsResult struct {
	Provider         string
	Texts            []string
	DetectedLanguage string
	TargetLanguage   string
}

type GoogleWebConnector struct {
	client       *http.Client
	authURL      string
	translateURL string

	mu        sync.Mutex
	apiKey    string
	apiKeyAt  time.Time
	keyMissAt time.Time
}

func NewGoogleWebConnector(client *http.Client) *GoogleWebConnector {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &GoogleWebConnector{
		client:       client,
		authURL:      defaultGoogleWebAuthURL,
		translateURL: defaultGoogleWebTranslateURL,
	}
}

func (c *GoogleWebConnector) TranslateText(ctx context.Context, sourceLanguage, targetLanguage, text string) (Result, error) {
	sourceLanguage = normalizeLanguageCode(sourceLanguage, "auto")
	targetLanguage = normalizeLanguageCode(targetLanguage, "en")
	if targetLanguage == "auto" || targetLanguage == "auto-detect" {
		targetLanguage = "en"
	}

	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return Result{}, fmt.Errorf("translation source text is empty")
	}
	if utf8.RuneCountInString(text) > maxInputRunes {
		return Result{}, fmt.Errorf("message is too large to translate")
	}

	chunks := splitTextChunks(text, chunkRunes)
	translated := make([]string, 0, len(chunks))
	detected := ""
	for start := 0; start < len(chunks); start += batchSize {
		end := start + batchSize
		if end > len(chunks) {
			end = len(chunks)
		}
		batchText, batchDetected, err := c.translateBatch(ctx, sourceLanguage, targetLanguage, chunks[start:end])
		if err != nil {
			return Result{}, err
		}
		translated = append(translated, batchText...)
		if detected == "" {
			detected = firstNonEmpty(batchDetected)
		}
	}

	return Result{
		Provider:         ProviderGoogleWebBasic,
		Text:             strings.TrimSpace(strings.Join(translated, "\n\n")),
		DetectedLanguage: detected,
		TargetLanguage:   targetLanguage,
	}, nil
}

func (c *GoogleWebConnector) TranslateTexts(ctx context.Context, sourceLanguage, targetLanguage string, texts []string) (TextsResult, error) {
	sourceLanguage = normalizeLanguageCode(sourceLanguage, "auto")
	targetLanguage = normalizeLanguageCode(targetLanguage, "en")
	if targetLanguage == "auto" || targetLanguage == "auto-detect" {
		targetLanguage = "en"
	}

	out := make([]string, len(texts))
	type item struct {
		index int
		text  string
	}
	items := make([]item, 0, len(texts))
	totalRunes := 0
	for i, text := range texts {
		text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
		if text == "" {
			continue
		}
		totalRunes += utf8.RuneCountInString(text)
		if totalRunes > maxInputRunes {
			return TextsResult{}, fmt.Errorf("message is too large to translate")
		}
		items = append(items, item{index: i, text: text})
	}
	if len(items) == 0 {
		return TextsResult{}, fmt.Errorf("translation source text is empty")
	}

	detected := ""
	for start := 0; start < len(items); start += batchSize {
		end := start + batchSize
		if end > len(items) {
			end = len(items)
		}
		batch := items[start:end]
		requests := make([]string, len(batch))
		for i, item := range batch {
			requests[i] = item.text
		}
		translated, batchDetected, err := c.translateBatch(ctx, sourceLanguage, targetLanguage, requests)
		if err != nil {
			return TextsResult{}, err
		}
		for i, value := range translated {
			out[batch[i].index] = value
		}
		if detected == "" {
			detected = firstNonEmpty(batchDetected)
		}
	}

	return TextsResult{
		Provider:         ProviderGoogleWebBasic,
		Texts:            out,
		DetectedLanguage: detected,
		TargetLanguage:   targetLanguage,
	}, nil
}

func (c *GoogleWebConnector) translateBatch(ctx context.Context, sourceLanguage, targetLanguage string, chunks []string) ([]string, []string, error) {
	requests := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		requests = append(requests, "<pre>"+stdhtml.EscapeString(chunk)+"</pre>")
	}

	translatedHTML, detected, err := c.translateHTMLBatch(ctx, sourceLanguage, targetLanguage, requests)
	if err != nil {
		return nil, nil, err
	}
	if len(translatedHTML) != len(chunks) {
		return nil, nil, fmt.Errorf("google web translation returned %d chunks for %d requests", len(translatedHTML), len(chunks))
	}

	out := make([]string, 0, len(translatedHTML))
	for _, value := range translatedHTML {
		out = append(out, cleanTranslatedHTML(value))
	}
	return out, detected, nil
}

func (c *GoogleWebConnector) translateHTMLBatch(ctx context.Context, sourceLanguage, targetLanguage string, requests []string) ([]string, []string, error) {
	apiKey, err := c.googleAPIKey(ctx)
	if err != nil {
		return nil, nil, err
	}

	payload := []any{
		[]any{requests, sourceLanguage, targetLanguage},
		"te",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.translateURL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/application/json+protobuf")
	req.Header.Set("X-goog-api-key", apiKey)
	req.Header.Set("User-Agent", "Gofer/1.0")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, nil, fmt.Errorf("google web translation failed: %s %s", resp.Status, strings.TrimSpace(string(data)))
	}

	var outer []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&outer); err != nil {
		return nil, nil, err
	}
	if len(outer) == 0 {
		return nil, nil, fmt.Errorf("google web translation returned an empty response")
	}

	var translatedHTML []string
	if err := json.Unmarshal(outer[0], &translatedHTML); err != nil {
		return nil, nil, err
	}
	if len(translatedHTML) != len(requests) {
		return nil, nil, fmt.Errorf("google web translation returned %d chunks for %d requests", len(translatedHTML), len(requests))
	}

	detected := []string{}
	if len(outer) > 1 {
		_ = json.Unmarshal(outer[1], &detected)
	}

	for i, value := range translatedHTML {
		translatedHTML[i] = cleanTranslatedHTMLFragment(value)
	}
	return translatedHTML, detected, nil
}

func (c *GoogleWebConnector) googleAPIKey(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.apiKey != "" && time.Since(c.apiKeyAt) < 20*time.Minute {
		key := c.apiKey
		c.mu.Unlock()
		return key, nil
	}
	shouldFetch := c.keyMissAt.IsZero() || time.Since(c.keyMissAt) > 5*time.Minute
	c.mu.Unlock()

	if !shouldFetch {
		return "", fmt.Errorf("google web translation key unavailable")
	}

	key := c.fetchGoogleAPIKey(ctx)
	if key == "" {
		c.mu.Lock()
		c.keyMissAt = time.Now()
		c.mu.Unlock()
		return "", fmt.Errorf("google web translation key unavailable")
	}
	c.mu.Lock()
	c.apiKey = key
	c.apiKeyAt = time.Now()
	c.mu.Unlock()
	return key, nil
}

func (c *GoogleWebConnector) fetchGoogleAPIKey(ctx context.Context) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.authURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "Gofer/1.0")
	resp, err := c.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return ""
	}
	match := googleAPIKeyRE.FindSubmatch(data)
	if len(match) != 2 {
		return ""
	}
	return string(match[1])
}

func normalizeLanguageCode(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' {
			continue
		}
		return fallback
	}
	return value
}

func splitTextChunks(text string, limit int) []string {
	runes := []rune(text)
	if len(runes) <= limit {
		return []string{text}
	}

	var chunks []string
	for len(runes) > limit {
		cut := limit
		for i := limit; i > limit/2; i-- {
			if runes[i-1] == '\n' {
				cut = i
				break
			}
			if cut == limit && unicode.IsSpace(runes[i-1]) {
				cut = i
			}
		}
		chunk := strings.TrimSpace(string(runes[:cut]))
		if chunk != "" {
			chunks = append(chunks, chunk)
		}
		runes = runes[cut:]
	}
	if tail := strings.TrimSpace(string(runes)); tail != "" {
		chunks = append(chunks, tail)
	}
	return chunks
}

func cleanTranslatedHTML(value string) string {
	value = googleOriginalTagRE.ReplaceAllString(value, "")
	value = googleBreakTagRE.ReplaceAllString(value, "\n")
	value = googleAnyTagRE.ReplaceAllString(value, "")
	value = stdhtml.UnescapeString(value)
	value = strings.ReplaceAll(value, "\u00a0", " ")
	value = strings.ReplaceAll(value, "\u200b", "")
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(googleManySpacesRE.ReplaceAllString(line, " "))
	}
	value = strings.TrimSpace(strings.Join(lines, "\n"))
	value = googleManyNewlineRE.ReplaceAllString(value, "\n\n")
	return value
}

func cleanTranslatedHTMLFragment(value string) string {
	value = strings.ReplaceAll(value, "\u00a0", " ")
	value = strings.ReplaceAll(value, "\u200b", "")
	return strings.TrimSpace(value)
}

func firstNonEmpty(values []string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
