package themebuilder

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// SettingKey is the UI setting that held the one Custom theme's builder
// choices, as JSON, before themes had names; ListKey has replaced it. A
// theme's palette is made from its choices whenever a page is drawn, so the
// generator's improvements reach saved themes.
const SettingKey = "custom_theme"

// StyleName is the theme_style value, and data-theme, of the Custom theme.
const StyleName = "custom"

var radii = map[string]string{
	"square": "0px",
	"small":  "0.375rem",
	"medium": "0.65rem",
	"large":  "1rem",
}

// Theme is what the builder saves.
type Theme struct {
	// Mode is "light" or "dark". The theme has only that palette, and the app
	// shows it in that mode whatever the color mode setting says.
	Mode string `json:"mode"`
	Seed string `json:"seed"`
	// Secondary is empty to follow Seed.
	Secondary string `json:"secondary,omitempty"`
	// Tint is "primary", "secondary" or "none".
	Tint string `json:"tint"`
	// Strength is "subtle", "strong" or "strongest".
	Strength string `json:"strength"`
	// Contrast is "normal", "high" or "highest".
	Contrast string `json:"contrast"`
	// Sidebar is "darker" or "lighter" than the panes.
	Sidebar string `json:"sidebar"`
	// Depth is "raised", with Classic's shadows and bevels, or "flat".
	Depth   string `json:"depth"`
	Corners string `json:"corners"`
}

// Preset is a suggested seed color in the builder.
type Preset struct {
	Name  string
	Color string
}

// Presets are the builder's suggested colors.
var Presets = []Preset{
	{"Blue", "#2563eb"},
	{"Indigo", "#4f46e5"},
	{"Violet", "#7c3aed"},
	{"Pink", "#db2777"},
	{"Red", "#dc2626"},
	{"Orange", "#ea580c"},
	{"Amber", "#d97706"},
	{"Green", "#16a34a"},
	{"Teal", "#0d9488"},
	{"Slate", "#475569"},
}

// Parse reads saved builder choices, refusing anything unknown, since the
// values decide what goes in a <style> block.
func Parse(raw string) (Theme, bool) {
	var t Theme
	if strings.TrimSpace(raw) == "" || json.Unmarshal([]byte(raw), &t) != nil {
		return Theme{}, false
	}
	// Themes saved before the mode existed are light.
	if t.Mode == "" {
		t.Mode = "light"
	}
	if t.Mode != "light" && t.Mode != "dark" {
		return Theme{}, false
	}
	t.Seed = strings.ToLower(strings.TrimSpace(t.Seed))
	if _, ok := parseHex(t.Seed); !ok {
		return Theme{}, false
	}
	t.Secondary = strings.ToLower(strings.TrimSpace(t.Secondary))
	if _, ok := parseHex(t.Secondary); t.Secondary != "" && !ok {
		return Theme{}, false
	}
	// Themes saved before the tint existed took a subtle one from the
	// primary color.
	if t.Tint == "" {
		t.Tint = "primary"
	}
	if t.Tint != "primary" && t.Tint != "secondary" && t.Tint != "none" {
		return Theme{}, false
	}
	if t.Strength == "" {
		t.Strength = "subtle"
	}
	if t.Strength != "subtle" && t.Strength != "strong" && t.Strength != "strongest" {
		return Theme{}, false
	}
	if t.Contrast == "" {
		t.Contrast = "normal"
	}
	if t.Contrast != "normal" && t.Contrast != "high" && t.Contrast != "highest" {
		return Theme{}, false
	}
	// Before the sidebar was relative to the panes, it was light or dark.
	switch t.Sidebar {
	case "dark":
		t.Sidebar = "darker"
	case "light":
		t.Sidebar = "lighter"
	}
	if t.Sidebar != "darker" && t.Sidebar != "lighter" {
		return Theme{}, false
	}
	if t.Depth != "raised" && t.Depth != "flat" {
		return Theme{}, false
	}
	if _, ok := radii[t.Corners]; !ok {
		return Theme{}, false
	}
	return t, true
}

// FromSettings is the active Custom theme's choices, if one is saved.
func FromSettings(settings map[string]string) (Theme, bool) {
	saved, ok := ActiveFromSettings(settings)
	return saved.Theme, ok
}

// Encode is the stored form of a theme from Parse.
func (t Theme) Encode() string {
	raw, _ := json.Marshal(t)
	return string(raw)
}

// LookAttributes are the page root's attributes for the theme's look:
// data-depth="flat" and data-corners="square" when chosen.
func (t Theme) LookAttributes() map[string]string {
	attrs := map[string]string{}
	if t.Depth == "flat" {
		attrs["data-depth"] = "flat"
	}
	if t.Corners == "square" {
		attrs["data-corners"] = "square"
	}
	return attrs
}

// CSS is the theme's palette and corner radius. It applies to every element
// with data-theme="custom", so the color editor can read the colors the theme
// ships with; hand edits from themecolors use stronger selectors and win. The
// palette is for the theme's mode, and applies in either, since the page
// follows the theme's mode.
func (t Theme) CSS() string {
	values, ok := t.colors()
	if !ok {
		return ""
	}
	scope := fmt.Sprintf(`[data-theme="%s"]`, StyleName)
	names := sortedNames(values)
	var b strings.Builder
	fmt.Fprintf(&b, "%s{--radius:%s;}", scope, radii[t.Corners])
	// The .dark form outranks the built-in styles' .dark rules.
	fmt.Fprintf(&b, "%s,%s.dark{", scope, scope)
	for _, name := range names {
		fmt.Fprintf(&b, "--%s:%s;", name, values[name])
	}
	b.WriteString("}")
	return b.String()
}

// colors is the palette for the theme's mode.
func (t Theme) colors() (map[string]string, bool) {
	palette, ok := Generate(Options{Seed: t.Seed, Secondary: t.Secondary, Tint: t.Tint, Strength: t.Strength, Contrast: t.Contrast, Sidebar: t.Sidebar})
	if !ok {
		return nil, false
	}
	return palette[t.Mode], true
}

func sortedNames(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Dark reports whether the theme is a dark one.
func (t Theme) Dark() bool {
	return t.Mode == "dark"
}
