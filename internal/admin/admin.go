// Package admin is the one way administrative work gets done. The admin
// console calls it today; the admin API and the command line will. Each
// change takes the Actor asking for it; auth checks the permission and
// records the change in the change's own transaction; once the change has
// committed, the rest of Gofer hears about it through Hooks. Errors come
// back with a Code, so every caller can answer them its own way without
// knowing auth's errors.
package admin

import (
	"context"
	"errors"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/authz"
)

// Actor is the administrator asking for a change, and the session they
// ask from: permissions that need a recent verification look at it.
type Actor struct {
	UserID    string
	SessionID string
}

// ActorFromContext is the signed-in administrator making a request.
func ActorFromContext(ctx context.Context) (Actor, bool) {
	user, session := auth.GetCurrentUser(ctx), auth.GetCurrentSession(ctx)
	if user == nil || session == nil {
		return Actor{}, false
	}
	return Actor{UserID: user.ID, SessionID: session.ID}, true
}

// Code says why a change didn't go through, in words no language owns.
type Code string

const (
	// CodeForbidden: the actor may not make this change.
	CodeForbidden Code = "forbidden"
	// CodeVerificationRequired: the session must verify again first.
	CodeVerificationRequired Code = "verification_required"
	// CodeInvalid: the request itself is wrong, such as a bad field.
	CodeInvalid Code = "invalid"
	// CodeConfirmation: a typed confirmation didn't match.
	CodeConfirmation Code = "confirmation"
	// CodeUnavailable: the person or invitation can't take this change
	// now: gone, of the wrong kind, or in the wrong state.
	CodeUnavailable Code = "unavailable"
	// CodeInvitationInactive: the invitation was used, revoked or expired.
	CodeInvitationInactive Code = "invitation_inactive"
	// CodeLastAdministrator: the change would leave no active
	// administrator.
	CodeLastAdministrator Code = "last_administrator"
	// CodeNeedsAuthenticator: the change needs an authenticator app set
	// up first.
	CodeNeedsAuthenticator Code = "needs_authenticator"
	// CodeTooMany: a bulk change names more people than it takes at once.
	CodeTooMany Code = "too_many"
	// CodeInternal: something broke; the error says what.
	CodeInternal Code = "internal"
)

// CodeOf says why err stopped a change.
func CodeOf(err error) Code {
	var validation *auth.AdministratorUserInvitationValidationError
	switch {
	case err == nil:
		return ""
	case errors.Is(err, authz.ErrForbidden):
		return CodeForbidden
	case errors.Is(err, auth.ErrRecentStepUpRequired):
		return CodeVerificationRequired
	case errors.As(err, &validation), errors.Is(err, auth.ErrAdministratorUserStatusInvalid),
		errors.Is(err, errBulkActionInvalid), errors.Is(err, errBulkNobody):
		return CodeInvalid
	case errors.Is(err, errBulkTooMany):
		return CodeTooMany
	case errors.Is(err, auth.ErrAdministratorUserDeletionConfirmationInvalid):
		return CodeConfirmation
	case errors.Is(err, auth.ErrAdministratorUserInvitationNotActive):
		return CodeInvitationInactive
	case errors.Is(err, auth.ErrLastActiveAdmin):
		return CodeLastAdministrator
	case errors.Is(err, auth.ErrInstanceMFAEnrollmentNeeded):
		return CodeNeedsAuthenticator
	case errors.Is(err, auth.ErrAdministratorUserInvitationTargetInvalid),
		errors.Is(err, auth.ErrAdministratorUserMFATargetInvalid),
		errors.Is(err, auth.ErrAdministratorUserStatusTargetInvalid),
		errors.Is(err, auth.ErrEnrollmentTokenTargetInvalid),
		errors.Is(err, auth.ErrAdministratorUserDeletionTargetInvalid),
		errors.Is(err, auth.ErrAdministratorUserSignOutTargetInvalid),
		errors.Is(err, auth.ErrAdministratorUserSessionGone),
		errors.Is(err, errCredentialResetTarget):
		return CodeUnavailable
	}
	return CodeInternal
}

// HookError is a change that committed, followed by something the rest of
// Gofer failed to do about it. The change stands; the caller says so.
type HookError struct {
	Err error
}

func (err *HookError) Error() string { return "after the change: " + err.Err.Error() }
func (err *HookError) Unwrap() error { return err.Err }
