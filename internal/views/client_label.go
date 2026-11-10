package views

import (
	"context"
	"strings"
	"unicode"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

// ClientLabel names the browser or program a request came from, such as
// "Chrome on Linux", from its user agent; it falls back to the user agent
// itself, cut short.
func ClientLabel(ctx context.Context, userAgent string) string {
	normalized := strings.Map(func(value rune) rune {
		if unicode.IsControl(value) {
			return ' '
		}
		return value
	}, strings.ToValidUTF8(userAgent, ""))
	normalized = strings.Join(strings.Fields(normalized), " ")
	if normalized == "" {
		return i18n.T(ctx, "server.session.unknown_client")
	}
	lower := strings.ToLower(normalized)
	browser := ""
	switch {
	case strings.Contains(lower, "edg/") || strings.Contains(lower, "edga/") || strings.Contains(lower, "edgios/"):
		browser = "Microsoft Edge"
	case strings.Contains(lower, "opr/"):
		browser = "Opera"
	case strings.Contains(lower, "firefox/") || strings.Contains(lower, "fxios/"):
		browser = "Firefox"
	case strings.Contains(lower, "chrome/") || strings.Contains(lower, "crios/"):
		browser = "Chrome"
	case strings.Contains(lower, "safari/") && strings.Contains(lower, "version/"):
		browser = "Safari"
	case strings.Contains(lower, "curl/"):
		browser = "curl"
	case strings.Contains(lower, "wget/"):
		browser = "Wget"
	case strings.Contains(lower, "go-http-client/"):
		browser = i18n.T(ctx, "server.session.go_http_client")
	case strings.Contains(lower, "postmanruntime/"):
		browser = "Postman"
	}
	platform := ""
	switch {
	case strings.Contains(lower, "iphone"):
		platform = "iPhone"
	case strings.Contains(lower, "ipad"):
		platform = "iPad"
	case strings.Contains(lower, "android"):
		platform = "Android"
	case strings.Contains(lower, "windows"):
		platform = "Windows"
	case strings.Contains(lower, "cros"):
		platform = "ChromeOS"
	case strings.Contains(lower, "macintosh") || strings.Contains(lower, "mac os x"):
		platform = "macOS"
	case strings.Contains(lower, "linux"):
		platform = "Linux"
	}
	if browser != "" && platform != "" {
		return i18n.T(ctx, "server.session.browser_on_platform", "browser", browser, "platform", platform)
	}
	if browser != "" {
		return browser
	}
	runes := []rune(normalized)
	if len(runes) > 96 {
		return string(runes[:95]) + "…"
	}
	return normalized
}
