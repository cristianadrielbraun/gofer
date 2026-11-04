package notifications

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
)

// With settings kept per user, the language and date formats a user saves
// are the ones their next page is drawn with. The middleware runs on the base
// handler, which has to read them from the user's own store.
func TestSavedRegionalSettingsReachTheNextPage(t *testing.T) {
	f := newUserStorageFixture(t)
	// One setting at a time, as the Regional pickers save them, so the next
	// page reads the store rather than the cache this save fills.
	for _, body := range []string{`{"language":"es"}`, `{"date_format":"dd/mm/yyyy"}`, `{"time_format":"24h"}`} {
		if response := f.request("alice", "PATCH", "/api/settings/ui", body); response.Code != http.StatusOK {
			t.Fatalf("save %s: %d %s", body, response.Code, response.Body.String())
		}
	}
	var locale i18n.Locale
	var dateFormat, timeFormat string
	page := f.auth.Middleware(f.base.LocaleMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		locale = i18n.FromContext(r.Context())
		dateFormat, timeFormat = i18n.DatePrefs(r.Context())
	})))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "gofer_session", Value: f.sessions["alice"].Token})
	page.ServeHTTP(httptest.NewRecorder(), req)
	if locale != i18n.Locale("es") || dateFormat != "dd/mm/yyyy" || timeFormat != "24h" {
		t.Fatalf("next page drawn with %q, %q, %q; want es, dd/mm/yyyy, 24h", locale, dateFormat, timeFormat)
	}
}
