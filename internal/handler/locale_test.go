package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func localeTestHandler(t *testing.T) *Handler {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatalf("storage.New() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT OR IGNORE INTO users (id, username, username_normalized, name) VALUES ('locale-user', 'locale-user', 'locale-user', 'Locale User')`); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() { forgetSavedRegional("locale-user") })
	return &Handler{db: db}
}

func requestLocale(h *Handler, signedIn bool, header, cookie string) i18n.Locale {
	var got i18n.Locale
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = i18n.FromContext(r.Context()) })
	r := httptest.NewRequest(http.MethodGet, "/settings/regional", nil)
	if header != "" {
		r.Header.Set("Accept-Language", header)
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: i18n.CookieName, Value: cookie})
	}
	if signedIn {
		r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "locale-user"}))
	}
	h.LocaleMiddleware(next).ServeHTTP(httptest.NewRecorder(), r)
	return got
}

func TestLocaleMiddlewarePrefersTheSavedLanguage(t *testing.T) {
	h := localeTestHandler(t)
	if got := requestLocale(h, false, "es-AR,es;q=0.9", ""); got != "es" {
		t.Fatalf("signed out, Spanish browser: %q", got)
	}
	if got := requestLocale(h, true, "es-AR,es;q=0.9", ""); got != "es" {
		t.Fatalf("automatic follows the browser: %q", got)
	}
	if err := h.db.MergeUISettings(context.Background(), "locale-user", map[string]string{i18n.SettingKey: "en"}); err != nil {
		t.Fatal(err)
	}
	forgetSavedRegional("locale-user")
	if got := requestLocale(h, true, "es-AR,es;q=0.9", "es"); got != i18n.English {
		t.Fatalf("saved English beats the browser and the cookie: %q", got)
	}
	if got := requestLocale(h, false, "en-US", "es"); got != "es" {
		t.Fatalf("signed out, the cookie beats the browser: %q", got)
	}
}

func TestSavingSettingsUpdatesTheCachedLanguage(t *testing.T) {
	h := localeTestHandler(t)
	if got := requestLocale(h, true, "en-US", ""); got != i18n.English {
		t.Fatalf("before saving: %q", got)
	}
	body := bytes.NewBufferString(`{"language":"es"}`)
	r := httptest.NewRequest(http.MethodPatch, "/api/settings/ui", body)
	r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "locale-user"}))
	w := httptest.NewRecorder()
	h.handleSaveUISettings(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if got := requestLocale(h, true, "en-US", ""); got != "es" {
		t.Fatalf("after choosing Spanish: %q", got)
	}
	r = httptest.NewRequest(http.MethodPatch, "/api/settings/ui", bytes.NewBufferString(`{"language":"auto"}`))
	r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "locale-user"}))
	h.handleSaveUISettings(httptest.NewRecorder(), r)
	if got := requestLocale(h, true, "en-US", ""); got != i18n.English {
		t.Fatalf("after choosing automatic: %q", got)
	}
}

func TestLocaleScriptRoute(t *testing.T) {
	mux := http.NewServeMux()
	setupAssetsRoutes(mux)
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	w := get(i18n.ScriptPrefix + "es.js")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "window.GOFER_I18N=") || !strings.Contains(w.Body.String(), `"locale":"es"`) {
		t.Fatalf("es.js: %d %s", w.Code, w.Body.String())
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "application/javascript") {
		t.Fatalf("content type %q", w.Header().Get("Content-Type"))
	}
	for _, path := range []string{"xx.js", "es.json", "es"} {
		if w := get(i18n.ScriptPrefix + path); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, w.Code)
		}
	}
}

func TestLocaleMiddlewareCarriesTheDateSettings(t *testing.T) {
	h := localeTestHandler(t)
	if err := h.db.MergeUISettings(context.Background(), "locale-user", map[string]string{i18n.DateFormatSettingKey: "dd-mm-yyyy", i18n.TimeFormatSettingKey: "24h"}); err != nil {
		t.Fatal(err)
	}
	forgetSavedRegional("locale-user")
	var format, clock string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { format, clock = i18n.DatePrefs(r.Context()) })
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "locale-user"}))
	h.LocaleMiddleware(next).ServeHTTP(httptest.NewRecorder(), r)
	if format != "dd-mm-yyyy" || clock != "24h" {
		t.Fatalf("date settings = %q %q", format, clock)
	}
}

func TestLanguagePickedBeforeSignInIsSavedOnce(t *testing.T) {
	h := localeTestHandler(t)
	if err := h.db.MergeUISettings(context.Background(), "locale-user", map[string]string{i18n.SettingKey: "en"}); err != nil {
		t.Fatal(err)
	}
	forgetSavedRegional("locale-user")

	request := func(signedIn, picked bool) (i18n.Locale, *httptest.ResponseRecorder) {
		var got i18n.Locale
		next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = i18n.FromContext(r.Context()) })
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: i18n.CookieName, Value: "es"})
		if picked {
			r.AddCookie(&http.Cookie{Name: i18n.PickCookieName, Value: "1"})
		}
		if signedIn {
			r = r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: "locale-user"}))
		}
		w := httptest.NewRecorder()
		h.LocaleMiddleware(next).ServeHTTP(w, r)
		return got, w
	}

	// Signed out, the pick only shows the page in that language.
	if got, w := request(false, true); got != "es" || strings.Contains(w.Header().Get("Set-Cookie"), i18n.PickCookieName) {
		t.Fatalf("signed out: %q, Set-Cookie %q", got, w.Header().Get("Set-Cookie"))
	}
	// The cookie alone, as kept after signing out, doesn't change a saved language.
	if got, _ := request(true, false); got != i18n.English {
		t.Fatalf("cookie without a pick: %q", got)
	}
	// A pick replaces the saved language once the user is signed in, and is cleared.
	got, w := request(true, true)
	if got != "es" {
		t.Fatalf("after signing in with a pick: %q", got)
	}
	if setCookie := w.Header().Get("Set-Cookie"); !strings.Contains(setCookie, i18n.PickCookieName+"=") || !strings.Contains(setCookie, "Max-Age=0") {
		t.Fatalf("pick not cleared: %q", setCookie)
	}
	if saved := h.db.GetUISettings(context.Background(), "locale-user")[i18n.SettingKey]; saved != "es" {
		t.Fatalf("saved language = %q", saved)
	}
}
