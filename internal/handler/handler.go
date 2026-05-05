package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"gofer.email/internal/config"
	mail "gofer.email/internal/mail"
	"gofer.email/internal/mail/imap"
	"gofer.email/internal/mail/message"
	smtpclient "gofer.email/internal/mail/smtp"
	"gofer.email/internal/models"
	"gofer.email/internal/storage"
	"gofer.email/internal/store"
	"gofer.email/internal/views"
	"html/template"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	goimap "github.com/emersion/go-imap/v2"
)

type Handler struct {
	db           *storage.DB
	accountStore *config.AccountStore
	syncer       *mail.SyncOrchestrator
	blobStore    *store.BlobStore
}

func New(db *storage.DB, accountStore *config.AccountStore, syncer *mail.SyncOrchestrator, blobStore *store.BlobStore) *Handler {
	return &Handler{db: db, accountStore: accountStore, syncer: syncer, blobStore: blobStore}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	setupAssetsRoutes(mux)
	mux.HandleFunc("GET /", h.handleIndex)
	mux.HandleFunc("GET /email/{id}", h.handleEmailPartial)
	mux.HandleFunc("GET /email/{id}/body", h.handleEmailBody)
	mux.HandleFunc("GET /folder/{id}", h.handleFolderPartial)
	mux.HandleFunc("GET /folder/{id}/full", h.handleFolderFull)
	mux.HandleFunc("GET /folder/{id}/{email}", h.handleFolderWithEmail)
	mux.HandleFunc("GET /mail/folder/{id}/items", h.handleMailItems)
	mux.HandleFunc("GET /search", h.handleSearch)
	mux.HandleFunc("POST /api/accounts", h.handleCreateAccount)
	mux.HandleFunc("GET /api/accounts/{id}/edit", h.handleGetEditAccount)
	mux.HandleFunc("POST /api/accounts/{id}/edit", h.handleUpdateAccount)
	mux.HandleFunc("POST /api/accounts/{id}/test", h.handleTestAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", h.handleDeleteAccount)
	mux.HandleFunc("GET /settings", h.handleSettings)
	mux.HandleFunc("GET /settings/{tab}", h.handleSettingsTab)
	mux.HandleFunc("POST /api/settings/sync", h.handleSaveSyncSettings)
	mux.HandleFunc("GET /api/attachments/{id}/download", h.handleAttachmentDownload)
	mux.HandleFunc("GET /api/events", h.handleSSE)
	mux.HandleFunc("GET /api/folders/unread", h.handleFolderUnreadCounts)
	mux.HandleFunc("POST /compose", h.handleCompose)
	mux.HandleFunc("POST /api/messages/{id}/read", h.handleToggleRead)
	mux.HandleFunc("POST /api/messages/{id}/star", h.handleToggleStar)
	mux.HandleFunc("DELETE /api/messages/{id}", h.handleDeleteMessage)
	mux.HandleFunc("POST /api/messages/{id}/move", h.handleMoveMessage)
	mux.HandleFunc("POST /api/messages/{id}/refetch", h.handleRefetchBody)
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
	ctx := r.Context()

	accounts, _ := h.db.GetAccounts(ctx)
	totalCount, _ := h.db.GetFolderEmailCount(ctx, folderID)

	page, _ := h.db.GetEmailsRange(ctx, folderID, 0, 50)
	var emails []models.Email
	if page != nil {
		emails = page.Emails
	}

	var selectedEmail *models.Email
	if emailID != "" {
		selectedEmail, _ = h.db.GetEmailByID(ctx, emailID)
	}
	if selectedEmail == nil && len(emails) > 0 {
		selectedEmail, _ = h.db.GetEmailByID(ctx, emails[0].ID)
	}

	views.Layout(accounts, folderID, emails, selectedEmail, totalCount).Render(ctx, w)
}

func (h *Handler) handleFolderWithEmail(w http.ResponseWriter, r *http.Request) {
	folderID := r.PathValue("id")
	emailID := r.PathValue("email")
	if folderID == "" {
		folderID = "inbox"
	}

	ctx := r.Context()
	accounts, _ := h.db.GetAccounts(ctx)
	totalCount, _ := h.db.GetFolderEmailCount(ctx, folderID)

	page, _ := h.db.GetEmailsRange(ctx, folderID, 0, 50)
	var emails []models.Email
	if page != nil {
		emails = page.Emails
	}

	var selectedEmail *models.Email
	if emailID != "" {
		selectedEmail, _ = h.db.GetEmailByID(ctx, emailID)
	}
	if selectedEmail == nil && len(emails) > 0 {
		selectedEmail, _ = h.db.GetEmailByID(ctx, emails[0].ID)
	}

	views.Layout(accounts, folderID, emails, selectedEmail, totalCount).Render(ctx, w)
}

func (h *Handler) handleEmailPartial(w http.ResponseWriter, r *http.Request) {
	emailID := r.PathValue("id")
	if emailID == "" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()

	email, err := h.db.GetEmailByID(ctx, emailID)
	if err != nil || email == nil {
		http.NotFound(w, r)
		return
	}

	msgID, _ := strconv.ParseInt(emailID, 10, 64)
	if msgID > 0 && !h.db.IsBodyFetched(ctx, msgID) {
		h.fetchBody(ctx, msgID, email.AccountID)
		email, _ = h.db.GetEmailByID(ctx, emailID)
		if email == nil {
			http.NotFound(w, r)
			return
		}
	}

	w.Header().Set("Content-Type", "text/html")
	views.MailViewContent(email).Render(ctx, w)
}

var emailResizeScript = []byte(`<script>(function(){function r(){parent.postMessage({type:'emailBodyResize',height:document.documentElement.scrollHeight},'*')}r();document.querySelectorAll('img').forEach(function(i){i.onload=r});if(typeof MutationObserver!=='undefined'){new MutationObserver(r).observe(document.body,{childList:true,subtree:true})}})();</script>`)

func (h *Handler) handleEmailBody(w http.ResponseWriter, r *http.Request) {
	emailID := r.PathValue("id")
	if emailID == "" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()

	msgID, _ := strconv.ParseInt(emailID, 10, 64)
	if msgID > 0 && !h.db.IsBodyFetched(ctx, msgID) {
		info, err := h.db.GetMessageFetchInfo(ctx, msgID)
		if err != nil || info == nil {
			http.NotFound(w, r)
			return
		}
		h.fetchBody(ctx, msgID, info.AccountID)
	}

	body, err := h.db.GetEmailBody(ctx, emailID)
	if err != nil || body == nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buildBodyDocument(body, emailResizeScript))
}

func buildBodyDocument(body []byte, resizeScript []byte) []byte {
	s := string(body)
	lower := strings.ToLower(s)

	if strings.Contains(lower, "<html") {
		if idx := strings.LastIndex(lower, "</body>"); idx != -1 {
			return []byte(s[:idx] + string(resizeScript) + s[idx:])
		}
		return []byte(s + string(resizeScript))
	}

	doc := "<!DOCTYPE html><html><head><meta charset=\"utf-8\"><style>" +
		"body{margin:0;padding:8px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;font-size:14px;line-height:1.5;color:#333;word-wrap:break-word}" +
		"img{max-width:100%;height:auto}" +
		"</style></head><body>" +
		s +
		string(resizeScript) +
		"</body></html>"
	return []byte(doc)
}

func (h *Handler) fetchBody(ctx context.Context, msgID int64, accountID string) {
	info, err := h.db.GetMessageFetchInfo(ctx, msgID)
	if err != nil || info == nil {
		return
	}

	cfg, err := h.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		return
	}

	password, err := h.accountStore.DecryptPassword(ctx, accountID)
	if err != nil {
		return
	}

	client, err := imap.NewClient(ctx, cfg, password)
	if err != nil {
		return
	}
	defer client.Close()

	bodyData, err := client.FetchBody(ctx, info.FolderRemoteID, info.RemoteUID)
	if err != nil {
		return
	}

	parsed, err := message.ParseMessage(ctx, bytes.NewReader(bodyData), h.blobStore, accountID, msgID)
	if err != nil {
		return
	}

	var textPath, htmlPath string
	if parsed.TextBody != "" {
		p, err := h.blobStore.StoreBodyText(ctx, accountID, msgID, []byte(parsed.TextBody))
		if err == nil {
			textPath = p
		}
	}

	if len(parsed.HTMLBody) > 0 {
		sanitized := message.SanitizeHTML(parsed.HTMLBody)
		p, err := h.blobStore.StoreBodyHTML(ctx, accountID, msgID, sanitized)
		if err == nil {
			htmlPath = p
		}
	}

	snippet := parsed.Snippet
	if snippet == "" {
		snippet = parsed.Subject
	}

	if err := h.db.UpdateMessageBody(ctx, msgID, textPath, htmlPath, parsed.RawPath, snippet); err != nil {
		return
	}

	h.db.UpdateMessageHeaders(ctx, msgID, parsed.Subject, parsed.FromName, parsed.FromEmail, snippet)

	var toRecs, ccRecs []storage.Recipient
	for _, r := range parsed.To {
		toRecs = append(toRecs, storage.Recipient{Name: r.Name, Email: r.Email})
	}
	for _, r := range parsed.CC {
		ccRecs = append(ccRecs, storage.Recipient{Name: r.Name, Email: r.Email})
	}
	h.db.UpsertRecipients(ctx, msgID, toRecs, ccRecs)

	if len(parsed.Attachments) > 0 {
		var attRows []storage.AttachmentRow
		for _, a := range parsed.Attachments {
			attRows = append(attRows, storage.AttachmentRow{
				Filename:    a.Filename,
				ContentType: a.ContentType,
				SizeBytes:   a.Size,
				ContentID:   a.ContentID,
				Inline:      a.Inline,
				StoragePath: a.BlobPath,
			})
		}
		h.db.InsertAttachments(ctx, msgID, attRows)
	}
}

func (h *Handler) handleFolderPartial(w http.ResponseWriter, r *http.Request) {
	folderID := r.PathValue("id")
	if folderID == "" {
		folderID = "inbox"
	}

	ctx := r.Context()
	totalCount, _ := h.db.GetFolderEmailCount(ctx, folderID)

	page, _ := h.db.GetEmailsRange(ctx, folderID, 0, 50)
	var emails []models.Email
	if page != nil {
		emails = page.Emails
	}

	var selectedEmail *models.Email
	if len(emails) > 0 {
		selectedEmail, _ = h.db.GetEmailByID(ctx, emails[0].ID)
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		views.FolderPartial(emails, folderID, selectedEmail, totalCount).Render(ctx, w)
		return
	}

	accounts, _ := h.db.GetAccounts(ctx)
	views.Layout(accounts, folderID, emails, selectedEmail, totalCount).Render(ctx, w)
}

func (h *Handler) handleFolderFull(w http.ResponseWriter, r *http.Request) {
	folderID := r.PathValue("id")
	if folderID == "" {
		folderID = "inbox"
	}

	ctx := r.Context()
	totalCount, _ := h.db.GetFolderEmailCount(ctx, folderID)

	page, _ := h.db.GetEmailsRange(ctx, folderID, 0, 50)
	var emails []models.Email
	if page != nil {
		emails = page.Emails
	}

	var selectedEmail *models.Email
	if len(emails) > 0 {
		selectedEmail, _ = h.db.GetEmailByID(ctx, emails[0].ID)
	}

	w.Header().Set("Content-Type", "text/html")
	views.MailContentPartial(emails, folderID, selectedEmail, totalCount).Render(ctx, w)
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
	ctx := r.Context()

	var page *models.EmailPage

	if around := r.URL.Query().Get("around"); around != "" {
		page, _ = h.db.GetEmailsAroundEmail(ctx, folderID, around, limit)
	} else if startStr := r.URL.Query().Get("start"); startStr != "" {
		start, err := strconv.Atoi(startStr)
		if err != nil || start < 0 {
			start = 0
		}
		page, _ = h.db.GetEmailsRange(ctx, folderID, start, limit)
	} else if cursor := r.URL.Query().Get("after"); cursor != "" {
		page, _ = h.db.GetEmailsAfterCursor(ctx, folderID, cursor, limit)
	} else {
		page, _ = h.db.GetEmailsRange(ctx, folderID, 0, limit)
	}

	if page == nil {
		page = &models.EmailPage{}
	}

	w.Header().Set("Content-Type", "text/html")
	views.MailListItemsFragment(
		page.Emails, folderID,
		page.WindowStart, page.WindowEnd, page.TotalCount,
		page.NextCursor, page.HasMore,
		selectedEmailId,
	).Render(ctx, w)
}

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		w.Header().Set("Content-Type", "text/html")
		views.MailListEmails(nil, "", nil, 0).Render(r.Context(), w)
		return
	}

	emails, err := h.db.SearchMessages(r.Context(), q, 50)
	if err != nil {
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	views.MailListEmails(emails, "", nil, len(emails)).Render(r.Context(), w)
}

func (h *Handler) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.Header().Set("Content-Type", "text/html")
		views.AccountFormError("Invalid form data").Render(r.Context(), w)
		return
	}

	req := models.CreateAccountRequest{
		EmailAddress: r.FormValue("email_address"),
		DisplayName:  r.FormValue("display_name"),
		IMAPHost:     r.FormValue("imap_host"),
		IMAPPort:     atoiDefault(r.FormValue("imap_port"), 993),
		IMAPTLSMode:  r.FormValue("imap_tls_mode"),
		SMTPHost:     r.FormValue("smtp_host"),
		SMTPPort:     atoiDefault(r.FormValue("smtp_port"), 465),
		SMTPTLSMode:  r.FormValue("smtp_tls_mode"),
		Username:     r.FormValue("username"),
		Password:     r.FormValue("password"),
		AuthMethod:   r.FormValue("auth_method"),
		SmtpUsername: r.FormValue("smtp_username"),
		SmtpPassword: r.FormValue("smtp_password"),
	}

	if req.EmailAddress == "" || req.IMAPHost == "" || req.SMTPHost == "" || req.Username == "" || req.Password == "" {
		w.Header().Set("Content-Type", "text/html")
		views.AccountFormError("All required fields must be filled in").Render(r.Context(), w)
		return
	}

	account, err := h.accountStore.CreateAccount(r.Context(), &req)
	if err != nil {
		w.Header().Set("Content-Type", "text/html")
		views.AccountFormError(fmt.Sprintf("Failed to create account: %v", err)).Render(r.Context(), w)
		return
	}

	h.syncer.SyncAccount(r.Context(), account.ID)

	w.Header().Set("Content-Type", "text/html")
	views.WizardStepSuccess("Account created", account.ID, "add").Render(r.Context(), w)
}

func (h *Handler) handleGetEditAccount(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	if accountID == "" {
		http.Error(w, "account id required", http.StatusBadRequest)
		return
	}

	data, err := h.accountStore.GetEditData(r.Context(), accountID)
	if err != nil {
		http.Error(w, fmt.Sprintf("get account: %v", err), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	views.EditAccountDialog(*data).Render(r.Context(), w)
}

func (h *Handler) handleUpdateAccount(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	if accountID == "" {
		w.Header().Set("Content-Type", "text/html")
		views.AccountFormError("Account ID is required").Render(r.Context(), w)
		return
	}

	if err := r.ParseForm(); err != nil {
		w.Header().Set("Content-Type", "text/html")
		views.AccountFormError("Invalid form data").Render(r.Context(), w)
		return
	}

	req := models.CreateAccountRequest{
		EmailAddress: r.FormValue("email_address"),
		DisplayName:  r.FormValue("display_name"),
		IMAPHost:     r.FormValue("imap_host"),
		IMAPPort:     atoiDefault(r.FormValue("imap_port"), 993),
		IMAPTLSMode:  r.FormValue("imap_tls_mode"),
		SMTPHost:     r.FormValue("smtp_host"),
		SMTPPort:     atoiDefault(r.FormValue("smtp_port"), 465),
		SMTPTLSMode:  r.FormValue("smtp_tls_mode"),
		Username:     r.FormValue("username"),
		Password:     r.FormValue("password"),
		AuthMethod:   r.FormValue("auth_method"),
		SmtpUsername: r.FormValue("smtp_username"),
		SmtpPassword: r.FormValue("smtp_password"),
	}

	if req.EmailAddress == "" || req.IMAPHost == "" || req.SMTPHost == "" || req.Username == "" {
		w.Header().Set("Content-Type", "text/html")
		views.AccountFormError("All required fields must be filled in").Render(r.Context(), w)
		return
	}

	if err := h.accountStore.UpdateAccount(r.Context(), accountID, &req); err != nil {
		w.Header().Set("Content-Type", "text/html")
		views.AccountFormError(fmt.Sprintf("Failed to update account: %v", err)).Render(r.Context(), w)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	views.WizardStepSuccess("Account updated", accountID, "edit").Render(r.Context(), w)
}

func (h *Handler) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	if accountID == "" {
		http.Error(w, "account id required", http.StatusBadRequest)
		return
	}

	if err := h.accountStore.DeleteAccount(r.Context(), accountID); err != nil {
		http.Error(w, fmt.Sprintf("delete account: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Hx-Redirect", "/settings/accounts")
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) handleTestAccount(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	if accountID == "" {
		http.Error(w, "account id required", http.StatusBadRequest)
		return
	}

	cfg, err := h.accountStore.GetConfig(r.Context(), accountID)
	if err != nil {
		http.Error(w, fmt.Sprintf("get config: %v", err), http.StatusNotFound)
		return
	}

	password, err := h.accountStore.DecryptPassword(r.Context(), accountID)
	if err != nil {
		http.Error(w, fmt.Sprintf("decrypt password: %v", err), http.StatusInternalServerError)
		return
	}

	results := []models.ConnectionTestResult{}

	imapErr := imap.TestConnection(r.Context(), cfg, password)
	imapResult := models.ConnectionTestResult{
		Service: "imap",
		Message: fmt.Sprintf("%s:%d (%s)", cfg.IMAPHost, cfg.IMAPPort, cfg.IMAPTLSMode),
	}
	if imapErr != nil {
		imapResult.Error = imapErr.Error()
	} else {
		imapResult.Success = true
		imapResult.Message = "Connection successful"
	}
	results = append(results, imapResult)

	smtpPassword := password
	if cfg.SmtpUsername != "" {
		smtpPw, err := h.accountStore.DecryptSmtpPassword(r.Context(), accountID)
		if err != nil {
			http.Error(w, fmt.Sprintf("decrypt smtp password: %v", err), http.StatusInternalServerError)
			return
		}
		smtpPassword = smtpPw
	}

	smtpErr := smtpclient.TestConnection(r.Context(), cfg, smtpPassword)
	smtpResult := models.ConnectionTestResult{
		Service: "smtp",
		Message: fmt.Sprintf("%s:%d (%s)", cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPTLSMode),
	}
	if smtpErr != nil {
		smtpResult.Error = smtpErr.Error()
	} else {
		smtpResult.Success = true
		smtpResult.Message = "Connection successful"
	}
	results = append(results, smtpResult)

	w.Header().Set("Content-Type", "text/html")
	wizardType := r.URL.Query().Get("wizard")
	if wizardType != "" {
		views.ConnectionTestResults(results, accountID, wizardType).Render(r.Context(), w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"results": results,
	})
}

func (h *Handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/settings" {
		http.Redirect(w, r, "/settings/accounts", http.StatusMovedPermanently)
		return
	}
	ctx := r.Context()
	accounts, _ := h.db.GetAccounts(ctx)
	syncSettings := h.buildSyncSettings(ctx, accounts)

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		views.SettingsPartial(accounts, syncSettings, "accounts").Render(ctx, w)
		return
	}

	views.SettingsLayout(accounts, syncSettings, "accounts").Render(ctx, w)
}

func (h *Handler) handleSettingsTab(w http.ResponseWriter, r *http.Request) {
	tab := r.PathValue("tab")
	if tab != "accounts" && tab != "sync" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	accounts, _ := h.db.GetAccounts(ctx)
	syncSettings := h.buildSyncSettings(ctx, accounts)

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		views.SettingsPartial(accounts, syncSettings, tab).Render(ctx, w)
		return
	}

	views.SettingsLayout(accounts, syncSettings, tab).Render(ctx, w)
}

func (h *Handler) buildSyncSettings(ctx context.Context, accounts []models.Account) models.SyncSettings {
	syncInterval := h.db.GetSyncInterval(ctx)

	var accountStatuses []models.AccountSyncStatus
	for _, account := range accounts {
		folders, err := h.db.GetFoldersForAccount(ctx, account.ID)
		if err != nil {
			continue
		}

		idleRoles := h.db.GetIdleFoldersForAccount(ctx, account.ID)

		var status models.AccountSyncStatus
		status.AccountID = account.ID
		status.AccountName = account.Name
		status.AccountEmail = account.Email
		status.Color = account.Color
		status.Initials = account.Initials

		for _, f := range folders {
			lastSynced := ""
			if f.LastIncrementalAt.Valid {
				lastSynced = formatSyncTime(f.LastIncrementalAt.Time)
			} else if f.LastFullSyncAt.Valid {
				lastSynced = formatSyncTime(f.LastFullSyncAt.Time)
			}

			name := f.Role
			if name == "custom" {
				name = f.RemoteID
			}

			status.Folders = append(status.Folders, models.FolderSyncStatus{
				Name:         name,
				Icon:         folderIconFromRole(f.Role),
				Role:         f.Role,
				LastSyncedAt: lastSynced,
				MessageCount: f.TotalCount,
				IsIDLE:       idleRoles[f.Role],
			})
		}

		accountStatuses = append(accountStatuses, status)
	}

	return models.SyncSettings{
		SyncIntervalMinutes: syncInterval,
		Accounts:            accountStatuses,
	}
}

func formatSyncTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	diff := time.Since(t)
	switch {
	case diff < time.Minute:
		return "just now"
	case diff < time.Hour:
		return fmt.Sprintf("%dm ago", int(diff.Minutes()))
	case diff < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(diff.Hours()))
	default:
		return t.Format("Jan 2")
	}
}

func folderIconFromRole(role string) string {
	switch role {
	case "inbox":
		return "inbox"
	case "sent":
		return "send"
	case "drafts":
		return "file-pen"
	case "trash":
		return "trash-2"
	case "junk":
		return "shield-alert"
	case "archive":
		return "archive"
	case "starred":
		return "star"
	default:
		return "folder"
	}
}

func (h *Handler) handleSaveSyncSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	val := r.FormValue("sync_interval_minutes")
	n, err := strconv.Atoi(val)
	if err != nil || n < 1 {
		http.Error(w, "invalid interval", http.StatusBadRequest)
		return
	}

	if err := h.db.SetSetting(ctx, "sync_interval_minutes", strconv.Itoa(n)); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}

	perAccount := make(map[string][]string)
	for _, entry := range r.Form["idle_folders"] {
		parts := strings.SplitN(entry, ":", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			perAccount[parts[0]] = append(perAccount[parts[0]], parts[1])
		}
	}

	allAccountIDs := r.Form["account_ids"]
	for _, aid := range allAccountIDs {
		if _, exists := perAccount[aid]; !exists {
			perAccount[aid] = []string{"none"}
		}
	}

	if err := h.db.SetIdleFoldersAll(ctx, perAccount); err != nil {
		http.Error(w, "save idle folders failed", http.StatusInternalServerError)
		return
	}

	h.syncer.UpdateInterval(n)
	h.syncer.RestartIDLEWatchers(ctx)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func atoiDefault(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil && v > 0 {
		return v
	}
	return def
}

func (h *Handler) handleAttachmentDownload(w http.ResponseWriter, r *http.Request) {
	attIDStr := r.PathValue("id")
	attID, err := strconv.ParseInt(attIDStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()

	var filename, contentType, storagePath string
	err = h.db.Read().QueryRowContext(ctx,
		`SELECT filename, content_type, storage_path FROM attachments WHERE id = ?`, attID,
	).Scan(&filename, &contentType, &storagePath)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(storagePath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	http.ServeContent(w, r, filename, time.Time{}, f)
}

func (h *Handler) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := h.syncer.Events().Subscribe()
	defer h.syncer.Events().Unsubscribe(ch)

	fmt.Fprintf(w, "event: connected\ndata: {}\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-ch:
			m := map[string]string{
				"type":       string(event.Type),
				"account_id": event.AccountID,
				"folder_id":  event.FolderID,
			}
			if event.Status != "" {
				m["status"] = event.Status
			}
			if event.Error != "" {
				m["error"] = event.Error
			}
			data, _ := json.Marshal(m)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data)
			flusher.Flush()
		}
	}
}

func (h *Handler) handleFolderUnreadCounts(w http.ResponseWriter, r *http.Request) {
	counts, err := h.db.GetAllFolderUnreadCounts(r.Context())
	if err != nil {
		http.Error(w, "failed to get unread counts", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(counts)
}

func (h *Handler) handleCompose(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid form data"})
		return
	}

	ctx := r.Context()
	accountID := r.FormValue("account_id")
	if accountID == "" {
		accountID = h.accountStore.GetFirstAccountID(ctx)
	}
	if accountID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "no account configured"})
		return
	}

	cfg, err := h.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "account not found"})
		return
	}

	password, err := h.accountStore.DecryptPassword(ctx, accountID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to decrypt credentials"})
		return
	}

	smtpPassword := password
	if cfg.SmtpUsername != "" {
		smtpPw, err := h.accountStore.DecryptSmtpPassword(ctx, accountID)
		if err == nil && smtpPw != "" {
			smtpPassword = smtpPw
		}
	}

	account, err := h.accountStore.GetAccountByID(ctx, accountID)
	if err != nil || account == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "account not found"})
		return
	}

	toAddrs, err := message.ParseAddressList(r.FormValue("to"))
	if err != nil || len(toAddrs) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Please enter at least one recipient."})
		return
	}
	ccAddrs, _ := message.ParseAddressList(r.FormValue("cc"))
	bccAddrs, _ := message.ParseAddressList(r.FormValue("bcc"))

	body := r.FormValue("body")
	htmlBody := ""
	if body != "" {
		htmlBody = "<html><body><pre style=\"white-space:pre-wrap;font-family:sans-serif\">" + template.HTMLEscapeString(body) + "</pre></body></html>"
	}

	msg := &message.OutgoingMessage{
		FromName:   account.Name,
		FromEmail:  account.Email,
		To:         toAddrs,
		CC:         ccAddrs,
		Bcc:        bccAddrs,
		Subject:    r.FormValue("subject"),
		TextBody:   body,
		HTMLBody:   htmlBody,
		InReplyTo:  r.FormValue("in_reply_to"),
		References: r.FormValue("references"),
	}

	go func() {
		result, sendErr := smtpclient.SendMessage(context.Background(), cfg, smtpPassword, msg)

		evt := mail.Event{
			Type:      mail.EventSendResult,
			AccountID: accountID,
		}

		if sendErr != nil {
			evt.Status = "failed"
			evt.Error = sendErr.Error()
		} else {
			switch result {
			case models.SendSuccess:
				evt.Status = "sent"
			case models.SendAmbiguous:
				evt.Status = "ambiguous"
				evt.Error = "Send status unknown. The message may have been sent."
			default:
				evt.Status = "failed"
				evt.Error = "Failed to send message."
			}
		}

		h.syncer.Events().Publish(evt)
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "sending"})
}

func (h *Handler) getMessageInfo(ctx context.Context, idStr string) (int64, *storage.MessageMutationInfo, error) {
	msgID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("invalid message id")
	}
	info, err := h.db.GetMessageMutationInfo(ctx, msgID)
	if err != nil {
		return 0, nil, fmt.Errorf("get message info: %w", err)
	}
	if info == nil {
		return 0, nil, fmt.Errorf("message not found")
	}
	return msgID, info, nil
}

func (h *Handler) connectIMAP(ctx context.Context, accountID string) (*imap.Client, error) {
	cfg, err := h.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("get config: %w", err)
	}
	password, err := h.accountStore.DecryptPassword(ctx, accountID)
	if err != nil {
		return nil, fmt.Errorf("decrypt password: %w", err)
	}
	return imap.NewClient(ctx, cfg, password)
}

func (h *Handler) publishMutation(accountID, folderID string) {
	h.syncer.Events().Publish(mail.Event{
		Type:      mail.EventMutation,
		AccountID: accountID,
		FolderID:  folderID,
	})
}

func (h *Handler) handleToggleRead(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	ctx := r.Context()

	msgID, info, err := h.getMessageInfo(ctx, idStr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var currentState bool
	h.db.Read().QueryRowContext(ctx,
		`SELECT is_read FROM message_folder_state WHERE message_id = ? LIMIT 1`, msgID,
	).Scan(&currentState)

	if err := h.db.SetMessageRead(ctx, msgID, !currentState); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	go func() {
		client, err := h.connectIMAP(context.Background(), info.AccountID)
		if err != nil {
			return
		}
		defer client.Close()

		op := goimap.StoreFlagsAdd
		if !currentState {
			op = goimap.StoreFlagsDel
		}
		client.StoreFlags(context.Background(), info.FolderRemoteID, info.RemoteUID, op, []goimap.Flag{goimap.FlagSeen})
	}()

	h.publishMutation(info.AccountID, info.FolderID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"is_read": !currentState})
}

func (h *Handler) handleToggleStar(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	ctx := r.Context()

	msgID, info, err := h.getMessageInfo(ctx, idStr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var currentState bool
	h.db.Read().QueryRowContext(ctx,
		`SELECT is_starred FROM message_folder_state WHERE message_id = ? LIMIT 1`, msgID,
	).Scan(&currentState)

	if err := h.db.SetMessageStarred(ctx, msgID, !currentState); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	go func() {
		client, err := h.connectIMAP(context.Background(), info.AccountID)
		if err != nil {
			return
		}
		defer client.Close()

		op := goimap.StoreFlagsAdd
		if !currentState {
			op = goimap.StoreFlagsDel
		}
		client.StoreFlags(context.Background(), info.FolderRemoteID, info.RemoteUID, op, []goimap.Flag{goimap.FlagFlagged})
	}()

	h.publishMutation(info.AccountID, info.FolderID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"is_starred": !currentState})
}

func (h *Handler) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	ctx := r.Context()

	msgID, info, err := h.getMessageInfo(ctx, idStr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if info.FolderRole == "trash" {
		go func() {
			client, err := h.connectIMAP(context.Background(), info.AccountID)
			if err != nil {
				return
			}
			defer client.Close()
			client.DeleteMessages(context.Background(), info.FolderRemoteID, []uint32{info.RemoteUID})
		}()

		h.db.MarkMessageDeleted(ctx, msgID)
	} else {
		trashFolderID, trashRemoteID, err := h.db.GetFolderIDByRole(ctx, info.AccountID, "trash")
		if err != nil || trashFolderID == "" {
			http.Error(w, "no trash folder found", http.StatusBadRequest)
			return
		}

		states, _ := h.db.GetMessageAllFolderStates(ctx, msgID)
		var isRead, isStarred bool
		for _, s := range states {
			if s.FolderID == info.FolderID {
				isRead = s.IsRead
				isStarred = s.IsStarred
				break
			}
		}

		go func() {
			client, err := h.connectIMAP(context.Background(), info.AccountID)
			if err != nil {
				return
			}
			defer client.Close()
			client.MoveMessage(context.Background(), info.FolderRemoteID, info.RemoteUID, trashRemoteID)
		}()

		h.db.RemoveMessageFromFolder(ctx, msgID, info.FolderID)
		h.db.AddMessageToFolder(ctx, msgID, trashFolderID, 0, isRead, isStarred)
	}

	h.publishMutation(info.AccountID, info.FolderID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

func (h *Handler) handleMoveMessage(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	ctx := r.Context()

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}

	destFolderID := r.FormValue("folder_id")
	if destFolderID == "" {
		http.Error(w, "folder_id required", http.StatusBadRequest)
		return
	}

	msgID, info, err := h.getMessageInfo(ctx, idStr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var destRemoteID string
	err = h.db.Read().QueryRowContext(ctx,
		`SELECT remote_id FROM folders WHERE id = ?`, destFolderID,
	).Scan(&destRemoteID)
	if err != nil {
		http.Error(w, "destination folder not found", http.StatusBadRequest)
		return
	}

	states, _ := h.db.GetMessageAllFolderStates(ctx, msgID)
	var isRead, isStarred bool
	for _, s := range states {
		if s.FolderID == info.FolderID {
			isRead = s.IsRead
			isStarred = s.IsStarred
			break
		}
	}

	go func() {
		client, err := h.connectIMAP(context.Background(), info.AccountID)
		if err != nil {
			return
		}
		defer client.Close()
		client.MoveMessage(context.Background(), info.FolderRemoteID, info.RemoteUID, destRemoteID)
	}()

	h.db.RemoveMessageFromFolder(ctx, msgID, info.FolderID)
	h.db.AddMessageToFolder(ctx, msgID, destFolderID, 0, isRead, isStarred)

	h.publishMutation(info.AccountID, info.FolderID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "moved"})
}

func (h *Handler) handleRefetchBody(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	ctx := r.Context()

	msgID, info, err := h.getMessageInfo(ctx, idStr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := h.db.ClearEmailData(ctx, msgID); err != nil {
		http.Error(w, "failed to clear message data", http.StatusInternalServerError)
		return
	}

	h.fetchBody(ctx, msgID, info.AccountID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "refetched"})
}
