package handler

import (
	"context"
	"log"
	"net/http"

	"github.com/cristianadrielbraun/gofer/internal/admin"
	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/i18n"
	"github.com/cristianadrielbraun/gofer/internal/views"
)

// adminService makes the console's changes to people; this handler's
// runtime follows them up.
func (h *Handler) adminService() *admin.Service {
	return admin.NewService(h.auth, adminRuntimeHooks{h})
}

// adminRuntimeHooks are what the running mail does after a change to
// someone commits.
type adminRuntimeHooks struct{ h *Handler }

func (hooks adminRuntimeHooks) UserStatusChanged(ctx context.Context, userID string, status auth.UserStatus) error {
	return hooks.h.refreshOwnedUserStatus(ctx, userID, status)
}

// adminActor is the signed-in administrator asking for a change. Without
// managed sign-in, or without one, the change is refused.
func (h *Handler) adminActor(w http.ResponseWriter, r *http.Request) (admin.Actor, bool) {
	actor, ok := admin.ActorFromContext(r.Context())
	if !ok || h.auth == nil || !h.auth.IsEnabled() {
		http.Error(w, i18n.T(r.Context(), "server.error.admin_access_required"), http.StatusForbidden)
		return admin.Actor{}, false
	}
	return actor, true
}

// adminUserChange is how the Users page answers one kind of change that
// didn't go through: what to ask when the session must verify again, what
// to say when the person can't take the change, and what to say when it
// broke. The keys are message keys.
type adminUserChange struct {
	name    string
	verify  string
	refused map[admin.Code]string
	failed  string
}

var (
	adminUserInviteChange = adminUserChange{
		name: "create administrator user invitation", verify: "auth.admin.verify.create_invitation",
		failed: "auth.admin.users.invitation_create_failed",
	}
	adminUserRevokeChange = adminUserChange{
		name: "revoke administrator user invitation", verify: "auth.admin.verify.revoke_invitation",
		refused: map[admin.Code]string{
			admin.CodeUnavailable:        "auth.admin.users.invitation_inactive",
			admin.CodeInvitationInactive: "auth.admin.users.invitation_inactive",
		},
		failed: "auth.admin.users.invitation_revoke_failed",
	}
	adminUserRotateChange = adminUserChange{
		name: "rotate administrator user invitation", verify: "auth.admin.verify.replace_invitation",
		refused: map[admin.Code]string{admin.CodeUnavailable: "auth.admin.users.invitation_target_gone"},
		failed:  "auth.admin.users.invitation_replace_failed",
	}
	adminUserMFAChange = adminUserChange{
		name: "set administrator user MFA policy", verify: "auth.admin.verify.mfa_requirement",
		refused: map[admin.Code]string{admin.CodeUnavailable: "auth.admin.users.user_gone"},
		failed:  "auth.admin.users.mfa_change_failed",
	}
	adminUserStatusChange = adminUserChange{
		name: "set administrator user status", verify: "auth.admin.verify.user_access",
		refused: map[admin.Code]string{
			admin.CodeLastAdministrator:  "auth.admin.users.last_admin",
			admin.CodeNeedsAuthenticator: "auth.admin.users.needs_totp",
			admin.CodeInvalid:            "auth.admin.users.user_unavailable",
			admin.CodeUnavailable:        "auth.admin.users.user_unavailable",
		},
		failed: "auth.admin.users.access_change_failed",
	}
	adminUserResetChange = adminUserChange{
		name: "issue administrator user credential reset", verify: "auth.admin.verify.reset_token",
		refused: map[admin.Code]string{admin.CodeUnavailable: "auth.admin.users.reset_user_gone"},
		failed:  "auth.admin.users.reset_failed",
	}
	adminUserRequiredChange = adminUserChange{
		name: "require webmail password change", verify: "auth.admin.verify.require_password",
		failed: "auth.admin.require_password_unavailable",
	}
	adminUserDeleteChange = adminUserChange{
		name: "prepare administrator user deletion", verify: "auth.admin.verify.delete_user",
		refused: map[admin.Code]string{
			admin.CodeConfirmation: "auth.admin.users.delete_confirm",
			admin.CodeUnavailable:  "auth.admin.users.delete_only_disabled",
		},
		failed: "auth.admin.users.delete_failed",
	}
)

// adminUserChangeFailed answers a change to someone that didn't go
// through, on the Users page with form as it was sent.
func (h *Handler) adminUserChangeFailed(w http.ResponseWriter, r *http.Request, err error, change adminUserChange, form views.AdminUserInvitationFormData) {
	ctx := r.Context()
	switch code := admin.CodeOf(err); code {
	case admin.CodeForbidden:
		http.Error(w, i18n.T(ctx, "server.error.admin_access_required"), http.StatusForbidden)
	case admin.CodeVerificationRequired:
		h.renderAdminUsers(w, r, http.StatusForbidden, form, nil, i18n.T(ctx, change.verify))
	default:
		if key, ok := change.refused[code]; ok {
			h.renderAdminUsers(w, r, http.StatusBadRequest, form, nil, i18n.T(ctx, key))
			return
		}
		log.Printf("%s: %v", change.name, err)
		h.renderAdminUsers(w, r, http.StatusInternalServerError, form, nil, i18n.T(ctx, change.failed))
	}
}
