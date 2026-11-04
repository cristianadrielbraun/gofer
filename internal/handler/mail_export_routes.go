package handler

import (
	"context"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) registerMailExportRoutes(register func(string, http.HandlerFunc)) {
	register("GET /api/mail/exports", h.handleMailExports)
	register("GET /api/mail/exports/jobs", h.handleMailExportJobs)
	register("GET /api/mail/exports/new", h.handleNewMailExport)
	register("POST /api/mail/exports", h.handleStartMailExport)
	register("POST /api/mail/exports/{id}/cancel", h.handleCancelMailExport)
	register("POST /api/mail/exports/{id}/resume", h.handleResumeMailExport)
	register("POST /api/mail/exports/{id}/delete", h.handleDeleteMailExport)
	register("GET /api/mail/exports/{id}/download", h.handleDownloadMailExport)
}

func (h *Handler) mailExportsAvailable(w http.ResponseWriter, r *http.Request) bool {
	if h.mailExports == nil {
		http.Error(w, i18n.T(r.Context(), "server.error.mail_export_is_not_available"), http.StatusServiceUnavailable)
		return false
	}
	return true
}

func mailExportAccountView(account models.Account) views.MailExportAccount {
	return views.MailExportAccount{ID: account.ID, Email: account.Email, Name: account.Name, Provider: account.Provider, Color: account.Color, Pending: account.ReconnectPending}
}

func (h *Handler) mailExportPanelData(r *http.Request, message string) (views.MailExportPanelData, error) {
	ctx, owner := r.Context(), h.userID(r.Context())
	data := views.MailExportPanelData{Message: message}
	accounts, err := h.mailExportAccounts(ctx, owner)
	if err != nil {
		return data, err
	}
	for _, account := range accounts {
		if account.IsDeleting {
			continue
		}
		data.Accounts = append(data.Accounts, mailExportAccountView(account))
	}
	return data, nil
}

// mailJobStarted closes the dialog a job was started from and has the job
// lists on the page refresh.
func mailJobStarted(w http.ResponseWriter, dialogID string) {
	w.Header().Set("HX-Trigger", `{"gofer-close-dialog":"`+dialogID+`","mail-jobs-changed":true}`)
}

func (h *Handler) mailExportJobViews(ctx context.Context, owner string) ([]views.MailExportJob, error) {
	jobs, err := h.mailExports.jobs(owner)
	if err != nil {
		return nil, err
	}
	result := make([]views.MailExportJob, 0, len(jobs))
	for _, job := range jobs {
		processed, total := job.totals()
		view := views.MailExportJob{
			ID: job.ID, Account: job.AccountEmail, State: job.State, Error: job.Error,
			Processed: processed, Total: total, Exported: job.written(), Failed: job.Failed, NotSaved: job.NotSaved,
			SavedOnly: job.LocalOnly, Started: i18n.Dates(ctx).ShortDateTime(job.CreatedAt.Local()),
		}
		if job.State == mailJobRunning && h.mailExports.isWaiting(job.ID) {
			view.Waiting = true
		} else if folder := job.current(); folder != nil {
			view.Folder = folder.Path
		}
		for _, skipped := range job.Skipped {
			view.Skipped = append(view.Skipped, skipped.Folder+": "+serverText(ctx, skipped.Reason))
		}
		result = append(result, view)
	}
	return result, nil
}

func (h *Handler) renderMailExportPanel(w http.ResponseWriter, r *http.Request, message string) {
	data, err := h.mailExportPanelData(r, message)
	if err != nil {
		log.Printf("mail export: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.could_not_list_exports"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.MailExportPanel(data).Render(r.Context(), w)
}

func (h *Handler) renderMailExportJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := h.mailExportJobViews(r.Context(), h.userID(r.Context()))
	if err != nil {
		log.Printf("mail export: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.could_not_list_exports"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.MailExportJobs(jobs).Render(r.Context(), w)
}

func (h *Handler) handleMailExports(w http.ResponseWriter, r *http.Request) {
	if !h.mailExportsAvailable(w, r) {
		return
	}
	h.renderMailExportPanel(w, r, "")
}

func (h *Handler) handleMailExportJobs(w http.ResponseWriter, r *http.Request) {
	if !h.mailExportsAvailable(w, r) {
		return
	}
	h.renderMailExportJobs(w, r)
}

func (h *Handler) handleNewMailExport(w http.ResponseWriter, r *http.Request) {
	if !h.mailExportsAvailable(w, r) {
		return
	}
	ctx, owner := r.Context(), h.userID(r.Context())
	account, found, err := h.mailExportAccount(ctx, owner, r.URL.Query().Get("account"))
	if err != nil || !found {
		h.renderMailExportPanel(w, r, "That account is no longer in Gofer.")
		return
	}
	data := views.MailExportFormData{Account: mailExportAccountView(account)}
	folders, err := h.mailExportFolders(ctx, owner, account.ID)
	if err != nil {
		log.Printf("mail export: list folders: %v", err)
		data.Message = i18n.T(r.Context(), "server.msg.the_account_s_folders_could_not_be")
	}
	for _, folder := range folders {
		data.Folders = append(data.Folders, views.MailExportFolder{ID: folder.ID, Path: folder.Path, Role: folder.Role, Messages: folder.Messages})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.MailExportForm(data).Render(ctx, w)
}

func (h *Handler) handleStartMailExport(w http.ResponseWriter, r *http.Request) {
	if !h.mailExportsAvailable(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.bad_request"), http.StatusBadRequest)
		return
	}
	ctx, owner := r.Context(), h.userID(r.Context())
	account, found, err := h.mailExportAccount(ctx, owner, r.PostForm.Get("account"))
	if err != nil || !found {
		h.renderMailExportPanel(w, r, "That account is no longer in Gofer.")
		return
	}
	// An account still waiting for its password can't download anything.
	savedOnly := r.PostForm.Get("saved_only") == "1" || account.ReconnectPending
	if _, err := h.createMailExport(ctx, owner, account, r.PostForm["folder"], savedOnly); err != nil {
		log.Printf("mail export: start: %v", err)
		message := "The export could not be started."
		if strings.Contains(err.Error(), "choose at least one folder") {
			message = "Choose at least one folder to export."
		}
		h.renderMailExportPanel(w, r, message)
		return
	}
	mailJobStarted(w, "mail-export-dialog")
	h.renderMailExportPanel(w, r, "")
}

func (h *Handler) handleCancelMailExport(w http.ResponseWriter, r *http.Request) {
	h.changeMailExport(w, r, h.mailExports.cancelJob)
}

func (h *Handler) handleResumeMailExport(w http.ResponseWriter, r *http.Request) {
	h.changeMailExport(w, r, h.mailExports.resume)
}

func (h *Handler) handleDeleteMailExport(w http.ResponseWriter, r *http.Request) {
	h.changeMailExport(w, r, h.mailExports.remove)
}

func (h *Handler) changeMailExport(w http.ResponseWriter, r *http.Request, change func(*mailExportJob)) {
	if !h.mailExportsAvailable(w, r) {
		return
	}
	job, err := h.mailExports.loadOwned(h.userID(r.Context()), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	change(job)
	h.renderMailExportJobs(w, r)
}

func (h *Handler) handleDownloadMailExport(w http.ResponseWriter, r *http.Request) {
	if !h.mailExportsAvailable(w, r) {
		return
	}
	job, err := h.mailExports.loadOwned(h.userID(r.Context()), r.PathValue("id"))
	if errors.Is(err, os.ErrNotExist) || (err == nil && job.State != mailJobDone) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.could_not_read_export"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+mailExportArchiveName(job)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	if err := writeMailExportArchive(h.mailExports.jobDir(job.ID), w, job); err != nil {
		log.Printf("mail export %s: download: %v", job.ID, err)
	}
}
