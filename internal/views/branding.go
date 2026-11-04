package views

import (
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/assetver"
	"github.com/cristianadrielbraun/gofer/internal/branding"
)

// brandName is the installation's name: Gofer, or the one an administrator
// gave it.
func brandName() string { return branding.Name() }

// brandAdminName names the administration workspace.
func brandAdminName() string { return branding.Name() + " Admin" }

// brandLogoURL is the installation's logo.
func brandLogoURL() string { return brandAssetURL(branding.LogoURL()) }

// brandFaviconURL and brandFaviconType are the installation's favicon.
func brandFaviconURL() string  { return brandAssetURL(branding.FaviconURL()) }
func brandFaviconType() string { return branding.FaviconType() }

// brandIconURL is the raster icon notifications show.
func brandIconURL() string { return brandAssetURL(branding.IconURL()) }

// manifestURL is the web app manifest, which carries the installation's name.
func manifestURL() string {
	return "/assets/manifest.webmanifest?v=" + branding.Version()
}

func brandAssetURL(url string) string {
	if strings.HasPrefix(url, "/assets/") {
		return assetver.URL(url)
	}
	return url
}
