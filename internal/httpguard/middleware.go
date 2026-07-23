package httpguard

import (
	"net/http"
	"strings"
)

func (c *Config) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)

		if !c.trustsHost(r.Host) {
			http.Error(w, "request host is not trusted", http.StatusMisdirectedRequest)
			return
		}
		if requiresSameOrigin(r) && !c.isSameOriginRequest(r) {
			http.Error(w, "cross-origin request blocked", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func requiresSameOrigin(r *http.Request) bool {
	if r.URL.Path == "/api/events" {
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
		return len(origins) == 1 && c.trustsOrigin(origins[0])
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
	headers.Set("Content-Security-Policy", "frame-ancestors 'self'")
	headers.Set("Cross-Origin-Resource-Policy", "same-origin")
	headers.Set("Referrer-Policy", "same-origin")
	headers.Set("X-Content-Type-Options", "nosniff")
	headers.Set("X-Frame-Options", "SAMEORIGIN")
}
