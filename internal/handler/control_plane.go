package handler

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/admin"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// RegisterAuthenticationRoutes mounts installation-wide assets and application
// authentication on the central handler. Mailbox OAuth callbacks and private
// security settings belong to the mailbox router. Administration is registered
// separately because its diagnostics and lifecycle actions depend on storage mode.
func (h *Handler) RegisterAuthenticationRoutes(mux *http.ServeMux) {
	setupAssetsRoutes(mux)

	mux.HandleFunc("GET /login", h.handleLogin)
	mux.HandleFunc("POST /login", h.handleLoginSubmit)
	mux.HandleFunc("POST /login/methods", h.handleLoginMethods)
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
		mux.Handle(pattern, h.consoleMovedToSettings(h.adminOnly(withAuditClient(h.recordDeniedAttempts(handler)))))
	}
	mux.Handle("GET "+auth.ManagementAccountPath, h.managementAccountOnly(http.HandlerFunc(h.handleManagementAccountSecurity)))
	mux.Handle("GET /admin/account/security", h.managementAccountOnly(redirectAdminPage(auth.ManagementAccountPath)))
	adminRoute("GET /admin", h.handleAdminRedirect)
	adminRoute("GET /admin/{$}", h.handleAdminRedirect)
	// System has only its diagnostics until its health page arrives.
	for _, path := range []string{views.AdminSystemPath, views.AdminDiagnosticsPath} {
		adminRoute("GET "+path, redirectAdminPage(views.AdminAvatarsPath))
		adminRoute("GET "+path+"/{$}", redirectAdminPage(views.AdminAvatarsPath))
	}
	adminRoute("GET "+strings.TrimSuffix(views.AdminAvatarsPath, "/"), redirectAdminPage(views.AdminAvatarsPath))
	adminRoute("GET "+views.AdminAvatarsPath+"{$}", h.handleAdmin)
	adminRoute("GET "+views.AdminAvatarsPath+"{tab}", h.handleAdmin)
	adminRoute("GET "+views.AdminContactsPath, h.handleAdminContacts)
	adminRoute("GET "+views.AdminContactsPath+"/{$}", h.handleAdminContacts)
	adminRoute("GET "+views.AdminLabelsPath, h.handleAdminLabels)
	adminRoute("GET "+views.AdminLabelsPath+"/{$}", h.handleAdminLabels)
	adminRoute("GET "+views.AdminOperationsPath, h.handleAdminOperations)
	adminRoute("GET "+views.AdminOperationsPath+"/{$}", h.handleAdminOperations)
	h.registerOldAdminAddresses(adminRoute)
	adminRoute("GET /admin/users", h.handleAdminUsers)
	adminRoute("GET "+views.AdminOverviewPath, h.handleAdminOverview)
	adminRoute("GET /admin/users/{$}", h.handleAdminUsers)
	adminRoute("GET "+views.AdminUsersExportPath, h.handleExportAdminUsers)
	adminRoute("GET /admin/users/{userID}", h.handleAdminUserPage(views.AdminUserTabOverview))
	adminRoute("GET /admin/users/{userID}/"+views.AdminUserTabSignIn, h.handleAdminUserPage(views.AdminUserTabSignIn))
	adminRoute("GET /admin/activity", h.handleAdminSecurityActivity)
	adminRoute("GET "+views.AdminAuditActivityPath, h.handleAdminAuditActivity)
	adminRoute("GET "+views.AdminAuditActivityPath+"/export", h.handleExportAdminAudit)
	adminRoute("POST "+views.AdminAuditActivityPath+"/retention", h.handleSetAdminAuditRetention)
	adminRoute("POST /admin/activity/retention", h.handleSetAuthenticationEventRetention)
	for _, route := range adminUserActionDialogRoutes {
		adminRoute("GET "+route.pattern, h.handleAdminUserActionDialog(route.action))
	}
	adminRoute("POST /admin/users/invitations", h.handleCreateAdminUserInvitation)
	adminRoute("POST /admin/users/invitations/{reference}/revoke", h.handleRevokeAdminUserInvitation)
	adminRoute("POST /admin/users/invitations/{reference}/rotate", h.handleRotateAdminUserInvitation)
	adminRoute("POST /admin/users/{userID}/mfa-policy", h.handleSetAdminUserMFAPolicy)
	adminRoute("POST /admin/users/{userID}/status", h.handleSetAdminUserStatus)
	adminRoute("POST /admin/users/{userID}/credential-reset", h.handleIssueAdminUserCredentialReset)
	adminRoute("POST /admin/users/{userID}/require-password-change", h.handleRequireUserPasswordChange)
	adminRoute("POST /admin/users/{userID}/delete", h.handleDeleteAdminUser)
	adminRoute("POST /admin/users/{userID}/sign-out", h.handleSignOutAdminUser)
	// Each change to many people has an address of its own: a pattern with
	// the change as a wildcard would overlap /admin/users/{userID}/….
	for _, action := range admin.BulkActions {
		adminRoute("GET "+adminUsersBulkPath(action), h.handleAdminUsersBulkDialog(action))
		adminRoute("POST "+adminUsersBulkPath(action), h.handleAdminUsersBulk(action))
	}
	adminRoute("GET /admin/security", h.handleAdminSecurity)
	adminRoute("POST /admin/security/http-discovery", h.handleAddHTTPDiscoveryException)
	adminRoute("POST /admin/security/plaintext", h.handleAddPlaintextTransportException)
	adminRoute("POST /admin/security/private-target", h.handleAddPrivateTargetException)
	adminRoute("POST /admin/security/exceptions/{id}/delete", h.handleDeleteMailSecurityException)
	adminRoute("POST /admin/security/proxies", h.handleSaveOutboundProxy)
	adminRoute("POST /admin/security/proxies/{id}/delete", h.handleDeleteOutboundProxy)
	adminRoute("POST /admin/security/proxies/{id}/everyone", h.handleSetOutboundProxyEveryone)
	adminRoute("POST /admin/security/proxies/{id}/people", h.handleSetOutboundProxyPerson)
	adminRoute("POST /admin/security/proxies/policy", h.handleSetOwnOutboundProxiesPolicy)
	h.registerAdminMailAllowlistRoutes(adminRoute)
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

// registerOldAdminAddresses keeps the addresses the admin pages had before
// they moved, so bookmarks and links still arrive. Each sends its query
// along: the chosen webmail user, and notices after a security change.
func (h *Handler) registerOldAdminAddresses(adminRoute func(string, http.HandlerFunc)) {
	adminRoute("GET /admin/avatars", redirectAdminPage(views.AdminAvatarsPath))
	adminRoute("GET /admin/avatars/{$}", redirectAdminPage(views.AdminAvatarsPath))
	adminRoute("GET /admin/avatars/{tab}", func(w http.ResponseWriter, r *http.Request) {
		redirectAdminPage(views.AdminAvatarsPath+url.PathEscape(r.PathValue("tab")))(w, r)
	})
	for old, page := range map[string]string{
		"/admin/contacts":   views.AdminContactsPath,
		"/admin/labels":     views.AdminLabelsPath,
		"/admin/operations": views.AdminOperationsPath,
	} {
		adminRoute("GET "+old, redirectAdminPage(page))
		adminRoute("GET "+old+"/{$}", redirectAdminPage(page))
	}
}

// redirectAdminPage sends a request on to target, keeping its query.
func redirectAdminPage(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		destination := target
		if r.URL.RawQuery != "" {
			destination += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, destination, http.StatusFound)
	}
}

// consoleMovedToSettings sends an open instance's owner, whose console
// moved to Settings, from a console page to the same thing there. The
// console's data and changes aren't theirs any more.
func (h *Handler) consoleMovedToSettings(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.auth == nil || h.auth.IsEnabled() {
			next.ServeHTTP(w, r)
			return
		}
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) || strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, ownerSettingsFor(r.URL.Path), http.StatusFound)
	})
}

// ownerSettingsFor is where a console page went in an owner's Settings.
func ownerSettingsFor(path string) string {
	under := func(prefixes ...string) bool {
		for _, prefix := range prefixes {
			if path == prefix || strings.HasPrefix(path, strings.TrimSuffix(prefix, "/")+"/") {
				return true
			}
		}
		return false
	}
	switch {
	case under(views.AdminContactsPath, "/admin/contacts"):
		return views.OwnerDiagnosticsPath + "/contacts"
	case under(views.AdminLabelsPath, "/admin/labels"):
		return views.OwnerDiagnosticsPath + "/labels"
	case under(views.AdminOperationsPath, "/admin/operations"):
		return "/settings/operations"
	case under("/admin/security", auth.ManagementAccountPath):
		return "/settings/security"
	case under(views.AdminAvatarsPath, "/admin/avatars"):
		tab := path
		for _, prefix := range []string{views.AdminAvatarsPath, "/admin/avatars/"} {
			tab = strings.TrimPrefix(tab, prefix)
		}
		if tab == path || strings.Contains(strings.Trim(tab, "/"), "/") {
			tab = ""
		}
		return views.OwnerAvatarDiagnosticsPath + url.PathEscape(strings.Trim(tab, "/"))
	}
	return views.OwnerAvatarDiagnosticsPath
}
