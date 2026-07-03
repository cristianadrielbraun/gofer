//go:build embedded_assets

package assets

import "embed"

// FS contains the browser assets needed by production builds.
//
// Build with `-tags embedded_assets` after generating css/output.css.
//
//go:embed css/output.css js/*.js logo.svg logo.png
var FS embed.FS
