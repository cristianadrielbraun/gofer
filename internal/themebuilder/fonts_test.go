package themebuilder

import (
	"strings"
	"testing"
)

const fontTheme = `{"mode":"light","seed":"#2563eb","sidebar":"darker","depth":"raised","corners":"medium"`

func TestParseKeepsOnlySafeFontNames(t *testing.T) {
	theme, ok := Parse(fontTheme + `,"bodyFont":" Inter ","headingFont":"Playfair Display"}`)
	if !ok || theme.BodyFont != "Inter" || theme.HeadingFont != "Playfair Display" {
		t.Fatalf("Parse = %+v, %v", theme, ok)
	}
	for _, font := range []string{`Inter\"`, `Inter;}body{color:red`, `Inter  Tight`, strings.Repeat("a", 61)} {
		if _, ok := Parse(fontTheme + `,"bodyFont":"` + font + `"}`); ok {
			t.Errorf("Parse accepted font %q", font)
		}
	}
}

func TestCSSLoadsFontsOnlyWithTheLibrary(t *testing.T) {
	theme, _ := Parse(fontTheme + `,"bodyFont":"Inter"}`)
	t.Cleanup(func() { FontStylesheet = nil })
	FontStylesheet = nil
	if css := theme.CSS(); strings.Contains(css, "font-sans") || strings.Contains(css, "@import") {
		t.Fatalf("fonts used without the library: %s", css)
	}
	FontStylesheet = func(families ...string) string { return "/fonts/google/css?family=" + families[0] }
	css := theme.CSS()
	if !strings.HasPrefix(css, `@import url("/fonts/google/css?family=Inter");`) {
		t.Fatalf("CSS does not start with the font import: %s", css)
	}
	if !strings.Contains(css, `--font-sans:"Inter","Nunito Sans",`) || !strings.Contains(css, `--default-font-family:"Inter",`) || strings.Contains(css, "--font-serif") {
		t.Fatalf("font variables wrong: %s", css)
	}
}
