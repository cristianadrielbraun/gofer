package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

// Hooks are what the rest of Gofer does once a change has committed.
type Hooks interface {
	// UserStatusChanged runs after someone is enabled or disabled, even
	// when they already were, so an earlier run that failed is repaired.
	UserStatusChanged(ctx context.Context, userID string, status auth.UserStatus) error
}

// Service makes administrative changes to people.
type Service struct {
	auth  *auth.Manager
	hooks Hooks
}

// NewService returns the service that changes people through manager,
// telling hooks (which may be nil) what changed.
func NewService(manager *auth.Manager, hooks Hooks) *Service {
	return &Service{auth: manager, hooks: hooks}
}

var errCredentialResetTarget = errors.New("only an active or disabled webmail user, or another administrator, can get a reset token")

// InviteUser creates a pending person and the invitation they join with.
func (s *Service) InviteUser(ctx context.Context, actor Actor, name, username string) (*auth.AdministratorUserInvitation, error) {
	return s.auth.CreateAdministratorUserInvitation(ctx, auth.CreateAdministratorUserInvitationOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, Name: name, Username: username,
	})
}

// InviteAdministrator creates a pending administrator and the invitation
// they join with on the administrator sign-up.
func (s *Service) InviteAdministrator(ctx context.Context, actor Actor, name, username string) (*auth.AdministratorUserInvitation, error) {
	return s.auth.CreateAdministratorInvitation(ctx, auth.CreateAdministratorUserInvitationOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, Name: name, Username: username,
	})
}

// RevokeInvitation withdraws the invitation that reference names.
func (s *Service) RevokeInvitation(ctx context.Context, actor Actor, reference string) error {
	return s.auth.RevokeAdministratorUserInvitation(ctx, actor.UserID, actor.SessionID, reference)
}

// RotateInvitation replaces the invitation that reference names with a
// new one.
func (s *Service) RotateInvitation(ctx context.Context, actor Actor, reference string) (*auth.AdministratorUserInvitation, error) {
	return s.auth.RotateAdministratorUserInvitation(ctx, auth.RotateAdministratorUserInvitationOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, ActionReference: reference,
	})
}

// SetMFARequirement requires two-factor authentication of one person, or
// stops requiring it. With passkeyCounts, a passkey is enough on its own.
func (s *Service) SetMFARequirement(ctx context.Context, actor Actor, userID string, required, passkeyCounts bool) (*auth.SetAdministratorUserMFAPolicyResult, error) {
	return s.auth.SetAdministratorUserMFAPolicy(ctx, auth.SetAdministratorUserMFAPolicyOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, TargetUserID: userID, Required: required, PasskeyCounts: passkeyCounts,
	})
}

// SetStatus enables or disables someone, then lets the rest of Gofer catch
// up: stop their mail, or start it again. If that fails, the result comes
// back with a *HookError.
func (s *Service) SetStatus(ctx context.Context, actor Actor, userID string, status auth.UserStatus) (*auth.SetAdministratorUserStatusResult, error) {
	result, err := s.auth.SetAdministratorUserStatus(ctx, auth.SetAdministratorUserStatusOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, TargetUserID: userID, Status: status,
	})
	if err != nil {
		return nil, err
	}
	if s.hooks != nil {
		if err := s.hooks.UserStatusChanged(ctx, userID, result.Status); err != nil {
			return result, &HookError{Err: err}
		}
	}
	return result, nil
}

// IssueCredentialReset gives a webmail user a password-reset token, or
// another administrator a sign-in reset, which changes nothing until they
// use it.
func (s *Service) IssueCredentialReset(ctx context.Context, actor Actor, userID string) (*auth.EnrollmentToken, *auth.User, error) {
	target, err := s.auth.GetUserByID(ctx, userID)
	if err != nil {
		return nil, nil, fmt.Errorf("load the person to reset: %w", err)
	}
	if target != nil && target.UserType == auth.UserTypeManagement {
		token, err := s.auth.ResetAdministratorSignIn(ctx, auth.ResetAdministratorSignInOptions{
			ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, TargetUserID: target.ID,
		})
		if err != nil {
			return nil, nil, err
		}
		return token, target, nil
	}
	if target == nil || target.UserType != auth.UserTypeWebmail || target.IsAdmin ||
		(target.Status != auth.UserStatusActive && target.Status != auth.UserStatusDisabled) {
		return nil, nil, errCredentialResetTarget
	}
	token, err := s.auth.IssueEnrollmentToken(ctx, auth.IssueEnrollmentTokenOptions{
		UserID: target.ID, CreatedBy: actor.UserID, ActorSessionID: actor.SessionID,
		Purpose: auth.EnrollmentTokenPurposeCredentialReset,
	})
	if err != nil {
		return nil, nil, err
	}
	return token, target, nil
}

// RequirePasswordChange makes a webmail user choose a new password at
// their next sign-in.
func (s *Service) RequirePasswordChange(ctx context.Context, actor Actor, userID string) error {
	return s.auth.RequirePasswordChange(ctx, auth.RequirePasswordChangeOptions{
		UserID: userID, CreatedBy: actor.UserID, ActorSessionID: actor.SessionID,
	})
}

// SignOut ends every session someone else has. They can sign in again
// straight away: nothing else about them changes.
func (s *Service) SignOut(ctx context.Context, actor Actor, userID string) (*auth.SignOutAdministratorUserResult, error) {
	return s.auth.SignOutAdministratorUser(ctx, auth.SignOutAdministratorUserOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, TargetUserID: userID,
	})
}

// SignOutSession ends one of someone else's sessions, named by the
// reference the actor's session was given for it.
func (s *Service) SignOutSession(ctx context.Context, actor Actor, userID, reference string) (*auth.SignOutAdministratorUserSessionResult, error) {
	return s.auth.SignOutAdministratorUserSession(ctx, auth.SignOutAdministratorUserSessionOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, TargetUserID: userID, Reference: reference,
	})
}

// PrepareDeletion marks a disabled person for deletion. Removing their
// data is the caller's to start, since it runs beside the mail it stops.
func (s *Service) PrepareDeletion(ctx context.Context, actor Actor, userID, confirmation string) (*auth.PrepareAdministratorUserDeletionResult, error) {
	return s.auth.PrepareAdministratorUserDeletion(ctx, auth.PrepareAdministratorUserDeletionOptions{
		ActorUserID: actor.UserID, ActorSessionID: actor.SessionID, TargetUserID: userID, Confirmation: confirmation,
	})
}
