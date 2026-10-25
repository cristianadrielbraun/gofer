// Package themebuilder makes a whole color palette, for light and dark mode,
// from a primary color, an optional secondary color and a background tone.
// The lightness of every role is fixed here, so contrast is the same whatever
// the colors; they give the palette its hues and how colorful it is. Text that
// still falls short of WCAG AA is nudged until it passes.
package themebuilder

import (
	"math"
	"strings"
)

// Options are what the builder asks for.
type Options struct {
	// Seed is the primary color, as #rrggbb.
	Seed string
	// Secondary is the color of secondary buttons, chips and hovered items, as
	// #rrggbb. Empty follows the primary color.
	Secondary string
	// Tone is where backgrounds, cards, borders and text get their hue:
	// "primary", "secondary" or "neutral" for greys. Empty is "primary".
	Tone string
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

// Generate makes the palette for the options, or false if a color is not
// #rrggbb or the tone or sidebar is unknown.
func Generate(opts Options) (Palette, bool) {
	primary, ok := parseHex(strings.ToLower(strings.TrimSpace(opts.Seed)))
	if !ok || (opts.Sidebar != "light" && opts.Sidebar != "dark") {
		return nil, false
	}
	secondary := primary
	if raw := strings.TrimSpace(opts.Secondary); raw != "" {
		if secondary, ok = parseHex(strings.ToLower(raw)); !ok {
			return nil, false
		}
	}
	b := builder{primary: newSource(primary), secondary: newSource(secondary)}
	switch opts.Tone {
	case "", "primary":
		b.surface = b.primary
	case "secondary":
		b.surface = b.secondary
	case "neutral":
		b.surface = source{}
	default:
		return nil, false
	}
	return Palette{
		"light": b.light(opts.Sidebar == "dark"),
		"dark":  b.dark(),
	}, true
}

// source is a color roles take their hue from.
type source struct {
	color lch
	// chroma is how colorful the color is, capped so a vivid one doesn't give
	// loud surfaces. A grey has none, and gives greys.
	chroma float64
}

func newSource(c rgb) source {
	color := toLCH(c)
	chroma := math.Min(color.c, 0.2)
	if chroma < 0.02 {
		chroma = 0
	}
	return source{color: color, chroma: chroma}
}

// tone is a muted color of the source's hue: the share of its chroma it keeps
// sets how colorful it is.
func (s source) tone(l, share float64) lch {
	return lch{l, s.chroma * share, s.color.h}
}

// fill is the source color itself, with its lightness kept in [minL, maxL].
func (s source) fill(minL, maxL float64) lch {
	c := s.color
	c.l = math.Max(minL, math.Min(maxL, c.l))
	return c
}

type builder struct {
	// primary colors main buttons, links and selection; secondary colors
	// secondary buttons, chips and hovered items; surface colors backgrounds,
	// cards, borders and text.
	primary, secondary, surface source
}

// hue is a fixed-hue color, such as red for destructive actions.
func hue(l, c, degrees float64) lch {
	return lch{l, c, degrees * math.Pi / 180}
}

func (b builder) light(darkSidebar bool) map[string]string {
	p := map[string]string{}
	background := b.surface.tone(0.965, 0.10)
	card := b.surface.tone(0.99, 0.05)
	muted := b.surface.tone(0.93, 0.12)
	secondary := b.secondary.tone(0.91, 0.30)
	accent := b.secondary.tone(0.935, 0.30)
	paper := b.surface.tone(0.995, 0.03)
	text := b.surface.tone(0.22, 0.10)

	set(p, "background", background)
	set(p, "card", card)
	set(p, "popover", card)
	set(p, "muted", muted)
	set(p, "secondary", secondary)
	set(p, "accent", accent)
	set(p, "border", b.surface.tone(0.84, 0.14))
	set(p, "input", b.surface.tone(0.78, 0.14))
	set(p, "paper", paper)

	for _, name := range []string{"foreground", "card-foreground", "popover-foreground", "paper-foreground"} {
		set(p, name, text)
	}
	set(p, "secondary-foreground", readable(text, MinTextContrast, secondary))
	set(p, "accent-foreground", readable(text, MinTextContrast, accent))
	set(p, "ink", b.surface.tone(0.18, 0.12))
	set(p, "muted-foreground", readable(b.surface.tone(0.48, 0.10), MinTextContrast, background, card, muted))

	primary := readable(b.primary.fill(0.40, 0.55), minUIContrast, background, card)
	primary, onPrimary := withText(primary, text, b.surface.tone(0.995, 0.02))
	set(p, "primary", primary)
	p["primary-foreground"] = onPrimary
	set(p, "ring", primary)
	set(p, "copper", readable(b.primary.fill(0.30, 0.50), MinTextContrast, paper, card))

	set(p, "destructive", readable(hue(0.52, 0.19, 27), MinTextContrast, background, card))
	set(p, "success", readable(hue(0.50, 0.12, 145), MinTextContrast, background, card))
	set(p, "brass", b.primary.tone(0.62, 0.5))

	if darkSidebar {
		b.darkSidebar(p, b.surface.tone(0.25, 0.25), b.secondary.tone(0.31, 0.35), b.surface.tone(0.33, 0.25))
	} else {
		sidebar := b.surface.tone(0.955, 0.14)
		sidebarAccent := b.secondary.tone(0.91, 0.30)
		set(p, "sidebar", sidebar)
		set(p, "sidebar-foreground", readable(b.surface.tone(0.38, 0.12), MinTextContrast, sidebar, sidebarAccent))
		set(p, "sidebar-accent", sidebarAccent)
		set(p, "sidebar-accent-foreground", readable(text, MinTextContrast, sidebarAccent))
		set(p, "sidebar-border", b.surface.tone(0.88, 0.14))
		sidebarPrimary, onSidebarPrimary := withText(b.primary.fill(0.40, 0.55), text, b.surface.tone(0.995, 0.02))
		set(p, "sidebar-primary", sidebarPrimary)
		p["sidebar-primary-foreground"] = onSidebarPrimary
		set(p, "sidebar-ring", sidebarPrimary)
	}
	return p
}

func (b builder) dark() map[string]string {
	p := map[string]string{}
	background := b.surface.tone(0.18, 0.12)
	card := b.surface.tone(0.215, 0.12)
	popover := b.surface.tone(0.235, 0.12)
	muted := b.surface.tone(0.245, 0.12)
	secondary := b.secondary.tone(0.275, 0.30)
	accent := b.secondary.tone(0.285, 0.30)
	paper := b.surface.tone(0.225, 0.08)
	text := b.surface.tone(0.93, 0.06)

	set(p, "background", background)
	set(p, "card", card)
	set(p, "popover", popover)
	set(p, "muted", muted)
	set(p, "secondary", secondary)
	set(p, "accent", accent)
	set(p, "border", b.surface.tone(0.36, 0.14))
	set(p, "input", b.surface.tone(0.40, 0.14))
	set(p, "paper", paper)

	for _, name := range []string{"foreground", "card-foreground", "popover-foreground"} {
		set(p, name, text)
	}
	set(p, "secondary-foreground", readable(text, MinTextContrast, secondary))
	set(p, "accent-foreground", readable(text, MinTextContrast, accent))
	set(p, "paper-foreground", b.surface.tone(0.90, 0.06))
	set(p, "ink", b.surface.tone(0.90, 0.08))
	set(p, "muted-foreground", readable(b.surface.tone(0.72, 0.08), MinTextContrast, background, card, popover, muted))

	primary := readable(b.primary.fill(0.68, 0.80), minUIContrast, background, card)
	primary, onPrimary := withText(primary, b.surface.tone(0.18, 0.10), text)
	set(p, "primary", primary)
	p["primary-foreground"] = onPrimary
	set(p, "ring", primary)
	set(p, "copper", readable(b.primary.fill(0.70, 0.82), MinTextContrast, paper, card))

	set(p, "destructive", readable(hue(0.68, 0.17, 25), MinTextContrast, background, card))
	set(p, "success", readable(hue(0.72, 0.12, 145), MinTextContrast, background, card))
	set(p, "brass", b.primary.tone(0.70, 0.5))

	b.darkSidebar(p, b.surface.tone(0.15, 0.14), b.secondary.tone(0.22, 0.30), b.surface.tone(0.27, 0.14))
	return p
}

// darkSidebar is a dark sidebar, in light mode when asked for and always in
// dark mode.
func (b builder) darkSidebar(p map[string]string, sidebar, sidebarAccent, border lch) {
	set(p, "sidebar", sidebar)
	set(p, "sidebar-foreground", readable(b.surface.tone(0.80, 0.16), MinTextContrast, sidebar, sidebarAccent))
	set(p, "sidebar-accent", sidebarAccent)
	set(p, "sidebar-accent-foreground", readable(b.surface.tone(0.92, 0.08), MinTextContrast, sidebarAccent))
	set(p, "sidebar-border", border)
	sidebarPrimary := readable(b.primary.fill(0.70, 0.82), minUIContrast, sidebar)
	sidebarPrimary, onSidebarPrimary := withText(sidebarPrimary, b.surface.tone(0.18, 0.10), b.surface.tone(0.995, 0.02))
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
