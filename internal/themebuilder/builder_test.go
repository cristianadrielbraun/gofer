package themebuilder

import (
	"fmt"
	"math"
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
	for _, sidebar := range []string{"lighter", "darker"} {
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
	for _, opts := range []Options{{Seed: "blue", Sidebar: "lighter"}, {Seed: "#36f", Sidebar: "lighter"}, {Seed: "#3366ff", Sidebar: "left"}} {
		if _, ok := Generate(opts); ok {
			t.Errorf("Generate(%+v) accepted", opts)
		}
	}
}

// Text pairs need WCAG AA, and AAA at high contrast; the primary and focus
// colors need 3:1 against what they sit on.
var textPairs = [][2]string{
	{"foreground", "background"}, {"foreground", "card"}, {"card-foreground", "card"},
	{"popover-foreground", "popover"}, {"secondary-foreground", "secondary"},
	{"accent-foreground", "accent"}, {"muted-foreground", "background"},
	{"muted-foreground", "card"}, {"muted-foreground", "muted"},
	{"sidebar-foreground", "sidebar"},
	{"sidebar-accent-foreground", "sidebar-accent"},
	{"paper-foreground", "paper"}, {"copper", "paper"}, {"ink", "background"},
	{"destructive", "background"}, {"destructive", "card"},
	{"success", "background"}, {"success", "card"},
}

// Text on filled buttons needs WCAG AA at any contrast, since high contrast
// keeps the buttons' colors.
var filledPairs = [][2]string{
	{"primary-foreground", "primary"}, {"sidebar-primary-foreground", "sidebar-primary"},
}

var uiPairs = [][2]string{
	{"primary", "background"}, {"primary", "card"}, {"sidebar-primary", "sidebar"},
}

// Secondary colors to pair with every seed: none, clashing, pale and grey.
var testSecondaries = []string{"", "#ff0000", "#fff7cc", "#0a246a", "#808080"}

func TestGenerateKeepsContrastForAnySeed(t *testing.T) {
	for _, seed := range testSeeds {
		for _, secondary := range testSecondaries {
			for _, tint := range []string{"primary", "secondary", "none"} {
				for _, strength := range []string{"subtle", "strong", "strongest"} {
					for _, contrast := range []string{"normal", "high", "highest"} {
						for _, sidebar := range []string{"lighter", "darker"} {
							checkContrast(t, Options{Seed: seed, Secondary: secondary, Tint: tint, Strength: strength, Contrast: contrast, Sidebar: sidebar})
						}
					}
				}
			}
		}
	}
}

func checkContrast(t *testing.T, opts Options) {
	t.Helper()
	palette, ok := Generate(opts)
	if !ok {
		t.Fatalf("Generate refused %+v", opts)
	}
	for _, mode := range themecolors.Modes {
		check := func(pairs [][2]string, min float64) {
			for _, pair := range pairs {
				if got := contrastOf(palette[mode][pair[0]], palette[mode][pair[1]]); got < min-0.005 {
					t.Errorf("%+v, %s: %s on %s = %.2f, want %.1f", opts, mode, pair[0], pair[1], got, min)
				}
			}
		}
		if opts.Contrast != "normal" {
			check(textPairs, highTextContrast)
		} else {
			check(textPairs, MinTextContrast)
		}
		check(filledPairs, MinTextContrast)
		check(uiPairs, minUIContrast)
	}
}

func TestSecondaryAndTintChooseHues(t *testing.T) {
	green := toLCH(mustRGB(t, "#16a34a")).h
	orange := toLCH(mustRGB(t, "#ea580c")).h
	palette, ok := Generate(Options{Seed: "#16a34a", Secondary: "#ea580c", Tint: "none", Sidebar: "lighter"})
	if !ok {
		t.Fatal("Generate refused valid options")
	}
	light := palette["light"]
	if h := toLCH(mustRGB(t, light["secondary"])).h; math.Abs(h-orange) > 0.3 {
		t.Errorf("secondary hue %.2f, want orange %.2f", h, orange)
	}
	if h := toLCH(mustRGB(t, light["primary"])).h; math.Abs(h-green) > 0.3 {
		t.Errorf("primary hue %.2f, want green %.2f", h, green)
	}
	if c := toLCH(mustRGB(t, light["background"])).c; c > 0.005 {
		t.Errorf("no tint gave background chroma %.3f", c)
	}

	byOrange, _ := Generate(Options{Seed: "#16a34a", Secondary: "#ea580c", Tint: "secondary", Sidebar: "lighter"})
	if h := toLCH(mustRGB(t, byOrange["light"]["background"])).h; math.Abs(h-orange) > 0.3 {
		t.Errorf("secondary tint gave background hue %.2f, want %.2f", h, orange)
	}
	if _, ok := Generate(Options{Seed: "#16a34a", Tint: "loud", Sidebar: "lighter"}); ok {
		t.Error("unknown tint accepted")
	}
	if _, ok := Generate(Options{Seed: "#16a34a", Strength: "loud", Sidebar: "lighter"}); ok {
		t.Error("unknown strength accepted")
	}

	strengths := []string{"subtle", "strong", "strongest"}
	for i := 1; i < len(strengths); i++ {
		weaker, _ := Generate(Options{Seed: "#16a34a", Strength: strengths[i-1], Sidebar: "lighter"})
		stronger, _ := Generate(Options{Seed: "#16a34a", Strength: strengths[i], Sidebar: "lighter"})
		for _, mode := range themecolors.Modes {
			for _, name := range []string{"background", "card", "border"} {
				weak, bold := toLCH(mustRGB(t, weaker[mode][name])).c, toLCH(mustRGB(t, stronger[mode][name])).c
				// Dark surfaces can't show much more color, so the step is
				// smaller than the chroma asked for.
				if bold < weak*1.3 {
					t.Errorf("%s %s: %s tint chroma %.3f, %s %.3f", mode, name, strengths[i], bold, strengths[i-1], weak)
				}
			}
		}
	}
	if _, ok := Generate(Options{Seed: "#16a34a", Secondary: "orange", Sidebar: "lighter"}); ok {
		t.Error("bad secondary accepted")
	}
}

func mustRGB(t *testing.T, hex string) rgb {
	t.Helper()
	c, ok := parseHex(hex)
	if !ok {
		t.Fatalf("bad hex %q", hex)
	}
	return c
}

func TestGreySeedGivesGreyPalette(t *testing.T) {
	palette, _ := Generate(Options{Seed: "#808080", Sidebar: "lighter"})
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
	green, _ := Generate(Options{Seed: "#16a34a", Sidebar: "lighter"})
	blue, _ := Generate(Options{Seed: "#2563eb", Sidebar: "lighter"})
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

func TestHigherContrastRaisesText(t *testing.T) {
	levels := []string{"normal", "high", "highest"}
	for _, sidebar := range []string{"lighter", "darker"} {
		for i := 1; i < len(levels); i++ {
			lower, _ := Generate(Options{Seed: "#16a34a", Contrast: levels[i-1], Sidebar: sidebar})
			higher, _ := Generate(Options{Seed: "#16a34a", Contrast: levels[i], Sidebar: sidebar})
			for _, mode := range themecolors.Modes {
				for _, pair := range [][2]string{{"foreground", "background"}, {"muted-foreground", "background"}, {"border", "background"}, {"sidebar-foreground", "sidebar"}, {"sidebar-border", "sidebar"}} {
					before := contrastOf(lower[mode][pair[0]], lower[mode][pair[1]])
					after := contrastOf(higher[mode][pair[0]], higher[mode][pair[1]])
					if after <= before {
						t.Errorf("%s %s sidebar, %s on %s: %s %.2f, %s %.2f", mode, sidebar, pair[0], pair[1], levels[i], after, levels[i-1], before)
					}
				}
			}
		}
	}
}

func TestParseTheme(t *testing.T) {
	theme, ok := Parse(`{"seed":"#16A34A","sidebar":"dark","depth":"flat","corners":"square"}`)
	if !ok {
		t.Fatal("Parse refused a valid theme")
	}
	// Themes saved before the mode existed are light, and their light or dark
	// sidebar is a lighter or darker one.
	if got := theme.Encode(); got != `{"mode":"light","seed":"#16a34a","tint":"primary","strength":"subtle","contrast":"normal","sidebar":"darker","depth":"flat","corners":"square"}` {
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
		`{"seed":"#16a34a","secondary":"sand","sidebar":"dark","depth":"flat","corners":"square"}`,
		`{"seed":"#16a34a","tint":"loud","sidebar":"dark","depth":"flat","corners":"square"}`,
		`{"seed":"#16a34a","strength":"loud","sidebar":"dark","depth":"flat","corners":"square"}`,
		`{"seed":"#16a34a","contrast":"loud","sidebar":"dark","depth":"flat","corners":"square"}`,
		`{"mode":"dim","seed":"#16a34a","sidebar":"dark","depth":"flat","corners":"square"}`,
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
	palette, _ := Generate(Options{Seed: "#16a34a", Sidebar: "lighter"})
	scope := `[data-theme="custom"],[data-theme="custom"].dark{`
	css := theme.CSS()
	for _, want := range []string{`[data-theme="custom"]{--radius:1rem;}`, scope + "--accent:" + palette["light"]["accent"] + ";"} {
		if !strings.Contains(css, want) {
			t.Errorf("CSS lacks %s\n%s", want, css)
		}
	}
	if strings.Contains(css, ":not(.dark)") {
		t.Errorf("a light theme has one palette, for either mode:\n%s", css)
	}
	// A message body shown dark reads the dark mode's message colors.
	probe := func(p map[string]string) string {
		return "{--copper:" + p["copper"] + ";--paper:" + p["paper"] + ";--paper-foreground:" + p["paper-foreground"] + ";}"
	}
	if want := `[data-theme="custom"][data-theme-probe].dark` + probe(palette["dark"]); !strings.Contains(css, want) {
		t.Errorf("CSS lacks %s\n%s", want, css)
	}

	// A dark theme carries the dark palette, and light message colors for a
	// body shown light.
	theme, _ = Parse(`{"mode":"dark","seed":"#16a34a","sidebar":"light","depth":"raised","corners":"large"}`)
	css = theme.CSS()
	if !theme.Dark() || !strings.Contains(css, scope+"--accent:"+palette["dark"]["accent"]+";") {
		t.Errorf("dark theme CSS lacks the dark palette:\n%s", css)
	}
	if want := `[data-theme="custom"][data-theme-probe]:not(.dark)` + probe(palette["light"]); !strings.Contains(css, want) {
		t.Errorf("CSS lacks %s\n%s", want, css)
	}
}

// The sidebar is darker or lighter than the panes in either mode.
func TestSidebarAgainstPanes(t *testing.T) {
	lightness := func(hex string) float64 { return toLCH(mustRGB(t, hex)).l }
	darker, _ := Generate(Options{Seed: "#16a34a", Sidebar: "darker"})
	lighter, _ := Generate(Options{Seed: "#16a34a", Sidebar: "lighter"})
	for _, mode := range []string{"light", "dark"} {
		background := lightness(darker[mode]["background"])
		if l := lightness(darker[mode]["sidebar"]); l >= background {
			t.Errorf("%s: darker sidebar %.2f is not darker than the background %.2f", mode, l, background)
		}
		if l := lightness(lighter[mode]["sidebar"]); l <= lightness(darker[mode]["sidebar"]) {
			t.Errorf("%s: lighter sidebar %.2f is not lighter than the darker one", mode, l)
		}
	}
	for _, mode := range []string{"light", "dark"} {
		if l := lightness(lighter[mode]["sidebar"]); l <= lightness(lighter[mode]["background"]) {
			t.Errorf("%s: lighter sidebar %.2f is not lighter than the background", mode, l)
		}
	}
}
