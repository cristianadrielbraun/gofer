package themebuilder

import (
	"fmt"
	"strings"
	"testing"
)

const testTheme = `"seed":"#16a34a","sidebar":"darker","depth":"flat","corners":"square"`

func savedJSON(id, name string) string {
	return fmt.Sprintf(`{"id":%q,"name":%q,%s}`, id, name, testTheme)
}

func TestParseList(t *testing.T) {
	themes, ok := ParseList(`[` + savedJSON("a1", "  Forest  ") + `,{"id":"b2","name":"Night","mode":"dark","seed":"#2563EB","sidebar":"lighter","depth":"raised","corners":"large"}]`)
	if !ok || len(themes) != 2 {
		t.Fatalf("ParseList = %v, %v", themes, ok)
	}
	if themes[0].Name != "Forest" || themes[1].Seed != "#2563eb" || themes[1].Mode != "dark" {
		t.Fatalf("themes = %+v", themes)
	}
	want := `[{"id":"a1","name":"Forest","mode":"light","seed":"#16a34a","tint":"primary","strength":"subtle","contrast":"normal","sidebar":"darker","depth":"flat","corners":"square"},` +
		`{"id":"b2","name":"Night","mode":"dark","seed":"#2563eb","tint":"primary","strength":"subtle","contrast":"normal","sidebar":"lighter","depth":"raised","corners":"large"}]`
	if got := EncodeList(themes); got != want {
		t.Fatalf("EncodeList = %s", got)
	}
	if got := EncodeList(nil); got != `[]` {
		t.Fatalf("EncodeList(nil) = %s", got)
	}
	if themes, ok := ParseList(`[]`); !ok || len(themes) != 0 {
		t.Fatal("an empty list should parse")
	}
	// Forty characters is the limit, however many bytes they take.
	if _, ok := ParseList(`[` + savedJSON("a1", strings.Repeat("é", MaxNameLength)) + `]`); !ok {
		t.Fatal("a name of MaxNameLength characters was refused")
	}

	many := make([]string, MaxThemes+1)
	for i := range many {
		many[i] = savedJSON(fmt.Sprintf("t%d", i), "Theme")
	}
	for name, raw := range map[string]string{
		"not json":      `not json`,
		"not a list":    savedJSON("a1", "Forest"),
		"bad id":        `[` + savedJSON("A-1", "Forest") + `]`,
		"empty id":      `[` + savedJSON("", "Forest") + `]`,
		"repeated id":   `[` + savedJSON("a1", "Forest") + `,` + savedJSON("a1", "Night") + `]`,
		"empty name":    `[` + savedJSON("a1", "   ") + `]`,
		"long name":     `[` + savedJSON("a1", strings.Repeat("x", MaxNameLength+1)) + `]`,
		"control char":  `[` + savedJSON("a1", "For\nest") + `]`,
		"bad theme":     `[{"id":"a1","name":"Forest","seed":"green","sidebar":"darker","depth":"flat","corners":"square"}]`,
		"too many":      `[` + strings.Join(many, ",") + `]`,
		"one bad entry": `[` + savedJSON("a1", "Forest") + `,{"id":"b2","name":"Night"}]`,
	} {
		if _, ok := ParseList(raw); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestListFromSettings(t *testing.T) {
	legacy := `{` + testTheme + `}`
	list := `[` + savedJSON("a1", "Forest") + `,` + savedJSON("b2", "Night") + `]`

	themes := ListFromSettings(map[string]string{SettingKey: legacy})
	if len(themes) != 1 || themes[0].ID != LegacyID || themes[0].Name != LegacyName || themes[0].Seed != "#16a34a" {
		t.Fatalf("the theme saved before names = %+v", themes)
	}
	if themes := ListFromSettings(map[string]string{SettingKey: legacy, ListKey: list}); len(themes) != 2 || themes[0].ID != "a1" {
		t.Fatalf("the list should replace the old theme, got %+v", themes)
	}
	// Deleting every theme leaves an empty list, which the old theme must not
	// fill.
	for _, raw := range []string{`[]`, `not json`} {
		if themes := ListFromSettings(map[string]string{SettingKey: legacy, ListKey: raw}); len(themes) != 0 {
			t.Errorf("%s: got %+v", raw, themes)
		}
	}
	if themes := ListFromSettings(map[string]string{}); len(themes) != 0 {
		t.Fatalf("no settings gave %+v", themes)
	}
}

func TestActiveFromSettings(t *testing.T) {
	list := `[` + savedJSON("a1", "Forest") + `,` + savedJSON("b2", "Night") + `]`
	for active, want := range map[string]string{"b2": "b2", "a1": "a1", "": "a1", "gone": "a1"} {
		theme, ok := ActiveFromSettings(map[string]string{ListKey: list, ActiveKey: active})
		if !ok || theme.ID != want {
			t.Errorf("active %q: got %q, %v, want %q", active, theme.ID, ok, want)
		}
	}
	if _, ok := ActiveFromSettings(map[string]string{ListKey: `[]`}); ok {
		t.Fatal("an empty list has no active theme")
	}
	if theme, ok := FromSettings(map[string]string{ListKey: list, ActiveKey: "b2"}); !ok || theme.Seed != "#16a34a" {
		t.Fatalf("FromSettings = %+v, %v", theme, ok)
	}
}

func TestCleanName(t *testing.T) {
	if name, ok := CleanName("  Forest at dusk "); !ok || name != "Forest at dusk" {
		t.Fatalf("CleanName = %q, %v", name, ok)
	}
	for _, bad := range []string{"", "  ", "a\tb", strings.Repeat("x", MaxNameLength+1)} {
		if _, ok := CleanName(bad); ok {
			t.Errorf("CleanName(%q) accepted", bad)
		}
	}
}

func TestSavedThemeColors(t *testing.T) {
	themes, ok := ParseList(`[{"id":"a1","name":"Forest",` + testTheme + `,"colors":{"light":{"primary":"#ABCDEF"}}},{"id":"b2","name":"Night",` + testTheme + `,"colors":{}}]`)
	if !ok {
		t.Fatal("ParseList refused hand-edited colors")
	}
	if themes[0].Colors["light"]["primary"] != "#abcdef" || themes[1].Colors != nil {
		t.Fatalf("colors = %v, %v", themes[0].Colors, themes[1].Colors)
	}
	if got := EncodeList(themes); !strings.Contains(got, `"colors":{"light":{"primary":"#abcdef"}}}`) || strings.Count(got, `"colors"`) != 1 {
		t.Fatalf("EncodeList = %s", got)
	}
	for _, colors := range []string{`{"light":{"primary":"red"}}`, `{"light":{"nope":"#123456"}}`, `{"sepia":{}}`, `[]`} {
		if _, ok := ParseList(`[{"id":"a1","name":"Forest",` + testTheme + `,"colors":` + colors + `}]`); ok {
			t.Errorf("colors %s accepted", colors)
		}
	}

	// The theme saved before names keeps its hand edits.
	legacy := ListFromSettings(map[string]string{SettingKey: `{` + testTheme + `}`, "theme_colors_custom": `{"light":{"primary":"#123456"}}`})
	if len(legacy) != 1 || legacy[0].Colors["light"]["primary"] != "#123456" {
		t.Fatalf("legacy hand edits = %+v", legacy)
	}

	// A card is drawn in its own palette, with its hand edits for its mode.
	css := legacy[0].CardCSS("#card")
	if !strings.HasPrefix(css, "#card{--radius:0px;") || !strings.Contains(css, "--primary:#123456;") || strings.Count(css, "{") != 1 {
		t.Fatalf("CardCSS = %s", css)
	}
}
