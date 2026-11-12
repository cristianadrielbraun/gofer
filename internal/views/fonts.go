package views

import (
	"context"
	"io"
	"strings"

	"github.com/a-h/templ"

	"github.com/cristianadrielbraun/gofer/internal/assetver"
)

// fontFace is one script of an interface font: the browser downloads the file
// only when a page shows a character in its range.
type fontFace struct {
	family, style, weight, file, unicodeRange string
}

// fontFaces are the interface fonts, served by Gofer itself; see
// assets/fonts/README.md.
var fontFaces = []fontFace{
	{"Fraunces", "italic", "400", "fraunces/fraunces-italic-latin.woff2", "U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD"},
	{"Fraunces", "italic", "400", "fraunces/fraunces-italic-latin-ext.woff2", "U+0100-02BA, U+02BD-02C5, U+02C7-02CC, U+02CE-02D7, U+02DD-02FF, U+0304, U+0308, U+0329, U+1D00-1DBF, U+1E00-1E9F, U+1EF2-1EFF, U+2020, U+20A0-20AB, U+20AD-20C4, U+2113, U+2C60-2C7F, U+A720-A7FF"},
	{"Fraunces", "italic", "400", "fraunces/fraunces-italic-vietnamese.woff2", "U+0102-0103, U+0110-0111, U+0128-0129, U+0168-0169, U+01A0-01A1, U+01AF-01B0, U+0300-0301, U+0303-0304, U+0308-0309, U+0323, U+0329, U+1EA0-1EF9, U+20AB"},
	{"Fraunces", "normal", "100 900", "fraunces/fraunces-latin.woff2", "U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD"},
	{"Fraunces", "normal", "100 900", "fraunces/fraunces-latin-ext.woff2", "U+0100-02BA, U+02BD-02C5, U+02C7-02CC, U+02CE-02D7, U+02DD-02FF, U+0304, U+0308, U+0329, U+1D00-1DBF, U+1E00-1E9F, U+1EF2-1EFF, U+2020, U+20A0-20AB, U+20AD-20C4, U+2113, U+2C60-2C7F, U+A720-A7FF"},
	{"Fraunces", "normal", "100 900", "fraunces/fraunces-vietnamese.woff2", "U+0102-0103, U+0110-0111, U+0128-0129, U+0168-0169, U+01A0-01A1, U+01AF-01B0, U+0300-0301, U+0303-0304, U+0308-0309, U+0323, U+0329, U+1EA0-1EF9, U+20AB"},
	{"Nunito Sans", "italic", "400", "nunito-sans/nunito-sans-italic-latin.woff2", "U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD"},
	{"Nunito Sans", "italic", "400", "nunito-sans/nunito-sans-italic-latin-ext.woff2", "U+0100-02BA, U+02BD-02C5, U+02C7-02CC, U+02CE-02D7, U+02DD-02FF, U+0304, U+0308, U+0329, U+1D00-1DBF, U+1E00-1E9F, U+1EF2-1EFF, U+2020, U+20A0-20AB, U+20AD-20C4, U+2113, U+2C60-2C7F, U+A720-A7FF"},
	{"Nunito Sans", "italic", "400", "nunito-sans/nunito-sans-italic-cyrillic.woff2", "U+0301, U+0400-045F, U+0490-0491, U+04B0-04B1, U+2116"},
	{"Nunito Sans", "italic", "400", "nunito-sans/nunito-sans-italic-cyrillic-ext.woff2", "U+0460-052F, U+1C80-1C8A, U+20B4, U+2DE0-2DFF, U+A640-A69F, U+FE2E-FE2F"},
	{"Nunito Sans", "italic", "400", "nunito-sans/nunito-sans-italic-vietnamese.woff2", "U+0102-0103, U+0110-0111, U+0128-0129, U+0168-0169, U+01A0-01A1, U+01AF-01B0, U+0300-0301, U+0303-0304, U+0308-0309, U+0323, U+0329, U+1EA0-1EF9, U+20AB"},
	{"Nunito Sans", "normal", "200 1000", "nunito-sans/nunito-sans-latin.woff2", "U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD"},
	{"Nunito Sans", "normal", "200 1000", "nunito-sans/nunito-sans-latin-ext.woff2", "U+0100-02BA, U+02BD-02C5, U+02C7-02CC, U+02CE-02D7, U+02DD-02FF, U+0304, U+0308, U+0329, U+1D00-1DBF, U+1E00-1E9F, U+1EF2-1EFF, U+2020, U+20A0-20AB, U+20AD-20C4, U+2113, U+2C60-2C7F, U+A720-A7FF"},
	{"Nunito Sans", "normal", "200 1000", "nunito-sans/nunito-sans-cyrillic.woff2", "U+0301, U+0400-045F, U+0490-0491, U+04B0-04B1, U+2116"},
	{"Nunito Sans", "normal", "200 1000", "nunito-sans/nunito-sans-cyrillic-ext.woff2", "U+0460-052F, U+1C80-1C8A, U+20B4, U+2DE0-2DFF, U+A640-A69F, U+FE2E-FE2F"},
	{"Nunito Sans", "normal", "200 1000", "nunito-sans/nunito-sans-vietnamese.woff2", "U+0102-0103, U+0110-0111, U+0128-0129, U+0168-0169, U+01A0-01A1, U+01AF-01B0, U+0300-0301, U+0303-0304, U+0308-0309, U+0323, U+0329, U+1EA0-1EF9, U+20AB"},
}

// preloadedFonts start downloading with the page instead of after the
// stylesheet is read: the Latin body and heading fonts almost every page uses.
var preloadedFonts = []string{"nunito-sans/nunito-sans-latin.woff2", "fraunces/fraunces-latin.woff2"}

// FontFaces declares the interface fonts with versioned links, so browsers
// keep each file until a release changes it.
func FontFaces() templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		var b strings.Builder
		for _, file := range preloadedFonts {
			b.WriteString(`<link rel="preload" href="` + assetver.URL("/assets/fonts/"+file) + `" as="font" type="font/woff2" crossorigin="anonymous"/>`)
		}
		b.WriteString("<style>")
		for _, face := range fontFaces {
			b.WriteString("@font-face{font-family:'" + face.family + "';font-style:" + face.style + ";font-weight:" + face.weight +
				";font-display:swap;src:url(" + assetver.URL("/assets/fonts/"+face.file) + ") format('woff2');unicode-range:" + face.unicodeRange + "}")
		}
		b.WriteString("</style>")
		_, err := io.WriteString(w, b.String())
		return err
	})
}
