package themecolors

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestParseAcceptsKnownColors(t *testing.T) {
	o, ok := Parse(`{"light":{"primary":"#1D4ED8"},"dark":{"sidebar":"#101820"}}`)
	if !ok {
		t.Fatal("Parse refused valid changes")
	}
	if got := o.Encode(); got != `{"dark":{"sidebar":"#101820"},"light":{"primary":"#1d4ed8"}}` {
		t.Fatalf("Encode = %s", got)
	}
	if o, ok := Parse(""); !ok || !o.Empty() || o.Encode() != "" {
		t.Fatal("empty input should be no changes")
	}
}

func TestParseRefusesUnsafeOrUnknownValues(t *testing.T) {
	for _, raw := range []string{
		`not json`,
		`{"sepia":{"primary":"#1d4ed8"}}`,
		`{"light":{"xp-face":"#1d4ed8"}}`,
		`{"light":{"primary":"red"}}`,
		`{"light":{"primary":"#1d4ed8;}body{display:none"}}`,
		`{"light":{"primary":"#36f"}}`,
	} {
		if _, ok := Parse(raw); ok {
			t.Errorf("Parse(%q) accepted", raw)
		}
	}
}

func TestCSSKeepsModesApart(t *testing.T) {
	o, _ := Parse(`{"light":{"primary":"#1d4ed8","card":"#ffffff"},"dark":{"muted-foreground":"#aaaaaa"}}`)
	css := o.CSS("classic")
	root := `:is(html,[data-theme-preview],[data-theme-probe])[data-theme="classic"]`
	for _, want := range []string{
		root + `:not(.dark){--card:#ffffff;--primary:#1d4ed8;}`,
		root + `:not(.dark) .mail-list-item{--primary:#1d4ed8;}`,
		root + `.dark{--muted-foreground:#aaaaaa;}`,
		root + `.dark .mail-list-item{--muted-foreground:#aaaaaa;}`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("CSS lacks %s\n%s", want, css)
		}
	}
	if o.CSS("express") != "" {
		t.Fatal("Express should get no overrides")
	}
	if (Overrides{}).CSS("classic") != "" {
		t.Fatal("no changes should write no CSS")
	}
}

// The editor lists every color the editable styles define, and only those.
func TestGroupsCoverThemeColors(t *testing.T) {
	defined := regexp.MustCompile(`--([a-z-]+):\s*#[0-9a-fA-F]{6}\s*;`)
	for _, style := range Styles {
		data, err := os.ReadFile("../../assets/css/themes/" + style + ".css")
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, m := range defined.FindAllStringSubmatch(string(data), -1) {
			seen[m[1]] = true
		}
		for name := range seen {
			if !known[name] {
				t.Errorf("%s defines --%s, which the editor does not list", style, name)
			}
		}
		for name := range known {
			if !seen[name] {
				t.Errorf("the editor lists --%s, which %s does not define", name, style)
			}
		}
	}
}
