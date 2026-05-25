package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	avatarresolver "github.com/cristianadrielbraun/gofer/internal/avatar"
	"github.com/cristianadrielbraun/gofer/internal/config"
	mail "github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/mail/imap"
	"github.com/cristianadrielbraun/gofer/internal/mail/message"
	smtpclient "github.com/cristianadrielbraun/gofer/internal/mail/smtp"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
	"github.com/cristianadrielbraun/gofer/internal/views"
	"html"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	goimap "github.com/emersion/go-imap/v2"
)

type Handler struct {
	db                   *storage.DB
	accountStore         *config.AccountStore
	syncer               *mail.SyncOrchestrator
	blobStore            *store.BlobStore
	auth                 *auth.Manager
	avatar               *avatarresolver.Resolver
	bodyClientMu         sync.Mutex
	bodyClients          map[string]*imap.Client
	bodyFetchMu          sync.Mutex
	bodyFetches          map[int64]chan struct{}
	avatarWarmupQueue    chan storage.SenderAvatarCandidate
	avatarWarmupMu       sync.Mutex
	avatarWarmupQueued   map[string]struct{}
	avatarWarmupForced   map[string]time.Time
	avatarBackfillMu     sync.RWMutex
	avatarBackfillState  models.AvatarBackfillState
	avatarBackfillCancel context.CancelFunc
	avatarBackfillRunID  int64
	contactBackfillMu    sync.RWMutex
	contactBackfillState models.ContactBackfillState
	contactSyncMu        sync.Mutex
	contactSyncRunning   map[string]struct{}
	contactSyncQueue     chan struct{}
	vapidPublicKey       string
}

const (
	composeAttachmentMaxBytes     int64 = 25 << 20
	composeMessageMaxBytes        int64 = 35 << 20
	contactImportMaxBytes         int64 = 5 << 20
	composeStagedAttachmentMaxAge       = 24 * time.Hour
)

func New(db *storage.DB, accountStore *config.AccountStore, syncer *mail.SyncOrchestrator, blobStore *store.BlobStore, authManager *auth.Manager, vapidPublicKey string) *Handler {
	h := &Handler{
		db:                 db,
		accountStore:       accountStore,
		syncer:             syncer,
		blobStore:          blobStore,
		auth:               authManager,
		avatar:             avatarresolver.NewResolver(),
		bodyClients:        make(map[string]*imap.Client),
		bodyFetches:        make(map[int64]chan struct{}),
		avatarWarmupQueue:  make(chan storage.SenderAvatarCandidate, avatarWarmupQueueSize),
		avatarWarmupQueued: make(map[string]struct{}),
		avatarWarmupForced: make(map[string]time.Time),
		contactSyncRunning: make(map[string]struct{}),
		contactSyncQueue:   make(chan struct{}, 1),
		vapidPublicKey:     vapidPublicKey,
	}
	db.SetContactActivityHook(func(event storage.ContactActivityNotification) {
		if h.syncer == nil {
			return
		}
		h.syncer.Events().Publish(mail.Event{Type: mail.EventContactActivity, Payload: map[string]any{
			"user_id":     event.UserID,
			"event_type":  event.EventType,
			"email":       event.Email,
			"message":     event.Message,
			"event_count": event.Count,
			"created_at":  event.CreatedAt,
		}})
	})
	h.startAvatarWarmupWorkers()
	return h
}

func (h *Handler) userID(ctx context.Context) string {
	u := auth.GetCurrentUser(ctx)
	if u != nil {
		return u.ID
	}
	return "default"
}

func (h *Handler) resolvePassword(ctx context.Context, cfg *models.AccountConfig, accountID string) (string, error) {
	if cfg.AuthMethod == "oauth2" && h.auth != nil {
		token, err := h.auth.GetOAuthTokenForAccount(ctx, accountID)
		if err != nil {
			return "", err
		}
		return token, nil
	}
	return h.accountStore.DecryptPassword(ctx, accountID)
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	setupAssetsRoutes(mux)

	mux.HandleFunc("GET /login", h.handleLogin)
	mux.HandleFunc("GET /auth/google", h.handleGoogleRedirect)
	mux.HandleFunc("GET /auth/google/callback", h.handleGoogleCallback)
	mux.HandleFunc("GET /auth/google/account/callback", h.handleGoogleAccountCallback)
	mux.HandleFunc("POST /auth/logout", h.handleLogout)
	mux.HandleFunc("POST /api/accounts/oauth2/authorize", h.handleAccountOAuthAuthorize)

	mux.HandleFunc("GET /", h.handleIndex)
	mux.HandleFunc("GET /admin", h.handleAdminRedirect)
	mux.HandleFunc("GET /admin/avatars", h.handleAdminRedirect)
	mux.HandleFunc("GET /admin/avatars/{$}", h.handleAdmin)
	mux.HandleFunc("GET /admin/avatars/{tab}", h.handleAdmin)
	mux.HandleFunc("GET /admin/contacts", h.handleAdminContacts)
	mux.HandleFunc("GET /admin/contacts/{$}", h.handleAdminContacts)
	mux.HandleFunc("GET /email/{id}", h.handleEmailPartial)
	mux.HandleFunc("GET /email/{id}/body", h.handleEmailBody)
	mux.HandleFunc("GET /folder/{id}", h.handleFolderPartial)
	mux.HandleFunc("GET /folder/{id}/full", h.handleFolderFull)
	mux.HandleFunc("GET /folder/{id}/{email}", h.handleFolderWithEmail)
	mux.HandleFunc("GET /mail/folder/{id}/items", h.handleMailItems)
	mux.HandleFunc("GET /mail/thread/{threadId}/subitems", h.handleThreadSubItems)
	mux.HandleFunc("GET /contacts", h.handleContacts)
	mux.HandleFunc("GET /contacts/items", h.handleContactItems)
	mux.HandleFunc("GET /search", h.handleSearch)
	mux.HandleFunc("GET /api/contacts/export", h.handleExportContacts)
	mux.HandleFunc("GET /api/contacts/{id}/export", h.handleExportContact)
	mux.HandleFunc("GET /api/contacts/search", h.handleContactSearch)
	mux.HandleFunc("POST /api/contacts", h.handleSaveContact)
	mux.HandleFunc("POST /api/contacts/import", h.handleImportContacts)
	mux.HandleFunc("POST /api/contacts/{id}/unify", h.handleUnifyContact)
	mux.HandleFunc("POST /api/contacts/{id}/fields/{fieldID}/prefer", h.handlePreferContactField)
	mux.HandleFunc("POST /api/contacts/{id}/delete", h.handleDeleteContact)
	mux.HandleFunc("POST /api/accounts", h.handleCreateAccount)
	mux.HandleFunc("GET /api/accounts/{id}/edit", h.handleGetEditAccount)
	mux.HandleFunc("POST /api/accounts/{id}/edit", h.handleUpdateAccount)
	mux.HandleFunc("POST /api/accounts/{id}/services", h.handleUpdateAccountService)
	mux.HandleFunc("POST /api/accounts/{id}/color", h.handleUpdateAccountColor)
	mux.HandleFunc("POST /api/accounts/{id}/contacts/sync", h.handleSaveAccountContactSync)
	mux.HandleFunc("POST /api/accounts/{id}/contacts/sync/test", h.handleTestAccountContactSync)
	mux.HandleFunc("POST /api/accounts/{id}/contacts/sync/discover", h.handleDiscoverAccountContactSync)
	mux.HandleFunc("GET /api/accounts/{id}/signatures", h.handleAccountSignatures)
	mux.HandleFunc("GET /api/accounts/{id}/signatures/manage", h.handleManageAccountSignatures)
	mux.HandleFunc("POST /api/accounts/{id}/signature-settings", h.handleSaveAccountSignatureSettings)
	mux.HandleFunc("POST /api/signatures", h.handleSaveSignature)
	mux.HandleFunc("DELETE /api/signatures/{id}", h.handleDeleteSignature)
	mux.HandleFunc("POST /api/accounts/{id}/test", h.handleTestAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", h.handleDeleteAccount)
	mux.HandleFunc("GET /settings", h.handleSettings)
	mux.HandleFunc("GET /settings/{tab}", h.handleSettingsTab)
	mux.HandleFunc("POST /api/settings/sync", h.handleSaveSyncSettings)
	mux.HandleFunc("GET /api/settings/signatures/manage", h.handleManageSignaturesSettings)
	mux.HandleFunc("GET /api/settings/ui", h.handleGetUISettings)
	mux.HandleFunc("PATCH /api/settings/ui", h.handleSaveUISettings)
	mux.HandleFunc("GET /api/push/vapid-public-key", h.handlePushVAPIDPublicKey)
	mux.HandleFunc("POST /api/push/subscription", h.handleSavePushSubscription)
	mux.HandleFunc("DELETE /api/push/subscription", h.handleDeletePushSubscription)
	mux.HandleFunc("GET /api/settings/contacts/suppressed", h.handleSuppressedContactsSettings)
	mux.HandleFunc("POST /api/settings/contacts/accounts/sync", h.handleSyncAccountContacts)
	mux.HandleFunc("POST /api/settings/contacts/providers/gmail/sync", h.handleSyncGmailContacts)
	mux.HandleFunc("POST /api/settings/contacts/suppressed/clear", h.handleClearSuppressedContacts)
	mux.HandleFunc("POST /api/settings/contacts/suppressed/{id}/clear", h.handleClearSuppressedContact)
	mux.HandleFunc("POST /api/settings/contacts/delete-observed", h.handleDeleteObservedContacts)
	mux.HandleFunc("GET /api/attachments/{id}/download", h.handleAttachmentDownload)
	mux.HandleFunc("GET /api/attachments/{id}/preview", h.handleAttachmentPreview)
	mux.HandleFunc("GET /api/inline-content/{messageID}/{contentID}", h.handleInlineContent)
	mux.HandleFunc("POST /compose/attachments", h.handleComposeAttachmentUpload)
	mux.HandleFunc("GET /compose/attachments/{id}/preview", h.handleComposeAttachmentPreview)
	mux.HandleFunc("DELETE /compose/attachments/{id}", h.handleComposeAttachmentDelete)
	mux.HandleFunc("GET /api/events", h.handleSSE)
	mux.HandleFunc("GET /api/sidebar/accounts/{id}", h.handleSidebarAccount)
	mux.HandleFunc("POST /api/mail/sync", h.handleSyncMail)
	mux.HandleFunc("POST /api/mail/sync/accounts/{id}", h.handleSyncMailAccount)
	mux.HandleFunc("POST /api/mail/sync/cancel", h.handleCancelSyncMail)
	mux.HandleFunc("GET /api/folders/unread", h.handleFolderUnreadCounts)
	mux.HandleFunc("GET /api/system/processing", h.handleProcessingStatus)
	mux.HandleFunc("POST /api/messages/{id}/prefetch-body", h.handlePrefetchBody)
	mux.HandleFunc("GET /api/compose/source", h.handleComposeSource)
	mux.HandleFunc("GET /compose/pane", h.handleComposePane)
	mux.HandleFunc("POST /compose", h.handleCompose)
	mux.HandleFunc("POST /compose/schedule", h.handleComposeSchedule)
	mux.HandleFunc("POST /compose/draft", h.handleComposeDraft)
	mux.HandleFunc("POST /compose/draft/discard", h.handleDiscardComposeDraft)
	mux.HandleFunc("GET /api/drafts/{id}", h.handleGetDraft)
	mux.HandleFunc("DELETE /api/drafts/{id}", h.handleDeleteDraft)
	mux.HandleFunc("POST /api/messages/read", h.handleMarkMessagesRead)
	mux.HandleFunc("POST /api/messages/star", h.handleMarkMessagesStarred)
	mux.HandleFunc("POST /api/messages/archive", h.handleArchiveMessages)
	mux.HandleFunc("POST /api/messages/delete", h.handleDeleteMessages)
	mux.HandleFunc("POST /api/messages/move", h.handleMoveMessages)
	mux.HandleFunc("POST /api/messages/{id}/read", h.handleToggleRead)
	mux.HandleFunc("POST /api/messages/{id}/star", h.handleToggleStar)
	mux.HandleFunc("POST /api/messages/{id}/thread/read", h.handleToggleThreadRead)
	mux.HandleFunc("POST /api/messages/{id}/thread/archive", h.handleArchiveThread)
	mux.HandleFunc("DELETE /api/messages/{id}/thread", h.handleDeleteThread)
	mux.HandleFunc("DELETE /api/messages/{id}", h.handleDeleteMessage)
	mux.HandleFunc("POST /api/messages/{id}/move", h.handleMoveMessage)
	mux.HandleFunc("POST /api/messages/{id}/refetch", h.handleRefetchBody)
	mux.HandleFunc("POST /api/remote-content/{id}/allow", h.handleAllowRemoteContent)
	mux.HandleFunc("GET /api/remote-assets/{messageID}/{filename}", h.handleRemoteAsset)
	mux.HandleFunc("GET /api/avatars/status", h.handleAvatarStatus)
	mux.HandleFunc("GET /api/admin/contacts/status", h.handleContactAdminStatus)
	mux.HandleFunc("GET /api/avatars/{hash}", h.handleAvatarImage)
	mux.HandleFunc("GET /api/avatars/attempts", h.handleAvatarAttempts)
	mux.HandleFunc("GET /api/avatars/senders", h.handleAvatarSenders)
	mux.HandleFunc("POST /api/avatars/warmup", h.handleAvatarWarmup)
	mux.HandleFunc("POST /api/avatars/senders/{hash}/recheck", h.handleRecheckAvatarSender)
	mux.HandleFunc("POST /admin/avatar-backfill/recheck", h.handleForceAvatarBackfill)
	mux.HandleFunc("POST /admin/avatar-backfill/cancel", h.handleCancelAvatarBackfill)
	mux.HandleFunc("POST /admin/contacts/backfill", h.handleForceContactBackfill)
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
	mux.HandleFunc("GET /sw.js", func(w http.ResponseWriter, r *http.Request) {
		if isDevelopment {
			w.Header().Set("Cache-Control", "no-store")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		http.ServeFile(w, r, "./assets/js/sw.js")
	})
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
	folderID = h.resolveFolderID(folderID)

	emailID := r.URL.Query().Get("email")
	ctx := r.Context()

	accounts, _ := h.db.GetAccountsIncludingDeleting(ctx, h.userID(ctx))
	if emailID == "" {
		views.Layout(accounts, folderID, nil, nil, -1, h.db.GetUISettings(ctx, h.userID(ctx)), nil, "").Render(ctx, w)
		return
	}

	views.Layout(accounts, folderID, nil, nil, -1, h.db.GetUISettings(ctx, h.userID(ctx)), nil, emailID).Render(ctx, w)
}

func (h *Handler) handleFolderWithEmail(w http.ResponseWriter, r *http.Request) {
	folderID := r.PathValue("id")
	emailID := r.PathValue("email")
	if folderID == "" {
		folderID = "inbox"
	}
	folderID = h.resolveFolderID(folderID)

	ctx := r.Context()
	accounts, _ := h.db.GetAccountsIncludingDeleting(ctx, h.userID(ctx))
	views.Layout(accounts, folderID, nil, nil, -1, h.db.GetUISettings(ctx, h.userID(ctx)), nil, emailID).Render(ctx, w)
}

func (h *Handler) handleEmailPartial(w http.ResponseWriter, r *http.Request) {
	emailID := r.PathValue("id")
	if emailID == "" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	ctx = h.contextWithUserTimezone(ctx, h.userID(ctx))

	email, err := h.db.GetEmailByID(ctx, emailID)
	if err != nil || email == nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	var thread []models.ThreadItem
	if r.URL.Query().Get("single") != "1" {
		thread, _ = h.db.GetThreadMessages(ctx, email.AccountID, email.ThreadID)
	}
	views.MailViewContent(email, thread).Render(ctx, w)
}

func (h *Handler) ensureContactsBackfilled(ctx context.Context) {
	userID := h.userID(ctx)
	settings := h.db.GetContactSettings(ctx, userID)
	if !settings.AutoCreateObserved || (!settings.ObserveSenders && !settings.ObserveRecipients) {
		return
	}
	sourceKey := ""
	if settings.ObserveSenders {
		sourceKey = "senders"
	}
	if settings.ObserveRecipients {
		if sourceKey != "" {
			sourceKey += ","
		}
		sourceKey += "recipients"
	}
	if done, _ := h.db.GetSetting(ctx, userID, "contacts_observed_backfilled_v1"); done == sourceKey {
		return
	}
	if err := h.db.BackfillObservedContacts(ctx, userID); err != nil {
		log.Printf("contacts: observed backfill failed: %v", err)
		return
	}
	if err := h.db.SetSetting(ctx, userID, "contacts_observed_backfilled_v1", sourceKey); err != nil {
		log.Printf("contacts: mark observed backfill failed: %v", err)
	}
}

func (h *Handler) handleContacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	h.ensureContactsBackfilled(ctx)
	userID := h.userID(ctx)
	ctx = h.contextWithUserTimezone(ctx, userID)
	filters := h.parseContactFilters(r)
	if filters.View == "" {
		filters.View = contactViewMode(h.db.GetUISettings(ctx, userID)["contacts_list_view"])
	}
	if filters.View == "" {
		filters.View = "cards"
	}
	contacts, err := h.db.ListContacts(ctx, userID, filters, 200, 0)
	if err != nil {
		http.Error(w, "failed to load contacts", http.StatusInternalServerError)
		return
	}
	totalCount, err := h.db.CountContacts(ctx, userID, filters)
	if err != nil {
		http.Error(w, "failed to count contacts", http.StatusInternalServerError)
		return
	}
	var selected *models.Contact
	var selectedProfile *models.ContactProfile
	if id := strings.TrimSpace(r.URL.Query().Get("contact")); id != "" {
		selected, _ = h.db.GetContact(ctx, userID, id)
		selectedProfile, _ = h.db.GetContactProfile(ctx, userID, id)
	}
	recentActivity := h.recentContactActivity(ctx, userID, selected)
	showNew := selected == nil && r.URL.Query().Get("new") == "1"
	syncQueued := selected != nil && r.URL.Query().Get("sync") == "queued"
	if r.URL.Query().Get("partial") == "detail" {
		accounts, _ := h.db.GetAccounts(ctx, userID)
		w.Header().Set("Content-Type", "text/html")
		views.ContactsDetail(selected, selectedProfile, false, syncQueued, accounts, recentActivity).Render(ctx, w)
		return
	}
	accounts, _ := h.db.GetAccounts(ctx, userID)

	if r.Header.Get("HX-Request") == "true" {
		uiSettings := h.db.GetUISettings(ctx, userID)
		width := uiSettings["mail_list_width"]
		if width == "" {
			width = "50%"
		}
		w.Header().Set("Content-Type", "text/html")
		if r.Header.Get("HX-Target") == "app-shell" {
			layoutAccounts, _ := h.db.GetAccountsIncludingDeleting(ctx, userID)
			views.ContactsShell(layoutAccounts, contacts, selected, selectedProfile, showNew, syncQueued, filters, totalCount, uiSettings, recentActivity).Render(ctx, w)
			return
		}
		views.ContactsPage(contacts, selected, selectedProfile, showNew, syncQueued, filters, totalCount, width, accounts, recentActivity).Render(ctx, w)
		return
	}

	layoutAccounts, _ := h.db.GetAccountsIncludingDeleting(ctx, userID)
	views.ContactsLayout(layoutAccounts, contacts, selected, selectedProfile, showNew, syncQueued, filters, totalCount, h.db.GetUISettings(ctx, userID), recentActivity).Render(ctx, w)
}

func (h *Handler) recentContactActivity(ctx context.Context, userID string, contact *models.Contact) []models.Email {
	if contact == nil || strings.TrimSpace(contact.Email) == "" {
		return nil
	}
	recent, err := h.db.RecentContactEmails(ctx, userID, contact.Email, 10)
	if err != nil {
		log.Printf("contacts: recent activity failed: %v", err)
		return nil
	}
	return recent
}

func (h *Handler) handleContactItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	h.ensureContactsBackfilled(ctx)
	userID := h.userID(ctx)
	ctx = h.contextWithUserTimezone(ctx, userID)
	filters := h.parseContactFilters(r)
	if filters.View == "" {
		filters.View = "cards"
	}
	start := atoiDefault(r.URL.Query().Get("start"), 0)
	if start < 0 {
		start = 0
	}
	limit := atoiDefault(r.URL.Query().Get("limit"), 100)
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	contacts, err := h.db.ListContacts(ctx, userID, filters, limit, start)
	if err != nil {
		http.Error(w, "failed to load contacts", http.StatusInternalServerError)
		return
	}
	totalCount, err := h.db.CountContacts(ctx, userID, filters)
	if err != nil {
		http.Error(w, "failed to count contacts", http.StatusInternalServerError)
		return
	}
	var selected *models.Contact
	if id := strings.TrimSpace(r.URL.Query().Get("selected")); id != "" {
		selected, _ = h.db.GetContact(ctx, userID, id)
	}
	accounts, _ := h.db.GetAccounts(ctx, userID)
	w.Header().Set("Content-Type", "text/html")
	views.ContactsItemsFragment(contacts, selected, filters, totalCount, start, accounts).Render(ctx, w)
}

func (h *Handler) parseContactFilters(r *http.Request) models.ContactFilters {
	q := r.URL.Query()
	filters := models.ContactFilters{
		Query:      strings.TrimSpace(q.Get("q")),
		Source:     strings.TrimSpace(q.Get("source")),
		SaveTarget: strings.TrimSpace(q.Get("save_target")),
		Activity:   strings.TrimSpace(q.Get("activity")),
		View:       contactViewMode(q.Get("view")),
	}
	if filters.Source != "manual" && filters.Source != "observed" && filters.Source != "synced" && !strings.HasPrefix(filters.Source, "synced:") {
		filters.Source = ""
	}
	if filters.Activity != "seen" && filters.Activity != "none" {
		filters.Activity = ""
	}
	if filters.SaveTarget != "local" && !strings.HasPrefix(filters.SaveTarget, "account:") && !strings.HasPrefix(filters.SaveTarget, "book:") {
		filters.SaveTarget = ""
	}
	return filters
}

func contactViewMode(mode string) string {
	if mode == "table" {
		return "table"
	}
	if mode == "cards" {
		return "cards"
	}
	return ""
}

func (h *Handler) handleSaveContact(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	userID := h.userID(ctx)
	additionalEmails, additionalEmailLabels := contactListFields(r.Form["additional_emails"], r.Form["additional_email_labels"])
	additionalPhones, additionalPhoneLabels := contactListFields(r.Form["additional_phones"], r.Form["additional_phone_labels"])
	contact := models.Contact{
		ID:                    strings.TrimSpace(r.URL.Query().Get("id")),
		Name:                  strings.TrimSpace(r.FormValue("name")),
		Email:                 strings.TrimSpace(r.FormValue("email")),
		EmailLabel:            contactFieldLabel(r.FormValue("email_label"), "primary"),
		AdditionalEmails:      additionalEmails,
		AdditionalEmailLabels: additionalEmailLabels,
		Phone:                 strings.TrimSpace(r.FormValue("phone")),
		PhoneLabel:            contactFieldLabel(r.FormValue("phone_label"), "primary"),
		AdditionalPhones:      additionalPhones,
		AdditionalPhoneLabels: additionalPhoneLabels,
		Organization:          strings.TrimSpace(r.FormValue("organization")),
		Title:                 strings.TrimSpace(r.FormValue("title")),
		Notes:                 strings.TrimSpace(r.FormValue("notes")),
		SaveTargets:           h.contactSaveTargets(ctx, r.FormValue("save_targets")),
	}
	var previous *models.Contact
	if contact.ID != "" {
		previous, _ = h.db.GetContact(ctx, userID, contact.ID)
	}
	saved, err := h.db.SaveContact(ctx, userID, contact)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	syncQueued := h.scheduleContactAccountSync(ctx, userID, saved, previous)
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":                  true,
			"contact_id":          saved.ID,
			"location":            "/contacts?contact=" + saved.ID,
			"contact_sync_queued": syncQueued,
		})
		return
	}
	http.Redirect(w, r, "/contacts?contact="+saved.ID, http.StatusSeeOther)
}

func contactListValues(rawValues ...string) []string {
	values := make([]string, 0, len(rawValues))
	seen := map[string]bool{}
	for _, raw := range rawValues {
		fields := strings.FieldsFunc(raw, func(r rune) bool {
			return r == '\n' || r == '\r' || r == ','
		})
		for _, field := range fields {
			value := strings.TrimSpace(field)
			if value == "" {
				continue
			}
			key := strings.ToLower(value)
			if seen[key] {
				continue
			}
			seen[key] = true
			values = append(values, value)
		}
	}
	return values
}

func contactFieldLabel(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return fallback
	}
	return value
}

func contactListFields(rawValues, rawLabels []string) ([]string, []string) {
	values := make([]string, 0, len(rawValues))
	labels := make([]string, 0, len(rawValues))
	seen := map[string]bool{}
	for i, raw := range rawValues {
		fields := strings.FieldsFunc(raw, func(r rune) bool {
			return r == '\n' || r == '\r' || r == ','
		})
		for _, field := range fields {
			value := strings.TrimSpace(field)
			if value == "" {
				continue
			}
			key := strings.ToLower(value)
			if seen[key] {
				continue
			}
			seen[key] = true
			label := ""
			if i < len(rawLabels) {
				label = rawLabels[i]
			}
			values = append(values, value)
			labels = append(labels, contactFieldLabel(label, "alternate"))
		}
	}
	return values, labels
}

func (h *Handler) contactSaveTargets(ctx context.Context, raw string) []string {
	allowed := map[string]bool{"local": true}
	if accounts, err := h.db.GetAccounts(ctx, h.userID(ctx)); err == nil {
		for _, account := range accounts {
			if account.ContactSyncEnabled {
				allowed["account:"+account.ID] = true
				for _, book := range account.ContactAddressBooks {
					if book.ID != "" {
						allowed["book:"+book.ID] = true
					}
				}
			}
		}
	}

	var targets []string
	for _, target := range strings.Split(raw, ",") {
		target = strings.TrimSpace(target)
		if allowed[target] {
			targets = append(targets, target)
		}
	}
	if len(targets) == 0 {
		targets = []string{"local"}
	}
	return targets
}

func (h *Handler) handleExportContacts(w http.ResponseWriter, r *http.Request) {
	contacts, err := h.db.ListContactsForExport(r.Context(), h.userID(r.Context()))
	if err != nil {
		http.Error(w, "failed to export contacts", http.StatusInternalServerError)
		return
	}
	serveVCard(w, "gofer-contacts.vcf", contacts)
}

func (h *Handler) handleExportContact(w http.ResponseWriter, r *http.Request) {
	contact, err := h.db.GetContact(r.Context(), h.userID(r.Context()), r.PathValue("id"))
	if err != nil {
		http.Error(w, "failed to export contact", http.StatusInternalServerError)
		return
	}
	if contact == nil {
		http.NotFound(w, r)
		return
	}
	serveVCard(w, contactVCardFilename(*contact), []models.Contact{*contact})
}

func serveVCard(w http.ResponseWriter, filename string, contacts []models.Contact) {
	data, err := renderVCard4(contacts)
	if err != nil {
		http.Error(w, "failed to render vCard", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/vcard; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handler) handleImportContacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseMultipartForm(contactImportMaxBytes); err != nil {
		http.Error(w, "invalid vCard import", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("vcard")
	if err != nil {
		http.Error(w, "missing vCard file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, contactImportMaxBytes+1))
	if err != nil || int64(len(data)) > contactImportMaxBytes {
		http.Error(w, "vCard file is too large", http.StatusBadRequest)
		return
	}

	contacts, err := parseVCardContacts(bytes.NewReader(data), h.contactSaveTargets(ctx, r.FormValue("save_targets")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	imported := 0
	for _, contact := range contacts {
		saved, err := h.db.SaveContact(ctx, h.userID(ctx), contact)
		if err != nil {
			http.Error(w, "failed to import contacts", http.StatusInternalServerError)
			return
		}
		h.scheduleContactAccountSync(ctx, h.userID(ctx), saved, nil)
		imported++
	}
	http.Redirect(w, r, fmt.Sprintf("/contacts?imported=%d", imported), http.StatusSeeOther)
}

func (h *Handler) handleUnifyContact(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := h.userID(ctx)
	contactID := strings.TrimSpace(r.PathValue("id"))
	if contactID == "" {
		http.NotFound(w, r)
		return
	}
	previous, _ := h.db.GetContact(ctx, userID, contactID)
	if previous == nil {
		http.NotFound(w, r)
		return
	}
	contact := *previous
	contact.ID = contactID
	if len(contact.SaveTargets) == 0 {
		contact.SaveTargets = []string{"local"}
	}
	saved, err := h.db.SaveContact(ctx, userID, contact)
	if err != nil {
		http.Error(w, "failed to unify contact", http.StatusBadRequest)
		return
	}
	syncQueued := h.scheduleContactAccountSync(ctx, userID, saved, previous)
	location := "/contacts?contact=" + url.QueryEscape(saved.ID)
	if syncQueued {
		location += "&sync=queued"
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", location)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

func (h *Handler) handlePreferContactField(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := h.userID(ctx)
	contactID := strings.TrimSpace(r.PathValue("id"))
	fieldID := strings.TrimSpace(r.PathValue("fieldID"))
	previous, _ := h.db.GetContact(ctx, userID, contactID)
	profile, err := h.db.PreferContactField(ctx, userID, contactID, fieldID)
	if err != nil {
		http.Error(w, "failed to prefer contact field", http.StatusBadRequest)
		return
	}
	if profile == nil {
		http.NotFound(w, r)
		return
	}
	syncQueued := false
	if updated, err := h.db.GetContact(ctx, userID, contactID); err == nil && updated != nil {
		syncQueued = h.scheduleContactAccountSync(ctx, userID, *updated, previous)
	}
	location := "/contacts?contact=" + url.QueryEscape(contactID)
	if syncQueued {
		location += "&sync=queued"
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", location)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, location, http.StatusSeeOther)
}

func (h *Handler) handleDeleteContact(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := h.userID(ctx)
	contact, err := h.db.GetContact(ctx, userID, r.PathValue("id"))
	if err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	if contact != nil {
		if err := h.deleteContactFromAccounts(ctx, userID, *contact); err != nil {
			http.Error(w, "Contact sync delete failed: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	settings := h.db.GetContactSettings(ctx, h.userID(ctx))
	if err := h.db.DeleteContact(ctx, userID, r.PathValue("id"), settings.PreventRecreateDeleted); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/contacts", http.StatusSeeOther)
}

func (h *Handler) handleContactSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	h.ensureContactsBackfilled(ctx)
	contacts, err := h.db.SearchContacts(ctx, h.userID(ctx), r.URL.Query().Get("q"), 12)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "search failed"})
		return
	}
	type result struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
		Value string `json:"value"`
	}
	items := make([]result, 0, len(contacts))
	for _, c := range contacts {
		value := c.Email
		if c.Name != "" && c.Name != c.Email {
			value = fmt.Sprintf("%s <%s>", c.Name, c.Email)
		}
		items = append(items, result{ID: c.ID, Name: c.Name, Email: c.Email, Value: value})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"results": items})
}

func emailResizeScript(emailID string) []byte {
	return []byte(fmt.Sprintf(`<script>(function(){var id=%q;function r(){var b=document.body,d=document.documentElement;if(!b||!d)return;var rect=b.getBoundingClientRect();var h=Math.max(b.scrollHeight,b.offsetHeight,d.scrollHeight,d.offsetHeight,Math.ceil(rect.bottom-rect.top))+16;parent.postMessage({type:'emailBodyResize',emailId:id,height:h},'*')}requestAnimationFrame(function(){requestAnimationFrame(r)});window.addEventListener('load',r);document.querySelectorAll('img').forEach(function(i){i.onload=r});if(document.fonts&&document.fonts.ready)document.fonts.ready.then(r);if(typeof MutationObserver!=='undefined'){new MutationObserver(function(){setTimeout(r,0)}).observe(document.body,{childList:true,subtree:true,attributes:true})}setTimeout(r,300)})();</script>`, emailID))
}

func remoteImagesDetectScript(emailID string) []byte {
	return []byte(fmt.Sprintf(`<script>(function(){var id=%q;if(document.querySelector('[data-remote-src]')){parent.postMessage({type:'remoteContentBlocked',emailId:id},'*')}})();</script>`, emailID))
}

func (h *Handler) handleEmailBody(w http.ResponseWriter, r *http.Request) {
	emailID := r.PathValue("id")
	if emailID == "" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()

	msgID, _ := strconv.ParseInt(emailID, 10, 64)
	theme := r.URL.Query().Get("theme")
	bg := r.URL.Query().Get("bg")
	fg := r.URL.Query().Get("fg")
	link := r.URL.Query().Get("link")
	original := r.URL.Query().Get("mode") == "original"
	loadRemote := r.URL.Query().Get("remote") == "true"
	var body []byte
	if msgID > 0 && !h.db.IsBodyFetched(ctx, msgID) {
		info, err := h.db.GetMessageFetchInfo(ctx, msgID)
		if err == nil && info != nil {
			if parsed, err := h.fetchParsedBody(ctx, msgID, info.AccountID); err == nil {
				if original {
					body = originalBodyFromParsedMessage(parsed, msgID)
				} else {
					body = bodyFromParsedMessage(parsed, msgID)
				}
				h.persistParsedBodyAsync(msgID, info.AccountID, parsed)
			}
		}
	}

	if body == nil {
		var err error
		if original && msgID > 0 {
			body, err = h.originalBodyFromStoredMessage(ctx, emailID, msgID)
		}
		if err == nil && body == nil {
			body, err = h.db.GetEmailBody(ctx, emailID)
		}
		if err != nil || body == nil {
			http.NotFound(w, r)
			return
		}
	}

	if !loadRemote && msgID > 0 {
		if h.db.IsRemoteContentAllowedForMessage(ctx, msgID) {
			loadRemote = true
		} else {
			senderEmail, _ := h.db.GetMessageSenderEmail(ctx, msgID)
			if senderEmail != "" && h.db.IsRemoteContentAllowedForSender(ctx, senderEmail) {
				loadRemote = true
			}
		}
	}

	if loadRemote {
		body = message.RestoreRemoteImages(body)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	doc := buildBodyDocument(body, emailResizeScript(emailID), theme, bg, fg, link, original)
	if !loadRemote {
		doc = append(doc, remoteImagesDetectScript(emailID)...)
	}
	w.Write(doc)
}

func safeEmailCSSColor(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}

	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "#") {
		hex := lower[1:]
		if len(hex) != 3 && len(hex) != 6 {
			return fallback
		}
		for _, c := range hex {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return fallback
			}
		}
		return value
	}

	if (strings.HasPrefix(lower, "rgb(") || strings.HasPrefix(lower, "rgba(")) && strings.HasSuffix(lower, ")") {
		if strings.ContainsAny(lower, ";{}") {
			return fallback
		}
		return value
	}

	return fallback
}

func buildDarkModeScript(bgColor, fgColor string) []byte {
	return []byte(fmt.Sprintf(`<script>(function(){
var db=%q,df=%q;
function p(s){if(!s)return null;s=String(s).trim().toLowerCase();if(s==="transparent")return null;var h=s.match(/^#([0-9a-f]{3}|[0-9a-f]{6})$/);if(h){var x=h[1];if(x.length===3)x=x[0]+x[0]+x[1]+x[1]+x[2]+x[2];return[parseInt(x.slice(0,2),16),parseInt(x.slice(2,4),16),parseInt(x.slice(4,6),16)]}var m=s.match(/rgba?\(([^)]+)\)/);if(!m)return null;var a=m[1].split(",");if(a.length<3)return null;if(a.length>3&&parseFloat(a[3])===0)return null;return[Math.round(parseFloat(a[0])),Math.round(parseFloat(a[1])),Math.round(parseFloat(a[2]))]}
function cl(s){var out=[],re=/#(?:[0-9a-f]{3}|[0-9a-f]{6})\b|rgba?\([^)]+\)/ig,m;while((m=re.exec(String(s||"")))!==null){var c=p(m[0]);if(c)out.push(c)}return out}
function av(a){if(!a.length)return null;var r=0,g=0,b=0;for(var i=0;i<a.length;i++){r+=a[i][0];g+=a[i][1];b+=a[i][2]}return[Math.round(r/a.length),Math.round(g/a.length),Math.round(b/a.length)]}
function lu(c){if(!c)return-1;var r=c[0]/255,g=c[1]/255,b=c[2]/255;
r=r<=.03928?r/12.92:Math.pow((r+.055)/1.055,2.4);
g=g<=.03928?g/12.92:Math.pow((g+.055)/1.055,2.4);
b=b<=.03928?b/12.92:Math.pow((b+.055)/1.055,2.4);
return .2126*r+.7152*g+.0722*b}
function sa(c){if(!c)return 0;var mx=Math.max(c[0],c[1],c[2]),mn=Math.min(c[0],c[1],c[2]);return mx===0?0:(mx-mn)/mx}
function cr(a,b){var la=lu(a),lb=lu(b),hi=Math.max(la,lb),lo=Math.min(la,lb);return(hi+.05)/(lo+.05)}
function eb(el){var cur=el;while(cur&&cur.nodeType===1){var c=p(getComputedStyle(cur).backgroundColor);if(c)return c;cur=cur.parentElement}return p(db)}
function ds(a,b){return a&&b?Math.max(Math.abs(a[0]-b[0]),Math.abs(a[1]-b[1]),Math.abs(a[2]-b[2])):999}
function mx(a,b,t){return[Math.round(a[0]*(1-t)+b[0]*t),Math.round(a[1]*(1-t)+b[1]*t),Math.round(a[2]*(1-t)+b[2]*t)]}
function rg(c){return"rgb("+c[0]+", "+c[1]+", "+c[2]+")"}
function ra(c,a){return"rgba("+c[0]+", "+c[1]+", "+c[2]+", "+a+")"}
function sb(s){var m=s&&String(s).match(/background(?:-color)?\s*:\s*([^;]+)/i);return m?m[1]:null}
function rb(){try{for(var i=0;i<document.styleSheets.length;i++){var rs=document.styleSheets[i].cssRules;if(!rs)continue;for(var j=0;j<rs.length;j++){var r=rs[j];if(!r.selectorText||!r.style)continue;var ss=String(r.selectorText).split(","),body=false;for(var k=0;k<ss.length;k++){if(ss[k].trim()==="body"){body=true;break}}if(!body)continue;var c=p(r.style.backgroundColor)||av(cl(r.style.background));if(c&&ds(c,base)>3)return c}}}catch(_){}return null}
function bw(v){var n=parseFloat(v);return isNaN(n)?0:n}
function hb(cs){return(bw(cs.borderTopWidth)>0&&cs.borderTopStyle!=="none"&&cs.borderTopStyle!=="hidden")||(bw(cs.borderRightWidth)>0&&cs.borderRightStyle!=="none"&&cs.borderRightStyle!=="hidden")||(bw(cs.borderBottomWidth)>0&&cs.borderBottomStyle!=="none"&&cs.borderBottomStyle!=="hidden")||(bw(cs.borderLeftWidth)>0&&cs.borderLeftStyle!=="none"&&cs.borderLeftStyle!=="hidden")}
var de=document.documentElement,bd=document.body,base=p(db)||[20,20,20],canvas=null;
function mb(c){if(!c)return db;if(canvas&&ds(c,canvas)<=3)return db;var shade=(255-Math.min(c[0],c[1],c[2]))/255,t=.045+Math.min(.16,shade*.85);if(sa(c)>.18)t=Math.max(t,.14);return rg(mx(base,c,t))}
function ml(c){if(c&&sa(c)>.16&&lu(c)>.35)return rg(mx(base,c,.30));return"rgba(255,255,255,0.22)"}
if(bd)canvas=p(bd.getAttribute("bgcolor"))||p(sb(bd.getAttribute("style")))||rb();
de.style.setProperty("background-color",db,"important");de.style.setProperty("color",df,"important");
if(bd){bd.style.setProperty("background-color",db,"important");bd.style.setProperty("color",df,"important")}
var bgEls=document.querySelectorAll("[bgcolor]");
for(var i=0;i<bgEls.length;i++){var ics=getComputedStyle(bgEls[i]),c=p(bgEls[i].getAttribute("bgcolor"))||p(ics.backgroundColor);if(c&&lu(c)>0.4){bgEls[i].removeAttribute("bgcolor");bgEls[i].style.setProperty("background-color",mb(c),"important")}}
var els=document.querySelectorAll("*");
for(var i=0;i<els.length;i++){
var el=els[i],t=el.tagName;
if(t==="IMG"||t==="VIDEO"||t==="SVG"||t==="CANVAS"||t==="STYLE"||t==="SCRIPT")continue;
var cs=getComputedStyle(el);
var gi=av(cl(cs.backgroundImage));
if(gi&&lu(gi)>0.4){el.style.setProperty("background-image","none","important");el.style.setProperty("background-color",mb(gi),"important")}
var bc=p(cs.backgroundColor);
if(bc&&lu(bc)>0.4)el.style.setProperty("background-color",mb(bc),"important");
var nbg=eb(el);
var fc=p(cs.color);
if(fc&&nbg&&cr(fc,nbg)<4.5){
el.style.setProperty("color",df,"important");
if(nbg&&cr(p(getComputedStyle(el).color),nbg)<4.5)el.style.setProperty("color","rgba(255,255,255,0.95)","important");
}
if(hb(cs)){var bdc=p(cs.borderTopColor)||p(cs.borderRightColor)||p(cs.borderBottomColor)||p(cs.borderLeftColor);if(bdc&&lu(bdc)>0.45&&sa(bdc)>.16){el.style.setProperty("border-color",ml(bdc),"important")}else if(!bdc||!nbg||cr(bdc,nbg)<2.2||lu(bdc)>0.55)el.style.setProperty("border-color","rgba(255,255,255,0.22)","important")}
var oc=p(cs.outlineColor);
if(oc&&bw(cs.outlineWidth)>0&&(!nbg||cr(oc,nbg)<2.2||lu(oc)>0.55))el.style.setProperty("outline-color","rgba(255,255,255,0.22)","important");
}
})();</script>`, bgColor, fgColor))
}

func buildLightModeScript(bgColor, fgColor, linkColor string) []byte {
	return []byte(fmt.Sprintf(`<script>(function(){
var pb=%q,pf=%q,pl=%q;
function p(s){if(!s)return null;s=String(s).trim().toLowerCase();if(s==="transparent")return null;var h=s.match(/^#([0-9a-f]{3}|[0-9a-f]{6})$/);if(h){var x=h[1];if(x.length===3)x=x[0]+x[0]+x[1]+x[1]+x[2]+x[2];return[parseInt(x.slice(0,2),16),parseInt(x.slice(2,4),16),parseInt(x.slice(4,6),16)]}var m=s.match(/rgba?\(([^)]+)\)/);if(!m)return null;var a=m[1].split(",");if(a.length<3)return null;if(a.length>3&&parseFloat(a[3])===0)return null;return[Math.round(parseFloat(a[0])),Math.round(parseFloat(a[1])),Math.round(parseFloat(a[2]))]}
function cl(s){var out=[],re=/#(?:[0-9a-f]{3}|[0-9a-f]{6})\b|rgba?\([^)]+\)/ig,m;while((m=re.exec(String(s||"")))!==null){var c=p(m[0]);if(c)out.push(c)}return out}
function av(a){if(!a.length)return null;var r=0,g=0,b=0;for(var i=0;i<a.length;i++){r+=a[i][0];g+=a[i][1];b+=a[i][2]}return[Math.round(r/a.length),Math.round(g/a.length),Math.round(b/a.length)]}
function lu(c){if(!c)return-1;var r=c[0]/255,g=c[1]/255,b=c[2]/255;r=r<=.03928?r/12.92:Math.pow((r+.055)/1.055,2.4);g=g<=.03928?g/12.92:Math.pow((g+.055)/1.055,2.4);b=b<=.03928?b/12.92:Math.pow((b+.055)/1.055,2.4);return .2126*r+.7152*g+.0722*b}
function sa(c){if(!c)return 0;var mx=Math.max(c[0],c[1],c[2]),mn=Math.min(c[0],c[1],c[2]);return mx===0?0:(mx-mn)/mx}
function cr(a,b){var la=lu(a),lb=lu(b),hi=Math.max(la,lb),lo=Math.min(la,lb);return(hi+.05)/(lo+.05)}
function ds(a,b){return a&&b?Math.max(Math.abs(a[0]-b[0]),Math.abs(a[1]-b[1]),Math.abs(a[2]-b[2])):999}
function mx(a,b,t){return[Math.round(a[0]*(1-t)+b[0]*t),Math.round(a[1]*(1-t)+b[1]*t),Math.round(a[2]*(1-t)+b[2]*t)]}
function rg(c){return"rgb("+c[0]+", "+c[1]+", "+c[2]+")"}
function ra(c,a){return"rgba("+c[0]+", "+c[1]+", "+c[2]+", "+a+")"}
function sb(s){var m=s&&String(s).match(/background(?:-color)?\s*:\s*([^;]+)/i);return m?m[1]:null}
function bw(v){var n=parseFloat(v);return isNaN(n)?0:n}
function hb(cs){return(bw(cs.borderTopWidth)>0&&cs.borderTopStyle!=="none"&&cs.borderTopStyle!=="hidden")||(bw(cs.borderRightWidth)>0&&cs.borderRightStyle!=="none"&&cs.borderRightStyle!=="hidden")||(bw(cs.borderBottomWidth)>0&&cs.borderBottomStyle!=="none"&&cs.borderBottomStyle!=="hidden")||(bw(cs.borderLeftWidth)>0&&cs.borderLeftStyle!=="none"&&cs.borderLeftStyle!=="hidden")}
function eb(el){var cur=el;while(cur&&cur.nodeType===1){var c=p(getComputedStyle(cur).backgroundColor);if(c)return c;cur=cur.parentElement}return p(pb)}
var de=document.documentElement,bd=document.body,base=p(pb)||[248,242,230],fg=p(pf)||[44,36,24],canvas=null;
function light(c){return c&&lu(c)>.82}
function dark(c){return c&&lu(c)<.28}
function cb(c){if(!c)return pb;if(ds(c,base)<=3||(canvas&&ds(c,canvas)<=3))return pb;if(sa(c)<.08){var nt=.07+Math.min(.14,Math.abs(lu(base)-lu(c))*.42);return rg(mx(base,fg,nt))}if(light(c)){return rg(mx(base,c,.22))}var t=.08+Math.min(.16,(sa(c)*.35)+Math.max(0,.50-lu(c))*.18);return rg(mx(base,c,t))}
function cf(c,bg){if(!c)return pf;if(sa(c)>.16)return rg(c);var l=lu(c),t=l<.22?.98:l<.38?.84:l<.55?.68:.52;var out=mx(base,fg,t);if(bg&&cr(out,bg)<4.5)out=fg;return rg(out)}
function bdcol(c,bg){if(c&&sa(c)>.16&&lu(c)<.42)return rg(mx(base,c,.24));return bg&&cr(c,bg)>=2.2?rg(c):ra(fg,.20)}
function rb(){try{for(var i=0;i<document.styleSheets.length;i++){var rs=document.styleSheets[i].cssRules;if(!rs)continue;for(var j=0;j<rs.length;j++){var r=rs[j];if(!r.selectorText||!r.style)continue;var ss=String(r.selectorText).split(","),body=false;for(var k=0;k<ss.length;k++){if(ss[k].trim()==="body"){body=true;break}}if(!body)continue;var c=p(r.style.backgroundColor)||av(cl(r.style.background));if(c&&ds(c,base)>3)return c}}}catch(_){}return null}
if(bd)canvas=p(bd.getAttribute("bgcolor"))||p(sb(bd.getAttribute("style")))||rb();
de.style.setProperty("background-color",pb,"important");de.style.setProperty("color",pf,"important");
if(bd){bd.style.setProperty("background-color",pb,"important");bd.style.setProperty("color",pf,"important")}
var bgEls=document.querySelectorAll("[bgcolor]");
for(var i=0;i<bgEls.length;i++){var c=p(bgEls[i].getAttribute("bgcolor"))||p(getComputedStyle(bgEls[i]).backgroundColor);if(c&&ds(c,base)>3){bgEls[i].removeAttribute("bgcolor");bgEls[i].style.setProperty("background-color",cb(c),"important")}}
var els=document.querySelectorAll("*");
for(var i=0;i<els.length;i++){
var el=els[i],t=el.tagName;
if(t==="IMG"||t==="VIDEO"||t==="SVG"||t==="CANVAS"||t==="STYLE"||t==="SCRIPT")continue;
var cs=getComputedStyle(el),gi=av(cl(cs.backgroundImage));
if(gi&&ds(gi,base)>3){el.style.setProperty("background-image","none","important");el.style.setProperty("background-color",cb(gi),"important")}
var bc=p(cs.backgroundColor);
if(bc&&ds(bc,base)>3)el.style.setProperty("background-color",cb(bc),"important");
var nbg=eb(el),fc=p(cs.color);
if(el.tagName==="A")el.style.setProperty("color",pl,"important");
else if(fc&&sa(fc)<.08)el.style.setProperty("color",cf(fc,nbg),"important");
else if(fc&&nbg&&cr(fc,nbg)<4.5){el.style.setProperty("color",pf,"important");if(cr(p(getComputedStyle(el).color),nbg)<4.5)el.style.setProperty("color",rg(fg),"important")}
if(hb(cs)){var bdc=p(cs.borderTopColor)||p(cs.borderRightColor)||p(cs.borderBottomColor)||p(cs.borderLeftColor);if(!bdc||!nbg||cr(bdc,nbg)<2.2||dark(bdc)||light(bdc))el.style.setProperty("border-color",bdcol(bdc,nbg),"important")}
var oc=p(cs.outlineColor);if(oc&&bw(cs.outlineWidth)>0&&(!nbg||cr(oc,nbg)<2.2||dark(oc)||light(oc)))el.style.setProperty("outline-color",ra(fg,.22),"important");
}
})();</script>`, bgColor, fgColor, linkColor))
}

func buildBodyDocument(body []byte, resizeScript []byte, theme string, bgColor string, fgColor string, linkColor string, original bool) []byte {
	s := string(body)
	lower := strings.ToLower(s)
	isDark := theme == "dark"
	injection := string(resizeScript)

	if original {
		if strings.Contains(lower, "<html") {
			if idx := strings.LastIndex(lower, "</body>"); idx != -1 {
				return []byte(s[:idx] + injection + s[idx:])
			}
			return []byte(s + injection)
		}
		return []byte("<!DOCTYPE html><html><head><meta charset=\"utf-8\"></head><body>" + s + injection + "</body></html>")
	}

	fallbackBg := "#f8f2e6"
	fallbackFg := "#2c2418"
	fallbackLink := "#1a0dab"
	scheme := "light"
	if isDark {
		fallbackBg = "#2a2520"
		fallbackFg = "#d8ccb4"
		fallbackLink = "#d49040"
		scheme = "dark"
	}
	bgColor = safeEmailCSSColor(bgColor, fallbackBg)
	fgColor = safeEmailCSSColor(fgColor, fallbackFg)
	linkColor = safeEmailCSSColor(linkColor, fallbackLink)
	baseStyles := "<style>" +
		":root{color-scheme:" + scheme + ";background:" + bgColor + ";color:" + fgColor + "}" +
		"html{overflow:hidden;background:" + bgColor + " !important;color:" + fgColor + "}" +
		"body{overflow:hidden;background:" + bgColor + " !important;color:" + fgColor + "}" +
		"body[bgcolor]{background-color:" + bgColor + " !important}" +
		"a{color:" + linkColor + "}" +
		"</style>"

	if strings.Contains(lower, "<html") {
		if headIdx := strings.LastIndex(lower, "</head>"); headIdx != -1 {
			s = s[:headIdx] + baseStyles + s[headIdx:]
		} else if bodyIdx := strings.Index(lower, "<body"); bodyIdx != -1 {
			insertIdx := bodyIdx
			if closeIdx := strings.Index(lower[bodyIdx:], ">"); closeIdx != -1 {
				insertIdx = bodyIdx + closeIdx + 1
			}
			s = s[:insertIdx] + baseStyles + s[insertIdx:]
		} else {
			s = baseStyles + s
		}
		lowerAfter := strings.ToLower(s)
		if idx := strings.LastIndex(lowerAfter, "</body>"); idx != -1 {
			if isDark {
				injection = string(buildDarkModeScript(bgColor, fgColor)) + injection
			} else {
				injection = string(buildLightModeScript(bgColor, fgColor, linkColor)) + injection
			}
			return []byte(s[:idx] + injection + s[idx:])
		}
		if isDark {
			injection = string(buildDarkModeScript(bgColor, fgColor)) + injection
		} else {
			injection = string(buildLightModeScript(bgColor, fgColor, linkColor)) + injection
		}
		return []byte(s + injection)
	}

	if isDark {
		injection = string(buildDarkModeScript(bgColor, fgColor)) + injection
	} else {
		injection = string(buildLightModeScript(bgColor, fgColor, linkColor)) + injection
	}
	doc := "<!DOCTYPE html><html><head><meta charset=\"utf-8\"><style>" +
		"html{margin:0;overflow:hidden;color-scheme:" + scheme + ";background:" + bgColor + ";color:" + fgColor + "}" +
		"body{margin:0;padding:8px;overflow:hidden;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;font-size:14px;line-height:1.5;background:" + bgColor + ";color:" + fgColor + ";word-wrap:break-word}" +
		"img{max-width:100%;height:auto}" +
		"a{color:" + linkColor + "}" +
		"</style></head><body>" +
		s +
		injection +
		"</body></html>"
	return []byte(doc)
}

func bodyFromParsedMessage(parsed *message.ParsedMessage, msgID int64) []byte {
	if parsed == nil {
		return nil
	}

	cidToURL := make(map[string]string)
	for _, a := range parsed.Attachments {
		if a.Inline && a.ContentID != "" {
			cidToURL[a.ContentID] = "/api/inline-content/" + strconv.FormatInt(msgID, 10) + "/" + a.ContentID
		}
	}

	if len(parsed.HTMLBody) > 0 {
		sanitized := message.SanitizeHTML(parsed.HTMLBody)
		return message.RewriteCIDReferences(sanitized, cidToURL)
	}
	if parsed.TextBody != "" {
		wrapped := "<pre style=\"white-space:pre-wrap;word-wrap:break-word;font-family:inherit;margin:0;padding:8px\">" +
			template.HTMLEscapeString(parsed.TextBody) + "</pre>"
		return []byte(wrapped)
	}
	return nil
}

func originalBodyFromParsedMessage(parsed *message.ParsedMessage, msgID int64) []byte {
	if parsed == nil || len(parsed.HTMLBody) == 0 {
		return bodyFromParsedMessage(parsed, msgID)
	}
	cidToURL := make(map[string]string)
	for _, a := range parsed.Attachments {
		if a.Inline && a.ContentID != "" {
			cidToURL[a.ContentID] = "/api/inline-content/" + strconv.FormatInt(msgID, 10) + "/" + a.ContentID
		}
	}
	sanitized := message.SanitizeOriginalHTML(parsed.HTMLBody)
	return message.RewriteCIDReferences(sanitized, cidToURL)
}

func (h *Handler) originalBodyFromStoredMessage(ctx context.Context, emailID string, msgID int64) ([]byte, error) {
	body, err := h.db.GetEmailOriginalHTMLBody(ctx, emailID)
	if err != nil {
		return nil, err
	}
	if body == nil {
		var accountID, rawPath string
		_ = h.db.Read().QueryRowContext(ctx, `SELECT account_id, raw_path FROM messages WHERE id = ?`, msgID).Scan(&accountID, &rawPath)
		if rawPath != "" {
			if raw, readErr := os.ReadFile(rawPath); readErr == nil {
				if extracted, extractErr := message.ExtractHTMLBody(bytes.NewReader(raw)); extractErr == nil && len(extracted) > 0 {
					body = extracted
					if p, storeErr := h.blobStore.StoreBodyOriginalHTML(ctx, accountID, msgID, extracted); storeErr == nil {
						_ = h.db.UpdateMessageOriginalHTMLPath(ctx, msgID, p)
					}
				}
			}
		}
		if body == nil {
			return nil, nil
		}
	}
	cidToURL := make(map[string]string)
	atts, err := h.db.GetAttachments(ctx, msgID)
	if err == nil {
		for _, a := range atts {
			if a.Inline && a.ContentID != "" {
				cidToURL[a.ContentID] = "/api/inline-content/" + strconv.FormatInt(msgID, 10) + "/" + a.ContentID
			}
		}
	}
	body = message.SanitizeOriginalHTML(body)
	body = message.RewriteCIDReferences(body, cidToURL)
	return body, nil
}

func (h *Handler) storeParsedBody(ctx context.Context, parsed *message.ParsedMessage, msgID int64, accountID string) {
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

	cidToURL := make(map[string]string)
	for _, a := range parsed.Attachments {
		if a.Inline && a.ContentID != "" {
			cidToURL[a.ContentID] = "/api/inline-content/" + strconv.FormatInt(msgID, 10) + "/" + a.ContentID
		}
	}

	var textPath, htmlPath, originalHTMLPath string
	if parsed.TextBody != "" {
		p, err := h.blobStore.StoreBodyText(ctx, accountID, msgID, []byte(parsed.TextBody))
		if err == nil {
			textPath = p
		}
	}

	if len(parsed.HTMLBody) > 0 {
		if p, err := h.blobStore.StoreBodyOriginalHTML(ctx, accountID, msgID, parsed.HTMLBody); err == nil {
			originalHTMLPath = p
		}
		sanitized := message.SanitizeHTML(parsed.HTMLBody)
		sanitized = message.RewriteCIDReferences(sanitized, cidToURL)
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
	if originalHTMLPath != "" {
		_ = h.db.UpdateMessageOriginalHTMLPath(ctx, msgID, originalHTMLPath)
	}

	var toRecs, ccRecs []storage.Recipient
	for _, r := range parsed.To {
		toRecs = append(toRecs, storage.Recipient{Name: r.Name, Email: r.Email})
	}
	for _, r := range parsed.CC {
		ccRecs = append(ccRecs, storage.Recipient{Name: r.Name, Email: r.Email})
	}
	h.db.UpsertRecipients(ctx, msgID, toRecs, ccRecs)

	h.db.UpdateMessageHeaders(ctx, msgID, parsed.Subject, parsed.FromName, parsed.FromEmail, snippet)
	h.db.UpdateMessageThreadHeaders(ctx, msgID, accountID, parsed.InReplyTo, parsed.References, parsed.Subject)
}

func (h *Handler) ensureBodyFetched(ctx context.Context, msgID int64, accountID string) {
	if h.db.IsBodyFetched(ctx, msgID) {
		return
	}
	h.fetchAndStoreBody(ctx, msgID, accountID)
}

func (h *Handler) fetchAndStoreBody(ctx context.Context, msgID int64, accountID string) {
	h.bodyFetchMu.Lock()
	if done, ok := h.bodyFetches[msgID]; ok {
		h.bodyFetchMu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		return
	}
	done := make(chan struct{})
	h.bodyFetches[msgID] = done
	h.bodyFetchMu.Unlock()

	defer func() {
		h.bodyFetchMu.Lock()
		delete(h.bodyFetches, msgID)
		close(done)
		h.bodyFetchMu.Unlock()
	}()

	if h.db.IsBodyFetched(ctx, msgID) {
		return
	}
	parsed, err := h.fetchParsedBody(ctx, msgID, accountID)
	if err != nil || parsed == nil {
		return
	}
	h.storeParsedBody(ctx, parsed, msgID, accountID)
}

func (h *Handler) persistParsedBodyAsync(msgID int64, accountID string, parsed *message.ParsedMessage) {
	go h.persistParsedBody(context.Background(), msgID, accountID, parsed)
}

func (h *Handler) persistParsedBody(ctx context.Context, msgID int64, accountID string, parsed *message.ParsedMessage) {
	if parsed == nil || h.db.IsBodyFetched(ctx, msgID) {
		return
	}

	h.bodyFetchMu.Lock()
	if done, ok := h.bodyFetches[msgID]; ok {
		h.bodyFetchMu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		if ctx.Err() == nil && !h.db.IsBodyFetched(ctx, msgID) {
			h.persistParsedBody(ctx, msgID, accountID, parsed)
		}
		return
	}
	done := make(chan struct{})
	h.bodyFetches[msgID] = done
	h.bodyFetchMu.Unlock()

	defer func() {
		h.bodyFetchMu.Lock()
		delete(h.bodyFetches, msgID)
		close(done)
		h.bodyFetchMu.Unlock()
	}()

	if !h.db.IsBodyFetched(ctx, msgID) {
		h.storeParsedBody(ctx, parsed, msgID, accountID)
	}
}

func (h *Handler) fetchParsedBody(ctx context.Context, msgID int64, accountID string) (*message.ParsedMessage, error) {
	var bodyData []byte

	var rawPath string
	h.db.Read().QueryRowContext(ctx,
		`SELECT raw_path FROM messages WHERE id = ?`, msgID,
	).Scan(&rawPath)

	if rawPath != "" {
		if data, err := os.ReadFile(rawPath); err == nil && len(data) > 0 {
			bodyData = data
		}
	}

	if bodyData == nil {
		info, err := h.db.GetMessageFetchInfo(ctx, msgID)
		if err != nil || info == nil {
			return nil, err
		}

		bodyData, err = h.fetchBodyRemote(ctx, accountID, info.FolderRemoteID, info.RemoteUID)
		if err != nil {
			return nil, err
		}
	}

	parsed, err := message.ParseMessage(ctx, bytes.NewReader(bodyData), h.blobStore, accountID, msgID)
	if err != nil {
		return nil, err
	}
	return parsed, nil
}

func (h *Handler) fetchBodyRemote(ctx context.Context, accountID, folderRemoteID string, remoteUID uint32) ([]byte, error) {
	bodyData, err := h.fetchBodyRemoteWithCachedClient(ctx, accountID, folderRemoteID, remoteUID)
	if err == nil {
		return bodyData, nil
	}
	h.closeBodyClient(accountID)
	return h.fetchBodyRemoteWithCachedClient(ctx, accountID, folderRemoteID, remoteUID)
}

func (h *Handler) fetchBodyRemoteWithCachedClient(ctx context.Context, accountID, folderRemoteID string, remoteUID uint32) ([]byte, error) {
	client, err := h.getBodyClient(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return client.FetchBody(ctx, folderRemoteID, remoteUID)
}

func (h *Handler) getBodyClient(ctx context.Context, accountID string) (*imap.Client, error) {
	h.bodyClientMu.Lock()
	if client := h.bodyClients[accountID]; client != nil {
		h.bodyClientMu.Unlock()
		return client, nil
	}
	h.bodyClientMu.Unlock()

	cfg, err := h.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		return nil, err
	}
	password, err := h.resolvePassword(ctx, cfg, accountID)
	if err != nil {
		return nil, err
	}
	client, err := imap.NewClient(ctx, cfg, password)
	if err != nil {
		return nil, err
	}

	h.bodyClientMu.Lock()
	if existing := h.bodyClients[accountID]; existing != nil {
		h.bodyClientMu.Unlock()
		client.Close()
		return existing, nil
	}
	h.bodyClients[accountID] = client
	h.bodyClientMu.Unlock()
	return client, nil
}

func (h *Handler) closeBodyClient(accountID string) {
	h.bodyClientMu.Lock()
	client := h.bodyClients[accountID]
	delete(h.bodyClients, accountID)
	h.bodyClientMu.Unlock()
	if client != nil {
		client.Close()
	}
}

func (h *Handler) handleFolderPartial(w http.ResponseWriter, r *http.Request) {
	folderID := r.PathValue("id")
	if folderID == "" {
		folderID = "inbox"
	}
	folderID = h.resolveFolderID(folderID)

	ctx := r.Context()
	ctx = h.contextWithUserTimezone(ctx, h.userID(ctx))
	accounts, _ := h.db.GetAccounts(ctx, h.userID(ctx))
	if r.Header.Get("HX-Request") != "true" {
		views.Layout(accounts, folderID, nil, nil, -1, h.db.GetUISettings(ctx, h.userID(ctx)), nil, "").Render(ctx, w)
		return
	}

	filters := parseEmailFilters(r)
	totalCount, _ := h.db.GetFolderEmailCountFilteredForUser(ctx, h.userID(ctx), folderID, filters)

	page, _ := h.db.GetEmailsRangeFilteredForUserWithTotal(ctx, h.userID(ctx), folderID, 0, 50, filters, totalCount)
	var emails []models.Email
	if page != nil {
		emails = page.Emails
	}

	var selectedEmail *models.Email
	var selectedThread []models.ThreadItem

	w.Header().Set("Content-Type", "text/html")
	views.FolderPartial(accounts, emails, folderID, selectedEmail, totalCount, selectedThread, h.db.GetUISettings(ctx, h.userID(ctx))).Render(ctx, w)
}

func (h *Handler) handleFolderFull(w http.ResponseWriter, r *http.Request) {
	folderID := r.PathValue("id")
	if folderID == "" {
		folderID = "inbox"
	}
	folderID = h.resolveFolderID(folderID)

	ctx := r.Context()
	ctx = h.contextWithUserTimezone(ctx, h.userID(ctx))
	accounts, _ := h.db.GetAccounts(ctx, h.userID(ctx))
	filters := parseEmailFilters(r)
	totalCount, _ := h.db.GetFolderEmailCountFilteredForUser(ctx, h.userID(ctx), folderID, filters)

	var selectedEmail *models.Email
	var selectedThread []models.ThreadItem
	selectedEmailID := r.URL.Query().Get("selected")

	var page *models.EmailPage
	if selectedEmailID != "" && !emailFiltersActive(filters) {
		page, _ = h.db.GetEmailsAroundEmailForUser(ctx, h.userID(ctx), folderID, selectedEmailID, 50)
	}
	if page == nil {
		page, _ = h.db.GetEmailsRangeFilteredForUserWithTotal(ctx, h.userID(ctx), folderID, 0, 50, filters, totalCount)
	}
	windowStart := 0
	var emails []models.Email
	if page != nil {
		emails = page.Emails
		windowStart = page.WindowStart
	}

	if selectedEmailID != "" {
		selectedEmail = &models.Email{ID: h.visibleMailListSelectionID(ctx, emails, selectedEmailID)}
	}

	w.Header().Set("Content-Type", "text/html")
	views.MailContentPartial(accounts, emails, folderID, selectedEmail, totalCount, selectedThread, h.db.GetUISettings(ctx, h.userID(ctx)), selectedEmailID, windowStart).Render(ctx, w)
}

func (h *Handler) visibleMailListSelectionID(ctx context.Context, emails []models.Email, selectedEmailID string) string {
	if selectedEmailID == "" {
		return ""
	}
	for _, email := range emails {
		if email.ID == selectedEmailID {
			return selectedEmailID
		}
	}
	selectedEmail, err := h.db.GetEmailByID(ctx, selectedEmailID)
	if err != nil || selectedEmail == nil || selectedEmail.ThreadID == "" {
		return selectedEmailID
	}
	for _, email := range emails {
		if email.ThreadID != "" && email.ThreadID == selectedEmail.ThreadID {
			return email.ID
		}
	}
	return selectedEmailID
}

func (h *Handler) handleMailItems(w http.ResponseWriter, r *http.Request) {
	folderID := r.PathValue("id")
	if folderID == "" {
		folderID = "inbox"
	}
	folderID = h.resolveFolderID(folderID)

	limit := 50
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 200 {
		limit = l
	}

	selectedEmailId := r.URL.Query().Get("selected")
	knownTotal := -1
	if kt, err := strconv.Atoi(r.URL.Query().Get("known_total")); err == nil && kt >= 0 {
		knownTotal = kt
	}
	ctx := r.Context()
	uiSettings := h.db.GetUISettings(ctx, h.userID(ctx))
	ctx = storage.WithTimezone(ctx, uiSettings["timezone"])
	filters := parseEmailFilters(r)

	var page *models.EmailPage
	var pageErr error

	if around := r.URL.Query().Get("around"); around != "" && !emailFiltersActive(filters) {
		page, pageErr = h.db.GetEmailsAroundEmailForUser(ctx, h.userID(ctx), folderID, around, limit)
	} else if startStr := r.URL.Query().Get("start"); startStr != "" {
		start, err := strconv.Atoi(startStr)
		if err != nil || start < 0 {
			start = 0
		}
		page, pageErr = h.db.GetEmailsRangeFilteredForUserWithTotal(ctx, h.userID(ctx), folderID, start, limit, filters, knownTotal)
	} else if cursor := r.URL.Query().Get("after"); cursor != "" && !emailFiltersActive(filters) {
		page, pageErr = h.db.GetEmailsAfterCursorForUser(ctx, h.userID(ctx), folderID, cursor, limit)
	} else {
		page, pageErr = h.db.GetEmailsRangeFilteredForUser(ctx, h.userID(ctx), folderID, 0, limit, filters)
	}

	if pageErr != nil {
		log.Printf("mail items %s: %v", folderID, pageErr)
		http.Error(w, "mail items unavailable", http.StatusServiceUnavailable)
		return
	}

	if page == nil {
		page = &models.EmailPage{}
	}

	w.Header().Set("Content-Type", "text/html")
	viewMode := r.URL.Query().Get("view")
	if viewMode == "" {
		viewMode = uiSettings["mail_list_view"]
	}
	views.MailListItemsFragment(
		page.Emails, folderID,
		page.WindowStart, page.WindowEnd, page.TotalCount,
		page.NextCursor, page.HasMore,
		selectedEmailId,
		uiSettings["sender_display"],
		viewMode,
	).Render(ctx, w)
}

func parseEmailFilters(r *http.Request) models.EmailFilters {
	q := r.URL.Query()
	return models.EmailFilters{
		Unread:      q.Get("unread") == "1",
		Starred:     q.Get("starred") == "1",
		Attachments: q.Get("attachments") == "1",
		Read:        q.Get("read") == "1",
		NoAttach:    q.Get("no_attachments") == "1",
		HasLabels:   q.Get("has_labels") == "1",
		ThreadsOnly: q.Get("threads_only") == "1",
		From:        strings.TrimSpace(q.Get("from")),
		To:          strings.TrimSpace(q.Get("to")),
		Subject:     strings.TrimSpace(q.Get("subject")),
		Body:        strings.TrimSpace(q.Get("body")),
		FromDomain:  strings.TrimSpace(q.Get("from_domain")),
		Attachment:  strings.TrimSpace(q.Get("attachment")),
		Label:       strings.TrimSpace(q.Get("label")),
		AccountID:   strings.TrimSpace(q.Get("account_id")),
		Query:       strings.TrimSpace(q.Get("q")),
		After:       strings.TrimSpace(q.Get("after_date")),
		Before:      strings.TrimSpace(q.Get("before_date")),
	}
}

func emailFiltersActive(filters models.EmailFilters) bool {
	return filters.Unread || filters.Starred || filters.Attachments || filters.Read || filters.NoAttach || filters.HasLabels || filters.ThreadsOnly || filters.From != "" || filters.To != "" || filters.Subject != "" || filters.Body != "" || filters.FromDomain != "" || filters.Attachment != "" || filters.Label != "" || filters.AccountID != "" || filters.Query != "" || filters.After != "" || filters.Before != ""
}

func (h *Handler) resolveFolderID(requested string) string {
	if requested == "" {
		requested = "inbox"
	}
	return requested
}

func (h *Handler) handleThreadSubItems(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadId")
	if threadID == "" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	ctx = h.contextWithUserTimezone(ctx, h.userID(ctx))

	var accountID string
	row := h.db.Read().QueryRowContext(ctx,
		`SELECT m.account_id FROM messages m WHERE m.thread_id = ? LIMIT 1`, threadID)
	if err := row.Scan(&accountID); err != nil {
		http.NotFound(w, r)
		return
	}

	items, err := h.db.GetThreadMessages(ctx, accountID, threadID)
	if err != nil || len(items) == 0 {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	uiSettings := h.db.GetUISettings(ctx, h.userID(ctx))
	views.MailListThreadSubItems(items, uiSettings["sender_display"]).Render(ctx, w)
}

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	ctx := h.contextWithUserTimezone(r.Context(), h.userID(r.Context()))
	if q == "" {
		w.Header().Set("Content-Type", "text/html")
		uiSettings := h.db.GetUISettings(ctx, h.userID(ctx))
		accounts, _ := h.db.GetAccounts(ctx, h.userID(ctx))
		views.MailListEmails(accounts, nil, "", nil, 0, 0, uiSettings["sender_display"], uiSettings["mail_list_view"], uiSettings["mail_list_navigation"]).Render(ctx, w)
		return
	}

	emails, err := h.db.SearchMessages(ctx, h.userID(ctx), q, 50)
	if err != nil {
		http.Error(w, "search failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	uiSettings := h.db.GetUISettings(ctx, h.userID(ctx))
	accounts, _ := h.db.GetAccounts(ctx, h.userID(ctx))
	views.MailListEmails(accounts, emails, "", nil, len(emails), 0, uiSettings["sender_display"], uiSettings["mail_list_view"], uiSettings["mail_list_navigation"]).Render(ctx, w)
}

func (h *Handler) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.Header().Set("Content-Type", "text/html")
		views.AccountFormError("Invalid form data").Render(r.Context(), w)
		return
	}

	req := models.CreateAccountRequest{
		Provider:     r.FormValue("provider"),
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
		w.Header().Set("Content-Type", "application/html")
		views.AccountFormError("All required fields must be filled in").Render(r.Context(), w)
		return
	}
	if req.AuthMethod != "oauth2" && req.Password == "" {
		w.Header().Set("Content-Type", "text/html")
		views.AccountFormError("Password is required for PLAIN auth").Render(r.Context(), w)
		return
	}

	account, err := h.accountStore.CreateAccount(r.Context(), h.userID(r.Context()), &req)
	if err != nil {
		w.Header().Set("Content-Type", "text/html")
		views.AccountFormError(fmt.Sprintf("Failed to create account: %v", err)).Render(r.Context(), w)
		return
	}

	h.syncer.SyncAccount(r.Context(), account.ID)
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := h.SyncContactAccount(bg, account.ID); err != nil && !errors.Is(err, errContactSyncAlreadyRunning) {
			log.Printf("contacts sync %s after account create: %v", account.ID, err)
		}
	}()

	w.Header().Set("Content-Type", "text/html")
	views.AddAccountPostCreateStep(account.ID).Render(r.Context(), w)
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
		Provider:     r.FormValue("provider"),
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

func (h *Handler) handleUpdateAccountService(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := h.userID(ctx)
	accountID := r.PathValue("id")
	if accountID == "" {
		http.Error(w, "account id required", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	enabled := r.FormValue("enabled") == "true"
	switch r.FormValue("service") {
	case "email":
		if err := h.accountStore.SetEmailSyncEnabled(ctx, userID, accountID, enabled); err != nil {
			http.Error(w, "could not update email sync", http.StatusInternalServerError)
			return
		}
		if enabled {
			h.syncer.StartAccount(context.Background(), accountID)
		} else {
			h.syncer.StopAccount(accountID)
		}
	case "contacts":
		if err := h.accountStore.SetContactSyncEnabled(ctx, userID, accountID, enabled); err != nil {
			http.Error(w, "could not update contact sync", http.StatusInternalServerError)
			return
		}
		if enabled {
			go func() {
				bg, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				if _, err := h.SyncContactAccount(bg, accountID); err != nil && !errors.Is(err, errContactSyncAlreadyRunning) {
					log.Printf("contacts sync %s after service enable: %v", accountID, err)
				}
			}()
		}
	default:
		http.Error(w, "unknown service", http.StatusBadRequest)
		return
	}

	account, err := h.accountStore.GetAccountByID(ctx, accountID)
	if err != nil || account == nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	views.SettingsAccountCard(*account).Render(ctx, w)
}

func (h *Handler) handleUpdateAccountColor(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	if accountID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "account id required"})
		return
	}

	if err := r.ParseForm(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid form data"})
		return
	}

	color, ok := normalizeAccountColor(r.FormValue("color"))
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "choose a valid hex color"})
		return
	}

	if err := h.accountStore.UpdateAccountColor(r.Context(), h.userID(r.Context()), accountID, color); err != nil {
		w.Header().Set("Content-Type", "application/json")
		if err == sql.ErrNoRows {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "account not found"})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to update account color"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"color": color})
}

func (h *Handler) handleAccountSignatures(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	if accountID == "" {
		http.Error(w, "account id required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	userID := h.userID(ctx)
	settings, err := h.db.GetAccountSignatureSettings(ctx, userID, accountID)
	if err != nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	signatures, err := h.db.ListSignatures(ctx, userID)
	if err != nil {
		http.Error(w, "failed to load signatures", http.StatusInternalServerError)
		return
	}
	mode := r.URL.Query().Get("mode")
	var defaultSignature *models.Signature
	if sig, ok, err := h.db.GetComposeSignature(ctx, userID, accountID, mode); err == nil && ok {
		defaultSignature = sig
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"signatures":        signatures,
		"settings":          settings,
		"default_signature": defaultSignature,
	})
}

func (h *Handler) handleManageAccountSignatures(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	if accountID == "" {
		http.Error(w, "account id required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	userID := h.userID(ctx)
	settings, err := h.db.GetAccountSignatureSettings(ctx, userID, accountID)
	if err != nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	account, err := h.accountStore.GetAccountByID(ctx, accountID)
	if err != nil || account == nil {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	signatures, err := h.db.ListSignatures(ctx, userID)
	if err != nil {
		http.Error(w, "failed to load signatures", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	views.AccountSignaturesDialog(models.AccountSignatureData{Account: *account, Signatures: signatures, Settings: settings}).Render(ctx, w)
}

func (h *Handler) handleManageSignaturesSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accounts, _ := h.db.GetAccounts(ctx, h.userID(ctx))
	w.Header().Set("Content-Type", "text/html")
	views.ComposeSignaturesManager(h.buildAccountSignatureData(ctx, accounts)).Render(ctx, w)
}

func (h *Handler) handleSaveSignature(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid form data"})
		return
	}
	htmlBody := strings.TrimSpace(r.FormValue("html_body"))
	textBody := strings.TrimSpace(r.FormValue("text_body"))
	if htmlBody == "" && textBody != "" {
		htmlBody = plainTextToSignatureHTML(textBody)
	}
	if htmlBody != "" {
		htmlBody = string(message.SanitizeHTML([]byte(htmlBody)))
	}
	if textBody == "" {
		textBody = signatureHTMLToText(htmlBody)
	}
	sig, err := h.db.SaveSignature(r.Context(), h.userID(r.Context()), models.Signature{
		ID:       strings.TrimSpace(r.FormValue("id")),
		Name:     r.FormValue("name"),
		HTMLBody: htmlBody,
		TextBody: textBody,
	})
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sig)
}

func (h *Handler) handleDeleteSignature(w http.ResponseWriter, r *http.Request) {
	signatureID := r.PathValue("id")
	if signatureID == "" {
		http.Error(w, "signature id required", http.StatusBadRequest)
		return
	}
	if err := h.db.DeleteSignature(r.Context(), h.userID(r.Context()), signatureID); err != nil {
		http.Error(w, "failed to delete signature", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

func (h *Handler) handleSaveAccountSignatureSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid form data"})
		return
	}
	settings := models.AccountSignatureSettings{
		AccountID:          r.PathValue("id"),
		NewSignatureID:     r.FormValue("new_signature_id"),
		ReplySignatureID:   r.FormValue("reply_signature_id"),
		ForwardSignatureID: r.FormValue("forward_signature_id"),
		NewEnabled:         formBool(r, "new_enabled"),
		ReplyEnabled:       formBool(r, "reply_enabled"),
		ForwardEnabled:     formBool(r, "forward_enabled"),
		ReplyPlacement:     r.FormValue("reply_placement"),
		ForwardPlacement:   r.FormValue("forward_placement"),
	}
	if err := h.db.SaveAccountSignatureSettings(r.Context(), h.userID(r.Context()), settings); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to save signature settings"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "saved"})
}

func formBool(r *http.Request, name string) bool {
	v := strings.ToLower(strings.TrimSpace(r.FormValue(name)))
	return v == "1" || v == "true" || v == "on" || v == "yes"
}

func plainTextToSignatureHTML(text string) string {
	lines := strings.Split(template.HTMLEscapeString(text), "\n")
	return "<p>" + strings.Join(lines, "<br>") + "</p>"
}

func signatureHTMLToText(raw string) string {
	replacer := strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n", "</p>", "\n", "</div>", "\n")
	raw = replacer.Replace(raw)
	var b strings.Builder
	inTag := false
	for _, r := range raw {
		switch r {
		case '<':
			inTag = true
		case '>':
			inTag = false
		default:
			if !inTag {
				b.WriteRune(r)
			}
		}
	}
	return strings.TrimSpace(html.UnescapeString(b.String()))
}

func (h *Handler) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("id")
	if accountID == "" {
		http.Error(w, "account id required", http.StatusBadRequest)
		return
	}

	h.syncer.StopAccount(accountID)
	if err := h.accountStore.MarkAccountDeleting(r.Context(), accountID); err != nil {
		http.Error(w, fmt.Sprintf("mark account deleting: %v", err), http.StatusInternalServerError)
		return
	}

	go func(id string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		if err := h.blobStore.DeleteAccount(id); err != nil {
			log.Printf("warning: failed to clean up blob storage for account %s: %v", id, err)
		}

		if err := h.accountStore.DeleteAccount(ctx, id); err != nil {
			log.Printf("delete account %s failed: %v", id, err)
			return
		}

		log.Printf("delete account %s complete", id)
	}(accountID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "accepted", "account_id": accountID})
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

	password, err := h.resolvePassword(r.Context(), cfg, accountID)
	if err != nil {
		http.Error(w, fmt.Sprintf("get credentials: %v", err), http.StatusInternalServerError)
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
	accounts, _ := h.db.GetAccounts(ctx, h.userID(ctx))
	syncSettings := h.buildSyncSettings(ctx, accounts)
	uiSettings := h.db.GetUISettings(ctx, h.userID(ctx))
	signatureData := h.buildAccountSignatureData(ctx, accounts)

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		views.SettingsPartial(accounts, syncSettings, "accounts", uiSettings, signatureData).Render(ctx, w)
		return
	}

	views.SettingsLayout(accounts, syncSettings, "accounts", uiSettings, signatureData).Render(ctx, w)
}

func (h *Handler) handleSettingsTab(w http.ResponseWriter, r *http.Request) {
	tab := r.PathValue("tab")
	if tab != "accounts" && tab != "sync" && tab != "contacts" && tab != "appearance" && tab != "regional" && tab != "compose-display" && tab != "advanced" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	accounts, _ := h.db.GetAccounts(ctx, h.userID(ctx))
	syncSettings := h.buildSyncSettings(ctx, accounts)
	uiSettings := h.db.GetUISettings(ctx, h.userID(ctx))
	signatureData := h.buildAccountSignatureData(ctx, accounts)

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		views.SettingsPartial(accounts, syncSettings, tab, uiSettings, signatureData).Render(ctx, w)
		return
	}

	views.SettingsLayout(accounts, syncSettings, tab, uiSettings, signatureData).Render(ctx, w)
}

func (h *Handler) buildAccountSignatureData(ctx context.Context, accounts []models.Account) []models.AccountSignatureData {
	userID := h.userID(ctx)
	signatures, _ := h.db.ListSignatures(ctx, userID)
	data := make([]models.AccountSignatureData, 0, len(accounts))
	for _, account := range accounts {
		settings, _ := h.db.GetAccountSignatureSettings(ctx, userID, account.ID)
		data = append(data, models.AccountSignatureData{Account: account, Signatures: signatures, Settings: settings})
	}
	return data
}

func (h *Handler) buildSyncSettings(ctx context.Context, accounts []models.Account) models.SyncSettings {
	syncInterval := h.db.GetSyncInterval(ctx, h.userID(ctx))

	var accountStatuses []models.AccountSyncStatus
	for _, account := range accounts {
		if !account.EmailSyncEnabled {
			continue
		}
		folders, err := h.db.GetFoldersForAccount(ctx, account.ID)
		if err != nil {
			continue
		}

		idleRoles := h.db.GetIdleFoldersForAccount(ctx, h.userID(ctx), account.ID)

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

	if err := h.db.SetSetting(ctx, h.userID(ctx), "sync_interval_minutes", strconv.Itoa(n)); err != nil {
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

	if err := h.db.SetIdleFoldersAll(ctx, h.userID(ctx), perAccount); err != nil {
		http.Error(w, "save idle folders failed", http.StatusInternalServerError)
		return
	}

	h.syncer.UpdateInterval(n)
	h.syncer.RestartIDLEWatchers(ctx)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (h *Handler) handleGetUISettings(w http.ResponseWriter, r *http.Request) {
	settings := h.db.GetUISettings(r.Context(), h.userID(r.Context()))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(settings)
}

func (h *Handler) handleSaveUISettings(w http.ResponseWriter, r *http.Request) {
	var updates map[string]string
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	current := h.db.GetUISettings(r.Context(), h.userID(r.Context()))
	for k, v := range updates {
		current[k] = v
	}

	if err := h.db.SetUISettings(r.Context(), h.userID(r.Context()), current); err != nil {
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (h *Handler) contextWithUserTimezone(ctx context.Context, userID string) context.Context {
	settings := h.db.GetUISettings(ctx, userID)
	return storage.WithTimezone(ctx, settings["timezone"])
}

type pushSubscriptionRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

type pushSubscriptionDeleteRequest struct {
	Endpoint string `json:"endpoint"`
}

func (h *Handler) handlePushVAPIDPublicKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"public_key": h.vapidPublicKey})
}

func (h *Handler) handleSavePushSubscription(w http.ResponseWriter, r *http.Request) {
	var req pushSubscriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.Endpoint = strings.TrimSpace(req.Endpoint)
	req.Keys.P256DH = strings.TrimSpace(req.Keys.P256DH)
	req.Keys.Auth = strings.TrimSpace(req.Keys.Auth)
	if req.Endpoint == "" || req.Keys.P256DH == "" || req.Keys.Auth == "" {
		http.Error(w, "invalid subscription", http.StatusBadRequest)
		return
	}

	if err := h.db.SaveWebPushSubscription(r.Context(), storage.WebPushSubscription{
		Endpoint:  req.Endpoint,
		UserID:    h.userID(r.Context()),
		P256DH:    req.Keys.P256DH,
		Auth:      req.Keys.Auth,
		UserAgent: r.UserAgent(),
	}); err != nil {
		http.Error(w, "save subscription failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (h *Handler) handleDeletePushSubscription(w http.ResponseWriter, r *http.Request) {
	var req pushSubscriptionDeleteRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	req.Endpoint = strings.TrimSpace(req.Endpoint)
	if req.Endpoint != "" {
		if err := h.db.DeleteWebPushSubscription(r.Context(), h.userID(r.Context()), req.Endpoint); err != nil {
			http.Error(w, "delete subscription failed", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (h *Handler) handleDeleteObservedContacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	settings := h.db.GetContactSettings(ctx, h.userID(ctx))
	if _, err := h.db.DeleteObservedContacts(ctx, h.userID(ctx), settings.PreventRecreateDeleted); err != nil {
		http.Error(w, "delete observed contacts failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings/contacts", http.StatusSeeOther)
}

func (h *Handler) handleSuppressedContactsSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	suppressed, err := h.db.ListSuppressedContacts(ctx, h.userID(ctx), 200)
	if err != nil {
		http.Error(w, "failed to load suppressed contacts", http.StatusInternalServerError)
		return
	}
	total, err := h.db.CountSuppressedContacts(ctx, h.userID(ctx))
	if err != nil {
		http.Error(w, "failed to count suppressed contacts", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	views.SettingsSuppressedContacts(suppressed, total).Render(ctx, w)
}

func (h *Handler) handleClearSuppressedContacts(w http.ResponseWriter, r *http.Request) {
	if _, err := h.db.ClearSuppressedContacts(r.Context(), h.userID(r.Context())); err != nil {
		http.Error(w, "failed to clear suppressed contacts", http.StatusInternalServerError)
		return
	}
	h.handleSuppressedContactsSettings(w, r)
}

func (h *Handler) handleClearSuppressedContact(w http.ResponseWriter, r *http.Request) {
	if err := h.db.ClearSuppressedContact(r.Context(), h.userID(r.Context()), r.PathValue("id")); err != nil {
		http.Error(w, "failed to clear suppressed contact", http.StatusInternalServerError)
		return
	}
	h.handleSuppressedContactsSettings(w, r)
}

func atoiDefault(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil && v > 0 {
		return v
	}
	return def
}

func normalizeAccountColor(color string) (string, bool) {
	color = strings.TrimSpace(color)
	if len(color) == 6 {
		color = "#" + color
	}
	if len(color) != 7 || color[0] != '#' {
		return "", false
	}
	for _, r := range color[1:] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return "", false
		}
	}
	return strings.ToLower(color), true
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

func (h *Handler) handleAttachmentPreview(w http.ResponseWriter, r *http.Request) {
	attID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var filename, contentType, storagePath string
	err = h.db.Read().QueryRowContext(r.Context(),
		`SELECT filename, content_type, storage_path FROM attachments WHERE id = ?`, attID,
	).Scan(&filename, &contentType, &storagePath)
	if err != nil || !isPreviewableImage(contentType, filename) {
		http.NotFound(w, r)
		return
	}
	serveAttachmentPreview(w, r, filename, contentType, storagePath)
}

func (h *Handler) handleComposeAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	h.cleanupUnreferencedComposeAttachments(r.Context())
	if err := r.ParseMultipartForm(composeAttachmentMaxBytes); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid attachment upload"})
		return
	}
	file, header, err := r.FormFile("attachment")
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "missing attachment"})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, composeAttachmentMaxBytes+1))
	if err != nil || int64(len(data)) > composeAttachmentMaxBytes {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "attachment is too large"})
		return
	}
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(data)
	}
	id, path, err := h.blobStore.StoreComposeAttachment(r.Context(), header.Filename, bytes.NewReader(data))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to store attachment"})
		return
	}
	_ = path
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":           id,
		"filename":     header.Filename,
		"content_type": contentType,
		"size":         len(data),
		"preview_url":  composeAttachmentPreviewURL(id, contentType, header.Filename),
		"content_id":   composeInlineContentID(id),
	})
}

func (h *Handler) handleComposeAttachmentPreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	path, err := h.blobStore.ComposeAttachmentPath(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	filename := strings.TrimPrefix(filepath.Base(path), id+"-")
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	f.Close()
	contentType := http.DetectContentType(buf[:n])
	if !isPreviewableImage(contentType, filename) {
		http.NotFound(w, r)
		return
	}
	serveAttachmentPreview(w, r, filename, contentType, path)
}

func (h *Handler) handleComposeAttachmentDelete(w http.ResponseWriter, r *http.Request) {
	_ = h.blobStore.DeleteComposeAttachment(r.PathValue("id"))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

func (h *Handler) cleanupUnreferencedComposeAttachments(ctx context.Context) {
	rows, err := h.db.Read().QueryContext(ctx, `SELECT storage_path FROM attachments WHERE storage_path LIKE ?`, "%/_compose/%")
	if err != nil {
		return
	}
	defer rows.Close()
	keep := make(map[string]bool)
	for rows.Next() {
		var path string
		if rows.Scan(&path) == nil && path != "" {
			keep[filepath.Clean(path)] = true
		}
	}
	_, _ = h.blobStore.CleanupComposeAttachments(composeStagedAttachmentMaxAge, keep)
}

func composeAttachmentPreviewURL(id, contentType, filename string) string {
	if !isPreviewableImage(contentType, filename) {
		return ""
	}
	return "/compose/attachments/" + id + "/preview"
}

func composeInlineContentID(id string) string {
	id = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return -1
	}, id)
	if id == "" {
		id = strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "inline-" + id + "@gofer"
}

func cleanContentID(cid string) string {
	cid = strings.TrimSpace(strings.Trim(cid, "<>"))
	return strings.Map(func(r rune) rune {
		if r > 32 && r != '<' && r != '>' && r != '"' && r != '\'' && r != '\\' {
			return r
		}
		return -1
	}, cid)
}

func attachmentPreviewURL(id int64, contentType, filename string) string {
	if !isPreviewableImage(contentType, filename) {
		return ""
	}
	return "/api/attachments/" + strconv.FormatInt(id, 10) + "/preview"
}

func isPreviewableImage(contentType, filename string) bool {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	switch contentType {
	case "image/png", "image/jpeg", "image/jpg", "image/gif", "image/webp", "image/svg+xml", "image/bmp", "image/x-icon", "image/vnd.microsoft.icon":
		return true
	}
	lower := strings.ToLower(filename)
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".bmp", ".ico"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

func serveAttachmentPreview(w http.ResponseWriter, r *http.Request, filename, contentType, storagePath string) {
	f, err := os.Open(storagePath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s"`, filename))
	w.Header().Set("Cache-Control", "private, max-age=300")
	http.ServeContent(w, r, filename, time.Time{}, f)
}

func (h *Handler) handleInlineContent(w http.ResponseWriter, r *http.Request) {
	messageID := r.PathValue("messageID")
	contentID := r.PathValue("contentID")
	if messageID == "" || contentID == "" {
		http.NotFound(w, r)
		return
	}

	msgID, err := strconv.ParseInt(messageID, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	var filename, contentType, storagePath string
	err = h.db.Read().QueryRowContext(ctx,
		`SELECT filename, content_type, storage_path FROM attachments WHERE message_id = ? AND content_id = ? LIMIT 1`,
		msgID, contentID,
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
	w.Header().Set("Cache-Control", "private, max-age=31536000")
	http.ServeContent(w, r, filename, time.Time{}, f)
}

func (h *Handler) handleAllowRemoteContent(w http.ResponseWriter, r *http.Request) {
	emailID := r.PathValue("id")
	if emailID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	msgID, err := strconv.ParseInt(emailID, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	if req.Mode == "once" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		return
	}

	senderEmail, err := h.db.GetMessageSenderEmail(ctx, msgID)
	if err != nil {
		http.Error(w, "message not found", http.StatusNotFound)
		return
	}

	info, _ := h.db.GetMessageFetchInfo(ctx, msgID)
	if info == nil {
		http.Error(w, "message info not found", http.StatusNotFound)
		return
	}
	accountID := info.AccountID

	body, err := h.db.GetEmailBody(ctx, emailID)
	if err != nil || body == nil {
		http.Error(w, "body not found", http.StatusNotFound)
		return
	}

	remoteURLs := message.ExtractRemoteURLs(string(body))
	urlToLocal := make(map[string]string)
	for _, remoteURL := range remoteURLs {
		data, err := downloadRemoteResource(remoteURL)
		if err != nil || len(data) == 0 {
			continue
		}
		localPath, err := h.blobStore.StoreRemoteAsset(accountID, msgID, remoteURL, data)
		if err != nil {
			continue
		}
		urlToLocal[remoteURL] = "/api/remote-assets/" + emailID + "/" + filepath.Base(localPath)
	}

	rewritten := message.RewriteToLocalAssets(body, urlToLocal)
	localBodyPath, err := h.blobStore.StoreRemoteBodyHTML(accountID, msgID, rewritten)
	if err != nil {
		http.Error(w, "store failed", http.StatusInternalServerError)
		return
	}

	if err := h.db.UpdateMessageBodyHTMLPath(ctx, msgID, localBodyPath); err != nil {
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}

	if req.Mode == "email" {
		h.db.AllowRemoteContentForMessage(ctx, msgID)
	} else if req.Mode == "sender" {
		h.db.AllowRemoteContentForMessage(ctx, msgID)
		if senderEmail != "" {
			h.db.AllowRemoteContentForSender(ctx, senderEmail)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func downloadRemoteResource(url string) ([]byte, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 5*1024*1024))
}

func (h *Handler) handleRemoteAsset(w http.ResponseWriter, r *http.Request) {
	messageID := r.PathValue("messageID")
	filename := r.PathValue("filename")
	if messageID == "" || filename == "" {
		http.NotFound(w, r)
		return
	}

	msgID, err := strconv.ParseInt(messageID, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	info, _ := h.db.GetMessageFetchInfo(ctx, msgID)
	if info == nil {
		http.NotFound(w, r)
		return
	}

	assetPath := filepath.Join(h.blobStore.RemoteAssetsDir(info.AccountID, msgID), filename)
	f, err := os.Open(assetPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	w.Header().Set("Cache-Control", "public, max-age=31536000")
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

	userID := h.userID(r.Context())
	userAccounts, _ := h.db.GetAccountIDs(r.Context(), userID)
	accountSet := make(map[string]bool, len(userAccounts))
	for _, id := range userAccounts {
		accountSet[id] = true
	}

	ch := h.syncer.Events().Subscribe()
	defer h.syncer.Events().Unsubscribe(ch)
	ticker := time.NewTicker(1200 * time.Millisecond)
	defer ticker.Stop()
	lastProcessingActive := false

	fmt.Fprintf(w, "event: connected\ndata: {}\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			state := h.db.GetThreadingState()
			active := state.InProgress || (state.Total > 0 && state.Processed < state.Total)
			if active || lastProcessingActive != active {
				m := map[string]any{
					"type":        string(mail.EventProcessingStatus),
					"in_progress": state.InProgress,
					"processed":   state.Processed,
					"total":       state.Total,
				}
				data, _ := json.Marshal(m)
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", mail.EventProcessingStatus, data)
				flusher.Flush()
			}
			lastProcessingActive = active
		case event := <-ch:
			if event.AccountID != "" && !accountSet[event.AccountID] {
				continue
			}
			if eventUser, _ := event.Payload["user_id"].(string); eventUser != "" && eventUser != userID {
				continue
			}
			m := map[string]any{
				"type":       string(event.Type),
				"account_id": event.AccountID,
				"folder_id":  event.FolderID,
			}
			for key, value := range event.Payload {
				m[key] = value
			}
			if event.FolderRole != "" {
				m["folder_role"] = event.FolderRole
			}
			if event.Status != "" {
				m["status"] = event.Status
			}
			if event.Error != "" {
				m["error"] = event.Error
			}
			if event.Current > 0 {
				m["current"] = event.Current
			}
			if event.Total > 0 {
				m["total"] = event.Total
			}
			if event.AvatarHash != "" {
				m["avatar_hash"] = event.AvatarHash
			}
			if event.AvatarURL != "" {
				m["avatar_url"] = event.AvatarURL
			}
			if event.AvatarDataURL != "" {
				m["avatar_data_url"] = event.AvatarDataURL
			}
			data, _ := json.Marshal(m)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, data)
			flusher.Flush()
		}
	}
}

func (h *Handler) handleFolderUnreadCounts(w http.ResponseWriter, r *http.Request) {
	counts, err := h.db.GetAllFolderUnreadCounts(r.Context(), h.userID(r.Context()))
	if err != nil {
		http.Error(w, "failed to get unread counts", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(counts)
}

func (h *Handler) handleSidebarAccount(w http.ResponseWriter, r *http.Request) {
	accountID := strings.TrimSpace(r.PathValue("id"))
	if accountID == "" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	userID := h.userID(ctx)
	accounts, err := h.db.GetAccountsIncludingDeleting(ctx, userID)
	if err != nil {
		http.Error(w, "failed to load account", http.StatusInternalServerError)
		return
	}

	for _, account := range accounts {
		if account.ID != accountID || account.IsDeleting || !account.EmailSyncEnabled {
			continue
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		activeFolder := strings.TrimSpace(r.URL.Query().Get("active_folder"))
		views.SidebarAccountSection(account, activeFolder, h.db.GetUISettings(ctx, userID)).Render(ctx, w)
		return
	}

	http.NotFound(w, r)
}

func (h *Handler) handleSyncMail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := h.userID(ctx)
	accountIDs, err := h.db.GetEmailSyncAccountIDs(ctx, userID)
	if err != nil {
		htmlStatus(w, http.StatusInternalServerError, "Could not find email-sync accounts.")
		return
	}
	if len(accountIDs) == 0 {
		htmlStatus(w, http.StatusBadRequest, "Connect an email account before syncing mail.")
		return
	}

	runID, started := h.syncer.SyncAccounts(context.Background(), userID, accountIDs)
	if !started {
		w.Header().Set("X-Gofer-Mail-Sync-Running", "true")
		htmlStatus(w, http.StatusOK, "Mail sync is already running.")
		return
	}

	w.Header().Set("X-Gofer-Mail-Sync-Run-ID", runID)
	w.Header().Set("X-Gofer-Mail-Sync-Accounts", strconv.Itoa(len(accountIDs)))
	if len(accountIDs) == 1 {
		htmlStatus(w, http.StatusOK, "Mail sync started for 1 account.")
		return
	}
	htmlStatus(w, http.StatusOK, fmt.Sprintf("Mail sync started for %d accounts.", len(accountIDs)))
}

func (h *Handler) handleSyncMailAccount(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := h.userID(ctx)
	accountID := strings.TrimSpace(r.PathValue("id"))
	if accountID == "" {
		htmlStatus(w, http.StatusBadRequest, "Choose an account before syncing mail.")
		return
	}

	accountIDs, err := h.db.GetEmailSyncAccountIDs(ctx, userID)
	if err != nil {
		htmlStatus(w, http.StatusInternalServerError, "Could not find email-sync accounts.")
		return
	}
	found := false
	for _, id := range accountIDs {
		if id == accountID {
			found = true
			break
		}
	}
	if !found {
		htmlStatus(w, http.StatusNotFound, "That account is not available for mail sync.")
		return
	}

	runID, started := h.syncer.SyncAccounts(context.Background(), userID, []string{accountID})
	if !started {
		w.Header().Set("X-Gofer-Mail-Sync-Running", "true")
		htmlStatus(w, http.StatusOK, "Mail sync is already running.")
		return
	}

	w.Header().Set("X-Gofer-Mail-Sync-Run-ID", runID)
	w.Header().Set("X-Gofer-Mail-Sync-Accounts", "1")
	htmlStatus(w, http.StatusOK, "Mail sync started for 1 account.")
}

func (h *Handler) handleCancelSyncMail(w http.ResponseWriter, r *http.Request) {
	if h.syncer.CancelManualSync(h.userID(r.Context())) {
		htmlStatus(w, http.StatusOK, "Mail sync cancellation requested.")
		return
	}
	htmlStatus(w, http.StatusOK, "No mail sync is running.")
}

func (h *Handler) handleProcessingStatus(w http.ResponseWriter, r *http.Request) {
	state := h.db.GetThreadingState()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state)
}

func (h *Handler) handleComposePane(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accounts, _ := h.db.GetAccounts(ctx, h.userID(ctx))
	views.ComposePane(accounts).Render(ctx, w)
}

func (h *Handler) handleComposeDraft(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeComposeJSONError(w, http.StatusBadRequest, "invalid form data")
		return
	}

	ctx := r.Context()
	saved, composeErr := h.saveComposeDraftFromForm(ctx, r)
	if composeErr != nil {
		writeComposeJSONError(w, composeErr.status, composeErr.message)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "saved", "draft_id": saved.DraftID})
}

type composeRequestError struct {
	status  int
	message string
}

type composeDraftSaveResult struct {
	AccountID     string
	DraftID       string
	MessageID     int64
	DraftFolderID string
}

func writeComposeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func (h *Handler) saveComposeDraftFromForm(ctx context.Context, r *http.Request) (composeDraftSaveResult, *composeRequestError) {
	accountID := r.FormValue("account_id")
	if accountID == "" {
		accountID = h.accountStore.GetFirstAccountID(ctx, h.userID(ctx))
	}
	if accountID == "" {
		return composeDraftSaveResult{}, &composeRequestError{status: http.StatusBadRequest, message: "no account configured"}
	}

	account, err := h.accountStore.GetAccountByID(ctx, accountID)
	if err != nil || account == nil {
		return composeDraftSaveResult{}, &composeRequestError{status: http.StatusNotFound, message: "account not found"}
	}

	draftFolderID, _, err := h.db.GetFolderIDByRole(ctx, accountID, "drafts")
	if err != nil || draftFolderID == "" {
		return composeDraftSaveResult{}, &composeRequestError{status: http.StatusBadRequest, message: "drafts folder not available"}
	}

	draftID := strings.TrimSpace(r.FormValue("draft_id"))
	if draftID == "" {
		draftID = message.NewMessageID()
	}
	body := r.FormValue("body")
	htmlBody := strings.TrimSpace(r.FormValue("html_body"))
	if htmlBody != "" {
		htmlBody = string(message.SanitizeHTML([]byte(htmlBody)))
	}
	subject := r.FormValue("subject")
	snippet := sentSnippet(body, subject)
	outgoingAttachments, attachmentRows := h.collectComposeAttachments(r)
	if err := validateComposeMessageSize(outgoingAttachments, body, htmlBody); err != nil {
		return composeDraftSaveResult{}, &composeRequestError{status: http.StatusBadRequest, message: err.Error()}
	}

	msgID, err := h.db.SaveDraftMessage(ctx, storage.DraftMessageInput{
		AccountID:         accountID,
		FolderID:          draftFolderID,
		InternetMessageID: draftID,
		InReplyTo:         r.FormValue("in_reply_to"),
		References:        r.FormValue("references"),
		Subject:           subject,
		FromName:          account.Name,
		FromEmail:         account.Email,
		Snippet:           snippet,
		ToRecipients:      parseDraftRecipients(r.FormValue("to")),
		CCRecipients:      parseDraftRecipients(r.FormValue("cc")),
		BCCRecipients:     parseDraftRecipients(r.FormValue("bcc")),
		Date:              time.Now().UTC(),
	})
	if err != nil {
		return composeDraftSaveResult{}, &composeRequestError{status: http.StatusInternalServerError, message: "failed to save draft"}
	}

	var textPath, htmlPath string
	if body != "" {
		if p, err := h.blobStore.StoreBodyText(ctx, accountID, msgID, []byte(body)); err == nil {
			textPath = p
		}
	}
	if htmlBody != "" {
		if p, err := h.blobStore.StoreBodyHTML(ctx, accountID, msgID, []byte(htmlBody)); err == nil {
			htmlPath = p
		}
	}
	_ = h.db.UpdateMessageBody(ctx, msgID, textPath, htmlPath, "", snippet)
	_ = h.db.ReplaceAttachments(ctx, msgID, attachmentRows)
	h.publishMutation(accountID, draftFolderID)

	return composeDraftSaveResult{AccountID: accountID, DraftID: draftID, MessageID: msgID, DraftFolderID: draftFolderID}, nil
}

func (h *Handler) handleDiscardComposeDraft(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid form data"})
		return
	}
	ctx := r.Context()
	accountID := r.FormValue("account_id")
	draftID := r.FormValue("draft_id")
	draftPaths := h.composeDraftAttachmentPaths(ctx, accountID, draftID)
	folderID, err := h.db.DeleteDraftMessage(ctx, accountID, draftID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to discard draft"})
		return
	}
	if folderID != "" {
		h.publishMutation(accountID, folderID)
	}
	h.deleteComposeAttachmentPaths(draftPaths)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "discarded"})
}

func parseDraftRecipients(raw string) []storage.Recipient {
	addrs, err := message.ParseAddressList(raw)
	if err != nil {
		return nil
	}
	recipients := make([]storage.Recipient, 0, len(addrs))
	for _, addr := range addrs {
		recipients = append(recipients, storage.Recipient{Name: addr.Name, Email: addr.Address})
	}
	return recipients
}

func validateComposeMessageSize(attachments []message.OutgoingAttachment, body, htmlBody string) error {
	total := int64(len(body) + len(htmlBody) + 4096)
	for _, att := range attachments {
		size := att.Size
		if size <= 0 && att.Path != "" {
			if info, err := os.Stat(att.Path); err == nil {
				size = info.Size()
			}
		}
		// Base64 expands by 4/3, plus CRLF every 76 bytes and MIME headers.
		encoded := ((size + 2) / 3) * 4
		encoded += encoded/76*2 + 1024
		total += encoded
	}
	if total > composeMessageMaxBytes {
		return fmt.Errorf("message is too large: estimated %s after encoding. The send limit is %s total, including attachments", formatByteSize(total), formatByteSize(composeMessageMaxBytes))
	}
	return nil
}

func formatByteSize(size int64) string {
	if size >= 1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
	}
	if size >= 1024 {
		return fmt.Sprintf("%d KB", size/1024)
	}
	return fmt.Sprintf("%d B", size)
}

func (h *Handler) collectComposeAttachments(r *http.Request) ([]message.OutgoingAttachment, []storage.AttachmentRow) {
	ctx := r.Context()
	var outgoing []message.OutgoingAttachment
	var rows []storage.AttachmentRow

	ids := r.Form["attachment_id"]
	filenames := r.Form["attachment_filename"]
	contentTypes := r.Form["attachment_content_type"]
	sizes := r.Form["attachment_size"]
	for i, id := range ids {
		path, err := h.blobStore.ComposeAttachmentPath(id)
		if err != nil {
			continue
		}
		filename := formValueAt(filenames, i, filepath.Base(path))
		contentType := formValueAt(contentTypes, i, "application/octet-stream")
		size, _ := strconv.ParseInt(formValueAt(sizes, i, "0"), 10, 64)
		outgoing = append(outgoing, message.OutgoingAttachment{Filename: filename, ContentType: contentType, Path: path, Size: size})
		rows = append(rows, storage.AttachmentRow{Filename: filename, ContentType: contentType, SizeBytes: size, StoragePath: path})
	}

	inlineIDs := r.Form["inline_attachment_id"]
	inlineCIDs := r.Form["inline_attachment_cid"]
	inlineFilenames := r.Form["inline_attachment_filename"]
	inlineContentTypes := r.Form["inline_attachment_content_type"]
	inlineSizes := r.Form["inline_attachment_size"]
	for i, id := range inlineIDs {
		path, err := h.blobStore.ComposeAttachmentPath(id)
		if err != nil {
			continue
		}
		contentID := cleanContentID(formValueAt(inlineCIDs, i, composeInlineContentID(id)))
		if contentID == "" {
			contentID = composeInlineContentID(id)
		}
		filename := formValueAt(inlineFilenames, i, filepath.Base(path))
		contentType := formValueAt(inlineContentTypes, i, "application/octet-stream")
		size, _ := strconv.ParseInt(formValueAt(inlineSizes, i, "0"), 10, 64)
		outgoing = append(outgoing, message.OutgoingAttachment{Filename: filename, ContentType: contentType, Path: path, Size: size, ContentID: contentID, Inline: true})
		rows = append(rows, storage.AttachmentRow{Filename: filename, ContentType: contentType, SizeBytes: size, ContentID: contentID, Inline: true, StoragePath: path})
	}

	for _, existingID := range r.Form["existing_attachment_id"] {
		attID, err := strconv.ParseInt(existingID, 10, 64)
		if err != nil {
			continue
		}
		var filename, contentType, storagePath string
		var size int64
		err = h.db.Read().QueryRowContext(ctx,
			`SELECT filename, content_type, size_bytes, storage_path FROM attachments WHERE id = ?`, attID,
		).Scan(&filename, &contentType, &size, &storagePath)
		if err != nil {
			continue
		}
		outgoing = append(outgoing, message.OutgoingAttachment{Filename: filename, ContentType: contentType, Path: storagePath, Size: size})
		rows = append(rows, storage.AttachmentRow{Filename: filename, ContentType: contentType, SizeBytes: size, StoragePath: storagePath})
	}

	existingInlineCIDs := r.Form["existing_inline_attachment_cid"]
	for i, existingID := range r.Form["existing_inline_attachment_id"] {
		attID, err := strconv.ParseInt(existingID, 10, 64)
		if err != nil {
			continue
		}
		var filename, contentType, storagePath, contentID string
		var size int64
		err = h.db.Read().QueryRowContext(ctx,
			`SELECT filename, content_type, size_bytes, storage_path, content_id FROM attachments WHERE id = ?`, attID,
		).Scan(&filename, &contentType, &size, &storagePath, &contentID)
		if err != nil {
			continue
		}
		if cid := cleanContentID(formValueAt(existingInlineCIDs, i, "")); cid != "" {
			contentID = cid
		}
		if contentID == "" {
			contentID = composeInlineContentID(existingID)
		}
		outgoing = append(outgoing, message.OutgoingAttachment{Filename: filename, ContentType: contentType, Path: storagePath, Size: size, ContentID: contentID, Inline: true})
		rows = append(rows, storage.AttachmentRow{Filename: filename, ContentType: contentType, SizeBytes: size, ContentID: contentID, Inline: true, StoragePath: storagePath})
	}

	return outgoing, rows
}

func formValueAt(values []string, idx int, fallback string) string {
	if idx >= 0 && idx < len(values) && values[idx] != "" {
		return values[idx]
	}
	return fallback
}

func (h *Handler) handleComposeSource(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	accountID := q.Get("account_id")
	messageID := q.Get("message_id")
	if accountID == "" || messageID == "" {
		http.Error(w, "missing message", http.StatusBadRequest)
		return
	}
	localID, err := h.db.GetMessageLocalIDByInternetID(r.Context(), accountID, messageID)
	if err != nil || localID == 0 {
		http.NotFound(w, r)
		return
	}
	email, err := h.db.GetEmailByID(r.Context(), strconv.FormatInt(localID, 10))
	if err != nil || email == nil {
		http.NotFound(w, r)
		return
	}
	type composeSourceAttachment struct {
		ID          int64  `json:"id"`
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
		Existing    bool   `json:"existing"`
		PreviewURL  string `json:"preview_url"`
	}
	attachments := make([]composeSourceAttachment, 0, len(email.Attachments))
	for _, att := range email.Attachments {
		if att.Inline {
			continue
		}
		attachments = append(attachments, composeSourceAttachment{
			ID:          att.ID,
			Filename:    att.Filename,
			ContentType: att.ContentType,
			Size:        att.SizeBytes,
			Existing:    true,
			PreviewURL:  attachmentPreviewURL(att.ID, att.ContentType, att.Filename),
		})
	}
	htmlBody := ""
	if originalBody, err := h.originalBodyFromStoredMessage(r.Context(), email.ID, localID); err == nil && len(originalBody) > 0 {
		htmlBody = string(originalBody)
	}
	if htmlBody == "" {
		htmlBody = email.HTMLBody
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"account_id":  email.AccountID,
		"message_id":  email.InternetMessageID,
		"references":  email.References,
		"subject":     email.Subject,
		"from_name":   email.From.Name,
		"from_email":  email.From.Email,
		"date":        email.DateFull,
		"to":          contactsToAddressList(email.To),
		"cc":          contactsToAddressList(email.CC),
		"body":        email.TextBody,
		"html_body":   htmlBody,
		"attachments": attachments,
	})
}

func (h *Handler) handleGetDraft(w http.ResponseWriter, r *http.Request) {
	email, err := h.db.GetEmailByID(r.Context(), r.PathValue("id"))
	if err != nil || email == nil || !email.IsDraft {
		http.NotFound(w, r)
		return
	}
	type draftAttachment struct {
		ID          int64  `json:"id"`
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
		Existing    bool   `json:"existing"`
		PreviewURL  string `json:"preview_url"`
		ContentID   string `json:"content_id,omitempty"`
	}
	attachments := make([]draftAttachment, 0, len(email.Attachments))
	inlineImages := make([]draftAttachment, 0)
	for _, att := range email.Attachments {
		if att.Inline {
			inlineImages = append(inlineImages, draftAttachment{ID: att.ID, Filename: att.Filename, ContentType: att.ContentType, Size: att.SizeBytes, Existing: true, PreviewURL: attachmentPreviewURL(att.ID, att.ContentType, att.Filename), ContentID: att.ContentID})
			continue
		}
		attachments = append(attachments, draftAttachment{ID: att.ID, Filename: att.Filename, ContentType: att.ContentType, Size: att.SizeBytes, Existing: true})
		attachments[len(attachments)-1].PreviewURL = attachmentPreviewURL(att.ID, att.ContentType, att.Filename)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"account_id":    email.AccountID,
		"draft_id":      email.InternetMessageID,
		"to":            contactsToAddressList(email.To),
		"cc":            contactsToAddressList(email.CC),
		"bcc":           contactsToAddressList(email.BCC),
		"subject":       email.Subject,
		"body":          email.TextBody,
		"html_body":     email.HTMLBody,
		"in_reply_to":   email.InReplyTo,
		"references":    email.References,
		"attachments":   attachments,
		"inline_images": inlineImages,
	})
}

func (h *Handler) handleDeleteDraft(w http.ResponseWriter, r *http.Request) {
	email, err := h.db.GetEmailByID(r.Context(), r.PathValue("id"))
	if err != nil || email == nil || !email.IsDraft {
		http.NotFound(w, r)
		return
	}
	draftPaths := attachmentStoragePaths(email.Attachments)
	folderID, err := h.db.DeleteDraftMessage(r.Context(), email.AccountID, email.InternetMessageID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "failed to discard draft"})
		return
	}
	if folderID != "" {
		h.publishMutation(email.AccountID, folderID)
	}
	h.deleteComposeAttachmentPaths(draftPaths)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "discarded"})
}

func contactsToAddressList(contacts []models.Contact) string {
	parts := make([]string, 0, len(contacts))
	for _, c := range contacts {
		if c.Email == "" {
			continue
		}
		if c.Name != "" && c.Name != c.Email {
			parts = append(parts, fmt.Sprintf("%s <%s>", c.Name, c.Email))
		} else {
			parts = append(parts, c.Email)
		}
	}
	return strings.Join(parts, ", ")
}

func (h *Handler) handleCompose(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeComposeJSONError(w, http.StatusBadRequest, "invalid form data")
		return
	}

	ctx := r.Context()
	accountID := r.FormValue("account_id")
	if accountID == "" {
		accountID = h.accountStore.GetFirstAccountID(ctx, h.userID(ctx))
	}
	if accountID == "" {
		writeComposeJSONError(w, http.StatusBadRequest, "no account configured")
		return
	}

	account, err := h.accountStore.GetAccountByID(ctx, accountID)
	if err != nil || account == nil {
		writeComposeJSONError(w, http.StatusNotFound, "account not found")
		return
	}

	toAddrs, err := message.ParseAddressList(r.FormValue("to"))
	if err != nil || len(toAddrs) == 0 {
		writeComposeJSONError(w, http.StatusBadRequest, "Please enter at least one recipient.")
		return
	}
	ccAddrs, _ := message.ParseAddressList(r.FormValue("cc"))
	bccAddrs, _ := message.ParseAddressList(r.FormValue("bcc"))
	attachments, _ := h.collectComposeAttachments(r)
	if err := validateComposeMessageSize(attachments, r.FormValue("body"), r.FormValue("html_body")); err != nil {
		writeComposeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	body := r.FormValue("body")
	htmlBody := strings.TrimSpace(r.FormValue("html_body"))
	if htmlBody != "" {
		htmlBody = string(message.SanitizeHTML([]byte(htmlBody)))
		if !strings.Contains(strings.ToLower(htmlBody), "<html") {
			htmlBody = "<html><body>" + htmlBody + "</body></html>"
		}
	} else if body != "" {
		htmlBody = "<html><body><pre style=\"white-space:pre-wrap;font-family:sans-serif\">" + template.HTMLEscapeString(body) + "</pre></body></html>"
	}
	inReplyTo, references := h.validComposeThreadHeaders(ctx, accountID, r.FormValue("subject"), r.FormValue("in_reply_to"), r.FormValue("references"))

	msg := &message.OutgoingMessage{
		FromName:    account.Name,
		FromEmail:   account.Email,
		To:          toAddrs,
		CC:          ccAddrs,
		Bcc:         bccAddrs,
		Subject:     r.FormValue("subject"),
		TextBody:    body,
		HTMLBody:    htmlBody,
		InReplyTo:   inReplyTo,
		References:  references,
		MessageID:   message.NewMessageID(),
		Date:        time.Now().UTC(),
		Attachments: attachments,
	}
	draftID := strings.TrimSpace(r.FormValue("draft_id"))

	go func() {
		status, sendErr := h.sendOutgoingMessage(context.Background(), accountID, msg, draftID)

		evt := mail.Event{
			Type:      mail.EventSendResult,
			AccountID: accountID,
		}

		evt.Status = status
		if sendErr != "" {
			evt.Error = sendErr
		}

		h.syncer.Events().Publish(evt)
	}()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "sending"})
}

func (h *Handler) sendOutgoingMessage(ctx context.Context, accountID string, msg *message.OutgoingMessage, draftID string) (string, string) {
	cfg, err := h.accountStore.GetConfig(ctx, accountID)
	if err != nil {
		return "failed", "account not found"
	}

	password, err := h.resolvePassword(ctx, cfg, accountID)
	if err != nil {
		return "failed", "failed to get credentials"
	}

	smtpPassword := password
	if cfg.SmtpUsername != "" {
		smtpPw, err := h.accountStore.DecryptSmtpPassword(ctx, accountID)
		if err == nil && smtpPw != "" {
			smtpPassword = smtpPw
		}
	}

	result, err := smtpclient.SendMessage(ctx, cfg, smtpPassword, msg)
	if err != nil {
		return "failed", err.Error()
	}

	switch result {
	case models.SendSuccess:
		h.saveSentMessage(ctx, accountID, msg)
		if draftID != "" {
			if folderID, err := h.db.DeleteDraftMessage(ctx, accountID, draftID); err == nil && folderID != "" {
				h.publishMutation(accountID, folderID)
			}
		}
		return "sent", ""
	case models.SendAmbiguous:
		return "ambiguous", "Send status unknown. The message may have been sent."
	default:
		return "failed", "Failed to send message."
	}
}

func (h *Handler) handleComposeSchedule(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeComposeJSONError(w, http.StatusBadRequest, "invalid form data")
		return
	}

	userID := h.userID(r.Context())
	loc, err := h.scheduleLocation(r.Context(), userID, r.FormValue("schedule_timezone"))
	if err != nil {
		writeComposeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	scheduledFor, err := parseScheduledSendWallTime(r.FormValue("schedule_date"), r.FormValue("schedule_hour"), r.FormValue("schedule_minute"), loc)
	if err != nil {
		writeComposeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !scheduledFor.After(time.Now().Add(30 * time.Second)) {
		writeComposeJSONError(w, http.StatusBadRequest, "Choose a time at least 1 minute in the future.")
		return
	}

	if toAddrs, err := message.ParseAddressList(r.FormValue("to")); err != nil || len(toAddrs) == 0 {
		writeComposeJSONError(w, http.StatusBadRequest, "Please enter at least one recipient.")
		return
	}

	saved, composeErr := h.saveComposeDraftFromForm(r.Context(), r)
	if composeErr != nil {
		writeComposeJSONError(w, composeErr.status, composeErr.message)
		return
	}

	scheduled, err := h.db.UpsertScheduledSend(r.Context(), saved.AccountID, saved.MessageID, scheduledFor)
	if err != nil {
		writeComposeJSONError(w, http.StatusInternalServerError, "failed to schedule message")
		return
	}
	h.publishMutation(saved.AccountID, "scheduled")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":        "scheduled",
		"draft_id":      saved.DraftID,
		"scheduled_for": scheduled.ScheduledFor.Format(time.RFC3339),
	})
}

func (h *Handler) scheduleLocation(ctx context.Context, userID, submittedTimezone string) (*time.Location, error) {
	settings := h.db.GetUISettings(ctx, userID)
	timezone := strings.TrimSpace(settings["timezone"])
	if timezone == "" || timezone == "local" {
		timezone = strings.TrimSpace(submittedTimezone)
	}
	if timezone == "" || timezone == "local" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, fmt.Errorf("Use a valid timezone.")
	}
	return loc, nil
}

func parseScheduledSendWallTime(dateValue, hourValue, minuteValue string, loc *time.Location) (time.Time, error) {
	dateValue = strings.TrimSpace(dateValue)
	hourValue = strings.TrimSpace(hourValue)
	minuteValue = strings.TrimSpace(minuteValue)
	if dateValue == "" || hourValue == "" || minuteValue == "" {
		return time.Time{}, fmt.Errorf("Choose when to send this message.")
	}
	dateParts := strings.Split(dateValue, "-")
	if len(dateParts) != 3 {
		return time.Time{}, fmt.Errorf("Use a valid schedule date.")
	}
	year, errYear := strconv.Atoi(dateParts[0])
	month, errMonth := strconv.Atoi(dateParts[1])
	day, errDay := strconv.Atoi(dateParts[2])
	hour, errHour := strconv.Atoi(hourValue)
	minute, errMinute := strconv.Atoi(minuteValue)
	if errYear != nil || errMonth != nil || errDay != nil || errHour != nil || errMinute != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 || minute%5 != 0 {
		return time.Time{}, fmt.Errorf("Use a valid schedule time.")
	}
	if loc == nil {
		loc = time.Local
	}
	scheduled := time.Date(year, time.Month(month), day, hour, minute, 0, 0, loc)
	local := scheduled.In(loc)
	if local.Year() != year || local.Month() != time.Month(month) || local.Day() != day || local.Hour() != hour || local.Minute() != minute {
		return time.Time{}, fmt.Errorf("Use a valid schedule time.")
	}
	return scheduled.UTC(), nil
}

func (h *Handler) StartScheduledSendWorker(ctx context.Context) {
	go func() {
		if err := h.db.ResetStaleScheduledSends(ctx, time.Now().Add(-15*time.Minute)); err != nil {
			log.Printf("scheduled-send: reset stale sends: %v", err)
		}
		h.runDueScheduledSends(ctx)

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.runDueScheduledSends(ctx)
			}
		}
	}()
}

func (h *Handler) runDueScheduledSends(ctx context.Context) {
	sends, err := h.db.ClaimDueScheduledSends(ctx, time.Now(), 5)
	if err != nil {
		log.Printf("scheduled-send: claim due sends: %v", err)
		return
	}
	for _, scheduled := range sends {
		if err := h.sendScheduledMessage(ctx, scheduled); err != nil {
			log.Printf("scheduled-send: send %s: %v", scheduled.ID, err)
		}
	}
}

func (h *Handler) sendScheduledMessage(ctx context.Context, scheduled storage.ScheduledSend) error {
	email, err := h.db.GetEmailByID(ctx, strconv.FormatInt(scheduled.MessageID, 10))
	if err != nil {
		_ = h.db.FinishScheduledSendWithError(ctx, scheduled.ID, storage.ScheduledSendFailed, err.Error())
		return err
	}
	if email == nil || !email.IsDraft {
		msg := "scheduled draft no longer exists"
		_ = h.db.FinishScheduledSendWithError(ctx, scheduled.ID, storage.ScheduledSendFailed, msg)
		return errors.New(msg)
	}

	toAddrs, err := message.ParseAddressList(contactsToAddressList(email.To))
	if err != nil || len(toAddrs) == 0 {
		msg := "scheduled draft has no valid recipients"
		_ = h.db.FinishScheduledSendWithError(ctx, scheduled.ID, storage.ScheduledSendFailed, msg)
		return errors.New(msg)
	}
	ccAddrs, _ := message.ParseAddressList(contactsToAddressList(email.CC))
	bccAddrs, _ := message.ParseAddressList(contactsToAddressList(email.BCC))
	body := email.TextBody
	htmlBody := strings.TrimSpace(email.HTMLBody)
	if htmlBody != "" {
		if !strings.Contains(strings.ToLower(htmlBody), "<html") {
			htmlBody = "<html><body>" + htmlBody + "</body></html>"
		}
	} else if body != "" {
		htmlBody = "<html><body><pre style=\"white-space:pre-wrap;font-family:sans-serif\">" + template.HTMLEscapeString(body) + "</pre></body></html>"
	}
	inReplyTo, references := h.validComposeThreadHeaders(ctx, email.AccountID, email.Subject, email.InReplyTo, email.References)
	fromName := email.From.Name
	fromEmail := email.From.Email
	if account, err := h.accountStore.GetAccountByID(ctx, email.AccountID); err == nil && account != nil {
		fromName = account.Name
		fromEmail = account.Email
	}

	msg := &message.OutgoingMessage{
		FromName:    fromName,
		FromEmail:   fromEmail,
		To:          toAddrs,
		CC:          ccAddrs,
		Bcc:         bccAddrs,
		Subject:     email.Subject,
		TextBody:    body,
		HTMLBody:    htmlBody,
		InReplyTo:   inReplyTo,
		References:  references,
		MessageID:   message.NewMessageID(),
		Date:        time.Now().UTC(),
		Attachments: outgoingAttachmentsFromStored(email.Attachments),
	}

	status, errText := h.sendOutgoingMessage(ctx, email.AccountID, msg, email.InternetMessageID)
	if status == "sent" {
		if err := h.db.CompleteScheduledSend(ctx, scheduled.ID, msg.MessageID); err != nil {
			return err
		}
	} else {
		storageStatus := storage.ScheduledSendFailed
		if status == "ambiguous" {
			storageStatus = storage.ScheduledSendAmbiguous
		}
		if err := h.db.FinishScheduledSendWithError(ctx, scheduled.ID, storageStatus, errText); err != nil {
			return err
		}
	}

	h.publishMutation(email.AccountID, "scheduled")
	h.syncer.Events().Publish(mail.Event{Type: mail.EventSendResult, AccountID: email.AccountID, Status: status, Error: errText})
	return nil
}

func outgoingAttachmentsFromStored(atts []models.Attachment) []message.OutgoingAttachment {
	out := make([]message.OutgoingAttachment, 0, len(atts))
	for _, att := range atts {
		out = append(out, message.OutgoingAttachment{
			Filename:    att.Filename,
			ContentType: att.ContentType,
			Path:        att.StoragePath,
			Size:        att.SizeBytes,
			ContentID:   att.ContentID,
			Inline:      att.Inline,
		})
	}
	return out
}

func (h *Handler) saveSentMessage(ctx context.Context, accountID string, msg *message.OutgoingMessage) {
	sentFolderID, _, err := h.db.GetFolderIDByRole(ctx, accountID, "sent")
	if err != nil || sentFolderID == "" {
		return
	}

	toRecipients := make([]storage.Recipient, 0, len(msg.To))
	for _, addr := range msg.To {
		toRecipients = append(toRecipients, storage.Recipient{Name: addr.Name, Email: addr.Address})
	}
	ccRecipients := make([]storage.Recipient, 0, len(msg.CC))
	for _, addr := range msg.CC {
		ccRecipients = append(ccRecipients, storage.Recipient{Name: addr.Name, Email: addr.Address})
	}

	snippet := sentSnippet(msg.TextBody, msg.Subject)
	if err := h.db.UpsertSyncMessages(ctx, []storage.SyncMessage{{
		AccountID:    accountID,
		FolderID:     sentFolderID,
		MessageID:    msg.MessageID,
		InReplyTo:    msg.InReplyTo,
		References:   msg.References,
		Subject:      msg.Subject,
		FromName:     msg.FromName,
		FromEmail:    msg.FromEmail,
		DateSent:     msg.Date,
		Snippet:      snippet,
		IsRead:       true,
		ToRecipients: toRecipients,
		CCRecipients: ccRecipients,
	}}); err != nil {
		return
	}

	localID, err := h.db.GetMessageLocalIDByInternetID(ctx, accountID, msg.MessageID)
	if err != nil || localID == 0 {
		return
	}

	var textPath, htmlPath, rawPath string
	if msg.TextBody != "" {
		if p, err := h.blobStore.StoreBodyText(ctx, accountID, localID, []byte(msg.TextBody)); err == nil {
			textPath = p
		}
	}
	if msg.HTMLBody != "" {
		if p, err := h.blobStore.StoreBodyHTML(ctx, accountID, localID, message.SanitizeHTML([]byte(msg.HTMLBody))); err == nil {
			htmlPath = p
		}
	}
	if raw, err := message.BuildMIMEMessage(msg); err == nil {
		if p, err := h.blobStore.StoreRaw(ctx, accountID, localID, raw); err == nil {
			rawPath = p
		}
	}
	_ = h.db.UpdateMessageBody(ctx, localID, textPath, htmlPath, rawPath, snippet)
	if len(msg.Attachments) > 0 {
		var attRows []storage.AttachmentRow
		for i, att := range msg.Attachments {
			f, err := os.Open(att.Path)
			if err != nil {
				continue
			}
			storedPath, err := h.blobStore.StoreAttachment(ctx, accountID, localID, int64(i+1), att.Filename, f)
			f.Close()
			if err != nil {
				continue
			}
			attRows = append(attRows, storage.AttachmentRow{Filename: att.Filename, ContentType: att.ContentType, SizeBytes: att.Size, ContentID: att.ContentID, Inline: att.Inline, StoragePath: storedPath})
		}
		_ = h.db.ReplaceAttachments(ctx, localID, attRows)
	}
	h.deleteComposeAttachmentPaths(outgoingAttachmentPaths(msg.Attachments))
	h.publishMutation(accountID, sentFolderID)
}

func outgoingAttachmentPaths(atts []message.OutgoingAttachment) []string {
	paths := make([]string, 0, len(atts))
	for _, att := range atts {
		if att.Path != "" {
			paths = append(paths, att.Path)
		}
	}
	return paths
}

func attachmentStoragePaths(atts []models.Attachment) []string {
	paths := make([]string, 0, len(atts))
	for _, att := range atts {
		if att.StoragePath != "" {
			paths = append(paths, att.StoragePath)
		}
	}
	return paths
}

func (h *Handler) composeDraftAttachmentPaths(ctx context.Context, accountID, internetMessageID string) []string {
	if accountID == "" || internetMessageID == "" {
		return nil
	}
	rows, err := h.db.Read().QueryContext(ctx, `
		SELECT att.storage_path
		FROM attachments att
		JOIN messages m ON m.id = att.message_id
		JOIN message_folder_state mfs ON m.id = mfs.message_id
		WHERE m.account_id = ? AND m.internet_message_id = ? AND mfs.is_draft = 1`, accountID, internetMessageID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if rows.Scan(&path) == nil && path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func (h *Handler) deleteComposeAttachmentPaths(paths []string) {
	for _, path := range paths {
		clean := filepath.Clean(path)
		if strings.Contains(clean, string(filepath.Separator)+"_compose"+string(filepath.Separator)) {
			_ = os.Remove(clean)
		}
	}
}

func (h *Handler) validComposeThreadHeaders(ctx context.Context, accountID, subject, inReplyToRaw, referencesRaw string) (string, string) {
	if !message.IsReplyOrForwardSubject(subject) {
		return "", ""
	}
	ids := message.ParseMessageIDs(inReplyToRaw)
	if len(ids) == 0 {
		return "", ""
	}
	parentID := ids[0]

	var parentSubject string
	err := h.db.Read().QueryRowContext(ctx,
		`SELECT normalized_subject FROM messages WHERE account_id = ? AND message_id_normalized = ? LIMIT 1`, accountID, parentID,
	).Scan(&parentSubject)
	if err != nil || parentSubject == "" || parentSubject != message.BaseSubject(subject) {
		return "", ""
	}

	inReplyTo := "<" + parentID + ">"
	return inReplyTo, message.FormatReferences(referencesRaw, inReplyTo)
}

func sentSnippet(body, subject string) string {
	snippet := strings.TrimSpace(body)
	if snippet == "" {
		return subject
	}
	snippet = strings.Join(strings.Fields(snippet), " ")
	runes := []rune(snippet)
	if len(runes) > 180 {
		return string(runes[:180])
	}
	return snippet
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
	password, err := h.resolvePassword(ctx, cfg, accountID)
	if err != nil {
		return nil, fmt.Errorf("get credentials: %w", err)
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

func (h *Handler) publishThreadMutation(infos []storage.ThreadMessageMutationInfo) {
	seen := make(map[string]bool)
	for _, info := range infos {
		if info.FolderID == "" || seen[info.FolderID] {
			continue
		}
		seen[info.FolderID] = true
		h.publishMutation(info.AccountID, info.FolderID)
	}
}

func threadUIDsByFolder(infos []storage.ThreadMessageMutationInfo) map[string][]uint32 {
	groups := make(map[string][]uint32)
	for _, info := range infos {
		if info.FolderRemoteID == "" || info.RemoteUID == 0 {
			continue
		}
		groups[info.FolderRemoteID] = append(groups[info.FolderRemoteID], info.RemoteUID)
	}
	return groups
}

type messageBulkTarget struct {
	ID     string `json:"id"`
	Thread bool   `json:"thread"`
}

type messageBulkRequest struct {
	Targets  []messageBulkTarget `json:"targets"`
	State    string              `json:"state"`
	FolderID string              `json:"folder_id"`
}

func decodeMessageBulkRequest(r *http.Request) (messageBulkRequest, error) {
	var payload messageBulkRequest
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		return payload, err
	}
	if len(payload.Targets) == 0 {
		return payload, errors.New("no messages selected")
	}
	return payload, nil
}

func messageBulkTargets(payload messageBulkRequest) []messageBulkTarget {
	seenMessages := map[string]bool{}
	seenThreads := map[string]bool{}
	targets := make([]messageBulkTarget, 0, len(payload.Targets))
	for _, target := range payload.Targets {
		id := strings.TrimSpace(target.ID)
		if id == "" {
			continue
		}
		key := id
		if target.Thread {
			key = "thread:" + id
			if seenThreads[key] {
				continue
			}
			seenThreads[key] = true
		} else {
			if seenMessages[key] {
				continue
			}
			seenMessages[key] = true
		}
		targets = append(targets, messageBulkTarget{ID: id, Thread: target.Thread})
	}
	return targets
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

	targetRead := !currentState
	switch r.URL.Query().Get("state") {
	case "read":
		targetRead = true
	case "unread":
		targetRead = false
	}

	if err := h.db.SetMessageRead(ctx, msgID, targetRead); err != nil {
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
		if !targetRead {
			op = goimap.StoreFlagsDel
		}
		client.StoreFlags(context.Background(), info.FolderRemoteID, info.RemoteUID, op, []goimap.Flag{goimap.FlagSeen})
	}()

	h.publishMutation(info.AccountID, info.FolderID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"is_read": targetRead})
}

func (h *Handler) handleMarkMessagesRead(w http.ResponseWriter, r *http.Request) {
	payload, err := decodeMessageBulkRequest(r)
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	ctx := context.WithoutCancel(r.Context())
	updated := 0
	seenMessages := map[string]bool{}
	seenThreads := map[string]bool{}

	for _, target := range messageBulkTargets(payload) {
		id := strings.TrimSpace(target.ID)
		if id == "" {
			continue
		}

		if target.Thread {
			email, err := h.db.GetEmailByID(ctx, id)
			if err != nil || email == nil || email.ThreadID == "" {
				continue
			}
			threadKey := email.AccountID + ":" + email.ThreadID
			if seenThreads[threadKey] {
				continue
			}
			seenThreads[threadKey] = true

			infos, err := h.db.GetThreadMutationInfos(ctx, email.AccountID, email.ThreadID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if len(infos) == 0 {
				continue
			}
			if err := h.db.SetThreadRead(ctx, email.AccountID, email.ThreadID, true); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			updated++

			go func(accountID string, infos []storage.ThreadMessageMutationInfo) {
				client, err := h.connectIMAP(context.Background(), accountID)
				if err != nil {
					return
				}
				defer client.Close()
				for folderRemoteID, uids := range threadUIDsByFolder(infos) {
					client.StoreFlagsBatch(context.Background(), folderRemoteID, uids, goimap.StoreFlagsAdd, []goimap.Flag{goimap.FlagSeen})
				}
			}(email.AccountID, infos)

			h.publishThreadMutation(infos)
			continue
		}

		if seenMessages[id] {
			continue
		}
		seenMessages[id] = true

		msgID, info, err := h.getMessageInfo(ctx, id)
		if err != nil {
			continue
		}
		if err := h.db.SetMessageRead(ctx, msgID, true); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		updated++

		go func(info *storage.MessageMutationInfo) {
			client, err := h.connectIMAP(context.Background(), info.AccountID)
			if err != nil {
				return
			}
			defer client.Close()
			client.StoreFlags(context.Background(), info.FolderRemoteID, info.RemoteUID, goimap.StoreFlagsAdd, []goimap.Flag{goimap.FlagSeen})
		}(info)

		h.publishMutation(info.AccountID, info.FolderID)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]int{"updated": updated})
}

func (h *Handler) handleMarkMessagesStarred(w http.ResponseWriter, r *http.Request) {
	payload, err := decodeMessageBulkRequest(r)
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	targetStarred := payload.State != "unstarred" && payload.State != "false"
	ctx := context.WithoutCancel(r.Context())
	updated := 0

	for _, target := range messageBulkTargets(payload) {
		if target.Thread {
			email, err := h.db.GetEmailByID(ctx, target.ID)
			if err != nil || email == nil || email.ThreadID == "" {
				continue
			}
			infos, err := h.db.GetThreadMutationInfos(ctx, email.AccountID, email.ThreadID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			for _, info := range infos {
				if err := h.db.SetMessageStarred(ctx, info.MessageID, targetStarred); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			updated++
			go func(accountID string, infos []storage.ThreadMessageMutationInfo, starred bool) {
				client, err := h.connectIMAP(context.Background(), accountID)
				if err != nil {
					return
				}
				defer client.Close()
				op := goimap.StoreFlagsAdd
				if !starred {
					op = goimap.StoreFlagsDel
				}
				for folderRemoteID, uids := range threadUIDsByFolder(infos) {
					client.StoreFlagsBatch(context.Background(), folderRemoteID, uids, op, []goimap.Flag{goimap.FlagFlagged})
				}
			}(email.AccountID, infos, targetStarred)
			h.publishThreadMutation(infos)
			continue
		}

		msgID, info, err := h.getMessageInfo(ctx, target.ID)
		if err != nil {
			continue
		}
		if err := h.db.SetMessageStarred(ctx, msgID, targetStarred); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		updated++
		go func(info *storage.MessageMutationInfo, starred bool) {
			client, err := h.connectIMAP(context.Background(), info.AccountID)
			if err != nil {
				return
			}
			defer client.Close()
			op := goimap.StoreFlagsAdd
			if !starred {
				op = goimap.StoreFlagsDel
			}
			client.StoreFlags(context.Background(), info.FolderRemoteID, info.RemoteUID, op, []goimap.Flag{goimap.FlagFlagged})
		}(info, targetStarred)
		h.publishMutation(info.AccountID, info.FolderID)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]int{"updated": updated})
}

func (h *Handler) handleArchiveMessages(w http.ResponseWriter, r *http.Request) {
	payload, err := decodeMessageBulkRequest(r)
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	updated := 0

	for _, target := range messageBulkTargets(payload) {
		if target.Thread {
			_, currentInfo, err := h.getMessageInfo(ctx, target.ID)
			if err != nil || currentInfo.FolderRole == "archive" || currentInfo.FolderRole == "trash" {
				continue
			}
			email, err := h.db.GetEmailByID(ctx, target.ID)
			if err != nil || email == nil || email.ThreadID == "" {
				continue
			}
			archiveFolderID, archiveRemoteID, err := h.db.GetFolderIDByRole(ctx, email.AccountID, "archive")
			if err != nil || archiveFolderID == "" || archiveFolderID == currentInfo.FolderID {
				continue
			}
			infos, err := h.db.GetThreadMutationInfosInFolder(ctx, email.AccountID, email.ThreadID, currentInfo.FolderID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			for _, info := range infos {
				if err := h.db.RemoveMessageFromFolder(ctx, info.MessageID, info.FolderID); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				if err := h.db.AddMessageToFolder(ctx, info.MessageID, archiveFolderID, 0, info.IsRead, info.IsStarred); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			updated++
			h.publishThreadMutation(infos)
			h.publishMutation(email.AccountID, archiveFolderID)
			go func(accountID, archiveRemoteID string, infos []storage.ThreadMessageMutationInfo) {
				client, err := h.connectIMAP(context.Background(), accountID)
				if err != nil {
					return
				}
				defer client.Close()
				for folderRemoteID, uids := range threadUIDsByFolder(infos) {
					client.MoveMessages(context.Background(), folderRemoteID, uids, archiveRemoteID)
				}
			}(email.AccountID, archiveRemoteID, infos)
			continue
		}

		msgID, info, err := h.getMessageInfo(ctx, target.ID)
		if err != nil || info.FolderRole == "archive" || info.FolderRole == "trash" {
			continue
		}
		archiveFolderID, archiveRemoteID, err := h.db.GetFolderIDByRole(ctx, info.AccountID, "archive")
		if err != nil || archiveFolderID == "" || archiveFolderID == info.FolderID {
			continue
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
		if err := h.db.RemoveMessageFromFolder(ctx, msgID, info.FolderID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := h.db.AddMessageToFolder(ctx, msgID, archiveFolderID, 0, isRead, isStarred); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		updated++
		h.publishMutation(info.AccountID, info.FolderID)
		h.publishMutation(info.AccountID, archiveFolderID)
		go func(info *storage.MessageMutationInfo, archiveRemoteID string) {
			client, err := h.connectIMAP(context.Background(), info.AccountID)
			if err != nil {
				return
			}
			defer client.Close()
			client.MoveMessage(context.Background(), info.FolderRemoteID, info.RemoteUID, archiveRemoteID)
		}(info, archiveRemoteID)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]int{"updated": updated})
}

func (h *Handler) handleDeleteMessages(w http.ResponseWriter, r *http.Request) {
	payload, err := decodeMessageBulkRequest(r)
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	updated := 0

	for _, target := range messageBulkTargets(payload) {
		if target.Thread {
			email, err := h.db.GetEmailByID(ctx, target.ID)
			if err != nil || email == nil || email.ThreadID == "" {
				continue
			}
			infos, err := h.db.GetThreadMutationInfos(ctx, email.AccountID, email.ThreadID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			trashFolderID, trashRemoteID, err := h.db.GetFolderIDByRole(ctx, email.AccountID, "trash")
			if err != nil || trashFolderID == "" {
				continue
			}
			deleteInfos := make([]storage.ThreadMessageMutationInfo, 0)
			moveInfos := make([]storage.ThreadMessageMutationInfo, 0)
			for _, info := range infos {
				if info.FolderRole == "trash" {
					deleteInfos = append(deleteInfos, info)
				} else {
					moveInfos = append(moveInfos, info)
				}
			}
			for _, info := range deleteInfos {
				if err := h.db.MarkMessageDeleted(ctx, info.MessageID); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			for _, info := range moveInfos {
				if err := h.db.RemoveMessageFromFolder(ctx, info.MessageID, info.FolderID); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				if err := h.db.AddMessageToFolder(ctx, info.MessageID, trashFolderID, 0, info.IsRead, info.IsStarred); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			updated++
			h.publishThreadMutation(infos)
			h.publishMutation(email.AccountID, trashFolderID)
			go func(accountID, trashRemoteID string, deleteInfos, moveInfos []storage.ThreadMessageMutationInfo) {
				client, err := h.connectIMAP(context.Background(), accountID)
				if err != nil {
					return
				}
				defer client.Close()
				for folderRemoteID, uids := range threadUIDsByFolder(deleteInfos) {
					client.DeleteMessages(context.Background(), folderRemoteID, uids)
				}
				for folderRemoteID, uids := range threadUIDsByFolder(moveInfos) {
					client.MoveMessages(context.Background(), folderRemoteID, uids, trashRemoteID)
				}
			}(email.AccountID, trashRemoteID, deleteInfos, moveInfos)
			continue
		}

		msgID, info, err := h.getMessageInfo(ctx, target.ID)
		if err != nil {
			continue
		}
		if info.FolderRole == "trash" {
			if err := h.db.MarkMessageDeleted(ctx, msgID); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			go func(info *storage.MessageMutationInfo) {
				client, err := h.connectIMAP(context.Background(), info.AccountID)
				if err != nil {
					return
				}
				defer client.Close()
				client.DeleteMessages(context.Background(), info.FolderRemoteID, []uint32{info.RemoteUID})
			}(info)
		} else {
			trashFolderID, trashRemoteID, err := h.db.GetFolderIDByRole(ctx, info.AccountID, "trash")
			if err != nil || trashFolderID == "" {
				continue
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
			if err := h.db.RemoveMessageFromFolder(ctx, msgID, info.FolderID); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := h.db.AddMessageToFolder(ctx, msgID, trashFolderID, 0, isRead, isStarred); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			go func(info *storage.MessageMutationInfo, trashRemoteID string) {
				client, err := h.connectIMAP(context.Background(), info.AccountID)
				if err != nil {
					return
				}
				defer client.Close()
				client.MoveMessage(context.Background(), info.FolderRemoteID, info.RemoteUID, trashRemoteID)
			}(info, trashRemoteID)
			h.publishMutation(info.AccountID, trashFolderID)
		}
		updated++
		h.publishMutation(info.AccountID, info.FolderID)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]int{"updated": updated})
}

func (h *Handler) handleMoveMessages(w http.ResponseWriter, r *http.Request) {
	payload, err := decodeMessageBulkRequest(r)
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	destFolderID := strings.TrimSpace(payload.FolderID)
	if destFolderID == "" {
		http.Error(w, "folder_id required", http.StatusBadRequest)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	var destRemoteID string
	if err := h.db.Read().QueryRowContext(ctx, `SELECT remote_id FROM folders WHERE id = ?`, destFolderID).Scan(&destRemoteID); err != nil {
		http.Error(w, "destination folder not found", http.StatusBadRequest)
		return
	}
	updated := 0

	for _, target := range messageBulkTargets(payload) {
		if target.Thread {
			_, currentInfo, err := h.getMessageInfo(ctx, target.ID)
			if err != nil || currentInfo.FolderID == destFolderID {
				continue
			}
			email, err := h.db.GetEmailByID(ctx, target.ID)
			if err != nil || email == nil || email.ThreadID == "" {
				continue
			}
			infos, err := h.db.GetThreadMutationInfosInFolder(ctx, email.AccountID, email.ThreadID, currentInfo.FolderID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			for _, info := range infos {
				if err := h.db.RemoveMessageFromFolder(ctx, info.MessageID, info.FolderID); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				if err := h.db.AddMessageToFolder(ctx, info.MessageID, destFolderID, 0, info.IsRead, info.IsStarred); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			updated++
			h.publishThreadMutation(infos)
			h.publishMutation(email.AccountID, destFolderID)
			go func(accountID, destRemoteID string, infos []storage.ThreadMessageMutationInfo) {
				client, err := h.connectIMAP(context.Background(), accountID)
				if err != nil {
					return
				}
				defer client.Close()
				for folderRemoteID, uids := range threadUIDsByFolder(infos) {
					client.MoveMessages(context.Background(), folderRemoteID, uids, destRemoteID)
				}
			}(email.AccountID, destRemoteID, infos)
			continue
		}

		msgID, info, err := h.getMessageInfo(ctx, target.ID)
		if err != nil || info.FolderID == destFolderID {
			continue
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
		if err := h.db.RemoveMessageFromFolder(ctx, msgID, info.FolderID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := h.db.AddMessageToFolder(ctx, msgID, destFolderID, 0, isRead, isStarred); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		updated++
		h.publishMutation(info.AccountID, info.FolderID)
		h.publishMutation(info.AccountID, destFolderID)
		go func(info *storage.MessageMutationInfo, destRemoteID string) {
			client, err := h.connectIMAP(context.Background(), info.AccountID)
			if err != nil {
				return
			}
			defer client.Close()
			client.MoveMessage(context.Background(), info.FolderRemoteID, info.RemoteUID, destRemoteID)
		}(info, destRemoteID)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]int{"updated": updated})
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

func (h *Handler) handleToggleThreadRead(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	ctx := r.Context()

	email, err := h.db.GetEmailByID(ctx, idStr)
	if err != nil || email == nil || email.ThreadID == "" {
		http.Error(w, "message not found", http.StatusBadRequest)
		return
	}

	infos, err := h.db.GetThreadMutationInfos(ctx, email.AccountID, email.ThreadID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(infos) == 0 {
		http.Error(w, "thread not found", http.StatusBadRequest)
		return
	}

	hasUnread, err := h.db.ThreadHasUnread(ctx, email.AccountID, email.ThreadID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	targetRead := hasUnread
	switch r.URL.Query().Get("state") {
	case "read":
		targetRead = true
	case "unread":
		targetRead = false
	}
	if err := h.db.SetThreadRead(ctx, email.AccountID, email.ThreadID, targetRead); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	go func() {
		client, err := h.connectIMAP(context.Background(), email.AccountID)
		if err != nil {
			return
		}
		defer client.Close()

		op := goimap.StoreFlagsAdd
		if !targetRead {
			op = goimap.StoreFlagsDel
		}
		for folderRemoteID, uids := range threadUIDsByFolder(infos) {
			client.StoreFlagsBatch(context.Background(), folderRemoteID, uids, op, []goimap.Flag{goimap.FlagSeen})
		}
	}()

	h.publishThreadMutation(infos)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"is_read": targetRead})
}

func (h *Handler) handleArchiveThread(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	ctx := r.Context()

	_, info, err := h.getMessageInfo(ctx, idStr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if info.FolderRole == "archive" || info.FolderRole == "trash" {
		http.Error(w, "thread cannot be archived from this folder", http.StatusBadRequest)
		return
	}

	email, err := h.db.GetEmailByID(ctx, idStr)
	if err != nil || email == nil || email.ThreadID == "" {
		http.Error(w, "message not found", http.StatusBadRequest)
		return
	}

	archiveFolderID, archiveRemoteID, err := h.db.GetFolderIDByRole(ctx, email.AccountID, "archive")
	if err != nil || archiveFolderID == "" {
		http.Error(w, "no archive folder found", http.StatusBadRequest)
		return
	}
	if archiveFolderID == info.FolderID {
		http.Error(w, "thread is already archived", http.StatusBadRequest)
		return
	}

	infos, err := h.db.GetThreadMutationInfosInFolder(ctx, email.AccountID, email.ThreadID, info.FolderID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(infos) == 0 {
		http.Error(w, "thread not found in current folder", http.StatusBadRequest)
		return
	}

	go func() {
		client, err := h.connectIMAP(context.Background(), email.AccountID)
		if err != nil {
			return
		}
		defer client.Close()
		for folderRemoteID, uids := range threadUIDsByFolder(infos) {
			client.MoveMessages(context.Background(), folderRemoteID, uids, archiveRemoteID)
		}
	}()

	for _, threadInfo := range infos {
		h.db.RemoveMessageFromFolder(ctx, threadInfo.MessageID, threadInfo.FolderID)
		h.db.AddMessageToFolder(ctx, threadInfo.MessageID, archiveFolderID, 0, threadInfo.IsRead, threadInfo.IsStarred)
	}
	h.publishThreadMutation(infos)
	h.publishMutation(email.AccountID, archiveFolderID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "archived"})
}

func (h *Handler) handleDeleteThread(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	ctx := r.Context()

	email, err := h.db.GetEmailByID(ctx, idStr)
	if err != nil || email == nil || email.ThreadID == "" {
		http.Error(w, "message not found", http.StatusBadRequest)
		return
	}

	infos, err := h.db.GetThreadMutationInfos(ctx, email.AccountID, email.ThreadID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(infos) == 0 {
		http.Error(w, "thread not found", http.StatusBadRequest)
		return
	}

	trashFolderID, trashRemoteID, err := h.db.GetFolderIDByRole(ctx, email.AccountID, "trash")
	if err != nil || trashFolderID == "" {
		http.Error(w, "no trash folder found", http.StatusBadRequest)
		return
	}

	deleteInfos := make([]storage.ThreadMessageMutationInfo, 0)
	moveInfos := make([]storage.ThreadMessageMutationInfo, 0)
	for _, info := range infos {
		if info.FolderRole == "trash" {
			deleteInfos = append(deleteInfos, info)
			continue
		}
		moveInfos = append(moveInfos, info)
	}

	go func() {
		client, err := h.connectIMAP(context.Background(), email.AccountID)
		if err != nil {
			return
		}
		defer client.Close()
		for folderRemoteID, uids := range threadUIDsByFolder(deleteInfos) {
			client.DeleteMessages(context.Background(), folderRemoteID, uids)
		}
		for folderRemoteID, uids := range threadUIDsByFolder(moveInfos) {
			client.MoveMessages(context.Background(), folderRemoteID, uids, trashRemoteID)
		}
	}()

	for _, info := range deleteInfos {
		h.db.MarkMessageDeleted(ctx, info.MessageID)
	}
	for _, info := range moveInfos {
		h.db.RemoveMessageFromFolder(ctx, info.MessageID, info.FolderID)
		h.db.AddMessageToFolder(ctx, info.MessageID, trashFolderID, 0, info.IsRead, info.IsStarred)
	}
	h.publishThreadMutation(infos)
	h.publishMutation(email.AccountID, trashFolderID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
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

	h.ensureBodyFetched(ctx, msgID, info.AccountID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "refetched"})
}

func (h *Handler) handlePrefetchBody(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	ctx := r.Context()

	msgID, info, err := h.getMessageInfo(ctx, idStr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if !h.db.IsBodyFetched(ctx, msgID) {
		h.ensureBodyFetched(ctx, msgID, info.AccountID)
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	oauthError := r.URL.Query().Get("error")
	baseURL := h.auth.Config().BaseURL

	views.LoginPage(baseURL, oauthError).Render(r.Context(), w)
}

func (h *Handler) handleGoogleRedirect(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Error(w, "auth not enabled", http.StatusNotFound)
		return
	}

	state := h.auth.GenerateState()
	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_state",
		Value:    state,
		Path:     "/",
		MaxAge:   600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	url := h.auth.GoogleOAuthURL(state)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func (h *Handler) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsEnabled() {
		http.Error(w, "auth not enabled", http.StatusNotFound)
		return
	}

	stateCookie, err := r.Cookie("oauth_state")
	if err != nil || stateCookie.Value == "" {
		http.Redirect(w, r, "/login?error=missing_state", http.StatusSeeOther)
		return
	}

	stateParam := r.URL.Query().Get("state")
	if stateParam != stateCookie.Value {
		http.Redirect(w, r, "/login?error=invalid_state", http.StatusSeeOther)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:   "oauth_state",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})

	code := r.URL.Query().Get("code")
	if code == "" {
		errorDesc := r.URL.Query().Get("error")
		if errorDesc == "" {
			errorDesc = "no_code"
		}
		http.Redirect(w, r, "/login?error="+errorDesc, http.StatusSeeOther)
		return
	}

	user, session, err := h.auth.HandleGoogleCallback(r.Context(), code, r.UserAgent())
	if err != nil {
		http.Redirect(w, r, "/login?error=auth_failed", http.StatusSeeOther)
		return
	}

	_ = user
	auth.SetSessionCookie(w, session.Token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := auth.GetSessionToken(r)
	if token != "" {
		h.auth.DeleteSession(r.Context(), token)
	}
	auth.ClearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *Handler) handleAccountOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	if !h.auth.HasGoogleOAuth() {
		http.Error(w, "google oauth not configured", http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}

	formData := map[string]string{
		"provider":      r.FormValue("provider"),
		"email_address": r.FormValue("email_address"),
		"display_name":  r.FormValue("display_name"),
		"imap_host":     r.FormValue("imap_host"),
		"imap_port":     r.FormValue("imap_port"),
		"imap_tls_mode": r.FormValue("imap_tls_mode"),
		"smtp_host":     r.FormValue("smtp_host"),
		"smtp_port":     r.FormValue("smtp_port"),
		"smtp_tls_mode": r.FormValue("smtp_tls_mode"),
		"username":      r.FormValue("username"),
		"smtp_username": r.FormValue("smtp_username"),
		"smtp_password": r.FormValue("smtp_password"),
	}

	jsonData, _ := json.Marshal(formData)
	formEncoded := base64.StdEncoding.EncodeToString(jsonData)

	state := h.auth.GenerateState()
	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_account_state",
		Value:    state,
		Path:     "/",
		MaxAge:   600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "oauth_account_form",
		Value:    formEncoded,
		Path:     "/",
		MaxAge:   600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	url := h.auth.GoogleAccountOAuthURL(state)
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func (h *Handler) handleGoogleAccountCallback(w http.ResponseWriter, r *http.Request) {
	if !h.auth.HasGoogleOAuth() {
		log.Printf("gmail callback: google oauth not configured")
		http.Error(w, "google oauth not configured", http.StatusNotFound)
		return
	}

	stateCookie, err := r.Cookie("oauth_account_state")
	if err != nil || stateCookie.Value == "" {
		log.Printf("gmail callback: missing state cookie: %v", err)
		http.Redirect(w, r, "/settings/accounts?error=oauth_missing_state", http.StatusSeeOther)
		return
	}

	stateParam := r.URL.Query().Get("state")
	if stateParam != stateCookie.Value {
		log.Printf("gmail callback: state mismatch")
		http.Redirect(w, r, "/settings/accounts?error=oauth_invalid_state", http.StatusSeeOther)
		return
	}

	formCookie, err := r.Cookie("oauth_account_form")
	if err != nil || formCookie.Value == "" {
		log.Printf("gmail callback: missing form cookie: %v", err)
		http.Redirect(w, r, "/settings/accounts?error=oauth_no_form", http.StatusSeeOther)
		return
	}

	formBytes, err := base64.StdEncoding.DecodeString(formCookie.Value)
	if err != nil {
		log.Printf("gmail callback: base64 decode error: %v", err)
		http.Redirect(w, r, "/settings/accounts?error=oauth_bad_form", http.StatusSeeOther)
		return
	}

	var formData map[string]string
	if err := json.Unmarshal(formBytes, &formData); err != nil {
		log.Printf("gmail callback: json unmarshal error: %v", err)
		http.Redirect(w, r, "/settings/accounts?error=oauth_bad_form", http.StatusSeeOther)
		return
	}

	http.SetCookie(w, &http.Cookie{Name: "oauth_account_state", Value: "", Path: "/", MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: "oauth_account_form", Value: "", Path: "/", MaxAge: -1})

	code := r.URL.Query().Get("code")
	if code == "" {
		log.Printf("gmail callback: no code in URL")
		http.Redirect(w, r, "/settings/accounts?error=oauth_no_code", http.StatusSeeOther)
		return
	}

	token, err := h.auth.ExchangeAccountCode(r.Context(), code)
	if err != nil {
		log.Printf("gmail callback: token exchange failed: %v", err)
		http.Redirect(w, r, "/settings/accounts?error=oauth_exchange_failed", http.StatusSeeOther)
		return
	}

	info, err := h.auth.GetGoogleUserInfo(r.Context(), token)
	if err != nil {
		log.Printf("gmail callback: userinfo failed: %v", err)
		http.Redirect(w, r, "/settings/accounts?error=oauth_userinfo_failed", http.StatusSeeOther)
		return
	}

	requestedEmail := strings.TrimSpace(strings.ToLower(formData["email_address"]))
	googleEmail := strings.TrimSpace(strings.ToLower(info.Email))
	if requestedEmail != "" && googleEmail != "" && requestedEmail != googleEmail {
		log.Printf("gmail callback: requested email %q does not match authorized google account %q", formData["email_address"], info.Email)
		http.Redirect(w, r, "/settings/accounts?error=oauth_email_mismatch", http.StatusSeeOther)
		return
	}
	displayName := strings.TrimSpace(formData["display_name"])
	if displayName == "" {
		displayName = info.Name
	}

	req := providers.GmailAccountRequest(info.Email, displayName, info.Sub)
	req.Password = "_oauth2_"

	userID := h.userID(r.Context())
	existingAccountID, err := h.accountStore.FindProviderAccountID(r.Context(), userID, req.Provider, req.ProviderAccountID, req.EmailAddress)
	if err != nil {
		log.Printf("gmail callback: find existing account failed: %v", err)
		http.Redirect(w, r, "/settings/accounts?error=create_failed", http.StatusSeeOther)
		return
	}
	accountID := existingAccountID
	if accountID != "" {
		err = h.accountStore.UpdateAccount(r.Context(), accountID, req)
	} else {
		account, createErr := h.accountStore.CreateAccount(r.Context(), userID, req)
		if createErr == nil {
			accountID = account.ID
		}
		err = createErr
	}
	if err != nil {
		log.Printf("gmail callback: create account failed: %v", err)
		http.Redirect(w, r, "/settings/accounts?error=create_failed", http.StatusSeeOther)
		return
	}

	var expiresAt *time.Time
	if !token.Expiry.IsZero() {
		t := token.Expiry
		expiresAt = &t
	}

	err = h.auth.UpsertOAuthAccount(r.Context(), userID, providers.OAuthGoogle, info.Sub, token.AccessToken, token.RefreshToken, token.TokenType, expiresAt, "")
	if err != nil {
		log.Printf("warning: failed to store oauth tokens for account %s: %v", accountID, err)
	}

	h.syncer.SyncAccount(context.Background(), accountID)
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := h.SyncContactAccount(bg, accountID); err != nil && !errors.Is(err, errContactSyncAlreadyRunning) {
			log.Printf("contacts sync %s after gmail connect: %v", accountID, err)
		}
	}()

	http.Redirect(w, r, "/settings/accounts", http.StatusSeeOther)
}
