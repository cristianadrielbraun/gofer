package handler

import (
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

// RegisterAuthenticationRoutes mounts installation-wide assets and application
// authentication on the central handler. Mailbox OAuth callbacks and private
// security settings belong to the mailbox router. Administration is registered
// separately because its diagnostics and lifecycle actions depend on storage mode.
func (h *Handler) RegisterAuthenticationRoutes(mux *http.ServeMux) {
	setupAssetsRoutes(mux)

	mux.HandleFunc("GET /login", h.handleLogin)
	mux.HandleFunc("POST /login", h.handleLoginSubmit)
	mux.HandleFunc("GET /admin/login", h.handleAdminLogin)
	mux.HandleFunc("POST /admin/login", h.handleAdminLoginSubmit)
	mux.HandleFunc("POST "+loginPasskeyStartPath, h.handleLoginPasskeyStart)
	mux.HandleFunc("POST "+loginPasskeyFinishPath, h.handleLoginPasskeyFinish)
	mux.HandleFunc("GET /login/mfa", h.handleLoginMFA)
	mux.HandleFunc("POST /login/mfa", h.handleLoginMFASubmit)
	mux.HandleFunc("GET /login/mfa/enroll", h.handleMFAEnrollment)
	mux.HandleFunc("POST /login/mfa/enroll", h.handleMFAEnrollmentSubmit)
	mux.HandleFunc("GET /login/mfa/enroll/codes", h.handleMFAEnrollmentCodes)
	mux.HandleFunc("POST /login/mfa/enroll/codes", h.handleMFAEnrollmentCodesSubmit)
	mux.HandleFunc("GET /login/mfa/recovery", h.handleRecoveryCodeLogin)
	mux.HandleFunc("POST /login/mfa/recovery", h.handleRecoveryCodeLoginSubmit)
	mux.HandleFunc("GET /login/recovery/mfa", h.handleRecoveryRepairMFA)
	mux.HandleFunc("POST /login/recovery/mfa", h.handleRecoveryRepairMFASubmit)
	mux.HandleFunc("GET /login/recovery/codes", h.handleRecoveryRepairCodes)
	mux.HandleFunc("POST /login/recovery/codes", h.handleRecoveryRepairCodesSubmit)
	mux.HandleFunc("GET /setup", h.handleSetup)
	mux.HandleFunc("POST /setup", h.handleSetupSubmit)
	mux.HandleFunc("GET /setup/owner", h.handleSetupOwner)
	mux.HandleFunc("POST /setup/owner", h.handleSetupOwnerSubmit)
	mux.HandleFunc("GET /setup/password", h.handleSetupPassword)
	mux.HandleFunc("POST /setup/password", h.handleSetupPasswordSubmit)
	mux.HandleFunc("GET /setup/mfa", h.handleSetupMFA)
	mux.HandleFunc("POST /setup/mfa", h.handleSetupMFASubmit)
	mux.HandleFunc("GET /setup/recovery", h.handleSetupRecovery)
	mux.HandleFunc("POST /setup/recovery", h.handleSetupRecoverySubmit)
	mux.HandleFunc("GET /setup/review", h.handleSetupReview)
	mux.HandleFunc("POST /setup/review", h.handleSetupReviewSubmit)
	mux.HandleFunc("GET /account/enroll", h.handleInvitationEnrollment)
	mux.HandleFunc("POST /account/enroll", h.handleInvitationEnrollmentSubmit)
	mux.HandleFunc("POST /account/enroll/google", h.handleInvitationGoogleEnrollment)
	mux.HandleFunc("GET /account/enroll/complete", h.handleInvitationEnrollmentComplete)
	mux.HandleFunc("GET /account/redeem", h.handleCredentialRedemption)
	mux.HandleFunc("POST /account/redeem", h.handleCredentialRedemptionSubmit)
	mux.HandleFunc("GET /account/redeem/complete", h.handleCredentialRedemptionComplete)
	mux.HandleFunc("GET /account/recover", h.handlePasswordResetRequest)
	mux.HandleFunc("POST /account/recover", h.handlePasswordResetRequestSubmit)
	mux.HandleFunc("GET /auth/google", h.handleGoogleRedirect)
	mux.HandleFunc("GET /auth/google/login/callback", h.handleGoogleCallback)
	mux.HandleFunc("GET /auth/microsoft", h.handleMicrosoftRedirect)
	mux.HandleFunc("GET /auth/microsoft/login/callback", h.handleMicrosoftCallback)
	mux.HandleFunc("GET /auth/oidc", h.handleOIDCRedirect)
	mux.HandleFunc("GET /auth/oidc/callback", h.handleOIDCCallback)
	mux.HandleFunc("POST /auth/logout", h.handleLogout)
	mux.HandleFunc("GET "+auth.RequiredPasswordChangePath, h.handleRequiredPasswordChange)
	mux.HandleFunc("POST "+auth.RequiredPasswordChangePath, h.handleRequiredPasswordChangeSubmit)

}

func (h *Handler) registerAdministrationRoutes(mux *http.ServeMux) {
	adminRoute := func(pattern string, handler http.HandlerFunc) {
		mux.Handle(pattern, h.adminOnly(handler))
	}
	mux.Handle("GET /admin/account/security", h.managementAccountOnly(http.HandlerFunc(h.handleManagementAccountSecurity)))
	adminRoute("GET /admin", h.handleAdminRedirect)
	adminRoute("GET /admin/{$}", h.handleAdminRedirect)
	adminRoute("GET /admin/avatars", h.handleAdminRedirect)
	adminRoute("GET /admin/avatars/{$}", h.handleAdmin)
	adminRoute("GET /admin/avatars/{tab}", h.handleAdmin)
	adminRoute("GET /admin/contacts", h.handleAdminContacts)
	adminRoute("GET /admin/contacts/{$}", h.handleAdminContacts)
	adminRoute("GET /admin/users", h.handleAdminUsers)
	adminRoute("GET /admin/users/{$}", h.handleAdminUsers)
	adminRoute("GET /admin/activity", h.handleAdminSecurityActivity)
	adminRoute("POST /admin/activity/retention", h.handleSetAuthenticationEventRetention)
	adminRoute("POST /admin/users/invitations", h.handleCreateAdminUserInvitation)
	adminRoute("POST /admin/users/invitations/{reference}/revoke", h.handleRevokeAdminUserInvitation)
	adminRoute("POST /admin/users/invitations/{reference}/rotate", h.handleRotateAdminUserInvitation)
	adminRoute("POST /admin/users/{userID}/mfa-policy", h.handleSetAdminUserMFAPolicy)
	adminRoute("POST /admin/users/{userID}/status", h.handleSetAdminUserStatus)
	adminRoute("POST /admin/users/{userID}/credential-reset", h.handleIssueAdminUserCredentialReset)
	adminRoute("POST /admin/users/{userID}/require-password-change", h.handleRequireUserPasswordChange)
	adminRoute("POST /admin/users/{userID}/delete", h.handleDeleteAdminUser)
	adminRoute("GET /admin/labels", h.handleAdminLabels)
	adminRoute("GET /admin/labels/{$}", h.handleAdminLabels)
	adminRoute("GET /admin/operations", h.handleAdminOperations)
	adminRoute("GET /admin/operations/{$}", h.handleAdminOperations)
	adminRoute("GET /admin/security", h.handleAdminSecurity)
	adminRoute("POST /admin/security/http-discovery", h.handleAddHTTPDiscoveryException)
	adminRoute("POST /admin/security/plaintext", h.handleAddPlaintextTransportException)
	adminRoute("POST /admin/security/private-target", h.handleAddPrivateTargetException)
	adminRoute("POST /admin/security/exceptions/{id}/delete", h.handleDeleteMailSecurityException)
	adminRoute("GET /api/admin/events", h.handleSSE)
	adminRoute("GET /api/admin/mail-operations/status", h.handleAdminMailOperationsStatus)
	adminRoute("GET /api/system/processing", h.handleProcessingStatus)
	adminRoute("GET /api/admin/avatars/status", h.handleAvatarStatus)
	adminRoute("GET /api/admin/contacts/status", h.handleContactAdminStatus)
	adminRoute("GET /api/admin/labels/status", h.handleLabelAdminStatus)
	adminRoute("GET /api/admin/avatars/attempts", h.handleAvatarAttempts)
	adminRoute("GET /api/admin/avatars/senders", h.handleAvatarSenders)
	adminRoute("GET /api/admin/avatars/{hash}", h.handleAdminAvatarImage)
	adminRoute("POST /api/admin/avatars/senders/{hash}/recheck", h.handleRecheckAvatarSender)
	adminRoute("POST /admin/avatar-backfill/recheck", h.handleForceAvatarBackfill)
	adminRoute("POST /admin/avatar-backfill/cancel", h.handleCancelAvatarBackfill)
	adminRoute("POST /admin/contacts/backfill", h.handleForceContactBackfill)
}

// RegisterAdministrationRoutes mounts the central control plane after owned
// mailbox registration. Lifecycle actions use that handler's deletion runtime;
// a partial router without the runtime refuses owned user deletion.
func (h *Handler) RegisterAdministrationRoutes(mux *http.ServeMux) {
	h.registerAdministrationRoutes(mux)
}
