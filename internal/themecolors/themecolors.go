// Package themecolors holds the colors a user changed in a built-in style.
// Each style keeps its own changes, for light and dark mode, and everything
// else about the style stays as it ships.
package themecolors

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// SettingPrefix starts the UI setting that stores a style's changes as JSON,
// such as theme_colors_classic.
const SettingPrefix = "theme_colors_"

// Styles are the styles whose colors can be changed. Express is left out:
// its bevelled look comes from its own variables.
var Styles = []string{"classic", "minimal"}

// Modes are the color modes each style has its own colors for.
var Modes = []string{"light", "dark"}

// Variable is a color a style defines, as the editor names it.
type Variable struct {
	Name  string
	Label string
	Hint  string
}

// Group is a set of related colors in the editor.
type Group struct {
	Label     string
	Variables []Variable
}

// Groups are every color a style defines, in the editor's order.
var Groups = []Group{
	{"Page", []Variable{
		{"background", "Background", "Behind the mail list and panes"},
		{"foreground", "Text", "Main text"},
		{"ink", "Ink", "Strong text and fine lines"},
	}},
	{"Primary", []Variable{
		{"primary", "Primary", "Main buttons, links and selection"},
		{"primary-foreground", "Text on primary", ""},
		{"ring", "Focus ring", "Outline around the focused control"},
	}},
	{"Secondary", []Variable{
		{"secondary", "Secondary", "Secondary buttons and chips"},
		{"secondary-foreground", "Text on secondary", ""},
	}},
	{"Accent", []Variable{
		{"accent", "Accent", "Hovered and highlighted items"},
		{"accent-foreground", "Text on accent", ""},
	}},
	{"Muted", []Variable{
		{"muted", "Muted", "Quiet backgrounds"},
		{"muted-foreground", "Secondary text", "Dates, hints and labels"},
	}},
	{"Panels", []Variable{
		{"card", "Card", "Cards and panels"},
		{"card-foreground", "Text on cards", ""},
		{"popover", "Menu", "Menus and popovers"},
		{"popover-foreground", "Text in menus", ""},
	}},
	{"Borders", []Variable{
		{"border", "Border", "Lines between areas"},
		{"input", "Input border", "Edges of fields"},
	}},
	{"Sidebar", []Variable{
		{"sidebar", "Sidebar", "Sidebar background"},
		{"sidebar-foreground", "Sidebar text", ""},
		{"sidebar-primary", "Sidebar primary", "Active folder and counts"},
		{"sidebar-primary-foreground", "Text on sidebar primary", ""},
		{"sidebar-accent", "Sidebar accent", "Hovered folders"},
		{"sidebar-accent-foreground", "Text on sidebar accent", ""},
		{"sidebar-border", "Sidebar border", ""},
		{"sidebar-ring", "Sidebar focus ring", ""},
	}},
	{"Reading pane", []Variable{
		{"paper", "Message background", "Behind message bodies"},
		{"paper-foreground", "Message text", ""},
		{"copper", "Message links", "Links and highlights in messages"},
	}},
	{"Status", []Variable{
		{"destructive", "Destructive", "Delete and error"},
		{"success", "Success", "Confirmations"},
		{"brass", "Detail", "Small decorative details"},
	}},
}

var known = func() map[string]bool {
	names := map[string]bool{}
	for _, group := range Groups {
		for _, v := range group.Variables {
			names[v.Name] = true
		}
	}
	return names
}()

// Mail list rows set their own --primary and --muted-foreground in Classic,
// so a change to either is repeated there.
var mailListVariables = []string{"primary", "muted-foreground"}

// Overrides are a style's changed colors: mode, then variable, then #rrggbb.
type Overrides map[string]map[string]string

// SettingKey is the UI setting for a style's changes.
func SettingKey(style string) string {
	return SettingPrefix + style
}

// IsStyle reports whether a style's colors can be changed.
func IsStyle(style string) bool {
	for _, s := range Styles {
		if s == style {
			return true
		}
	}
	return false
}

// Parse reads stored changes. It refuses anything but known modes, colors the
// style defines and #rrggbb values, since they end up in a <style> block.
// Empty input is no changes.
func Parse(raw string) (Overrides, bool) {
	if strings.TrimSpace(raw) == "" {
		return Overrides{}, true
	}
	var in map[string]map[string]string
	if json.Unmarshal([]byte(raw), &in) != nil {
		return nil, false
	}
	out := Overrides{}
	for mode, values := range in {
		if mode != "light" && mode != "dark" {
			return nil, false
		}
		for name, value := range values {
			value = strings.ToLower(strings.TrimSpace(value))
			if !known[name] || !isHex(value) {
				return nil, false
			}
			if out[mode] == nil {
				out[mode] = map[string]string{}
			}
			out[mode][name] = value
		}
	}
	return out, true
}

// FromSettings is a style's saved changes, or none if they don't parse.
func FromSettings(settings map[string]string, style string) Overrides {
	if !IsStyle(style) {
		return Overrides{}
	}
	o, ok := Parse(settings[SettingKey(style)])
	if !ok {
		return Overrides{}
	}
	return o
}

// Empty reports whether no color is changed.
func (o Overrides) Empty() bool {
	for _, values := range o {
		if len(values) > 0 {
			return false
		}
	}
	return true
}

// Encode is the stored form, or "" for no changes.
func (o Overrides) Encode() string {
	if o.Empty() {
		return ""
	}
	raw, _ := json.Marshal(o)
	return string(raw)
}

// CSS overrides the style's variables on the page, on the style previews in
// Settings and on the reading pane's color probe. The selectors outrank the
// style's own rules, and the light rule skips dark mode so a light change
// never shows there.
func (o Overrides) CSS(style string) string {
	if !IsStyle(style) || o.Empty() {
		return ""
	}
	root := fmt.Sprintf(`:is(html,[data-theme-preview],[data-theme-probe])[data-theme="%s"]`, style)
	scopes := map[string]string{"light": root + ":not(.dark)", "dark": root + ".dark"}
	var b strings.Builder
	for _, mode := range Modes {
		values := o[mode]
		if len(values) == 0 {
			continue
		}
		b.WriteString(scopes[mode] + "{")
		for _, name := range sortedNames(values) {
			fmt.Fprintf(&b, "--%s:%s;", name, values[name])
		}
		b.WriteString("}")
		var rows strings.Builder
		for _, name := range mailListVariables {
			if value, ok := values[name]; ok {
				fmt.Fprintf(&rows, "--%s:%s;", name, value)
			}
		}
		if rows.Len() > 0 {
			fmt.Fprintf(&b, "%s .mail-list-item{%s}", scopes[mode], rows.String())
		}
	}
	return b.String()
}

func sortedNames(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func isHex(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for _, c := range s[1:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
