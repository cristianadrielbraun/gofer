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

// regionalPrefs are the settings that shape every page's text: its
// language and how dates and times are written.
type regionalPrefs struct{ language, dateFormat, timeFormat string }

var regionalSettingKeys = []string{i18n.SettingKey, i18n.DateFormatSettingKey, i18n.TimeFormatSettingKey}

// savedRegional caches each signed-in user's regional settings, so a
// request doesn't read the settings to pick its language and date format.
// Saving the settings replaces the entry.
var savedRegional sync.Map // user ID -> regionalPrefs

// LocaleMiddleware puts the request's language and date settings in its
// context. The language is the user's saved choice, else the language
// cookie, else the browser's languages. It goes inside the authentication
// middleware, which supplies the user.
func (h *Handler) LocaleMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			next.ServeHTTP(w, r)
			return
		}
		locale := i18n.FromRequest(r)
		var prefs regionalPrefs
		if user := auth.GetCurrentUser(r.Context()); user != nil && strings.TrimSpace(user.ID) != "" {
			h.savePickedLanguage(w, r, user.ID)
			prefs = h.savedRegionalPrefs(r.Context(), user.ID)
			if saved, ok := i18n.FromSetting(prefs.language); ok {
				locale = saved
			}
			// Work done later for this user, such as a push, writes in the
			// language this page used.
			i18n.RememberUser(user.ID, locale, prefs.dateFormat, prefs.timeFormat)
		}
		ctx := i18n.WithDatePrefs(i18n.WithLocale(r.Context(), locale), prefs.dateFormat, prefs.timeFormat)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// savePickedLanguage saves the language picked on a page shown before sign-in
// as the signed-in user's language, replacing any they had saved, since it is
// their latest choice. The pick is used once: its mark is cleared either way.
func (h *Handler) savePickedLanguage(w http.ResponseWriter, r *http.Request, userID string) {
	if _, err := r.Cookie(i18n.PickCookieName); err != nil {
		return
	}
	http.SetCookie(w, &http.Cookie{Name: i18n.PickCookieName, Value: "", Path: "/", MaxAge: -1, SameSite: http.SameSiteLaxMode})
	cookie, err := r.Cookie(i18n.CookieName)
	if err != nil {
		return
	}
	locale, ok := i18n.Parse(cookie.Value)
	if !ok {
		return
	}
	updates := map[string]string{i18n.SettingKey: string(locale)}
	if err := h.withSettingsDB(r.Context(), userID, func(db *storage.DB) error {
		return db.MergeUISettings(r.Context(), userID, updates)
	}); err != nil {
		return
	}
	rememberSavedRegional(userID, updates)
}

// useSavedRegionalForBackground lets work done outside a request find a
// user's saved language and date settings.
func (h *Handler) useSavedRegionalForBackground() {
	i18n.SetUserPrefsLoader(func(ctx context.Context, userID string) (string, string, string, bool) {
		if h.db == nil {
			return "", "", "", false
		}
		prefs := h.savedRegionalPrefs(ctx, userID)
		return prefs.language, prefs.dateFormat, prefs.timeFormat, true
	})
}

func (h *Handler) savedRegionalPrefs(ctx context.Context, userID string) regionalPrefs {
	if cached, ok := savedRegional.Load(userID); ok {
		return cached.(regionalPrefs)
	}
	var prefs regionalPrefs
	if err := h.withSettingsDB(ctx, userID, func(db *storage.DB) error {
		settings := db.GetUISettings(ctx, userID)
		prefs = regionalPrefs{settings[i18n.SettingKey], settings[i18n.DateFormatSettingKey], settings[i18n.TimeFormatSettingKey]}
		return nil
	}); err != nil {
		// Try again on the next request rather than caching a guess.
		return regionalPrefs{}
	}
	savedRegional.Store(userID, prefs)
	return prefs
}

// rememberSavedRegional updates the cache after settings are saved. A save
// without any regional key leaves the cached choices alone; a partial one
// starts over from the stored settings.
func rememberSavedRegional(userID string, updates map[string]string) {
	present := 0
	for _, key := range regionalSettingKeys {
		if _, ok := updates[key]; ok {
			present++
		}
	}
	switch present {
	case 0:
		return
	case len(regionalSettingKeys):
		savedRegional.Store(userID, regionalPrefs{updates[i18n.SettingKey], updates[i18n.DateFormatSettingKey], updates[i18n.TimeFormatSettingKey]})
	default:
		savedRegional.Delete(userID)
	}
}

// forgetSavedRegional drops a user's cached choices, for changes such as a
// profile import that write settings without going through the save route.
func forgetSavedRegional(userID string) {
	savedRegional.Delete(userID)
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

// withSettingsDB runs fn on the database that holds userID's own settings:
// their own store when settings are kept per user, else the handler's.
func (h *Handler) withSettingsDB(ctx context.Context, userID string, fn func(*storage.DB) error) error {
	if h.userStorage == nil && h.settingsRouting != nil {
		return h.settingsRouting.WithUser(ctx, userID, fn)
	}
	return h.withUserDB(ctx, userID, fn)
}
