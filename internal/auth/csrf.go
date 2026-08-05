package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

const (
	CSRFFormFieldName           = "_csrf"
	CSRFHeaderName              = "X-CSRF-Token"
	sessionCSRFFormMaximumBytes = 8 << 10
	csrfKeyContext              = "gofer-csrf-session-key-v1"
	csrfActionContext           = "gofer-csrf-action-v1"
)

type csrfContextKey struct{}

func contextWithSessionCSRF(ctx context.Context, sessionToken string) context.Context {
	if sessionToken == "" {
		return ctx
	}
	mac := hmac.New(sha256.New, []byte(sessionToken))
	_, _ = mac.Write([]byte(csrfKeyContext))
	return context.WithValue(ctx, csrfContextKey{}, mac.Sum(nil))
}

// CSRFToken returns a proof bound to the authenticated session, HTTP method,
// and exact path. It never exposes or persists the raw session bearer.
func CSRFToken(ctx context.Context, method, path string) string {
	key, ok := ctx.Value(csrfContextKey{}).([]byte)
	if !ok || len(key) != sha256.Size {
		return ""
	}
	action, ok := canonicalCSRFAction(method, path)
	if !ok {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(csrfActionContext))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(action))
	return hex.EncodeToString(mac.Sum(nil))
}

func canonicalCSRFAction(method, path string) (string, bool) {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" || path == "" || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#") {
		return "", false
	}
	return method + "\x00" + path, true
}

func requiresSessionCSRF(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	return r.URL.Path == "/auth/logout" ||
		r.URL.Path == "/settings/security/password" ||
		strings.HasPrefix(r.URL.Path, "/admin/security/")
}

func validSessionCSRF(r *http.Request) bool {
	presented, ok := requestCSRFToken(r)
	if !ok {
		return false
	}
	expected := CSRFToken(r.Context(), r.Method, r.URL.Path)
	return expected != "" && hmac.Equal([]byte(presented), []byte(expected))
}

func requestCSRFToken(r *http.Request) (string, bool) {
	if values := r.Header.Values(CSRFHeaderName); len(values) != 0 {
		if len(values) != 1 || !canonicalPresentedCSRFToken(values[0]) {
			return "", false
		}
		return values[0], true
	}
	if err := r.ParseForm(); err != nil {
		return "", false
	}
	values := r.PostForm[CSRFFormFieldName]
	if len(values) != 1 || !canonicalPresentedCSRFToken(values[0]) {
		return "", false
	}
	return values[0], true
}

func canonicalPresentedCSRFToken(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
