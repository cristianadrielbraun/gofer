package handler

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const managementHandoffInvitationPath = "/admin/separate/invitations"

func (h *Handler) handleManagementSeparation(w http.ResponseWriter, r *http.Request) {
	h.renderManagementSeparation(w, r, http.StatusOK, views.ManagementHandoffFormData{}, nil, "")
}

func (h *Handler) handleCreateManagementHandoff(w http.ResponseWriter, r *http.Request) {
	user := auth.GetCurrentUser(r.Context())
	session := auth.GetCurrentSession(r.Context())
	if user == nil || session == nil {
		http.Error(w, "management account separation required", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderManagementSeparation(w, r, http.StatusBadRequest, views.ManagementHandoffFormData{}, nil, "Invalid invitation form data.")
		return
	}
	form := views.ManagementHandoffFormData{
		Name: r.PostFormValue("name"), Username: r.PostFormValue("username"), Email: r.PostFormValue("email"),
	}
	handoff, err := h.auth.CreateManagementHandoff(r.Context(), auth.CreateManagementHandoffOptions{
		ActorUserID: user.ID, ActorSessionID: session.ID,
		Name: form.Name, Username: form.Username, Email: form.Email,
	})
	if err != nil {
		var validationErr *auth.AdministratorUserInvitationValidationError
		switch {
		case errors.As(err, &validationErr):
			form.FieldErrors = validationErr.Fields
			h.renderManagementSeparation(w, r, http.StatusBadRequest, form, nil, "Correct the highlighted invitation details.")
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			h.renderManagementSeparation(w, r, http.StatusForbidden, form, nil, "Verify this session from Account security before creating the management invitation.")
		case errors.Is(err, auth.ErrManagementHandoffUnavailable):
			h.renderManagementSeparation(w, r, http.StatusConflict, form, nil, "A management handoff is already pending or this account no longer needs separation.")
		default:
			log.Printf("create management handoff: %v", err)
			h.renderManagementSeparation(w, r, http.StatusInternalServerError, form, nil, "Unable to create the management invitation right now.")
		}
		return
	}
	invitation := &views.ManagementHandoffInvitationData{
		Name: handoff.Name, Username: handoff.Target.Username, Email: handoff.Target.Email,
		RedemptionURL: strings.TrimRight(h.auth.Config().BaseURL, "/") + enrollmentRedemptionPath,
		Token:         handoff.Token.Token, ExpiresAt: handoff.Token.ExpiresAt,
	}
	h.renderManagementSeparation(w, r, http.StatusCreated, views.ManagementHandoffFormData{}, invitation, "")
}

func (h *Handler) renderManagementSeparation(w http.ResponseWriter, r *http.Request, status int, form views.ManagementHandoffFormData, invitation *views.ManagementHandoffInvitationData, pageError string) {
	user := auth.GetCurrentUser(r.Context())
	if user == nil {
		http.Error(w, "management account separation required", http.StatusForbidden)
		return
	}
	pending, err := h.auth.GetPendingManagementHandoff(r.Context(), user.ID)
	if err != nil {
		log.Printf("load pending management handoff: %v", err)
		http.Error(w, "failed to load management handoff", http.StatusInternalServerError)
		return
	}
	data := views.ManagementHandoffPageData{
		Form: form, Invitation: invitation, Error: pageError,
		CSRFToken: auth.CSRFToken(r.Context(), http.MethodPost, managementHandoffInvitationPath),
	}
	if pending != nil {
		data.Pending = &views.ManagementHandoffInvitationData{
			Name: pending.Name, Username: pending.Target.Username, Email: pending.Target.Email,
		}
	}
	var page bytes.Buffer
	if err := views.ManagementHandoffPage(data).Render(r.Context(), &page); err != nil {
		log.Printf("render management handoff: %v", err)
		http.Error(w, "failed to render management handoff", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = page.WriteTo(w)
}

func (h *Handler) handleManagementAccountSecurity(w http.ResponseWriter, r *http.Request) {
	h.renderPasswordSecurityTab(w, r, http.StatusOK, nil)
}

func (h *Handler) handleActivateManagementHandoff(w http.ResponseWriter, r *http.Request) {
	user := auth.GetCurrentUser(r.Context())
	if user == nil || !user.IsManagement() || user.IsAdmin {
		http.Error(w, "pending management enrollment required", http.StatusForbidden)
		return
	}
	session, err := h.auth.CompleteManagementHandoff(r.Context(), auth.GetSessionToken(r), r.UserAgent())
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrManagementEnrollmentIncomplete), errors.Is(err, auth.ErrRecentStepUpRequired):
			http.Redirect(w, r, "/admin/account/security?activation_incomplete=1", http.StatusSeeOther)
		case errors.Is(err, auth.ErrManagementHandoffUnavailable):
			http.Error(w, "management handoff is no longer available", http.StatusConflict)
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		default:
			log.Printf("complete management handoff: %v", err)
			http.Error(w, "unable to complete management handoff", http.StatusInternalServerError)
		}
		return
	}
	auth.SetSessionCookie(w, session.Token, h.auth.Config().SecureCookies)
	auth.ClearPreAuthCookie(w, h.auth.Config().SecureCookies)
	auth.ClearReturnToCookie(w, h.auth.Config().SecureCookies)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}
