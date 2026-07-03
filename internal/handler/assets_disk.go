//go:build !embedded_assets

package handler

import "net/http"

func assetFileSystem() http.FileSystem {
	return http.Dir("./assets")
}

func serveServiceWorker(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "./assets/js/sw.js")
}
