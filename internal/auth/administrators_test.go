package auth

import (
	"errors"
	"testing"
	"time"
)

// An administrator who hasn't set up an authenticator app can't sign in, so
// they don't keep the last one who can from being disabled.
func TestLastAdministratorCountsOnlyThoseWhoCanSignIn(t *testing.T) {
	now := time.Date(2026, time.November, 12, 16, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "newcomer", true, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	if err := manager.SetUserStatus(t.Context(), "administrator", UserStatusDisabled, "newcomer"); !errors.Is(err, ErrLastActiveAdmin) {
		t.Fatalf("disabling the only administrator with an authenticator app = %v", err)
	}
	insertPolicyTestTOTP(t, manager, "newcomer", now)
	if err := manager.SetUserStatus(t.Context(), "administrator", UserStatusDisabled, "newcomer"); err != nil {
		t.Fatalf("disabling one of two administrators who can sign in = %v", err)
	}
}

func TestPendingAdministratorsInvitationCanBeReplacedAndRevoked(t *testing.T) {
	now := time.Date(2026, time.November, 12, 16, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	invited, err := manager.CreateAdministratorInvitation(t.Context(), CreateAdministratorUserInvitationOptions{
		ActorUserID: "administrator", ActorSessionID: adminSession.ID, Name: "Rafe Ollin", Username: "rafe.ollin",
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := manager.ListAdministratorUserPage(t.Context(), "administrator", adminSession.ID, AdministratorUserQuery{Administrators: true})
	if err != nil || len(page.Users) != 2 {
		t.Fatalf("administrators = %+v, %v", page, err)
	}
	var reference string
	for _, person := range page.Users {
		if person.ID == invited.User.ID {
			reference = person.InvitationActionReference
			if person.InvitationState != AdministratorUserInvitationActive {
				t.Fatalf("pending administrator's invitation state = %q", person.InvitationState)
			}
		}
	}
	if reference == "" {
		t.Fatal("a pending administrator has no invitation actions")
	}
	rotated, err := manager.RotateAdministratorUserInvitation(t.Context(), RotateAdministratorUserInvitationOptions{
		ActorUserID: "administrator", ActorSessionID: adminSession.ID, ActionReference: reference,
	})
	if err != nil || rotated.User.UserType != UserTypeManagement || rotated.Token.Token == invited.Token.Token {
		t.Fatalf("replaced administrator invitation = %+v, %v", rotated, err)
	}
	// The replaced token no longer works; the new one does, on the
	// administrator sign-up only.
	if _, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{
		Token: invited.Token.Token, NewPassword: "an invented passphrase for rafe", Purpose: EnrollmentTokenPurposeEnrollment, Administrator: true,
	}); !errors.Is(err, ErrEnrollmentTokenInvalid) {
		t.Fatalf("replaced token = %v", err)
	}
	if err := manager.RevokeAdministratorUserInvitation(t.Context(), "administrator", adminSession.ID, reference); err != nil {
		t.Fatalf("revoke administrator invitation = %v", err)
	}
	if _, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{
		Token: rotated.Token.Token, NewPassword: "an invented passphrase for rafe", Purpose: EnrollmentTokenPurposeEnrollment, Administrator: true,
	}); !errors.Is(err, ErrEnrollmentTokenInvalid) {
		t.Fatalf("revoked token = %v", err)
	}
	// Only administrators, when asked.
	insertActiveUser(t, manager, "mail-person", false, now)
	if page, err := manager.ListAdministratorUserPage(t.Context(), "administrator", adminSession.ID, AdministratorUserQuery{Administrators: true}); err != nil || len(page.Users) != 2 {
		t.Fatalf("administrators after adding a mail user = %+v, %v", page, err)
	}
}
