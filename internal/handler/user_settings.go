package handler

import (
	"context"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// Security actions always use the central authentication manager. Sharing their
// registrations keeps the same factor, session and CSRF behavior in both layouts.
func (h *Handler) registerSecuritySettingsRoutes(register func(string, http.HandlerFunc)) {
	register("GET "+securityActivityPath, h.handleSecurityActivityPage)
	register("GET /settings/security/sessions/history", h.handleSecuritySessionHistory)
	register("POST /settings/security/password", h.handleChangePassword)
	register("POST "+securityStepUpPath, h.handleSecurityStepUp)
	register("POST "+securityTOTPStartPath, h.handleSecurityTOTPStart)
	register("POST "+securityTOTPConfirmPath, h.handleSecurityTOTPConfirm)
	register("POST "+securityTOTPDisablePath, h.handleSecurityTOTPDisable)
	register("POST "+securityRecoveryStartPath, h.handleSecurityRecoveryStart)
	register("POST "+securityRecoveryCompletePath, h.handleSecurityRecoveryComplete)
	register("POST "+securityRecoveryRevokePath, h.handleSecurityRecoveryRevoke)
	register("POST "+securityManagementCancelPath, h.handleSecurityManagementCancel)
	register("POST "+securityPasskeyStartPath, h.handleSecurityPasskeyStart)
	register("POST "+securityPasskeyFinishPath, h.handleSecurityPasskeyFinish)
	register("POST "+securityPasskeyStepUpStartPath, h.handleSecurityPasskeyStepUpStart)
	register("POST "+securityPasskeyStepUpFinishPath, h.handleSecurityPasskeyStepUpFinish)
	register("POST "+securityGoogleIdentityLinkPath, h.handleSecurityGoogleIdentityLink)
	register("POST /settings/security/identities/google/{id}/unlink", h.handleSecurityGoogleIdentityUnlink)
	register("POST "+securityMicrosoftIdentityLinkPath, h.handleSecurityMicrosoftIdentityLink)
	register("POST /settings/security/identities/microsoft/{id}/unlink", h.handleSecurityMicrosoftIdentityUnlink)
	register("POST "+securityOIDCIdentityLinkPath, h.handleSecurityOIDCIdentityLink)
	register("POST /settings/security/identities/oidc/{id}/unlink", h.handleSecurityOIDCIdentityUnlink)
	register("POST "+securitySessionRevokeOthersPath, h.handleSecuritySessionRevokeOthers)
	register("POST /settings/security/sessions/{reference}/revoke", h.handleSecuritySessionRevoke)
	register("POST /settings/security/passkeys/{id}/remove", h.handleSecurityPasskeyRemove)
}

// Copy display preferences without retaining a user-store lease across central
// authentication work or rendering a response. Credentials remain central.
func (h *Handler) securityUISettings(ctx context.Context, owner string) (map[string]string, error) {
	var settings map[string]string
	err := h.withUserDB(ctx, owner, func(db *storage.DB) error {
		settings = db.GetUISettings(ctx, owner)
		return nil
	})
	return settings, err
}
