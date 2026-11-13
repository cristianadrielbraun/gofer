package admin

import "github.com/cristianadrielbraun/gofer/internal/auth"

// UserActions are the changes an administrator may start on one person.
// The changes check again when they're made; these decide what to offer.
type UserActions struct {
	// SetStatus enables or disables them.
	SetStatus bool
	// IssueCredentialReset gives them a password-reset token; for another
	// administrator, a reset of their whole sign-in.
	IssueCredentialReset bool
	// RequirePasswordChange makes them choose a new password.
	RequirePasswordChange bool
	// Delete removes them and their data, or resumes a removal that
	// stopped.
	Delete bool
	// SetMFARequirement requires two-factor authentication of them, or
	// stops requiring it.
	SetMFARequirement bool
	// ManageInvitation revokes or replaces their invitation.
	ManageInvitation bool
	// SignOut ends every session they have.
	SignOut bool
}

// ActionsFor says which changes the administrator actorUserID may start on
// user, under the instance's two-factor policy.
func ActionsFor(actorUserID string, user auth.AdministratorUserSummary, policy auth.InstanceMFAPolicy) UserActions {
	self := user.ID == actorUserID
	webmail := user.UserType == auth.UserTypeWebmail && !user.IsAdmin
	administrator := user.UserType == auth.UserTypeManagement && user.IsAdmin
	settled := user.Status == auth.UserStatusActive || user.Status == auth.UserStatusDisabled
	actions := UserActions{
		SetStatus:            settled && !self && !user.DeletionPending,
		IssueCredentialReset: (webmail || administrator && !self) && settled && !user.DeletionPending,
		Delete:               webmail && !self && !user.DeletionProtected && user.Status == auth.UserStatusDisabled,
		ManageInvitation:     user.InvitationActionReference != "",
		SignOut:              user.Status == auth.UserStatusActive && !self && !user.DeletionPending,
	}
	actions.RequirePasswordChange = webmail && actions.IssueCredentialReset && user.HasPassword
	switch {
	case user.UserType == auth.UserTypeManagement, user.DeletionPending:
		// Administrators always use 2FA; someone being deleted is past
		// changing.
	case user.MFARequired:
		actions.SetMFARequirement = true
	case policy.RequiresAllUsers():
		// Everyone has to; there's nothing to require one by one.
	default:
		actions.SetMFARequirement = true
	}
	return actions
}
