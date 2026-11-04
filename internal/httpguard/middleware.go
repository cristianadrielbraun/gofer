package httpguard

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"unicode/utf16"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

func (c *Config) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		r = c.withClientAddress(r)

		if !c.trustsHost(r.Host) {
			requestBlocked(w, r, "httpguard.untrusted_host", http.StatusMisdirectedRequest)
			return
		}
		// Browsers send policy reports on their own, without the headers the
		// same-origin check expects. Reports are only logged.
		if r.URL.Path == ScriptPolicyReportPath {
			handleScriptPolicyReport(w, r)
			return
		}
		if c.isOutlookWebhook(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if requiresSameOrigin(r) && !c.isSameOriginRequest(r) {
			requestBlocked(w, r, "httpguard.cross_origin", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// HTMX does not display error response bodies. Send a separate notification
// while keeping the rejection status and leaving the current form intact.
// requestBlocked refuses r with the message under key. The guard runs before
// the signed-in language is known, so the message follows the language
// cookie or the browser; the log keeps the English.
func requestBlocked(w http.ResponseWriter, r *http.Request, key string, status int) {
	log.Printf("HTTP guard: %s %s: %s", r.Method, r.URL.Path, i18n.Translate(i18n.English, key))
	message := i18n.Translate(i18n.FromRequest(r), key)
	if r.Header.Get("HX-Request") == "true" {
		trigger, _ := json.Marshal(map[string]string{"gofer-request-blocked": message})
		w.Header().Set("HX-Trigger", asciiJSON(trigger))
	}
	http.Error(w, message, status)
}

func requiresSameOrigin(r *http.Request) bool {
	if r.URL.Path == "/api/events" || r.URL.Path == "/api/admin/events" {
		return true
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

func (c *Config) isSameOriginRequest(r *http.Request) bool {
	fetchSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	if fetchSite == "cross-site" {
		return false
	}

	if origins := r.Header.Values("Origin"); len(origins) != 0 {
		if len(origins) != 1 {
			return false
		}
		// Some privacy-hardened browsers serialize a legitimate form origin as
		// "null" while still supplying browser-controlled same-origin Fetch
		// Metadata. Treat that narrow combination like an unavailable Origin;
		// every other null-origin request remains untrusted.
		if strings.TrimSpace(origins[0]) == "null" {
			return fetchSite == "same-origin"
		}
		return c.trustsOrigin(origins[0])
	}
	if referers := r.Header.Values("Referer"); len(referers) != 0 {
		return len(referers) == 1 && c.trustsReferer(referers[0])
	}
	if fetchSite == "same-origin" {
		return true
	}
	return r.Header.Get(automationHeader) == "1"
}

func setSecurityHeaders(w http.ResponseWriter) {
	headers := w.Header()
	setPagePolicy(headers)
	headers.Set("Cross-Origin-Resource-Policy", "same-origin")
	headers.Set("Referrer-Policy", "same-origin")
	headers.Set("X-Content-Type-Options", "nosniff")
	headers.Set("X-Frame-Options", "SAMEORIGIN")
}

// asciiJSON escapes the non-ASCII characters of encoded JSON, such as the
// accents of a translated message, since browsers read header bytes as
// Latin-1.
func asciiJSON(encoded []byte) string {
	var b strings.Builder
	for _, r := range string(encoded) {
		if r < 0x80 {
			b.WriteRune(r)
			continue
		}
		if r > 0xFFFF {
			high, low := utf16.EncodeRune(r)
			fmt.Fprintf(&b, "\\u%04x\\u%04x", high, low)
			continue
		}
		fmt.Fprintf(&b, "\\u%04x", r)
	}
	return b.String()
}
