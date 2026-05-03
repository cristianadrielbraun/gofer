package handler

import (
	"gofer.email/internal/models"
	"gofer.email/internal/views"
	"net/http"
	"os"

	"github.com/templui/templui/utils"
)

type Handler struct{}

func New() *Handler {
	return &Handler{}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	setupAssetsRoutes(mux)
	mux.HandleFunc("GET /", h.handleIndex)
}

func setupAssetsRoutes(mux *http.ServeMux) {
	isDevelopment := os.Getenv("GO_ENV") != "production"

	assetHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isDevelopment {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=31536000")
		}
		http.FileServer(http.Dir("./assets")).ServeHTTP(w, r)
	})

	mux.Handle("GET /assets/", http.StripPrefix("/assets/", assetHandler))
	utils.SetupScriptRoutes(mux, isDevelopment)
}

func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	folderID := r.URL.Query().Get("folder")
	if folderID == "" {
		folderID = "inbox"
	}

	emailID := r.URL.Query().Get("email")

	accounts := models.GetAccounts()
	emails := models.GetEmails(folderID)

	var selectedEmail *models.Email
	if emailID != "" {
		selectedEmail = models.GetEmailByID(emailID)
	}
	if selectedEmail == nil && len(emails) > 0 {
		selectedEmail = &emails[0]
	}

	views.Layout(accounts, folderID, emails, selectedEmail).Render(r.Context(), w)
}
