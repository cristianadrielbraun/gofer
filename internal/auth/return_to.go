package auth

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
)

const returnToCookieName = "gofer_auth_return_to"

// SafeReturnTo accepts only same-origin application paths.
func SafeReturnTo(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	parsed, err := url.Parse(raw)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/") || strings.HasPrefix(parsed.Path, "//") || strings.Contains(parsed.Path, `\`) {
		return ""
	}
	if parsed.Path == "/login" || strings.HasPrefix(parsed.Path, "/auth/") {
		return ""
	}

	return parsed.RequestURI()
}

func SetReturnToCookie(w http.ResponseWriter, raw string, secure bool) {
	returnTo := SafeReturnTo(raw)
	if returnTo == "" {
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     returnToCookieName,
		Value:    base64.RawURLEncoding.EncodeToString([]byte(returnTo)),
		Path:     "/",
		MaxAge:   10 * 60,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func GetReturnTo(r *http.Request) string {
	cookie, err := r.Cookie(returnToCookieName)
	if err != nil || cookie.Value == "" {
		return ""
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return ""
	}
	return SafeReturnTo(string(decoded))
}

func ClearReturnToCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     returnToCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}
