package handler

import (
	"encoding/json"
	"errors"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) registerProfileTransferRoutes(register func(string, http.HandlerFunc)) {
	register("GET /api/profile/export", h.handleExportProfile)
	register("GET /api/profile/import/upload", h.handleProfileImportUpload)
	register("POST /api/profile/import/preview", h.handlePreviewProfileImport)
	register("POST /api/profile/import", h.handleImportProfile)
}

func (h *Handler) handleExportProfile(w http.ResponseWriter, r *http.Request) {
	ctx, owner := r.Context(), h.userID(r.Context())
	now := time.Now()
	var file *profileFile
	err := h.withProfileStore(ctx, owner, func(store *config.AccountStore, db *storage.DB) error {
		var err error
		file, err = exportProfile(ctx, store, db, owner, r.URL.Query().Get("contacts") == "1", now)
		return err
	})
	if err != nil {
		log.Printf("profile export: %v", err)
		http.Error(w, i18n.T(r.Context(), "server.error.could_not_export_profile"), http.StatusInternalServerError)
		return
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		http.Error(w, i18n.T(r.Context(), "server.error.could_not_export_profile"), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="gofer-profile-`+now.Format("2006-01-02")+`.json"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(append(data, '\n'))
}

func (h *Handler) handleProfileImportUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.ProfileImportUploadForm("").Render(r.Context(), w)
}

func renderProfileImportError(w http.ResponseWriter, r *http.Request, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.ProfileImportUploadForm(message).Render(r.Context(), w)
}

func (h *Handler) handlePreviewProfileImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, profileImportMaxBytes+(128<<10))
	if err := r.ParseMultipartForm(profileImportMaxBytes); err != nil {
		renderProfileImportError(w, r, "Choose a profile file of up to 16 MB.")
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	upload, _, err := r.FormFile("profile")
	if err != nil {
		renderProfileImportError(w, r, "Choose a profile file.")
		return
	}
	defer upload.Close()
	data, err := io.ReadAll(io.LimitReader(upload, profileImportMaxBytes+1))
	if err != nil || len(data) > profileImportMaxBytes {
		renderProfileImportError(w, r, "Choose a profile file of up to 16 MB.")
		return
	}
	file, err := parseProfile(data)
	if err != nil {
		renderProfileImportError(w, r, err.Error())
		return
	}
	ctx, owner := r.Context(), h.userID(r.Context())
	existing, err := h.profileExistingAccounts(ctx, owner)
	if err != nil {
		userAccountError(w, r, err)
		return
	}
	plans := h.profileAccountPlans(file, existing)
	contacts, err := profileContacts(file)
	if err != nil {
		renderProfileImportError(w, r, "The contacts in this profile could not be read.")
		return
	}
	var conflicts []profileContactConflict
	if err := h.withProfileStore(ctx, owner, func(_ *config.AccountStore, db *storage.DB) error {
		var err error
		conflicts, err = findProfileContactConflicts(ctx, db, owner, contacts)
		return err
	}); err != nil {
		userAccountError(w, r, err)
		return
	}

	preview := views.ProfileImportPreviewData{
		Profile:        string(data),
		HasPreferences: len(file.Preferences.UI) > 0 || file.Preferences.SyncIntervalMinutes != "" || file.Preferences.IdleFolders != "" || len(file.Preferences.AllowedSenders) > 0,
	}
	if exported, err := time.Parse(time.RFC3339, file.ExportedAt); err == nil {
		preview.ExportedAt = i18n.Dates(r.Context()).LongMonthDate(exported)
	}
	for _, signature := range file.Signatures {
		if strings.TrimSpace(signature.HTMLBody) != "" || strings.TrimSpace(signature.TextBody) != "" {
			preview.Signatures = append(preview.Signatures, signature.Name)
		}
	}
	preview.Contacts = len(contacts)
	for _, conflict := range conflicts {
		preview.Conflicts = append(preview.Conflicts, views.ProfileImportContactConflict{
			Index:          conflict.Index,
			Name:           conflict.Imported.FallbackName(),
			Detail:         profileContactDetail(conflict.Imported),
			ExistingName:   conflict.Existing.FallbackName(),
			ExistingDetail: profileContactDetail(conflict.Existing),
			ByEmail:        conflict.ByEmail,
			Default:        conflict.DefaultAction(),
		})
	}
	for _, account := range file.Accounts {
		color, _ := normalizeAccountColor(account.Color)
		preview.Accounts = append(preview.Accounts, views.ProfileImportAccount{
			Ref: account.Ref, Email: account.EmailAddress, Name: account.DisplayName,
			Provider: account.Provider, Color: color, Status: string(plans[account.Ref].Status),
			CanReplace: plans[account.Ref].CanReplace,
			Needs:      profileImportPending(account).Missing(),
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.ProfileImportPreview(preview).Render(ctx, w)
}

func (h *Handler) handleImportProfile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, profileImportMaxBytes+(128<<10))
	if err := r.ParseForm(); err != nil {
		renderProfileImportError(w, r, "Choose a profile file of up to 16 MB.")
		return
	}
	file, err := parseProfile([]byte(r.PostForm.Get("profile")))
	if err != nil {
		renderProfileImportError(w, r, err.Error())
		return
	}
	options := profileImportOptions{
		Preferences:    r.PostForm.Get("preferences") == "1",
		Signatures:     r.PostForm.Get("signatures") == "1",
		Contacts:       r.PostForm.Get("contacts") == "1",
		Accounts:       map[string]bool{},
		AccountActions: map[string]string{},
		ContactActions: map[int]string{},
	}
	for _, ref := range r.PostForm["account"] {
		options.Accounts[ref] = true
	}
	for key, values := range r.PostForm {
		if len(values) == 0 {
			continue
		}
		if ref, ok := strings.CutPrefix(key, "account_action:"); ok {
			options.AccountActions[ref] = values[0]
		} else if index, ok := strings.CutPrefix(key, "contact_action:"); ok {
			if i, err := strconv.Atoi(index); err == nil {
				options.ContactActions[i] = values[0]
			}
		}
	}
	ctx, owner := r.Context(), h.userID(r.Context())
	result, err := h.importProfile(ctx, owner, file, options)
	if err != nil {
		log.Printf("profile import: %v", err)
		message := "The import stopped before it finished. Import the file again to add the rest; what was already imported is not added twice."
		if errors.Is(err, storage.ErrAccountRoute) {
			message = "Your storage is not available right now. Try again in a moment."
		}
		renderProfileImportError(w, r, message)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = views.ProfileImportResult(profileImportResultView(result)).Render(ctx, w)
}

func profileImportResultView(result profileImportResult) views.ProfileImportResultData {
	return views.ProfileImportResultData{
		Accounts:         result.Accounts,
		AccountsReplaced: result.AccountsReplaced,
		AccountFailures:  result.AccountFailures,
		Signatures:       result.Signatures,
		Contacts:         result.Contacts,
		ContactsReplaced: result.ContactsReplaced,
		ContactsSkipped:  result.ContactsSkipped,
		Preferences:      result.Preferences,
	}
}

// profileContactDetail is what tells two contacts with the same name apart:
// their email address, or else their phone number or organization.
func profileContactDetail(contact models.Contact) string {
	var parts []string
	for _, value := range []string{contact.Email, contact.Phone, contact.Organization} {
		if value = strings.TrimSpace(value); value != "" && value != contact.FallbackName() {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " · ")
}
