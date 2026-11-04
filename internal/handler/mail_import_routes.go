package handler

import (
	"context"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) registerMailImportRoutes(register func(string, http.HandlerFunc)) {
	register("GET /api/mail/imports", h.handleMailImports)
	register("GET /api/mail/imports/jobs", h.handleMailImportJobs)
	register("POST /api/mail/imports", h.handleUploadMailImport)
	register("GET /api/mail/imports/{id}/review", h.handleReviewMailImport)
	register("POST /api/mail/imports/{id}/start", h.handleStartMailImport)
	register("POST /api/mail/imports/{id}/cancel", h.handleCancelMailImport)
	register("POST /api/mail/imports/{id}/resume", h.handleResumeMailImport)
	register("POST /api/mail/imports/{id}/delete", h.handleDeleteMailImport)
}

func (h *Handler) mailImportsAvailable(w http.ResponseWriter, r *http.Request) bool {
	if h.mailImports == nil {
		http.Error(w, i18n.T(r.Context(), "server.error.mail_import_is_not_available"), http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (h *Handler) mailImportJobViews(ctx context.Context, owner string) ([]views.MailImportJob, error) {
	jobs, err := h.mailImports.jobs(owner)
	if err != nil {
		return nil, err
	}
	result := make([]views.MailImportJob, 0, len(jobs))
	for _, job := range jobs {
		processed, total, imported, duplicates, failed := job.totals()
		view := views.MailImportJob{
			ID: job.ID, Title: mailImportTitle(job), Account: job.AccountEmail, State: job.State, Error: job.Error,
			Processed: processed, Total: total, Imported: imported, Duplicates: duplicates, Failed: failed,
			Started: i18n.Dates(ctx).ShortDateTime(job.CreatedAt.Local()),
		}
		if job.State == mailJobRunning && h.mailImports.isWaiting(job.ID) {
			view.Waiting = true
		} else if source := job.current(); source != nil {
			view.Source = source.Name
		}
		for _, skipped := range job.Skipped {
			subject := skipped.Subject
			if subject == "" {
				subject = i18n.T(ctx, "mail.no_subject")
			}
			view.Skipped = append(view.Skipped, skipped.Source+": "+subject+" ("+serverText(ctx, skipped.Reason)+")")
		}
		result = append(result, view)
	}
	return result, nil
}

func mailImportTitle(job *mailImportJob) string {
	names := make([]string, 0, len(job.Files))
	for _, file := range job.Files {
		names = append(names, file.Name)
	}
	return strings.Join(names, ", ")
}

// renderMailImportPanel shows the upload form in the Import mail dialog.
func (h *Handler) renderMailImportPanel(w http.ResponseWriter, r *http.Request, message string) {
	renderMailImportUploadError(w, r, message)
}

func (h *Handler) renderMailImportJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := h.mailImportJobViews(r.Context(), h.userID(r.Context()))
	if err != nil {
		log.Printf("mail import: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.could_not_list_imports"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.MailImportJobs(jobs).Render(r.Context(), w)
}

func (h *Handler) handleMailImports(w http.ResponseWriter, r *http.Request) {
	if !h.mailImportsAvailable(w, r) {
		return
	}
	h.renderMailImportPanel(w, r, "")
}

func (h *Handler) handleMailImportJobs(w http.ResponseWriter, r *http.Request) {
	if !h.mailImportsAvailable(w, r) {
		return
	}
	h.renderMailImportJobs(w, r)
}

func renderMailImportUploadError(w http.ResponseWriter, r *http.Request, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.MailImportUploadForm(message).Render(r.Context(), w)
}

func (h *Handler) handleUploadMailImport(w http.ResponseWriter, r *http.Request) {
	if !h.mailImportsAvailable(w, r) {
		return
	}
	reader, err := r.MultipartReader()
	if err != nil {
		renderMailImportUploadError(w, r, "Choose the files to import.")
		return
	}
	job, err := h.saveMailImportUpload(h.userID(r.Context()), reader)
	if err != nil {
		message := "The files could not be uploaded."
		switch {
		case errors.Is(err, errMailImportNoFiles):
			message = "Choose the files to import."
		case errors.Is(err, errMailImportNoMessages):
			message = "No messages were found in those files. Choose mbox or eml files, or a ZIP of them."
		case errors.Is(err, errMailImportBadZip):
			message = "The ZIP file could not be read."
		default:
			log.Printf("mail import: upload: %v", err)
		}
		renderMailImportUploadError(w, r, message)
		return
	}
	h.renderMailImportReview(w, r, job, "", "")
}

// renderMailImportReview shows where an upload's messages will go in the
// account chosen, or the first account they can go into.
func (h *Handler) renderMailImportReview(w http.ResponseWriter, r *http.Request, job *mailImportJob, accountID, message string) {
	ctx := r.Context()
	accounts, err := h.mailExportAccounts(ctx, job.Owner)
	if err != nil {
		log.Printf("mail import: list accounts: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.could_not_list_accounts"), http.StatusInternalServerError)
		return
	}
	data := views.MailImportReviewData{ID: job.ID, ExportedFrom: job.ExportedFrom, Message: message}
	for _, file := range job.Files {
		data.Files = append(data.Files, file.Name)
	}
	var chosen *models.Account
	for i, account := range accounts {
		if account.IsDeleting {
			continue
		}
		problem := mailImportAccountProblem(account)
		data.Accounts = append(data.Accounts, views.MailImportAccount{MailExportAccount: mailExportAccountView(account), Problem: serverText(r.Context(), problem)})
		if problem == "" && (account.ID == accountID || (accountID == "" && chosen == nil)) {
			chosen = &accounts[i]
		}
	}
	for i, source := range job.Sources {
		data.Sources = append(data.Sources, views.MailImportSource{Index: i, Name: serverText(r.Context(), source.Name), Path: source.Path, Messages: source.Messages})
	}
	if chosen != nil {
		data.AccountID = chosen.ID
		folders, err := h.mailExportFolders(ctx, job.Owner, chosen.ID)
		if err != nil {
			log.Printf("mail import: list folders: %v", err)
			data.Message = i18n.T(r.Context(), "server.msg.the_account_s_folders_could_not_be")
		}
		targets := mailImportTargets(chosen.Provider, folders)
		for _, folder := range targets {
			data.Folders = append(data.Folders, views.MailImportFolder{ID: folder.ID, Path: folder.Path})
		}
		for i, source := range job.Sources {
			data.Sources[i].Target, data.Sources[i].NewName = suggestMailImportTarget(source, targets)
			if data.Sources[i].NewName == "" {
				data.Sources[i].NewName = source.Path
			}
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.MailImportReview(data).Render(ctx, w)
}

func (h *Handler) handleReviewMailImport(w http.ResponseWriter, r *http.Request) {
	if !h.mailImportsAvailable(w, r) {
		return
	}
	job, err := h.mailImports.loadOwned(h.userID(r.Context()), r.PathValue("id"))
	if err != nil || job.State != mailJobReview {
		renderMailImportUploadError(w, r, "That upload is no longer waiting. Upload the files again.")
		return
	}
	h.renderMailImportReview(w, r, job, r.URL.Query().Get("account"), "")
}

func (h *Handler) handleStartMailImport(w http.ResponseWriter, r *http.Request) {
	if !h.mailImportsAvailable(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.bad_request"), http.StatusBadRequest)
		return
	}
	ctx, owner := r.Context(), h.userID(r.Context())
	job, err := h.mailImports.loadOwned(owner, r.PathValue("id"))
	if err != nil || job.State != mailJobReview {
		renderMailImportUploadError(w, r, "That upload is no longer waiting. Upload the files again.")
		return
	}
	accountID := r.PostForm.Get("account")
	account, found, err := h.mailExportAccount(ctx, owner, accountID)
	if err != nil || !found || mailImportAccountProblem(account) != "" {
		h.renderMailImportReviewInPanel(w, r, job, "", "Choose an account to import into.")
		return
	}
	if err := h.startMailImport(ctx, job, account, r.PostForm); err != nil {
		message := "The import could not be started."
		switch {
		case errors.Is(err, errMailImportNoFolderName):
			message = "Name each new folder."
		case errors.Is(err, errMailImportNoTarget):
			message = "A folder you chose is no longer in the account. Choose again."
		case errors.Is(err, errMailImportNothingChosen):
			message = "Choose a folder for at least one set of messages."
		default:
			log.Printf("mail import: start: %v", err)
		}
		h.renderMailImportReviewInPanel(w, r, job, account.ID, message)
		return
	}
	mailJobStarted(w, "mail-import-dialog")
	h.renderMailImportPanel(w, r, "")
}

func (h *Handler) renderMailImportReviewInPanel(w http.ResponseWriter, r *http.Request, job *mailImportJob, accountID, message string) {
	h.renderMailImportReview(w, r, job, accountID, message)
}

func (h *Handler) handleCancelMailImport(w http.ResponseWriter, r *http.Request) {
	h.changeMailImport(w, r, h.mailImports.cancelJob)
}

func (h *Handler) handleResumeMailImport(w http.ResponseWriter, r *http.Request) {
	h.changeMailImport(w, r, h.mailImports.resume)
}

func (h *Handler) handleDeleteMailImport(w http.ResponseWriter, r *http.Request) {
	h.changeMailImport(w, r, h.mailImports.remove)
}

func (h *Handler) changeMailImport(w http.ResponseWriter, r *http.Request, change func(*mailImportJob)) {
	if !h.mailImportsAvailable(w, r) {
		return
	}
	job, err := h.mailImports.loadOwned(h.userID(r.Context()), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	change(job)
	if r.FormValue("panel") == "1" {
		h.renderMailImportPanel(w, r, "")
		return
	}
	h.renderMailImportJobs(w, r)
}
