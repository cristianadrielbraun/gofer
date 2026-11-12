package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"sync/atomic"
)

const (
	CSRFFormFieldName           = "_csrf"
	CSRFHeaderName              = "X-CSRF-Token"
	sessionCSRFFormMaximumBytes = 8 << 10
	// bulkCSRFFormMaximumBytes fits a change to 200 people at once: each
	// is named by a 36-character ID.
	bulkCSRFFormMaximumBytes = 16 << 10
	// AdminUsersBulkPath is where the admin console's changes to many
	// people at once go, each change at its own address below it.
	AdminUsersBulkPath = "/admin/users/bulk/"
	csrfKeyContext     = "gofer-csrf-session-key-v1"
	csrfActionContext  = "gofer-csrf-action-v1"
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

type csrfIssuedKey struct{}

// TrackCSRFTokens returns a context that records whether a CSRF token was
// issued while handling the request, and a function reporting it. Responses
// that carry a token must not be compressed: compression next to text an
// attacker controls can reveal the token through the response size (BREACH).
func TrackCSRFTokens(ctx context.Context) (context.Context, func() bool) {
	issued := new(atomic.Bool)
	return context.WithValue(ctx, csrfIssuedKey{}, issued), issued.Load
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
	if issued, ok := ctx.Value(csrfIssuedKey{}).(*atomic.Bool); ok {
		issued.Store(true)
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
	return r.URL.Path == "/auth/logout" || r.URL.Path == RequiredPasswordChangePath ||
		strings.HasPrefix(r.URL.Path, "/settings/security/") ||
		strings.HasPrefix(r.URL.Path, "/admin/users/") ||
		strings.HasPrefix(r.URL.Path, "/admin/activity/") ||
		strings.HasPrefix(r.URL.Path, "/admin/security/")
}

// sessionCSRFFormLimit is how large a form sent to path may be.
func sessionCSRFFormLimit(path string) int64 {
	if strings.HasPrefix(path, AdminUsersBulkPath) {
		return bulkCSRFFormMaximumBytes
	}
	return sessionCSRFFormMaximumBytes
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
