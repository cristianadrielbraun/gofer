// Package themebuilder makes a whole color palette, for light and dark mode,
// from a primary color, an optional secondary color and a tint.
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
	// Tint is where backgrounds, cards, borders and text get their hue:
	// "primary", "secondary" or "none" for greys. Empty is "primary".
	Tint string
	// Strength is how much of that hue they take: "subtle", "strong" or
	// "strongest".
	// Empty is "subtle".
	Strength string
	// Contrast is "normal", "high" or "highest". Higher contrast has darker
	// text on light themes, lighter on dark ones, and stronger borders; text
	// is held to WCAG AAA, and at highest is near black or white. Empty is
	// "normal".
	Contrast string
	// Sidebar is "darker" or "lighter" than the panes. In light mode a darker
	// sidebar is a dark one; in dark mode a lighter sidebar is a step brighter
	// than the panes, still dark.
	Sidebar string
}

// MinTextContrast is WCAG AA for body text.
const MinTextContrast = 4.5

// highTextContrast is WCAG AAA for body text, which high contrast holds text
// to.
const highTextContrast = 7.0

// minUIContrast is WCAG AA for controls and focus outlines.
const minUIContrast = 3.0

// Palette is every color a theme defines: mode, then variable, then #rrggbb.
type Palette map[string]map[string]string

// Generate makes the palette for the options, or false if a color is not
// #rrggbb or the tint, strength, contrast or sidebar is unknown.
func Generate(opts Options) (Palette, bool) {
	primary, ok := parseHex(strings.ToLower(strings.TrimSpace(opts.Seed)))
	if !ok || (opts.Sidebar != "darker" && opts.Sidebar != "lighter") {
		return nil, false
	}
	secondary := primary
	if raw := strings.TrimSpace(opts.Secondary); raw != "" {
		if secondary, ok = parseHex(strings.ToLower(raw)); !ok {
			return nil, false
		}
	}
	b := builder{primary: newSource(primary), secondary: newSource(secondary)}
	switch opts.Tint {
	case "", "primary":
		b.surface = b.primary
	case "secondary":
		b.surface = b.secondary
	case "none":
		b.surface = source{}
	default:
		return nil, false
	}
	switch opts.Contrast {
	case "", "normal":
	case "high":
		b.contrast = highContrast
	case "highest":
		b.contrast = highestContrast
	default:
		return nil, false
	}
	switch opts.Strength {
	case "", "subtle":
	case "strong":
		b.surface.chroma *= strongTint
		b.strength = 1
	case "strongest":
		b.surface.chroma *= strongestTint
		b.strength = 2
	default:
		return nil, false
	}
	return Palette{
		"light": b.light(opts.Sidebar == "darker"),
		"dark":  b.dark(opts.Sidebar == "lighter"),
	}, true
}

// strongTint is how many times more colorful a strong tint is than a subtle
// one.
const strongTint = 2.2

// strongestTint is the same for the strongest tint.
const strongestTint = 4.0

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
	contrast                    contrastLevel
	// strength is the tint's: 0 subtle, 1 strong, 2 strongest.
	strength int
}

// room is the lightness of a surface for the tint's strength. Near white
// and near black a screen can show little color, so stronger tints bring
// the lightest and darkest surfaces in a little to leave the hue room.
func (b builder) room(subtle, strong, strongest float64) float64 {
	return []float64{subtle, strong, strongest}[b.strength]
}

type contrastLevel int

const (
	normalContrast contrastLevel = iota
	highContrast
	highestContrast
)

// by is the lightness for the builder's contrast level.
func (b builder) by(normal, high, highest float64) float64 {
	switch b.contrast {
	case highContrast:
		return high
	case highestContrast:
		return highest
	}
	return normal
}

// textMin is the contrast text must reach.
func (b builder) textMin() float64 {
	if b.contrast > normalContrast {
		return highTextContrast
	}
	return MinTextContrast
}

// hue is a fixed-hue color, such as red for destructive actions.
func hue(l, c, degrees float64) lch {
	return lch{l, c, degrees * math.Pi / 180}
}

func (b builder) light(darkSidebar bool) map[string]string {
	p := map[string]string{}
	background := b.surface.tone(b.room(0.965, 0.955, 0.94), 0.10)
	card := b.surface.tone(b.room(0.99, 0.98, 0.965), 0.05)
	muted := b.surface.tone(b.room(0.93, 0.925, 0.91), 0.12)
	secondary := b.secondary.tone(0.91, 0.30)
	accent := b.secondary.tone(0.935, 0.30)
	paper := b.surface.tone(b.room(0.995, 0.985, 0.97), 0.03)
	text := b.surface.tone(b.by(0.22, 0.14, 0.04), 0.10)

	set(p, "background", background)
	set(p, "card", card)
	set(p, "popover", card)
	set(p, "muted", muted)
	set(p, "secondary", secondary)
	set(p, "accent", accent)
	set(p, "border", b.surface.tone(b.by(0.84, 0.72, 0.56), 0.14))
	set(p, "input", b.surface.tone(b.by(0.78, 0.64, 0.46), 0.14))
	set(p, "paper", paper)

	for _, name := range []string{"foreground", "card-foreground", "popover-foreground", "paper-foreground"} {
		set(p, name, text)
	}
	set(p, "secondary-foreground", readable(text, b.textMin(), secondary))
	set(p, "accent-foreground", readable(text, b.textMin(), accent))
	set(p, "ink", b.surface.tone(b.by(0.18, 0.12, 0.02), 0.12))
	set(p, "muted-foreground", readable(b.surface.tone(b.by(0.48, 0.38, 0.26), 0.10), b.textMin(), background, card, muted))

	primary := readable(b.primary.fill(0.40, 0.55), minUIContrast, background, card)
	primary, onPrimary := withText(primary, text, b.surface.tone(0.995, 0.02))
	set(p, "primary", primary)
	p["primary-foreground"] = onPrimary
	set(p, "ring", primary)
	set(p, "copper", readable(b.primary.fill(0.30, 0.50), b.textMin(), paper, card))

	set(p, "destructive", readable(hue(0.52, 0.19, 27), b.textMin(), background, card))
	set(p, "success", readable(hue(0.50, 0.12, 145), b.textMin(), background, card))
	set(p, "brass", b.primary.tone(0.62, 0.5))

	if darkSidebar {
		b.darkSidebar(p, b.surface.tone(0.25, 0.25), b.secondary.tone(0.31, 0.35), b.surface.tone(0.33, 0.25))
	} else {
		sidebar := b.surface.tone(b.room(0.98, 0.97, 0.955), 0.10)
		sidebarAccent := b.secondary.tone(0.91, 0.30)
		set(p, "sidebar", sidebar)
		set(p, "sidebar-foreground", readable(b.surface.tone(b.by(0.38, 0.28, 0.12), 0.12), b.textMin(), sidebar, sidebarAccent))
		set(p, "sidebar-accent", sidebarAccent)
		set(p, "sidebar-accent-foreground", readable(text, b.textMin(), sidebarAccent))
		set(p, "sidebar-border", b.surface.tone(b.by(0.88, 0.78, 0.60), 0.14))
		sidebarPrimary, onSidebarPrimary := withText(b.primary.fill(0.40, 0.55), text, b.surface.tone(0.995, 0.02))
		set(p, "sidebar-primary", sidebarPrimary)
		p["sidebar-primary-foreground"] = onSidebarPrimary
		set(p, "sidebar-ring", sidebarPrimary)
	}
	return p
}

func (b builder) dark(lighterSidebar bool) map[string]string {
	p := map[string]string{}
	background := b.surface.tone(b.room(0.18, 0.19, 0.215), 0.12)
	card := b.surface.tone(b.room(0.215, 0.225, 0.25), 0.12)
	popover := b.surface.tone(b.room(0.235, 0.245, 0.265), 0.12)
	muted := b.surface.tone(b.room(0.245, 0.255, 0.275), 0.12)
	secondary := b.secondary.tone(0.275, 0.30)
	accent := b.secondary.tone(0.285, 0.30)
	paper := b.surface.tone(b.room(0.225, 0.235, 0.255), 0.08)
	text := b.surface.tone(b.by(0.93, 0.975, 1.0), 0.06)

	set(p, "background", background)
	set(p, "card", card)
	set(p, "popover", popover)
	set(p, "muted", muted)
	set(p, "secondary", secondary)
	set(p, "accent", accent)
	set(p, "border", b.surface.tone(b.by(0.36, 0.48, 0.62), 0.14))
	set(p, "input", b.surface.tone(b.by(0.40, 0.54, 0.68), 0.14))
	set(p, "paper", paper)

	for _, name := range []string{"foreground", "card-foreground", "popover-foreground"} {
		set(p, name, text)
	}
	set(p, "secondary-foreground", readable(text, b.textMin(), secondary))
	set(p, "accent-foreground", readable(text, b.textMin(), accent))
	set(p, "paper-foreground", b.surface.tone(b.by(0.90, 0.96, 1.0), 0.06))
	set(p, "ink", b.surface.tone(b.by(0.90, 0.96, 1.0), 0.08))
	set(p, "muted-foreground", readable(b.surface.tone(b.by(0.72, 0.82, 0.90), 0.08), b.textMin(), background, card, popover, muted))

	primary := readable(b.primary.fill(0.68, 0.80), minUIContrast, background, card)
	primary, onPrimary := withText(primary, b.surface.tone(0.18, 0.10), text)
	set(p, "primary", primary)
	p["primary-foreground"] = onPrimary
	set(p, "ring", primary)
	set(p, "copper", readable(b.primary.fill(0.70, 0.82), b.textMin(), paper, card))

	set(p, "destructive", readable(hue(0.68, 0.17, 25), b.textMin(), background, card))
	set(p, "success", readable(hue(0.72, 0.12, 145), b.textMin(), background, card))
	set(p, "brass", b.primary.tone(0.70, 0.5))

	if lighterSidebar {
		b.darkSidebar(p, b.surface.tone(0.25, 0.14), b.secondary.tone(0.32, 0.30), b.surface.tone(0.34, 0.14))
	} else {
		b.darkSidebar(p, b.surface.tone(0.15, 0.14), b.secondary.tone(0.22, 0.30), b.surface.tone(0.27, 0.14))
	}
	return p
}

// darkSidebar is a dark sidebar: the darker one in light mode, and either
// one in dark mode.
func (b builder) darkSidebar(p map[string]string, sidebar, sidebarAccent, border lch) {
	border.l += b.by(0, 0.12, 0.26)
	set(p, "sidebar", sidebar)
	set(p, "sidebar-foreground", readable(b.surface.tone(b.by(0.80, 0.90, 0.97), 0.16), b.textMin(), sidebar, sidebarAccent))
	set(p, "sidebar-accent", sidebarAccent)
	set(p, "sidebar-accent-foreground", readable(b.surface.tone(b.by(0.92, 0.97, 1.0), 0.08), b.textMin(), sidebarAccent))
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

// lowestContrast is measured on the colors as they are written out, in
// #rrggbb, since rounding them can cost a little contrast.
func lowestContrast(c lch, backgrounds []lch) float64 {
	lowest := math.Inf(1)
	color, _ := parseHex(c.hex())
	for _, bg := range backgrounds {
		written, _ := parseHex(bg.hex())
		lowest = math.Min(lowest, contrast(color, written))
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
