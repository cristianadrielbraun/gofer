//go:build embedded_assets

package assets

import "embed"

// FS contains the browser assets needed by production builds.
//
// Build with `-tags embedded_assets` after `task generate`, which writes
// css/output.css and the minified scripts in min/js.
//
//go:embed css/output.css js/*.js min/js/*.js fonts/*.woff2 fonts/COPYING.LIB fonts/README.md fonts/*/*.woff2 fonts/*/OFL.txt icons/express/*.png icons/express/*.svg icons/express/README.md notification-avatar.png logo.svg logo.png logo-192.png logo-maskable-512.png manifest.webmanifest
var FS embed.FS
