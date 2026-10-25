// Package themebuilder makes a whole color palette, for light and dark mode,
// from one color. The lightness of every role is fixed here, so contrast is
// the same whatever the color; the color gives the palette its hue and how
// colorful it is. Text that still falls short of WCAG AA is nudged until it
// passes.
package themebuilder

import (
	"math"
	"strings"
)

// Options are what the builder asks for.
type Options struct {
	// Seed is the theme's color, as #rrggbb.
	Seed string
	// Sidebar is "light" or "dark" in light mode. Dark mode always has a dark
	// sidebar.
	Sidebar string
}

// MinTextContrast is WCAG AA for body text.
const MinTextContrast = 4.5

// minUIContrast is WCAG AA for controls and focus outlines.
const minUIContrast = 3.0

// Palette is every color a theme defines: mode, then variable, then #rrggbb.
type Palette map[string]map[string]string

// Generate makes the palette for the options, or false if the seed is not a
// #rrggbb color or the sidebar is unknown.
func Generate(opts Options) (Palette, bool) {
	seed, ok := parseHex(strings.ToLower(strings.TrimSpace(opts.Seed)))
	if !ok || (opts.Sidebar != "light" && opts.Sidebar != "dark") {
		return nil, false
	}
	b := newBuilder(seed)
	return Palette{
		"light": b.light(opts.Sidebar == "dark"),
		"dark":  b.dark(),
	}, true
}

type builder struct {
	seed lch
	// chroma is how colorful the seed is, capped so a vivid seed doesn't give
	// loud surfaces. A grey seed has none, and gives a grey palette.
	chroma float64
}

func newBuilder(seed rgb) builder {
	c := toLCH(seed)
	chroma := math.Min(c.c, 0.2)
	if chroma < 0.02 {
		chroma = 0
	}
	return builder{seed: c, chroma: chroma}
}

// tone is a neutral of the seed's hue: the share of the seed's chroma it keeps
// sets how tinted it is.
func (b builder) tone(l, share float64) lch {
	return lch{l, b.chroma * share, b.seed.h}
}

// accent is the seed itself, with its lightness kept inside [minL, maxL].
func (b builder) accent(minL, maxL float64) lch {
	c := b.seed
	c.l = math.Max(minL, math.Min(maxL, c.l))
	return c
}

// hue is a fixed-hue color, such as red for destructive actions, with some of
// the seed's chroma so it sits with the rest.
func hue(l, c, degrees float64) lch {
	return lch{l, c, degrees * math.Pi / 180}
}

func (b builder) light(darkSidebar bool) map[string]string {
	p := map[string]string{}
	background := b.tone(0.965, 0.10)
	card := b.tone(0.99, 0.05)
	muted := b.tone(0.93, 0.12)
	secondary := b.tone(0.91, 0.18)
	accent := b.tone(0.935, 0.25)
	paper := b.tone(0.995, 0.03)
	text := b.tone(0.22, 0.10)

	set(p, "background", background)
	set(p, "card", card)
	set(p, "popover", card)
	set(p, "muted", muted)
	set(p, "secondary", secondary)
	set(p, "accent", accent)
	set(p, "border", b.tone(0.84, 0.14))
	set(p, "input", b.tone(0.78, 0.14))
	set(p, "paper", paper)

	for _, name := range []string{"foreground", "card-foreground", "popover-foreground", "secondary-foreground", "accent-foreground", "paper-foreground"} {
		set(p, name, text)
	}
	set(p, "ink", b.tone(0.18, 0.12))
	set(p, "muted-foreground", readable(b.tone(0.48, 0.10), MinTextContrast, background, card, muted))

	primary := readable(b.accent(0.40, 0.55), minUIContrast, background, card)
	primary, onPrimary := withText(primary, text, b.tone(0.995, 0.02))
	set(p, "primary", primary)
	p["primary-foreground"] = onPrimary
	set(p, "ring", primary)
	set(p, "copper", readable(b.accent(0.30, 0.50), MinTextContrast, paper, card))

	set(p, "destructive", readable(hue(0.52, 0.19, 27), MinTextContrast, background, card))
	set(p, "success", readable(hue(0.50, 0.12, 145), MinTextContrast, background, card))
	set(p, "brass", b.tone(0.62, 0.5))

	if darkSidebar {
		b.darkSidebar(p, b.tone(0.25, 0.25), b.tone(0.31, 0.30), b.tone(0.33, 0.25))
	} else {
		sidebar := b.tone(0.955, 0.14)
		sidebarAccent := b.tone(0.91, 0.22)
		set(p, "sidebar", sidebar)
		set(p, "sidebar-foreground", readable(b.tone(0.38, 0.12), MinTextContrast, sidebar, sidebarAccent))
		set(p, "sidebar-accent", sidebarAccent)
		set(p, "sidebar-accent-foreground", text)
		set(p, "sidebar-border", b.tone(0.88, 0.14))
		sidebarPrimary, onSidebarPrimary := withText(b.accent(0.40, 0.55), text, b.tone(0.995, 0.02))
		set(p, "sidebar-primary", sidebarPrimary)
		p["sidebar-primary-foreground"] = onSidebarPrimary
		set(p, "sidebar-ring", sidebarPrimary)
	}
	return p
}

func (b builder) dark() map[string]string {
	p := map[string]string{}
	background := b.tone(0.18, 0.12)
	card := b.tone(0.215, 0.12)
	popover := b.tone(0.235, 0.12)
	muted := b.tone(0.245, 0.12)
	secondary := b.tone(0.275, 0.18)
	accent := b.tone(0.285, 0.22)
	paper := b.tone(0.225, 0.08)
	text := b.tone(0.93, 0.06)

	set(p, "background", background)
	set(p, "card", card)
	set(p, "popover", popover)
	set(p, "muted", muted)
	set(p, "secondary", secondary)
	set(p, "accent", accent)
	set(p, "border", b.tone(0.36, 0.14))
	set(p, "input", b.tone(0.40, 0.14))
	set(p, "paper", paper)

	for _, name := range []string{"foreground", "card-foreground", "popover-foreground", "secondary-foreground", "accent-foreground"} {
		set(p, name, text)
	}
	set(p, "paper-foreground", b.tone(0.90, 0.06))
	set(p, "ink", b.tone(0.90, 0.08))
	set(p, "muted-foreground", readable(b.tone(0.72, 0.08), MinTextContrast, background, card, popover, muted))

	primary := readable(b.accent(0.68, 0.80), minUIContrast, background, card)
	primary, onPrimary := withText(primary, b.tone(0.18, 0.10), text)
	set(p, "primary", primary)
	p["primary-foreground"] = onPrimary
	set(p, "ring", primary)
	set(p, "copper", readable(b.accent(0.70, 0.82), MinTextContrast, paper, card))

	set(p, "destructive", readable(hue(0.68, 0.17, 25), MinTextContrast, background, card))
	set(p, "success", readable(hue(0.72, 0.12, 145), MinTextContrast, background, card))
	set(p, "brass", b.tone(0.70, 0.5))

	b.darkSidebar(p, b.tone(0.15, 0.14), b.tone(0.22, 0.18), b.tone(0.27, 0.14))
	return p
}

// darkSidebar is a dark sidebar, in light mode when asked for and always in
// dark mode.
func (b builder) darkSidebar(p map[string]string, sidebar, sidebarAccent, border lch) {
	set(p, "sidebar", sidebar)
	set(p, "sidebar-foreground", readable(b.tone(0.80, 0.16), MinTextContrast, sidebar, sidebarAccent))
	set(p, "sidebar-accent", sidebarAccent)
	set(p, "sidebar-accent-foreground", readable(b.tone(0.92, 0.08), MinTextContrast, sidebarAccent))
	set(p, "sidebar-border", border)
	sidebarPrimary := readable(b.accent(0.70, 0.82), minUIContrast, sidebar)
	sidebarPrimary, onSidebarPrimary := withText(sidebarPrimary, b.tone(0.18, 0.10), b.tone(0.995, 0.02))
	set(p, "sidebar-primary", sidebarPrimary)
	p["sidebar-primary-foreground"] = onSidebarPrimary
	set(p, "sidebar-ring", sidebarPrimary)
}

func set(p map[string]string, name string, c lch) {
	p[name] = c.hex()
}

// readable is c moved away in lightness from the backgrounds until it has at
// least min contrast with each. It moves toward black or white, whichever it
// is already nearer.
func readable(c lch, min float64, backgrounds ...lch) lch {
	darker := true
	for _, bg := range backgrounds {
		if c.l > bg.l {
			darker = false
		}
	}
	for range 100 {
		if lowestContrast(c, backgrounds) >= min || c.l <= 0 || c.l >= 1 {
			return c
		}
		if darker {
			c.l = math.Max(0, c.l-0.01)
		} else {
			c.l = math.Min(1, c.l+0.01)
		}
	}
	return c
}

func lowestContrast(c lch, backgrounds []lch) float64 {
	lowest := math.Inf(1)
	for _, bg := range backgrounds {
		lowest = math.Min(lowest, contrast(c.rgb(), bg.rgb()))
	}
	return lowest
}

// withText is a filled color and the text to put on it: whichever of the dark
// and light text reads better. If neither reaches AA, the fill moves away
// from the text until it does.
func withText(fill, darkText, lightText lch) (lch, string) {
	text := darkText
	if contrast(fill.rgb(), lightText.rgb()) >= contrast(fill.rgb(), darkText.rgb()) {
		text = lightText
	}
	return readable(fill, MinTextContrast, text), text.hex()
}
