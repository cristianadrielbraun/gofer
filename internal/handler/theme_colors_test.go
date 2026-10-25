package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeThemeColorSettings(t *testing.T) {
	updates := map[string]string{
		"theme_colors_classic": `{"light":{"primary":"#1D4ED8"}}`,
		"theme_colors_minimal": `{"light":{"primary":"red;}"}}`,
		"theme_colors_express": `{"light":{"primary":"#1d4ed8"}}`,
		"theme_colors_sepia":   `{}`,
		"theme":                "dark",
	}
	normalizeThemeColorSettings(updates)
	if got := updates["theme_colors_classic"]; got != `{"light":{"primary":"#1d4ed8"}}` {
		t.Fatalf("classic stored as %q", got)
	}
	for _, key := range []string{"theme_colors_minimal", "theme_colors_express", "theme_colors_sepia"} {
		if _, ok := updates[key]; ok {
			t.Errorf("%s was kept", key)
		}
	}
	if updates["theme"] != "dark" {
		t.Fatal("other settings were dropped")
	}

	updates = map[string]string{"theme_colors_classic": ""}
	normalizeThemeColorSettings(updates)
	if v, ok := updates["theme_colors_classic"]; !ok || v != "" {
		t.Fatal("an empty value should stay so it discards the changes")
	}
}

func TestThemeColorsCSSPreview(t *testing.T) {
	h := &Handler{}
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.handleThemeColorsCSS(rec, httptest.NewRequest("POST", "/api/settings/theme-colors.css", strings.NewReader(body)))
		return rec
	}

	rec := post(`{"style":"minimal","overrides":{"dark":{"sidebar":"#101820"}}}`)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("status %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), `[data-theme="minimal"].dark{--sidebar:#101820;}`) {
		t.Fatalf("body = %s", rec.Body.String())
	}

	if rec := post(`{"style":"minimal","overrides":{}}`); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("no changes: status %d, body %q", rec.Code, rec.Body.String())
	}
	for _, body := range []string{
		`{"style":"express","overrides":{"light":{"primary":"#1d4ed8"}}}`,
		`{"style":"classic","overrides":{"light":{"primary":"url(x)"}}}`,
		`not json`,
	} {
		if rec := post(body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", body, rec.Code)
		}
	}
}
