package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/branding"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func loadBranding(ctx context.Context, db *storage.DB) {
	if db == nil {
		return
	}
	saved, err := db.GetBranding(ctx)
	if err != nil {
		log.Printf("load branding: %v", err)
		return
	}
	branding.Set(brandingSettings(saved))
}

func brandingSettings(saved storage.Branding) branding.Settings {
	return branding.Settings{
		Name:    saved.Name,
		Logo:    branding.Image{Data: saved.Logo, ContentType: saved.LogoType},
		Favicon: branding.Image{Data: saved.Favicon, ContentType: saved.FaviconType},
	}
}

// serveBrandingImage serves the custom logo or favicon, or redirects to the default logo if none is set.
func serveBrandingImage(pick func(branding.Settings) branding.Image) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		image := pick(branding.Current())
		if image.Empty() {
			http.Redirect(w, r, "/assets/logo.svg", http.StatusFound)
			return
		}
		header := w.Header()
		header.Set("Content-Type", image.ContentType)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
		if r.URL.Query().Get("v") != "" {
			header.Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			header.Set("Cache-Control", "public, no-cache")
		}
		_, _ = w.Write(image.Data)
	}
}

// serveManifest serves the web app manifest with the installation's name and icons.
func serveManifest(files http.FileSystem) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		manifest, err := readManifest(files)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if branding.Customized() {
			manifest["name"] = branding.Name()
			manifest["short_name"] = branding.Name()
		}
		current := branding.Current()
		if !current.Favicon.Empty() || !current.Logo.Empty() {
			icons := []map[string]string{}
			if !current.Logo.Empty() {
				icons = append(icons, brandingManifestIcon(branding.LogoURL(), current.Logo.ContentType))
			}
			if !current.Favicon.Empty() {
				icons = append(icons, brandingManifestIcon(branding.FaviconURL(), current.Favicon.ContentType))
			}
			manifest["icons"] = icons
		}
		w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
		w.Header().Set("Cache-Control", "public, no-cache")
		_ = json.NewEncoder(w).Encode(manifest)
	}
}

func brandingManifestIcon(src, contentType string) map[string]string {
	return map[string]string{"src": src, "sizes": "any", "type": contentType, "purpose": "any"}
}

func readManifest(files http.FileSystem) (map[string]any, error) {
	file, err := files.Open("manifest.webmanifest")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	manifest := map[string]any{}
	if err := json.NewDecoder(file).Decode(&manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}
