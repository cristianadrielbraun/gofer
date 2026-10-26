package themebuilder

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/cristianadrielbraun/gofer/internal/themecolors"
)

// ListKey is the UI setting that stores every saved Custom theme, as a JSON
// list. Once it is set, even to an empty list, it replaces SettingKey, which
// held the one theme there was before themes had names.
const ListKey = "custom_themes"

// ActiveKey is the UI setting naming, by ID, the Custom theme the page is
// drawn with when the Custom style is chosen. The first theme is used when it
// names none that exists.
const ActiveKey = "custom_theme_active"

// MaxThemes is how many Custom themes can be saved.
const MaxThemes = 12

// MaxNameLength is the longest theme name, in characters.
const MaxNameLength = 40

// LegacyID and LegacyName are given to the theme saved before themes had
// names, when it is read as the first of the list.
const (
	LegacyID   = "theme1"
	LegacyName = "My theme"
)

var idPattern = regexp.MustCompile(`^[a-z0-9]{1,24}$`)

// Saved is a Custom theme with its ID and name, as the list stores it, and
// the colors changed by hand in the color editor since it was built.
type Saved struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Theme
	Colors themecolors.Overrides `json:"colors,omitempty"`
}

// ValidID reports whether id can name a saved theme.
func ValidID(id string) bool {
	return idPattern.MatchString(id)
}

// CleanName is name trimmed, or false if it is empty, too long or has
// control characters.
func CleanName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > MaxNameLength {
		return "", false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return name, true
}

// ParseList reads the saved themes, refusing the whole list if any theme in
// it does not parse, two share an ID, or there are more than MaxThemes.
func ParseList(raw string) ([]Saved, bool) {
	var entries []json.RawMessage
	if json.Unmarshal([]byte(raw), &entries) != nil || len(entries) > MaxThemes {
		return nil, false
	}
	themes := make([]Saved, 0, len(entries))
	seen := map[string]bool{}
	for _, entry := range entries {
		var head struct {
			ID     string          `json:"id"`
			Name   string          `json:"name"`
			Colors json.RawMessage `json:"colors"`
		}
		if json.Unmarshal(entry, &head) != nil || !ValidID(head.ID) || seen[head.ID] {
			return nil, false
		}
		name, ok := CleanName(head.Name)
		if !ok {
			return nil, false
		}
		theme, ok := Parse(string(entry))
		if !ok {
			return nil, false
		}
		colors, ok := parseColors(head.Colors)
		if !ok {
			return nil, false
		}
		seen[head.ID] = true
		themes = append(themes, Saved{ID: head.ID, Name: name, Theme: theme, Colors: colors})
	}
	return themes, true
}

// parseColors reads a theme's hand-edited colors; none is nil, so the stored
// form leaves them out.
func parseColors(raw json.RawMessage) (themecolors.Overrides, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, true
	}
	colors, ok := themecolors.Parse(string(raw))
	if !ok {
		return nil, false
	}
	if colors.Empty() {
		return nil, true
	}
	return colors, true
}

// EncodeList is the stored form of themes from ParseList.
func EncodeList(themes []Saved) string {
	if themes == nil {
		themes = []Saved{}
	}
	raw, _ := json.Marshal(themes)
	return string(raw)
}

// ListFromSettings is every saved Custom theme. Before the list existed one
// theme was kept on its own, and it is read as the first of the list.
func ListFromSettings(settings map[string]string) []Saved {
	if raw := strings.TrimSpace(settings[ListKey]); raw != "" {
		themes, _ := ParseList(raw)
		return themes
	}
	if theme, ok := Parse(settings[SettingKey]); ok {
		saved := Saved{ID: LegacyID, Name: LegacyName, Theme: theme}
		if colors := themecolors.FromSettings(settings, StyleName); !colors.Empty() {
			saved.Colors = colors
		}
		return []Saved{saved}
	}
	return nil
}

// ActiveFromSettings is the Custom theme the page is drawn with when the
// Custom style is chosen, if any is saved.
func ActiveFromSettings(settings map[string]string) (Saved, bool) {
	themes := ListFromSettings(settings)
	if len(themes) == 0 {
		return Saved{}, false
	}
	active := settings[ActiveKey]
	for _, theme := range themes {
		if theme.ID == active {
			return theme, true
		}
	}
	return themes[0], true
}

// CardCSS is the theme's palette, with its hand-edited colors, and corner
// radius on the elements selector picks, such as its card's preview in
// Settings, where every theme is shown in its own colors at once.
func (s Saved) CardCSS(selector string) string {
	values, ok := s.colors()
	if !ok {
		return ""
	}
	for name, value := range s.Colors[s.Mode] {
		values[name] = value
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s{--radius:%s;", selector, radii[s.Corners])
	for _, name := range sortedNames(values) {
		fmt.Fprintf(&b, "--%s:%s;", name, values[name])
	}
	b.WriteString("}")
	return b.String()
}
