package views

import (
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestRegionalSettingsRenderInTheRequestLanguage(t *testing.T) {
	render := func(locale i18n.Locale, settings map[string]string) string {
		var out strings.Builder
		ctx := i18n.WithLocale(context.Background(), locale)
		if err := SettingsPage(nil, models.SyncSettings{}, "regional", settings, nil).Render(ctx, &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	english := render(i18n.English, map[string]string{})
	if !strings.Contains(english, ">Language</h4>") || !strings.Contains(english, "Automatic") {
		t.Fatal("English page lacks the Language row")
	}
	spanish := render("es", map[string]string{i18n.SettingKey: "es"})
	for _, want := range []string{">Idioma</h4>", ">Zona horaria</h4>", "Idioma y región", `value="es"`} {
		if !strings.Contains(spanish, want) {
			t.Errorf("Spanish page lacks %q", want)
		}
	}
	if strings.Contains(spanish, ">Timezone</h4>") {
		t.Error("Spanish page still shows the English Timezone heading")
	}
	// Each language is listed in its own name, whatever the page language.
	if !strings.Contains(spanish, "English") || !strings.Contains(english, "Español") {
		t.Error("language names are not shown in their own language")
	}
}

func TestPageLangFollowsTheRequest(t *testing.T) {
	if got := pageLang(i18n.WithLocale(context.Background(), "es")); got != "es" {
		t.Fatalf("pageLang = %q", got)
	}
	if got := pageLang(context.Background()); got != "en" {
		t.Fatalf("default pageLang = %q", got)
	}
}
