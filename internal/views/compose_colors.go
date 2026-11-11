package views

// composeColor is a swatch in the compose color menus. Name is its message
// key under compose.color.
type composeColor struct {
	Name  string
	Value string
}

// Text colors are mid tones, readable on light and dark backgrounds.
var composeTextColors = []composeColor{
	{"gray", "#6b7280"}, {"red", "#dc2626"}, {"orange", "#ea580c"},
	{"yellow", "#ca8a04"}, {"green", "#16a34a"}, {"teal", "#0d9488"},
	{"blue", "#2563eb"}, {"purple", "#9333ea"}, {"pink", "#db2777"},
}

// Highlights are light; highlighted text is made dark so it stays readable.
var composeHighlightColors = []composeColor{
	{"gray", "#e5e7eb"}, {"red", "#fecaca"}, {"orange", "#fed7aa"},
	{"yellow", "#fef08a"}, {"green", "#bbf7d0"}, {"teal", "#99f6e4"},
	{"blue", "#bfdbfe"}, {"purple", "#e9d5ff"}, {"pink", "#fbcfe8"},
}
