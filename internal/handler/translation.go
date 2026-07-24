package handler

import (
	"context"
	"encoding/json"
	"fmt"
	stdhtml "html"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	mailmessage "github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/translation"
	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var (
	translationHTMLBreakRE   = regexp.MustCompile(`(?i)<\s*/?\s*(?:p|div|br|li|tr|h[1-6]|blockquote|pre)\b[^>]*>`)
	translationHTMLTagRE     = regexp.MustCompile(`(?s)<[^>]*>`)
	translationManySpacesRE  = regexp.MustCompile(`[ \t]{2,}`)
	translationManyNewlineRE = regexp.MustCompile(`\n{3,}`)
)

type translateMessageRequest struct {
	Provider       string `json:"provider"`
	SourceLanguage string `json:"source_language"`
	TargetLanguage string `json:"target_language"`
}

type translateMessageResponse struct {
	Status           string `json:"status"`
	Provider         string `json:"provider"`
	ProviderLabel    string `json:"provider_label"`
	SourceLanguage   string `json:"source_language,omitempty"`
	TargetLanguage   string `json:"target_language"`
	TargetLabel      string `json:"target_label"`
	Text             string `json:"text"`
	HTML             string `json:"html,omitempty"`
	OriginalLength   int    `json:"original_length"`
	TranslatedLength int    `json:"translated_length"`
}

func (h *Handler) handleTranslateMessage(w http.ResponseWriter, r *http.Request) {
	emailID := r.PathValue("id")
	msgID, err := strconv.ParseInt(emailID, 10, 64)
	if emailID == "" || err != nil || msgID <= 0 {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if !h.userOwnsMessage(ctx, msgID) {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}

	var req translateMessageRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	settings := h.db.GetUISettings(ctx, h.userID(ctx))
	providerName := normalizeTranslationProvider(firstNonEmptyString(req.Provider, settings["translation_provider"]))
	provider, ok := h.translationProvider(providerName)
	if !ok {
		http.Error(w, "translation provider is not configured", http.StatusBadRequest)
		return
	}

	targetLanguage := normalizeTranslationLanguage(firstNonEmptyString(req.TargetLanguage, settings["translation_target_language"]), "en")
	sourceLanguage := normalizeTranslationLanguage(req.SourceLanguage, "auto")

	text, htmlSource, err := h.messageTranslationSource(ctx, emailID, msgID)
	if err != nil {
		http.Error(w, "message body unavailable", http.StatusNotFound)
		return
	}

	result, translatedHTML, err := translateMessageBody(ctx, provider, sourceLanguage, targetLanguage, text, htmlSource)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	translatedText := result.Text
	if translatedHTML != "" {
		translatedText = plainTextFromHTML(translatedHTML)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(translateMessageResponse{
		Status:           "ok",
		Provider:         result.Provider,
		ProviderLabel:    translationProviderLabel(result.Provider),
		SourceLanguage:   result.DetectedLanguage,
		TargetLanguage:   result.TargetLanguage,
		TargetLabel:      translationLanguageLabel(result.TargetLanguage),
		Text:             translatedText,
		HTML:             translatedHTML,
		OriginalLength:   len([]rune(text)),
		TranslatedLength: len([]rune(translatedText)),
	})
}

func (h *Handler) handleTranslatedEmailBody(w http.ResponseWriter, r *http.Request) {
	emailID := r.PathValue("id")
	msgID, err := strconv.ParseInt(emailID, 10, 64)
	if emailID == "" || err != nil || msgID <= 0 {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	if !h.userOwnsMessage(ctx, msgID) {
		http.NotFound(w, r)
		return
	}

	settings := h.db.GetUISettings(ctx, h.userID(ctx))
	providerName := normalizeTranslationProvider(firstNonEmptyString(r.URL.Query().Get("provider"), settings["translation_provider"]))
	provider, ok := h.translationProvider(providerName)
	if !ok {
		h.writeTranslatedEmailError(w, emailID, "Translation provider is not configured.", http.StatusBadRequest)
		return
	}

	targetLanguage := normalizeTranslationLanguage(firstNonEmptyString(r.URL.Query().Get("target_language"), settings["translation_target_language"]), "en")
	sourceLanguage := normalizeTranslationLanguage(r.URL.Query().Get("source_language"), "auto")

	text, htmlSource, err := h.messageTranslationSource(ctx, emailID, msgID)
	if err != nil {
		h.writeTranslatedEmailError(w, emailID, "Message body unavailable.", http.StatusNotFound)
		return
	}

	result, translatedHTML, err := translateMessageBody(ctx, provider, sourceLanguage, targetLanguage, text, htmlSource)
	if err != nil {
		h.writeTranslatedEmailError(w, emailID, err.Error(), http.StatusBadGateway)
		return
	}

	var body []byte
	if translatedHTML != "" {
		body = []byte(translatedHTML)
	} else {
		body = []byte(`<pre style="white-space:pre-wrap;word-wrap:break-word;font-family:inherit;margin:0;padding:8px">` + stdhtml.EscapeString(result.Text) + `</pre>`)
	}

	loadRemote := r.URL.Query().Get("remote") == "true"
	if !loadRemote {
		if h.db.IsRemoteContentAllowedForMessage(ctx, msgID) {
			loadRemote = true
		} else {
			senderEmail, _ := h.db.GetMessageSenderEmailForUser(ctx, msgID, h.userID(ctx))
			if senderEmail != "" && h.db.IsRemoteContentAllowedForSender(ctx, senderEmail) {
				loadRemote = true
			}
		}
	}
	if loadRemote {
		body = mailmessage.RestoreRemoteImages(body)
	}

	theme := r.URL.Query().Get("theme")
	bg := r.URL.Query().Get("bg")
	fg := r.URL.Query().Get("fg")
	link := r.URL.Query().Get("link")
	original := r.URL.Query().Get("mode") == "original"

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	doc := buildBodyDocument(body, emailResizeScript(emailID), theme, bg, fg, link, original)
	if !loadRemote {
		doc = append(doc, remoteImagesDetectScript(emailID)...)
	}
	w.Write(doc)
}

func (h *Handler) writeTranslatedEmailError(w http.ResponseWriter, emailID, message string, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	body := []byte(`<div style="font:14px -apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;line-height:1.5;padding:8px;color:inherit">` + stdhtml.EscapeString(message) + `</div>`)
	w.Write(buildBodyDocument(body, emailResizeScript(emailID), "light", "", "", "", false))
}

func (h *Handler) translationProvider(providerName string) (translation.Provider, bool) {
	switch providerName {
	case translation.ProviderGoogleWebBasic, translation.ProviderGoogleWebLegacy, "google":
		return h.googleTranslator, true
	default:
		return nil, false
	}
}

func translateMessageBody(ctx context.Context, provider translation.Provider, sourceLanguage, targetLanguage, text, htmlSource string) (translation.Result, string, error) {
	if htmlSource != "" {
		batchProvider, ok := provider.(translation.TextBatchProvider)
		if !ok {
			return translation.Result{}, "", fmt.Errorf("translation provider cannot preserve html structure")
		}
		result, translatedHTML, err := translateHTMLTextNodes(ctx, batchProvider, sourceLanguage, targetLanguage, htmlSource)
		if err != nil {
			return translation.Result{}, "", err
		}
		return result, translatedHTML, nil
	}
	result, err := provider.TranslateText(ctx, sourceLanguage, targetLanguage, text)
	return result, "", err
}

type translatableTextNode struct {
	node   *xhtml.Node
	prefix string
	suffix string
	text   string
}

func translateHTMLTextNodes(ctx context.Context, provider translation.TextBatchProvider, sourceLanguage, targetLanguage, htmlSource string) (translation.Result, string, error) {
	nodes, fullDocument, err := parseTranslationHTML(htmlSource)
	if err != nil {
		return translation.Result{}, "", err
	}

	textNodes := collectTranslatableTextNodes(nodes)
	if len(textNodes) == 0 {
		return translation.Result{}, "", fmt.Errorf("message html has no translatable text")
	}

	texts := make([]string, len(textNodes))
	for i, node := range textNodes {
		texts[i] = node.text
	}
	result, err := provider.TranslateTexts(ctx, sourceLanguage, targetLanguage, texts)
	if err != nil {
		return translation.Result{}, "", err
	}
	if len(result.Texts) != len(textNodes) {
		return translation.Result{}, "", fmt.Errorf("translation provider returned %d text nodes for %d requests", len(result.Texts), len(textNodes))
	}

	for i, translated := range result.Texts {
		textNodes[i].node.Data = textNodes[i].prefix + strings.TrimSpace(translated) + textNodes[i].suffix
	}

	var rendered strings.Builder
	if fullDocument {
		if err := xhtml.Render(&rendered, nodes[0]); err != nil {
			return translation.Result{}, "", err
		}
	} else {
		for _, node := range nodes {
			if err := xhtml.Render(&rendered, node); err != nil {
				return translation.Result{}, "", err
			}
		}
	}

	translatedHTML := string(mailmessage.SanitizeHTML([]byte(rendered.String())))
	if strings.TrimSpace(translatedHTML) == "" {
		return translation.Result{}, "", fmt.Errorf("translated html was removed by sanitization")
	}

	return translation.Result{
		Provider:         result.Provider,
		Text:             translatedHTML,
		DetectedLanguage: result.DetectedLanguage,
		TargetLanguage:   result.TargetLanguage,
	}, translatedHTML, nil
}

func parseTranslationHTML(htmlSource string) ([]*xhtml.Node, bool, error) {
	if strings.Contains(strings.ToLower(htmlSource), "<html") {
		doc, err := xhtml.Parse(strings.NewReader(htmlSource))
		if err != nil {
			return nil, false, err
		}
		return []*xhtml.Node{doc}, true, nil
	}
	context := &xhtml.Node{Type: xhtml.ElementNode, DataAtom: atom.Body, Data: "body"}
	nodes, err := xhtml.ParseFragment(strings.NewReader(htmlSource), context)
	if err != nil {
		return nil, false, err
	}
	return nodes, false, nil
}

func collectTranslatableTextNodes(nodes []*xhtml.Node) []translatableTextNode {
	var out []translatableTextNode
	var walk func(*xhtml.Node, bool)
	walk = func(node *xhtml.Node, skip bool) {
		if node == nil {
			return
		}
		if node.Type == xhtml.ElementNode {
			switch strings.ToLower(node.Data) {
			case "script", "style", "noscript", "template":
				skip = true
			}
		}
		if !skip && node.Type == xhtml.TextNode {
			prefix, text, suffix := splitOuterWhitespace(node.Data)
			if strings.TrimSpace(text) != "" {
				out = append(out, translatableTextNode{node: node, prefix: prefix, suffix: suffix, text: text})
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child, skip)
		}
	}
	for _, node := range nodes {
		walk(node, false)
	}
	return out
}

func splitOuterWhitespace(value string) (string, string, string) {
	start := 0
	for start < len(value) {
		r, size := rune(value[start]), 1
		if r >= utf8.RuneSelf {
			r, size = utf8.DecodeRuneInString(value[start:])
		}
		if !isHTMLTextSpace(r) {
			break
		}
		start += size
	}
	end := len(value)
	for end > start {
		r, size := utf8.DecodeLastRuneInString(value[:end])
		if r == utf8.RuneError && size == 0 {
			break
		}
		if !isHTMLTextSpace(r) {
			break
		}
		end -= size
	}
	return value[:start], value[start:end], value[end:]
}

func isHTMLTextSpace(r rune) bool {
	switch r {
	case ' ', '\n', '\r', '\t', '\f':
		return true
	default:
		return false
	}
}

func (h *Handler) messageTranslationSource(ctx context.Context, emailID string, msgID int64) (string, string, error) {
	userID := h.userID(ctx)
	if !h.db.IsBodyFetched(ctx, msgID) {
		if info, err := h.db.GetMessageFetchInfoForUser(ctx, msgID, userID); err == nil && info != nil {
			if parsed, err := h.fetchParsedBody(ctx, msgID, info.AccountID); err == nil && parsed != nil {
				h.persistParsedBodyAsync(msgID, info.AccountID, parsed)
				htmlSource := ""
				if len(parsed.HTMLBody) > 0 {
					htmlSource = string(bodyFromParsedMessage(parsed, msgID))
				}
				if text := strings.TrimSpace(parsed.TextBody); text != "" {
					return text, htmlSource, nil
				}
				if htmlSource != "" {
					if text := plainTextFromHTML(htmlSource); text != "" {
						return text, htmlSource, nil
					}
				}
			}
		}
	}

	email, err := h.db.GetEmailByIDForUser(ctx, emailID, userID)
	if err != nil || email == nil {
		return "", "", fmt.Errorf("message not found")
	}
	htmlSource := ""
	if body, err := h.db.GetEmailBodyForUser(ctx, emailID, userID); err == nil && len(body) > 0 {
		htmlSource = string(body)
	}
	if text := strings.TrimSpace(email.TextBody); text != "" {
		return text, htmlSource, nil
	}
	if htmlSource != "" {
		if text := plainTextFromHTML(htmlSource); text != "" {
			return text, htmlSource, nil
		}
	}
	if text := mailmessage.PreviewFromText(email.Preview); text != "" {
		return text, "", nil
	}
	return "", "", fmt.Errorf("message body is empty")
}

func (h *Handler) userOwnsMessage(ctx context.Context, msgID int64) bool {
	info, err := h.db.GetMessageStorageInfoForUser(ctx, msgID, h.userID(ctx))
	return err == nil && info != nil
}

func normalizeTranslationProvider(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "-", "_")
	switch value {
	case "", "google", translation.ProviderGoogleWebLegacy, translation.ProviderGoogleWebBasic, "google_translate":
		return translation.ProviderGoogleWebBasic
	default:
		return value
	}
}

func normalizeTranslationLanguage(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return fallback
	}
	return value
}

func plainTextFromHTML(value string) string {
	value = translationHTMLBreakRE.ReplaceAllString(value, "\n")
	value = translationHTMLTagRE.ReplaceAllString(value, "")
	value = stdhtml.UnescapeString(value)
	value = strings.ReplaceAll(value, "\u00a0", " ")
	value = strings.ReplaceAll(value, "\u200b", "")
	lines := strings.Split(value, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(translationManySpacesRE.ReplaceAllString(line, " "))
	}
	value = strings.TrimSpace(strings.Join(lines, "\n"))
	return translationManyNewlineRE.ReplaceAllString(value, "\n\n")
}

func translationProviderLabel(provider string) string {
	switch provider {
	case translation.ProviderGoogleWebBasic, translation.ProviderGoogleWebLegacy:
		return "Google Web Translate (Basic)"
	default:
		return provider
	}
}

func translationLanguageLabel(code string) string {
	switch strings.ToLower(code) {
	case "ar":
		return "Arabic"
	case "cs":
		return "Czech"
	case "de":
		return "German"
	case "en":
		return "English"
	case "es":
		return "Spanish"
	case "fr":
		return "French"
	case "it":
		return "Italian"
	case "ja":
		return "Japanese"
	case "ko":
		return "Korean"
	case "nl":
		return "Dutch"
	case "pl":
		return "Polish"
	case "pt":
		return "Portuguese"
	case "ru":
		return "Russian"
	case "uk":
		return "Ukrainian"
	case "zh-cn", "zh":
		return "Chinese"
	default:
		if strings.TrimSpace(code) == "" {
			return "selected language"
		}
		return code
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
