package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/storage"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) registerMailTransferRoutes(register func(string, http.HandlerFunc)) {
	register("GET /api/mail/transfers", h.handleMailTransfers)
	register("GET /api/mail/transfers/jobs", h.handleMailTransferJobs)
	register("GET /api/mail/transfers/status", h.handleMailTransferStatus)
	register("GET /api/mail/transfers/new", h.handleNewMailTransfer)
	register("POST /api/mail/transfers", h.handleStartMailTransfer)
	register("POST /api/mail/transfers/{id}/cancel", h.handleCancelMailTransfer)
	register("POST /api/mail/transfers/{id}/resume", h.handleResumeMailTransfer)
	register("POST /api/mail/transfers/{id}/delete", h.handleDeleteMailTransfer)
	register("GET /api/messages/to-account", h.handleMessagesToAccountDialog)
	register("POST /api/messages/to-account", h.handleMessagesToAccount)
}

func (h *Handler) mailTransfersAvailable(w http.ResponseWriter) bool {
	if h.mailTransfers == nil {
		http.Error(w, "mail transfer is not available", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// mailTransferSourceProblem says why mail can't be read from an account, or
// nothing when it can.
func mailTransferSourceProblem(account models.Account) string {
	if account.ReconnectPending {
		return "Enter its password first."
	}
	return ""
}

// mailTransferFormData lists the accounts and, for the chosen pair, the
// folders to copy and where each goes. Without a choice it starts from the
// first account that can be read and another that can receive mail.
func (h *Handler) mailTransferFormData(r *http.Request, values url.Values, message string) (views.MailTransferFormData, error) {
	ctx, owner := r.Context(), h.userID(r.Context())
	data := views.MailTransferFormData{Message: message, Move: values.Get("mode") == "move"}
	accounts, err := h.mailExportAccounts(ctx, owner)
	if err != nil {
		return data, err
	}
	var live []models.Account
	for _, account := range accounts {
		if !account.IsDeleting {
			live = append(live, account)
		}
	}
	readable := map[string]models.Account{}
	writable := map[string]models.Account{}
	for _, account := range live {
		view := mailExportAccountView(account)
		sourceProblem, targetProblem := mailTransferSourceProblem(account), mailImportAccountProblem(account)
		data.Sources = append(data.Sources, views.MailImportAccount{MailExportAccount: view, Problem: sourceProblem})
		data.Targets = append(data.Targets, views.MailImportAccount{MailExportAccount: view, Problem: targetProblem})
		if sourceProblem == "" {
			readable[account.ID] = account
		}
		if targetProblem == "" {
			writable[account.ID] = account
		}
	}
	if len(live) < 2 {
		return data, nil
	}
	data.SourceID, data.TargetID = values.Get("from"), values.Get("to")
	if _, ok := readable[data.SourceID]; !ok {
		data.SourceID = ""
		for _, account := range live {
			if _, ok := readable[account.ID]; ok {
				data.SourceID = account.ID
				break
			}
		}
	}
	if _, ok := writable[data.TargetID]; !ok || data.TargetID == data.SourceID {
		data.TargetID = ""
		for _, account := range live {
			if _, ok := writable[account.ID]; ok && account.ID != data.SourceID {
				data.TargetID = account.ID
				break
			}
		}
	}
	if data.SourceID == "" || data.TargetID == "" {
		if data.Message == "" {
			data.Message = "Mail needs an account to come from and another to go into, both connected."
		}
		return data, nil
	}
	source, target := readable[data.SourceID], writable[data.TargetID]
	data.SourceEmail = source.Email
	sourceFolders, err := h.mailExportFolders(ctx, owner, source.ID)
	if err != nil {
		return data, err
	}
	targetFolders, err := h.mailExportFolders(ctx, owner, target.ID)
	if err != nil {
		return data, err
	}
	destinations := mailImportTargets(target.Provider, targetFolders)
	for _, folder := range destinations {
		data.Destination = append(data.Destination, views.MailImportFolder{ID: folder.ID, Path: folder.Path})
	}
	for _, folder := range sourceFolders {
		entry := views.MailTransferFolder{ID: folder.ID, Path: folder.Path, Role: folder.Role, Messages: folder.Messages}
		entry.Target, entry.NewName = suggestMailImportTarget(&mailImportSource{Path: folder.Path, Role: folder.Role}, destinations)
		if entry.NewName == "" {
			entry.NewName = folder.Path
		}
		data.Folders = append(data.Folders, entry)
	}
	return data, nil
}

func (h *Handler) mailTransferJobViews(owner string) ([]views.MailTransferJob, error) {
	jobs, err := h.mailTransfers.jobs(owner)
	if err != nil {
		return nil, err
	}
	result := make([]views.MailTransferJob, 0, len(jobs))
	for _, job := range jobs {
		processed, total, copied, duplicates, failed, deleted := job.totals()
		view := views.MailTransferJob{
			ID: job.ID, Source: job.SourceEmail, Target: job.TargetEmail, Move: job.Move, State: job.State, Error: job.Error,
			Processed: processed, Total: total, Copied: copied, Duplicates: duplicates, Failed: failed, Deleted: deleted,
			Started: job.CreatedAt.Local().Format("Jan 2, 15:04"),
		}
		if job.State == mailJobRunning && h.mailTransfers.isWaiting(job.ID) {
			view.Waiting = true
		} else if folder := job.current(); folder != nil {
			view.Folder = folder.Path
		}
		for _, skipped := range job.Skipped {
			subject := skipped.Subject
			if subject == "" {
				subject = "(no subject)"
			}
			view.Skipped = append(view.Skipped, skipped.Source+": "+subject+" ("+skipped.Reason+")")
		}
		result = append(result, view)
	}
	return result, nil
}

func (h *Handler) renderMailTransferPanel(w http.ResponseWriter, r *http.Request, values url.Values, message string) {
	form, err := h.mailTransferFormData(r, values, message)
	if err != nil {
		log.Printf("mail transfer: %v", err)
		http.Error(w, "could not list accounts", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.MailTransferPanel(views.MailTransferPanelData{Form: form}).Render(r.Context(), w)
}

func (h *Handler) renderMailTransferJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := h.mailTransferJobViews(h.userID(r.Context()))
	if err != nil {
		log.Printf("mail transfer: %v", err)
		http.Error(w, "could not list transfers", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.MailTransferJobs(jobs).Render(r.Context(), w)
}

func (h *Handler) handleMailTransfers(w http.ResponseWriter, r *http.Request) {
	if !h.mailTransfersAvailable(w) {
		return
	}
	h.renderMailTransferPanel(w, r, url.Values{}, "")
}

func (h *Handler) handleMailTransferJobs(w http.ResponseWriter, r *http.Request) {
	if !h.mailTransfersAvailable(w) {
		return
	}
	h.renderMailTransferJobs(w, r)
}

// mailTransferStatus is how far a transfer has got, for the toast that
// follows one started from the message list.
type mailTransferStatus struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	Waiting    bool   `json:"waiting,omitempty"`
	Folder     string `json:"folder,omitempty"`
	Processed  int    `json:"processed"`
	Total      int    `json:"total"`
	Copied     int    `json:"copied"`
	Duplicates int    `json:"duplicates"`
	Failed     int    `json:"failed"`
	Deleted    int    `json:"deleted"`
	Error      string `json:"error,omitempty"`
}

// handleMailTransferStatus answers for the transfers named by id; one that
// was removed meanwhile is left out.
func (h *Handler) handleMailTransferStatus(w http.ResponseWriter, r *http.Request) {
	if !h.mailTransfersAvailable(w) {
		return
	}
	owner := h.userID(r.Context())
	result := []mailTransferStatus{}
	for _, id := range r.URL.Query()["id"] {
		job, err := h.mailTransfers.loadOwned(owner, id)
		if err != nil {
			continue
		}
		processed, total, copied, duplicates, failed, deleted := job.totals()
		status := mailTransferStatus{
			ID: job.ID, State: job.State, Error: job.Error,
			Processed: processed, Total: total, Copied: copied, Duplicates: duplicates, Failed: failed, Deleted: deleted,
		}
		if job.State == mailJobRunning && h.mailTransfers.isWaiting(job.ID) {
			status.Waiting = true
		} else if folder := job.current(); folder != nil {
			status.Folder = folder.Path
		}
		result = append(result, status)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (h *Handler) handleNewMailTransfer(w http.ResponseWriter, r *http.Request) {
	if !h.mailTransfersAvailable(w) {
		return
	}
	form, err := h.mailTransferFormData(r, r.URL.Query(), "")
	if err != nil {
		log.Printf("mail transfer: %v", err)
		http.Error(w, "could not list accounts", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.MailTransferForm(form).Render(r.Context(), w)
}

func (h *Handler) handleStartMailTransfer(w http.ResponseWriter, r *http.Request) {
	if !h.mailTransfersAvailable(w) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx, owner := r.Context(), h.userID(r.Context())
	source, foundSource, err := h.mailExportAccount(ctx, owner, r.PostForm.Get("from"))
	if err != nil || !foundSource || mailTransferSourceProblem(source) != "" {
		h.renderMailTransferPanel(w, r, r.PostForm, "Choose the account the mail comes from.")
		return
	}
	target, foundTarget, err := h.mailExportAccount(ctx, owner, r.PostForm.Get("to"))
	if err != nil || !foundTarget || mailImportAccountProblem(target) != "" {
		h.renderMailTransferPanel(w, r, r.PostForm, "Choose the account the mail goes into.")
		return
	}
	if _, err := h.createMailTransfer(ctx, owner, source, target, r.PostForm.Get("mode") == "move", r.PostForm); err != nil {
		message := "The transfer could not be started."
		switch {
		case errors.Is(err, errMailTransferSameAccount):
			message = "Choose two different accounts."
		case errors.Is(err, errMailTransferNoFolders):
			message = "Choose at least one folder."
		case errors.Is(err, errMailImportNoFolderName):
			message = "Name each new folder."
		case errors.Is(err, errMailImportNoTarget):
			message = "A folder you chose is no longer in the account. Choose again."
		default:
			log.Printf("mail transfer: start: %v", err)
		}
		h.renderMailTransferPanel(w, r, r.PostForm, message)
		return
	}
	mailJobStarted(w, "mail-transfer-dialog")
	h.renderMailTransferPanel(w, r, url.Values{}, "")
}

func (h *Handler) handleCancelMailTransfer(w http.ResponseWriter, r *http.Request) {
	h.changeMailTransfer(w, r, h.mailTransfers.cancelJob)
}

func (h *Handler) handleResumeMailTransfer(w http.ResponseWriter, r *http.Request) {
	h.changeMailTransfer(w, r, h.mailTransfers.resume)
}

func (h *Handler) handleDeleteMailTransfer(w http.ResponseWriter, r *http.Request) {
	h.changeMailTransfer(w, r, h.mailTransfers.remove)
}

func (h *Handler) changeMailTransfer(w http.ResponseWriter, r *http.Request, change func(*mailTransferJob)) {
	if !h.mailTransfersAvailable(w) {
		return
	}
	job, err := h.mailTransfers.loadOwned(h.userID(r.Context()), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	change(job)
	h.renderMailTransferJobs(w, r)
}

// mailTransferDestination finds the account and folder a folder ID belongs
// to among the owner's accounts.
func (h *Handler) mailTransferDestination(ctx context.Context, owner, folderID string) (models.Account, storage.MailExportFolder, bool, error) {
	accounts, err := h.mailExportAccounts(ctx, owner)
	if err != nil {
		return models.Account{}, storage.MailExportFolder{}, false, err
	}
	for _, account := range accounts {
		if account.IsDeleting {
			continue
		}
		folders, err := h.mailExportFolders(ctx, owner, account.ID)
		if err != nil {
			return models.Account{}, storage.MailExportFolder{}, false, err
		}
		for _, folder := range mailImportTargets(account.Provider, folders) {
			if folder.ID == folderID {
				return account, folder, true, nil
			}
		}
	}
	return models.Account{}, storage.MailExportFolder{}, false, nil
}

// handleMessagesToAccountDialog asks before messages go to another account,
// naming both accounts.
func (h *Handler) handleMessagesToAccountDialog(w http.ResponseWriter, r *http.Request) {
	if !h.mailTransfersAvailable(w) {
		return
	}
	ctx, owner := r.Context(), h.userID(r.Context())
	query := r.URL.Query()
	data := views.MailToAccountDialogData{}
	data.Count, _ = strconv.Atoi(query.Get("count"))
	target, folder, found, err := h.mailTransferDestination(ctx, owner, query.Get("folder_id"))
	if err != nil {
		log.Printf("mail transfer: find folder: %v", err)
	}
	if !found {
		data.Problem = "That folder can't take mail from another account."
	} else {
		data.Target, data.Folder = target.Email, folder.Path
		if problem := mailImportAccountProblem(target); problem != "" {
			data.Problem = target.Email + ": " + problem
		}
	}
	accounts, err := h.mailExportAccounts(ctx, owner)
	if err != nil {
		log.Printf("mail transfer: list accounts: %v", err)
	}
	for _, id := range strings.Split(query.Get("accounts"), ",") {
		for _, account := range accounts {
			if account.ID != id || account.ID == target.ID {
				continue
			}
			data.Sources = append(data.Sources, account.Email)
			if problem := mailTransferSourceProblem(account); problem != "" && data.Problem == "" {
				data.Problem = account.Email + ": " + problem
			}
		}
	}
	if len(data.Sources) == 0 && data.Problem == "" {
		data.Problem = "These emails are already in that account."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.MailToAccountDialog(data).Render(ctx, w)
}

// handleMessagesToAccount copies or moves messages chosen in the message
// list into a folder of another account, as a transfer for each account
// they come from.
func (h *Handler) handleMessagesToAccount(w http.ResponseWriter, r *http.Request) {
	if !h.mailTransfersAvailable(w) {
		return
	}
	var payload struct {
		messageBulkRequest
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&payload); err != nil || len(payload.Targets) == 0 {
		http.Error(w, "No emails were chosen.", http.StatusBadRequest)
		return
	}
	ctx, owner := r.Context(), h.userID(r.Context())
	target, destination, found, err := h.mailTransferDestination(ctx, owner, payload.FolderID)
	if err != nil || !found {
		http.Error(w, "That folder can't take mail from another account.", http.StatusBadRequest)
		return
	}
	if problem := mailImportAccountProblem(target); problem != "" {
		http.Error(w, target.Email+": "+problem, http.StatusBadRequest)
		return
	}
	var infos []storage.ThreadMessageMutationInfo
	err = h.withProfileStore(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
		local := h
		if h.userAccounts != nil {
			local = &Handler{db: db, auth: h.auth}
		}
		targets, err := local.resolveMessageBulkTargets(ctx, payload.messageBulkRequest, strings.TrimSpace(payload.SourceFolderID), false)
		if err != nil {
			return err
		}
		for _, resolved := range targets {
			infos = append(infos, resolved.Infos...)
		}
		return nil
	})
	if err != nil {
		writeMessageTargetError(w, r, err)
		return
	}
	bySource := map[string]map[string][]int64{}
	for _, info := range infos {
		if info.AccountID == target.ID {
			http.Error(w, "Some of these emails are already in "+target.Email+". Move them separately.", http.StatusBadRequest)
			return
		}
		if bySource[info.AccountID] == nil {
			bySource[info.AccountID] = map[string][]int64{}
		}
		bySource[info.AccountID][info.FolderID] = append(bySource[info.AccountID][info.FolderID], info.MessageID)
	}
	move := payload.Mode != "copy"
	sources := map[string]models.Account{}
	for accountID := range bySource {
		source, ok, err := h.mailExportAccount(ctx, owner, accountID)
		if err != nil || !ok || mailTransferSourceProblem(source) != "" {
			http.Error(w, "One of the accounts these emails are in can't be read right now.", http.StatusBadRequest)
			return
		}
		sources[accountID] = source
	}
	var started []string
	for accountID, ids := range bySource {
		job, err := h.mailTransferMessages(ctx, owner, sources[accountID], target, destination, ids, move)
		if err != nil {
			log.Printf("mail transfer: start from the message list: %v", err)
			http.Error(w, "The emails could not be sent to the other account.", http.StatusInternalServerError)
			return
		}
		started = append(started, job.ID)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"messages": len(infos), "transfers": len(started), "jobs": started, "account": target.Email, "folder": destination.Path})
}
