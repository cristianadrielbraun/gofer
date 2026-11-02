package handler

import (
	"context"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// savedLanguages caches each signed-in user's language setting ("" for
// automatic), so a request doesn't read the settings to pick its language.
// Saving the setting replaces the entry.
var savedLanguages sync.Map // user ID -> string

// LocaleMiddleware puts the request's language in its context: the user's
// saved choice, else the language cookie, else the browser's languages.
// It goes inside the authentication middleware, which supplies the user.
func (h *Handler) LocaleMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			next.ServeHTTP(w, r)
			return
		}
		locale := i18n.FromRequest(r)
		if user := auth.GetCurrentUser(r.Context()); user != nil && strings.TrimSpace(user.ID) != "" {
			if saved, ok := i18n.FromSetting(h.savedLanguage(r.Context(), user.ID)); ok {
				locale = saved
			}
		}
		next.ServeHTTP(w, r.WithContext(i18n.WithLocale(r.Context(), locale)))
	})
}

func (h *Handler) savedLanguage(ctx context.Context, userID string) string {
	if cached, ok := savedLanguages.Load(userID); ok {
		return cached.(string)
	}
	var value string
	if err := h.withUserDB(ctx, userID, func(db *storage.DB) error {
		value = db.GetUISettings(ctx, userID)[i18n.SettingKey]
		return nil
	}); err != nil {
		// Try again on the next request rather than caching a guess.
		return ""
	}
	savedLanguages.Store(userID, value)
	return value
}

// rememberSavedLanguage updates the cache after settings are saved. A save
// without the language key leaves the cached choice alone.
func rememberSavedLanguage(userID string, updates map[string]string) {
	if value, ok := updates[i18n.SettingKey]; ok {
		savedLanguages.Store(userID, value)
	}
}

// forgetSavedLanguage drops a user's cached choice, for changes such as a
// profile import that write settings without going through the save route.
func forgetSavedLanguage(userID string) {
	savedLanguages.Delete(userID)
}

// serveLocaleScript serves a language's browser catalog. Pages link it with
// its content version, so it can be cached until it changes.
func serveLocaleScript(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutSuffix(r.PathValue("file"), ".js")
	locale, known := i18n.Parse(name)
	if !ok || !known {
		http.NotFound(w, r)
		return
	}
	body, version := i18n.Script(locale)
	switch {
	case os.Getenv("GO_ENV") != "production":
		w.Header().Set("Cache-Control", "no-store")
	case r.URL.Query().Get("v") == version:
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		w.Header().Set("Cache-Control", "public, no-cache")
	}
	w.Header().Set("ETag", `"`+version+`"`)
	if match := r.Header.Get("If-None-Match"); match == `"`+version+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	_, _ = w.Write(body)
}
