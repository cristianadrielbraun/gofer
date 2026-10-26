package themebuilder

import (
	"fmt"
	"math"
	"strconv"
)

type rgb struct{ r, g, b float64 }

// lch is an OKLCH color: perceptual lightness 0–1, chroma, and hue in radians.
type lch struct{ l, c, h float64 }

func parseHex(s string) (rgb, bool) {
	if len(s) != 7 || s[0] != '#' {
		return rgb{}, false
	}
	n, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return rgb{}, false
	}
	return rgb{float64(n>>16&0xff) / 255, float64(n>>8&0xff) / 255, float64(n&0xff) / 255}, true
}

func toHex(c rgb) string {
	ch := func(v float64) int { return int(math.Round(math.Max(0, math.Min(1, v)) * 255)) }
	return fmt.Sprintf("#%02x%02x%02x", ch(c.r), ch(c.g), ch(c.b))
}

func toLCH(c rgb) lch {
	l, a, b := toOklab(c)
	return lch{l, math.Hypot(a, b), math.Atan2(b, a)}
}

// rgb is the color in sRGB, with chroma lowered until it fits.
func (c lch) rgb() rgb {
	return fitGamut(c.l, c.c*math.Cos(c.h), c.c*math.Sin(c.h))
}

func (c lch) hex() string {
	return toHex(c.rgb())
}

// contrast is the WCAG contrast ratio between two colors, 1 to 21.
func contrast(a, b rgb) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func luminance(c rgb) float64 {
	l := toLinear(c)
	return 0.2126*l.r + 0.7152*l.g + 0.0722*l.b
}

func toLinear(c rgb) rgb {
	f := func(v float64) float64 {
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return rgb{f(c.r), f(c.g), f(c.b)}
}

func fromLinear(c rgb) rgb {
	f := func(v float64) float64 {
		if v <= 0.0031308 {
			return v * 12.92
		}
		return 1.055*math.Pow(v, 1/2.4) - 0.055
	}
	return rgb{f(c.r), f(c.g), f(c.b)}
}

// fitGamut is the OKLab color, with chroma lowered until it fits in sRGB.
func fitGamut(l, a, b float64) rgb {
	if c := fromOklab(l, a, b); inGamut(c) {
		return c
	}
	lo, hi := 0.0, 1.0
	out := fromOklab(l, 0, 0)
	for range 24 {
		mid := (lo + hi) / 2
		cand := fromOklab(l, a*mid, b*mid)
		if inGamut(cand) {
			out, lo = cand, mid
		} else {
			hi = mid
		}
	}
	return out
}

func inGamut(c rgb) bool {
	const eps = 1e-4
	return c.r >= -eps && c.r <= 1+eps && c.g >= -eps && c.g <= 1+eps && c.b >= -eps && c.b <= 1+eps
}

func toOklab(c rgb) (float64, float64, float64) {
	lin := toLinear(c)
	l := math.Cbrt(0.4122214708*lin.r + 0.5363325363*lin.g + 0.0514459929*lin.b)
	m := math.Cbrt(0.2119034982*lin.r + 0.6806995451*lin.g + 0.1073969566*lin.b)
	s := math.Cbrt(0.0883024619*lin.r + 0.2817188376*lin.g + 0.6299787005*lin.b)
	return 0.2104542553*l + 0.7936177850*m - 0.0040720468*s,
		1.9779984951*l - 2.4285922050*m + 0.4505937099*s,
		0.0259040371*l + 0.7827717662*m - 0.8086757660*s
}

func fromOklab(L, a, b float64) rgb {
	l := L + 0.3963377774*a + 0.2158037573*b
	m := L - 0.1055613458*a - 0.0638541728*b
	s := L - 0.0894841775*a - 1.2914855480*b
	l, m, s = l*l*l, m*m*m, s*s*s
	return fromLinear(rgb{
		4.0767416621*l - 3.3077115913*m + 0.2309699292*s,
		-1.2684380046*l + 2.6097574011*m - 0.3413193965*s,
		-0.0041960863*l - 0.7034186168*m + 1.7076147010*s,
	})
}
