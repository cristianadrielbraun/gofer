package handler

import (
	"errors"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/branding"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

const adminBrandingPath = "/admin/branding"

// maxBrandingUploadBytes bounds the whole form
const maxBrandingUploadBytes = 2*branding.MaxImageBytes + 64<<10

func (h *Handler) handleAdminBranding(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	verification := views.AdminSecurityVerificationData{}
	stepUpRequired := false
	if h.auth != nil && h.auth.Config().Enabled {
		access, err := h.auth.GetSecuritySettingsAccess(ctx, auth.GetSessionToken(r))
		switch {
		case err == nil:
			stepUpRequired = !access.StepUpFresh
			if stepUpRequired {
				verification = adminSecurityVerificationViewData(ctx, access, adminBrandingPath, r.URL.Query().Get("verification_required") == "1")
			}
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		default:
			log.Printf("admin branding: verify session: %v", err)
			http.Error(w, i18n.T(ctx, "server.error.failed_to_verify_admin_session"), http.StatusInternalServerError)
			return
		}
	}
	current := branding.Current()
	data := views.AdminBrandingData{
		Name:           current.Name,
		HasLogo:        !current.Logo.Empty(),
		HasFavicon:     !current.Favicon.Empty(),
		CSRFToken:      auth.CSRFToken(ctx, http.MethodPost, adminBrandingPath),
		Notice:         strings.TrimSpace(r.URL.Query().Get("notice")),
		Error:          strings.TrimSpace(r.URL.Query().Get("error")),
		StepUpRequired: stepUpRequired,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Header.Get("HX-Request") == "true" {
		_ = views.AdminBrandingPartial(data, verification).Render(ctx, w)
		return
	}
	uiSettings := h.db.GetUISettings(ctx, h.userID(ctx))
	_ = views.ManagementAdminBrandingLayout(uiSettings, data, verification).Render(ctx, w)
}

func (h *Handler) handleSaveAdminBranding(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if h.auth != nil && h.auth.Config().Enabled {
		err := h.auth.RequireRecentSecurityStepUp(ctx, auth.GetSessionToken(r))
		switch {
		case err == nil:
		case errors.Is(err, auth.ErrRecentStepUpRequired):
			redirectAdminBranding(w, r, "", i18n.T(ctx, "admin.branding.verify"))
			return
		case errors.Is(err, auth.ErrSecuritySessionInvalid):
			auth.ClearSessionCookie(w, h.auth.Config().SecureCookies)
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		default:
			log.Printf("admin branding: verify sensitive action: %v", err)
			http.Error(w, i18n.T(ctx, "server.error.failed_to_verify_admin_session"), http.StatusInternalServerError)
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBrandingUploadBytes)
	if err := r.ParseMultipartForm(maxBrandingUploadBytes); err != nil {
		redirectAdminBranding(w, r, "", i18n.T(ctx, "admin.branding.too_large"))
		return
	}
	name, err := branding.ValidateName(r.FormValue("name"))
	if err != nil {
		redirectAdminBranding(w, r, "", i18n.T(ctx, "admin.branding.name_too_long", "count", branding.MaxNameLength))
		return
	}
	next := branding.Current()
	next.Name = name
	for _, field := range []struct {
		name  string
		image *branding.Image
	}{{"logo", &next.Logo}, {"favicon", &next.Favicon}} {
		if r.FormValue("reset_"+field.name) == "yes" {
			*field.image = branding.Image{}
			continue
		}
		upload, err := brandingUpload(r.MultipartForm, field.name)
		if err != nil {
			message := i18n.T(ctx, "admin.branding.image_invalid")
			if errors.Is(err, branding.ErrImageTooLarge) {
				message = i18n.T(ctx, "admin.branding.too_large")
			}
			redirectAdminBranding(w, r, "", message)
			return
		}
		if !upload.Empty() {
			*field.image = upload
		}
	}
	user := ""
	if current := auth.GetCurrentUser(ctx); current != nil {
		user = current.ID
	}
	if err := h.db.SaveBranding(ctx, storage.Branding{
		Name:        next.Name,
		Logo:        next.Logo.Data,
		LogoType:    next.Logo.ContentType,
		Favicon:     next.Favicon.Data,
		FaviconType: next.Favicon.ContentType,
		UpdatedBy:   user,
	}); err != nil {
		log.Printf("admin branding: save: %v", err)
		redirectAdminBranding(w, r, "", i18n.T(ctx, "admin.branding.save_failed"))
		return
	}
	branding.Set(next)
	log.Printf("admin branding: updated by %q", user)
	redirectAdminBranding(w, r, i18n.T(ctx, "admin.branding.saved"), "")
}

// brandingUpload reads one uploaded image
func brandingUpload(form *multipart.Form, field string) (branding.Image, error) {
	if form == nil || len(form.File[field]) == 0 {
		return branding.Image{}, nil
	}
	header := form.File[field][0]
	if header.Size == 0 {
		return branding.Image{}, nil
	}
	if header.Size > branding.MaxImageBytes {
		return branding.Image{}, branding.ErrImageTooLarge
	}
	file, err := header.Open()
	if err != nil {
		return branding.Image{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, branding.MaxImageBytes+1))
	if err != nil {
		return branding.Image{}, err
	}
	contentType, err := branding.ValidateImage(data)
	if err != nil {
		return branding.Image{}, err
	}
	return branding.Image{Data: data, ContentType: contentType}, nil
}

func redirectAdminBranding(w http.ResponseWriter, r *http.Request, notice, message string) {
	values := url.Values{}
	if notice != "" {
		values.Set("notice", notice)
	}
	if message != "" {
		values.Set("error", message)
	}
	target := adminBrandingPath
	if query := values.Encode(); query != "" {
		target += "?" + query
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}
