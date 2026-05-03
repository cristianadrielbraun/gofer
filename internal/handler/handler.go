package handler

import (
	"gofer.email/internal/models"
	"gofer.email/internal/views"
	"net/http"
	"os"
	"strconv"
)

type Handler struct{}

func New() *Handler {
	return &Handler{}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	setupAssetsRoutes(mux)
	mux.HandleFunc("GET /", h.handleIndex)
	mux.HandleFunc("GET /email/{id}", h.handleEmailPartial)
	mux.HandleFunc("GET /folder/{id}", h.handleFolderPartial)
	mux.HandleFunc("GET /mail/folder/{id}/items", h.handleMailItems)
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
	totalCount := models.GetFolderEmailCount(folderID)

	page := models.GetEmailsRange(folderID, 0, 50)
	emails := page.Emails

	var selectedEmail *models.Email
	if emailID != "" {
		selectedEmail = models.GetEmailByID(emailID)
	}
	if selectedEmail == nil && len(emails) > 0 {
		selectedEmail = &emails[0]
	}

	views.Layout(accounts, folderID, emails, selectedEmail, totalCount).Render(r.Context(), w)
}

func (h *Handler) handleEmailPartial(w http.ResponseWriter, r *http.Request) {
	emailID := r.PathValue("id")
	if emailID == "" {
		http.NotFound(w, r)
		return
	}

	email := models.GetEmailByID(emailID)
	if email == nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	views.MailViewContent(email).Render(r.Context(), w)
}

func (h *Handler) handleFolderPartial(w http.ResponseWriter, r *http.Request) {
	folderID := r.PathValue("id")
	if folderID == "" {
		folderID = "inbox"
	}

	totalCount := models.GetFolderEmailCount(folderID)
	page := models.GetEmailsRange(folderID, 0, 50)
	emails := page.Emails

	var selectedEmail *models.Email
	if len(emails) > 0 {
		selectedEmail = &emails[0]
	}

	w.Header().Set("Content-Type", "text/html")
	views.FolderPartial(emails, folderID, selectedEmail, totalCount).Render(r.Context(), w)
}

func (h *Handler) handleMailItems(w http.ResponseWriter, r *http.Request) {
	folderID := r.PathValue("id")
	if folderID == "" {
		folderID = "inbox"
	}

	limit := 50
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 200 {
		limit = l
	}

	selectedEmailId := r.URL.Query().Get("selected")

	var page models.EmailPage

	if around := r.URL.Query().Get("around"); around != "" {
		page = models.GetEmailsAroundEmail(folderID, around, limit)
	} else if startStr := r.URL.Query().Get("start"); startStr != "" {
		start, err := strconv.Atoi(startStr)
		if err != nil || start < 0 {
			start = 0
		}
		page = models.GetEmailsRange(folderID, start, limit)
	} else if cursor := r.URL.Query().Get("after"); cursor != "" {
		page = models.GetEmailsAfterCursor(folderID, cursor, limit)
	} else {
		page = models.GetEmailsRange(folderID, 0, limit)
	}

	w.Header().Set("Content-Type", "text/html")
	views.MailListItemsFragment(
		page.Emails, folderID,
		page.WindowStart, page.WindowEnd, page.TotalCount,
		page.NextCursor, page.HasMore,
		selectedEmailId,
	).Render(r.Context(), w)
}
