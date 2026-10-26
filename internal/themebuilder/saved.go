package themebuilder

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
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

// Saved is a Custom theme with its ID and name, as the list stores it.
type Saved struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Theme
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
			ID   string `json:"id"`
			Name string `json:"name"`
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
		seen[head.ID] = true
		themes = append(themes, Saved{ID: head.ID, Name: name, Theme: theme})
	}
	return themes, true
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
		return []Saved{{ID: LegacyID, Name: LegacyName, Theme: theme}}
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
