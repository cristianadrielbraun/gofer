package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

func (h *Handler) handleAdminRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/avatars/", http.StatusFound)
}

func (h *Handler) handleAdmin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	activeTab, ok := adminAvatarTab(r)
	if !ok {
		http.NotFound(w, r)
		return
	}

	uiSettings := h.db.GetUISettings(ctx, h.userID(ctx))
	avatarStatus, err := h.avatarStatus(ctx)
	if err != nil {
		http.Error(w, "failed to get admin status", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		views.AdminPartial(avatarStatus, models.ContactAdminStatus{}, models.LabelAdminStatus{}, models.MailSecurityAdminData{}, models.MailOperationsAdminStatus{}, "avatars", activeTab).Render(ctx, w)
		return
	}

	views.AdminLayout(uiSettings, avatarStatus, models.ContactAdminStatus{}, models.LabelAdminStatus{}, models.MailSecurityAdminData{}, models.MailOperationsAdminStatus{}, "avatars", activeTab).Render(ctx, w)
}

func (h *Handler) handleAdminContacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uiSettings := h.db.GetUISettings(ctx, h.userID(ctx))
	contactStatus, err := h.contactAdminStatus(ctx)
	if err != nil {
		http.Error(w, "failed to get contact admin status", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		views.AdminPartial(models.AvatarStatus{}, contactStatus, models.LabelAdminStatus{}, models.MailSecurityAdminData{}, models.MailOperationsAdminStatus{}, "contacts", "").Render(ctx, w)
		return
	}

	views.AdminLayout(uiSettings, models.AvatarStatus{}, contactStatus, models.LabelAdminStatus{}, models.MailSecurityAdminData{}, models.MailOperationsAdminStatus{}, "contacts", "").Render(ctx, w)
}

func (h *Handler) handleAdminLabels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uiSettings := h.db.GetUISettings(ctx, h.userID(ctx))
	labelStatus, err := h.labelAdminStatus(ctx)
	if err != nil {
		http.Error(w, "failed to get label admin status", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		views.AdminPartial(models.AvatarStatus{}, models.ContactAdminStatus{}, labelStatus, models.MailSecurityAdminData{}, models.MailOperationsAdminStatus{}, "labels", "").Render(ctx, w)
		return
	}

	views.AdminLayout(uiSettings, models.AvatarStatus{}, models.ContactAdminStatus{}, labelStatus, models.MailSecurityAdminData{}, models.MailOperationsAdminStatus{}, "labels", "").Render(ctx, w)
}

func (h *Handler) handleAdminSecurity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	exceptions, err := h.db.ListMailSecurityExceptions(ctx)
	if err != nil {
		http.Error(w, "failed to load mail security exceptions", http.StatusInternalServerError)
		return
	}
	data := models.MailSecurityAdminData{
		Exceptions: exceptions,
		Notice:     strings.TrimSpace(r.URL.Query().Get("notice")),
		Error:      strings.TrimSpace(r.URL.Query().Get("error")),
	}
	uiSettings := h.db.GetUISettings(ctx, h.userID(ctx))
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html")
		views.AdminPartial(models.AvatarStatus{}, models.ContactAdminStatus{}, models.LabelAdminStatus{}, data, models.MailOperationsAdminStatus{}, "security", "").Render(ctx, w)
		return
	}
	views.AdminLayout(uiSettings, models.AvatarStatus{}, models.ContactAdminStatus{}, models.LabelAdminStatus{}, data, models.MailOperationsAdminStatus{}, "security", "").Render(ctx, w)
}

func (h *Handler) handleAddHTTPDiscoveryException(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectAdminSecurity(w, r, "", "Invalid form data.")
		return
	}
	if r.FormValue("acknowledge") != "yes" {
		redirectAdminSecurity(w, r, "", "Confirm that you understand the risk before adding an exception.")
		return
	}
	domain := normalizeDiscoveryExceptionDomain(r.FormValue("domain"))
	if !validDiscoveryExceptionDomain(domain) {
		redirectAdminSecurity(w, r, "", "Enter a valid email domain without a scheme or path.")
		return
	}
	user := h.userID(r.Context())
	if err := h.db.AddHTTPDiscoveryException(r.Context(), domain, user); err != nil {
		redirectAdminSecurity(w, r, "", "Could not add the HTTP discovery exception.")
		return
	}
	redirectAdminSecurity(w, r, "HTTP discovery is now allowed for "+domain+".", "")
}

func (h *Handler) handleAddPlaintextTransportException(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectAdminSecurity(w, r, "", "Invalid form data.")
		return
	}
	if r.FormValue("acknowledge") != "yes" {
		redirectAdminSecurity(w, r, "", "Confirm that you understand the risk before adding an exception.")
		return
	}
	protocol := strings.ToLower(strings.TrimSpace(r.FormValue("protocol")))
	host := normalizePlaintextExceptionHost(r.FormValue("host"))
	port, err := strconv.Atoi(strings.TrimSpace(r.FormValue("port")))
	if (protocol != "imap" && protocol != "smtp") || !validPlaintextExceptionHost(host) || err != nil || port < 1 || port > 65535 {
		redirectAdminSecurity(w, r, "", "Enter IMAP or SMTP with an exact host and a port between 1 and 65535.")
		return
	}
	if err := h.db.AddPlaintextTransportException(r.Context(), protocol, host, port, h.userID(r.Context())); err != nil {
		redirectAdminSecurity(w, r, "", "Could not add the plaintext transport exception.")
		return
	}
	h.restartAccountsUsingPlaintextException(r.Context(), protocol, host, port)
	redirectAdminSecurity(w, r, strings.ToUpper(protocol)+" plaintext is now allowed for "+host+":"+strconv.Itoa(port)+".", "")
}

func (h *Handler) handleAddPrivateTargetException(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		redirectAdminSecurity(w, r, "", "Invalid form data.")
		return
	}
	if r.FormValue("acknowledge") != "yes" {
		redirectAdminSecurity(w, r, "", "Confirm that you understand the risk before adding an exception.")
		return
	}
	protocol := strings.ToLower(strings.TrimSpace(r.FormValue("protocol")))
	host := normalizePrivateTargetHost(r.FormValue("host"))
	port, err := strconv.Atoi(strings.TrimSpace(r.FormValue("port")))
	if (protocol != "http" && protocol != "https" && protocol != "imap" && protocol != "smtp") || !validPrivateTargetHost(host) || err != nil || port < 1 || port > 65535 {
		redirectAdminSecurity(w, r, "", "Enter HTTP, HTTPS, IMAP, or SMTP with an exact host and a port between 1 and 65535.")
		return
	}
	if err := h.db.AddPrivateTargetException(r.Context(), protocol, host, port, h.userID(r.Context())); err != nil {
		redirectAdminSecurity(w, r, "", "Could not add the private target exception.")
		return
	}
	redirectAdminSecurity(w, r, strings.ToUpper(protocol)+" private target is now allowed for "+host+":"+strconv.Itoa(port)+".", "")
}

func (h *Handler) restartAccountsUsingPlaintextException(ctx context.Context, protocol, host string, port int) {
	if h.syncer == nil {
		return
	}
	items, err := h.db.ListMailSecurityExceptions(ctx)
	if err != nil {
		log.Printf("mail security: list accounts after adding %s %s:%d: %v", protocol, host, port, err)
		return
	}
	for _, item := range items {
		if item.Kind != models.MailSecurityExceptionPlaintextTransport || item.Protocol != protocol || item.Host != host || item.Port != port {
			continue
		}
		for _, account := range item.Accounts {
			h.closeBodyClient(account.ID)
			h.syncer.RestartAccount(account.ID)
		}
		return
	}
}

func (h *Handler) handleDeleteMailSecurityException(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	item, err := h.db.GetMailSecurityException(r.Context(), id)
	if err != nil {
		redirectAdminSecurity(w, r, "", "Could not load the security exception.")
		return
	}
	if item == nil {
		http.NotFound(w, r)
		return
	}
	if err := h.db.DeleteMailSecurityException(r.Context(), id); err != nil {
		redirectAdminSecurity(w, r, "", "Could not revoke the security exception.")
		return
	}
	for _, account := range item.Accounts {
		h.closeBodyClient(account.ID)
		if h.syncer != nil {
			h.syncer.StopAccount(account.ID)
		}
	}
	redirectAdminSecurity(w, r, "Security exception revoked.", "")
}

func redirectAdminSecurity(w http.ResponseWriter, r *http.Request, notice, message string) {
	values := url.Values{}
	if notice != "" {
		values.Set("notice", notice)
	}
	if message != "" {
		values.Set("error", message)
	}
	target := "/admin/security"
	if query := values.Encode(); query != "" {
		target += "?" + query
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func normalizeDiscoveryExceptionDomain(value string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
}

func validDiscoveryExceptionDomain(domain string) bool {
	if domain == "" || len(domain) > 253 || strings.ContainsAny(domain, "/:@ ") {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, char := range label {
			if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func normalizePlaintextExceptionHost(value string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
}

func validPlaintextExceptionHost(host string) bool {
	return host != "" && len(host) <= 253 && !strings.ContainsAny(host, "/@ ")
}

func normalizePrivateTargetHost(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	}
	return strings.ToLower(strings.TrimSuffix(value, "."))
}

func validPrivateTargetHost(host string) bool {
	if host == "" || len(host) > 253 || strings.ContainsAny(host, "/@?#% ") {
		return false
	}
	if address, err := netip.ParseAddr(host); err == nil {
		return address.Zone() == ""
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, char := range label {
			if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' {
				continue
			}
			return false
		}
	}
	return true
}

func (h *Handler) handleContactAdminStatus(w http.ResponseWriter, r *http.Request) {
	status, err := h.contactAdminStatus(r.Context())
	if err != nil {
		http.Error(w, "failed to get contact admin status", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (h *Handler) handleLabelAdminStatus(w http.ResponseWriter, r *http.Request) {
	status, err := h.labelAdminStatus(r.Context())
	if err != nil {
		http.Error(w, "failed to get label admin status", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (h *Handler) labelAdminStatus(ctx context.Context) (models.LabelAdminStatus, error) {
	return h.db.GetLabelAdminStatus(ctx, h.userID(ctx))
}

func (h *Handler) contactAdminStatus(ctx context.Context) (models.ContactAdminStatus, error) {
	status, err := h.db.GetContactAdminStatus(ctx, h.userID(ctx))
	if err != nil {
		return status, err
	}
	status.Backfill = h.getContactBackfillState()
	running := h.contactSyncRunningAccounts()
	for i := range status.AccountSync {
		status.AccountSync[i].Running = running[status.AccountSync[i].AccountID]
	}
	return status, nil
}

func (h *Handler) handleForceContactBackfill(w http.ResponseWriter, r *http.Request) {
	started := h.startContactBackfill(context.WithoutCancel(r.Context()), h.userID(r.Context()))
	if r.Header.Get("Accept") == "application/json" {
		w.Header().Set("Content-Type", "application/json")
		if !started {
			w.WriteHeader(http.StatusConflict)
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"started": started})
		return
	}
	http.Redirect(w, r, "/admin/contacts", http.StatusSeeOther)
}

func (h *Handler) startContactBackfill(ctx context.Context, userID string) bool {
	h.contactBackfillMu.Lock()
	if h.contactBackfillState.InProgress {
		h.contactBackfillMu.Unlock()
		return false
	}
	total, err := h.db.CountObservedContactBackfillCandidates(ctx, userID)
	if err != nil {
		total = 0
	}
	state := models.ContactBackfillState{InProgress: true, Total: total, StartedAt: time.Now().UTC()}
	h.contactBackfillState = state
	h.contactBackfillMu.Unlock()
	h.publishContactBackfill(userID, state)

	_ = h.db.LogContactActivity(ctx, userID, "backfill_forced", "", "Manual contact backfill requested", 0)
	go func() {
		backfillCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		err := h.db.BackfillObservedContactsWithProgress(backfillCtx, userID, func(processed int) {
			h.contactBackfillMu.Lock()
			h.contactBackfillState.Processed = processed
			state := h.contactBackfillState
			h.contactBackfillMu.Unlock()
			h.publishContactBackfill(userID, state)
		})
		h.contactBackfillMu.Lock()
		h.contactBackfillState.InProgress = false
		h.contactBackfillState.FinishedAt = time.Now().UTC()
		if err != nil {
			h.contactBackfillState.LastError = err.Error()
			log.Printf("contacts: manual backfill failed: %v", err)
		} else {
			h.contactBackfillState.LastError = ""
			h.contactBackfillState.Processed = h.contactBackfillState.Total
		}
		state := h.contactBackfillState
		h.contactBackfillMu.Unlock()
		h.publishContactBackfill(userID, state)
	}()
	return true
}

func (h *Handler) getContactBackfillState() models.ContactBackfillState {
	h.contactBackfillMu.RLock()
	defer h.contactBackfillMu.RUnlock()
	return h.contactBackfillState
}

func (h *Handler) publishContactBackfill(userID string, state models.ContactBackfillState) {
	if h.syncer == nil {
		return
	}
	h.syncer.Events().Publish(mail.Event{Type: mail.EventContactBackfill, Payload: map[string]any{"user_id": userID, "backfill": state}})
}

func adminAvatarTab(r *http.Request) (string, bool) {
	tab := r.PathValue("tab")
	if tab == "" {
		return "overview", true
	}
	switch tab {
	case "overview", "senders", "providers", "events":
		return tab, true
	default:
		return "", false
	}
}
