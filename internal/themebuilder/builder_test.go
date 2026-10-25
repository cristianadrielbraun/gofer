package themebuilder

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/themecolors"
)

// Seeds that tend to break palettes: greys, black and white, very light and
// very dark colors, and vivid ones at every hue.
var testSeeds = func() []string {
	seeds := []string{
		"#000000", "#ffffff", "#808080", "#111827", "#f5f5f4", "#3b3b3b",
		"#ffff00", "#fff7cc", "#ffe4e1", "#e0f7fa", "#00ff00", "#00ffff",
		"#0000ff", "#ff00ff", "#ff0000", "#1a0033", "#002200", "#330000",
		"#8f4c14", "#0a246a", "#14532d", "#7c3aed", "#ec4899", "#f97316",
	}
	// Vivid, mid and pale colors around the hue wheel.
	for h := 0; h < 360; h += 30 {
		for _, lc := range [][2]float64{{0.62, 0.2}, {0.45, 0.12}, {0.85, 0.08}} {
			seeds = append(seeds, hue(lc[0], lc[1], float64(h)).hex())
		}
	}
	return seeds
}()

func TestGenerateDefinesEveryEditorColor(t *testing.T) {
	for _, sidebar := range []string{"light", "dark"} {
		palette, ok := Generate(Options{Seed: "#2563eb", Sidebar: sidebar})
		if !ok {
			t.Fatal("Generate refused a valid seed")
		}
		for _, mode := range themecolors.Modes {
			for _, group := range themecolors.Groups {
				for _, v := range group.Variables {
					value := palette[mode][v.Name]
					if _, ok := parseHex(value); !ok {
						t.Errorf("%s sidebar, %s: %s = %q", sidebar, mode, v.Name, value)
					}
				}
			}
			if len(palette[mode]) != countEditorColors() {
				t.Errorf("%s sidebar, %s: %d colors, editor lists %d", sidebar, mode, len(palette[mode]), countEditorColors())
			}
		}
	}
}

func TestGenerateRefusesBadOptions(t *testing.T) {
	for _, opts := range []Options{{Seed: "blue", Sidebar: "light"}, {Seed: "#36f", Sidebar: "light"}, {Seed: "#3366ff", Sidebar: "left"}} {
		if _, ok := Generate(opts); ok {
			t.Errorf("Generate(%+v) accepted", opts)
		}
	}
}

// Text pairs need WCAG AA; the primary and focus colors need 3:1 against
// what they sit on.
var textPairs = [][2]string{
	{"foreground", "background"}, {"foreground", "card"}, {"card-foreground", "card"},
	{"popover-foreground", "popover"}, {"secondary-foreground", "secondary"},
	{"accent-foreground", "accent"}, {"muted-foreground", "background"},
	{"muted-foreground", "card"}, {"muted-foreground", "muted"},
	{"primary-foreground", "primary"}, {"sidebar-foreground", "sidebar"},
	{"sidebar-accent-foreground", "sidebar-accent"},
	{"sidebar-primary-foreground", "sidebar-primary"},
	{"paper-foreground", "paper"}, {"copper", "paper"}, {"ink", "background"},
	{"destructive", "background"}, {"destructive", "card"},
	{"success", "background"}, {"success", "card"},
}

var uiPairs = [][2]string{
	{"primary", "background"}, {"primary", "card"}, {"sidebar-primary", "sidebar"},
}

func TestGenerateKeepsContrastForAnySeed(t *testing.T) {
	for _, seed := range testSeeds {
		for _, sidebar := range []string{"light", "dark"} {
			palette, ok := Generate(Options{Seed: seed, Sidebar: sidebar})
			if !ok {
				t.Fatalf("Generate refused %s", seed)
			}
			for _, mode := range themecolors.Modes {
				check := func(pairs [][2]string, min float64) {
					for _, pair := range pairs {
						if got := contrastOf(palette[mode][pair[0]], palette[mode][pair[1]]); got < min-0.005 {
							t.Errorf("seed %s, %s sidebar, %s: %s on %s = %.2f, want %.1f", seed, sidebar, mode, pair[0], pair[1], got, min)
						}
					}
				}
				check(textPairs, MinTextContrast)
				check(uiPairs, minUIContrast)
			}
		}
	}
}

func TestGreySeedGivesGreyPalette(t *testing.T) {
	palette, _ := Generate(Options{Seed: "#808080", Sidebar: "light"})
	for _, mode := range themecolors.Modes {
		for _, name := range []string{"background", "card", "sidebar", "border"} {
			c, _ := parseHex(palette[mode][name])
			if chroma := toLCH(c).c; chroma > 0.005 {
				t.Errorf("%s %s has chroma %.3f: %s", mode, name, chroma, palette[mode][name])
			}
		}
	}
}

func TestSeedHueCarriesIntoSurfaces(t *testing.T) {
	green, _ := Generate(Options{Seed: "#16a34a", Sidebar: "light"})
	blue, _ := Generate(Options{Seed: "#2563eb", Sidebar: "light"})
	if green["light"]["background"] == blue["light"]["background"] {
		t.Fatal("different seeds gave the same background")
	}
	c, _ := parseHex(green["light"]["secondary"])
	if g := toLCH(c); g.c < 0.01 {
		t.Fatalf("green seed gave a grey secondary: %s", green["light"]["secondary"])
	}
}

func contrastOf(a, b string) float64 {
	ca, okA := parseHex(a)
	cb, okB := parseHex(b)
	if !okA || !okB {
		panic(fmt.Sprintf("bad colors %q %q", a, b))
	}
	return contrast(ca, cb)
}

func countEditorColors() int {
	n := 0
	for _, group := range themecolors.Groups {
		n += len(group.Variables)
	}
	return n
}

func TestParseTheme(t *testing.T) {
	theme, ok := Parse(`{"seed":"#16A34A","sidebar":"dark","depth":"flat","corners":"square"}`)
	if !ok {
		t.Fatal("Parse refused a valid theme")
	}
	if got := theme.Encode(); got != `{"seed":"#16a34a","sidebar":"dark","depth":"flat","corners":"square"}` {
		t.Fatalf("Encode = %s", got)
	}
	attrs := theme.LookAttributes()
	if attrs["data-depth"] != "flat" || attrs["data-corners"] != "square" {
		t.Fatalf("LookAttributes = %v", attrs)
	}
	for _, raw := range []string{
		``,
		`{"seed":"green","sidebar":"dark","depth":"flat","corners":"square"}`,
		`{"seed":"#16a34a","sidebar":"left","depth":"flat","corners":"square"}`,
		`{"seed":"#16a34a","sidebar":"dark","depth":"bevel","corners":"square"}`,
		`{"seed":"#16a34a","sidebar":"dark","depth":"flat","corners":"9px"}`,
	} {
		if _, ok := Parse(raw); ok {
			t.Errorf("Parse(%q) accepted", raw)
		}
	}
}

func TestThemeCSS(t *testing.T) {
	theme, _ := Parse(`{"seed":"#16a34a","sidebar":"light","depth":"raised","corners":"large"}`)
	if attrs := theme.LookAttributes(); len(attrs) != 0 {
		t.Fatalf("raised, round theme has look attributes %v", attrs)
	}
	css := theme.CSS()
	for _, want := range []string{`[data-theme="custom"]{--radius:1rem;}`, `[data-theme="custom"]:not(.dark){--accent:`, `[data-theme="custom"].dark{--accent:`} {
		if !strings.Contains(css, want) {
			t.Errorf("CSS lacks %s", want)
		}
	}
}
