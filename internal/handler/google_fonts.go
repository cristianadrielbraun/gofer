package handler

import (
	"log"
	"net/http"
	"sync"

	"github.com/cristianadrielbraun/gofer/internal/googlefonts"
)

var (
	googleFontsOnce    sync.Once
	googleFontsLibrary *googlefonts.Library
)

func googleFonts() *googlefonts.Library {
	googleFontsOnce.Do(func() {
		if !googlefonts.Enabled() {
			return
		}
		googleFontsLibrary = googlefonts.New(googlefonts.CacheDir(), nil)
		log.Printf("google fonts: Appearance can use the Google Fonts library, served through Gofer")
	})
	return googleFontsLibrary
}

func setupGoogleFontsRoutes(mux *http.ServeMux) {
	library := googleFonts()
	if library == nil {
		return
	}
	handler := library.Handler()
	mux.Handle("GET "+googlefonts.ListPath, handler)
	mux.Handle("GET "+googlefonts.CSSPath, handler)
	mux.Handle("GET "+googlefonts.ChoicePath, handler)
	mux.Handle("GET "+googlefonts.PreviewPath, handler)
	mux.Handle("GET "+googlefonts.FilePrefix, handler)
}
