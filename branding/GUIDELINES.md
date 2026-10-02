# Gofer — Logo guidelines

## 1. The logo
**Idea:** a wax seal pressed with an envelope flap and a gopher's ears. Your mail, sealed and kept by you (local-first), with a nod to Go's gopher.

| Version | File | Use it for |
|---|---|---|
| Symbol | `symbol/gofer-symbol.svg` | 64 px and up: app tiles, login screen, README header, social |
| Symbol, small-size cut | `symbol/gofer-symbol-small.svg` | **48 px and below**: favicon, sidebar, toolbar, notifications |
| Symbol, tight crop | `symbol/gofer-symbol-tight.svg`, `symbol/gofer-symbol-small-tight.svg` | Inline next to text (headers, sidebars, README). The frame hugs the seal, so it fills its box like a normal icon |
| Symbol, pressed | `symbol/gofer-symbol-pressed.svg` | Decorative, large only (≥128 px): splash, print, stickers |
| Horizontal lockup | `lockups/gofer-horizontal.svg` (`-dark` on dark backgrounds) | Primary logo: website header, docs, README |
| Horizontal, small | `lockups/gofer-horizontal-small.svg` (`-dark`) | Horizontal lockup under 160 px wide (e.g. app sidebar header) |
| Stacked lockup | `lockups/gofer-stacked.svg` (`-dark`, `-cream`) | Square-ish spaces: about screen, stickers, badges |
| Wordmark | `lockups/gofer-wordmark.svg` (`-cream`) | Only where the symbol already appears nearby |
| One-colour | `web/gofer-symbol-black.svg`, `-white.svg`, `-mono-7e3f16.svg`, `-mono-f7ecdd.svg` | Single-colour printing, embossing, engraving |
| Web/app icons | `web/` (favicon.ico, favicon.svg, PNGs, manifest, head snippet) | Browser and PWA |

The standard symbol files keep a 256 × 256 canvas with the seal centred, which is useful for avatars and app tiles. When the logo sits inline next to text, use the `-tight` files instead; otherwise the canvas padding makes the seal look too small.

The stamp is cut out of the wax: what looks like the stamp's line is the background showing through. Put the cut-out symbol only on calm, solid backgrounds.

## 2. Clear space
Keep a clear zone of **x** on every side, where **x = ¼ of the seal's height**. The zone scales with the logo.

## 3. Minimum size
| Version | Screen | Print |
|---|---|---|
| Symbol (main) | 64 px | 15 mm |
| Symbol, small-size cut | 16 px | 6 mm |
| Horizontal lockup | 160 px wide (below that, use `-small`, down to 96 px) | 30 mm wide |
| Stacked lockup | 96 px wide | 20 mm wide |

## 4. Colour
| Name | HEX | RGB | CMYK (approx.) | Role |
|---|---|---|---|---|
| Copper | `#C2702F` | 194 112 47 | 0 42 76 24 | The seal: primary brand colour |
| Espresso | `#1B130E` | 27 19 14 | 0 30 48 89 | Wordmark on light; dark backgrounds |
| Cream | `#F7ECDD` | 247 236 221 | 0 4 11 3 | Wordmark on dark; seal on copper tiles |
| Deep brown | `#7E3F16` | 126 63 22 | 0 50 83 51 | Pressed stamp (decorative only) |

CMYK values are straight conversions. Check them against a printed proof before any print run. No Pantone match has been chosen yet.

**Approved pairs:**
- copper seal on white, cream or espresso
- cream seal on copper
- espresso wordmark on white or cream
- cream wordmark on espresso
- black or white one-colour wherever colour isn't possible

**Contrast (WCAG):**
- copper on white 3.7:1 and copper on espresso 4.9:1, both enough for a graphic
- deep brown on copper 2.2:1, so the pressed version is decorative only

## 5. Typography
- **Wordmark:** Fraunces 72pt SemiBold, outlined in the files. Don't retype it.
- **UI type:** the app's existing Nunito Sans (body) and Fraunces (headings).
- **Licences:** both fonts are SIL Open Font License 1.1, which allows logo use.

## 6. Don'ts
- Don't stretch, rotate or recolour the seal outside the palette.
- Don't add shadows, outlines or gradients.
- Don't use the main symbol below 64 px. Use the small-size cut.
- Don't change the ear or flap geometry. The joints are precisely aligned.
- Don't rearrange or resize the parts of a lockup.
- Don't put the cut-out seal on photos or busy patterns.

## 7. Construction notes
- **Canvas:** 256 × 256, with the seal centred.
- **Stamp:** a circle, r = 56 (line 7). The flap arms start on the circle at 208° and 332° and meet 10 units below its centre, about 36° from horizontal (line 8).
- **Ears:** superellipses with n = 2.2, r = 17, set 3 units inside the circle at ±43.38° from vertical. That angle is solved so each ear's inner corner lies exactly on the flap's top edge.
- **Small cut:** line 13, ears r = 18 at ±43.28°.
- **Stamp position:** optically centred on the wax's centre of mass (horizontally), 2 units above it (vertically).
- **Wax edge:** r = 90 + 4.5·sin(5t + 4.6889) + 2.5·sin(3t + 2.0303).
- **Curves:** outlines fitted to cubic Béziers within 0.04 units, with corners kept exact.
