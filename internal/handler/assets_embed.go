//go:build embedded_assets

package handler

import (
	"net/http"

	appassets "github.com/cristianadrielbraun/gofer/assets"
)

func assetFileSystem() http.FileSystem {
	return http.FS(appassets.FS)
}

func serveServiceWorker(w http.ResponseWriter, r *http.Request) {
	http.ServeFileFS(w, r, appassets.FS, "js/sw.js")
}
