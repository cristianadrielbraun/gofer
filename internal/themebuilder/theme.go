package themebuilder

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/themecolors"
)

// SettingKey is the UI setting that stores the Custom theme's builder choices
// as JSON. The palette is made from them whenever a page is drawn, so the
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
	Seed    string `json:"seed"`
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
	t.Seed = strings.ToLower(strings.TrimSpace(t.Seed))
	if _, ok := parseHex(t.Seed); !ok {
		return Theme{}, false
	}
	if t.Sidebar != "light" && t.Sidebar != "dark" {
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

// FromSettings is the saved Custom theme, if there is a valid one.
func FromSettings(settings map[string]string) (Theme, bool) {
	return Parse(settings[SettingKey])
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
// ships with; hand edits from themecolors use stronger selectors and win.
func (t Theme) CSS() string {
	palette, ok := Generate(Options{Seed: t.Seed, Sidebar: t.Sidebar})
	if !ok {
		return ""
	}
	scope := fmt.Sprintf(`[data-theme="%s"]`, StyleName)
	var b strings.Builder
	fmt.Fprintf(&b, "%s{--radius:%s;}", scope, radii[t.Corners])
	for _, mode := range themecolors.Modes {
		selector := scope + ":not(.dark)"
		if mode == "dark" {
			selector = scope + ".dark"
		}
		values := palette[mode]
		names := make([]string, 0, len(values))
		for name := range values {
			names = append(names, name)
		}
		sort.Strings(names)
		b.WriteString(selector + "{")
		for _, name := range names {
			fmt.Fprintf(&b, "--%s:%s;", name, values[name])
		}
		b.WriteString("}")
	}
	return b.String()
}
