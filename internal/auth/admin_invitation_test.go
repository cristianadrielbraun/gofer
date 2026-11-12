package auth

import (
	"errors"
	"testing"
	"time"
)

func TestAdministratorInvitationOnlyMakesAnAdministratorOnItsOwnSignUp(t *testing.T) {
	now := time.Date(2026, time.November, 12, 14, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "ordinary", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	staleAdmin, err := manager.CreateAuthenticatedSession(t.Context(), "administrator", "Stale admin", AuthenticationMethodPassword, AssuranceLevelMultiFactor)
	if err != nil {
		t.Fatal(err)
	}
	ordinarySession, err := manager.CreateAuthenticatedSession(t.Context(), "ordinary", "Ordinary browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor)
	if err != nil {
		t.Fatal(err)
	}
	options := CreateAdministratorUserInvitationOptions{ActorUserID: "administrator", ActorSessionID: adminSession.ID, Name: "Odile Marsh", Username: "odile.marsh"}

	// Only a recently verified administrator invites administrators.
	for _, refused := range []struct {
		actor, session string
		want           error
	}{{"ordinary", ordinarySession.ID, ErrAdministratorRequired}, {"administrator", staleAdmin.ID, ErrRecentStepUpRequired}} {
		attempt := options
		attempt.ActorUserID, attempt.ActorSessionID = refused.actor, refused.session
		if invitation, err := manager.CreateAdministratorInvitation(t.Context(), attempt); invitation != nil || !errors.Is(err, refused.want) {
			t.Fatalf("invitation by %s = %+v, %v", refused.actor, invitation, err)
		}
	}

	invitation, err := manager.CreateAdministratorInvitation(t.Context(), options)
	if err != nil || invitation.Token.Token == "" || invitation.User.UserType != UserTypeManagement || !invitation.User.IsAdmin {
		t.Fatalf("CreateAdministratorInvitation() = %+v, %v", invitation, err)
	}
	var status, userType string
	var isAdmin int
	if err := manager.db.Read().QueryRow(`SELECT status, user_type, is_admin FROM users WHERE id = ?`, invitation.User.ID).Scan(&status, &userType, &isAdmin); err != nil ||
		status != "pending" || userType != "management" || isAdmin != 1 {
		t.Fatalf("invited administrator = %s %s %d, %v", status, userType, isAdmin, err)
	}
	records := auditRecords(t, manager)
	if len(records) != 1 || records[0].Action != "user.administrator_invited" || records[0].Target.ID != invitation.User.ID {
		t.Fatalf("audit records = %+v", records)
	}
	// Pending, they have no administrator rights.
	if err := manager.RequireAdministratorPermission(t.Context(), invitation.User.ID, "", "users.read"); !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("a pending administrator's rights = %v", err)
	}

	// The mail-user sign-up refuses an administrator's invitation.
	if result, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{
		Token: invitation.Token.Token, NewPassword: "a long invented passphrase for odile", Purpose: EnrollmentTokenPurposeEnrollment,
	}); result != nil || !errors.Is(err, ErrEnrollmentTokenInvalid) {
		t.Fatalf("mail-user sign-up with an administrator invitation = %+v, %v", result, err)
	}
	// And the administrator sign-up refuses a mail user's invitation.
	userInvitation, err := manager.CreateAdministratorUserInvitation(t.Context(), CreateAdministratorUserInvitationOptions{
		ActorUserID: "administrator", ActorSessionID: adminSession.ID, Name: "Pell Arno", Username: "pell.arno",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{
		Token: userInvitation.Token.Token, NewPassword: "a long invented passphrase for pell", Purpose: EnrollmentTokenPurposeEnrollment, Administrator: true,
	}); result != nil || !errors.Is(err, ErrEnrollmentTokenInvalid) {
		t.Fatalf("administrator sign-up with a mail user's invitation = %+v, %v", result, err)
	}

	// Even when everyone must use two-factor authentication, an
	// administrator sets it up at their first sign-in.
	if _, err := manager.db.Write().Exec(`INSERT INTO auth_system_state (id, initialized, mfa_policy) VALUES (1, 1, 'all_users') ON CONFLICT(id) DO UPDATE SET mfa_policy = 'all_users'`); err != nil {
		t.Fatal(err)
	}
	result, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{
		Token: invitation.Token.Token, NewPassword: "a long invented passphrase for odile", Purpose: EnrollmentTokenPurposeEnrollment, Administrator: true,
	})
	if err != nil || result.UserID != invitation.User.ID {
		t.Fatalf("administrator sign-up = %+v, %v", result, err)
	}
	if err := manager.db.Read().QueryRow(`SELECT status FROM users WHERE id = ?`, invitation.User.ID).Scan(&status); err != nil || status != "active" {
		t.Fatalf("status after sign-up = %s, %v", status, err)
	}
	if again, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{
		Token: invitation.Token.Token, NewPassword: "a long invented passphrase for odile", Purpose: EnrollmentTokenPurposeEnrollment, Administrator: true,
	}); again != nil || !errors.Is(err, ErrEnrollmentTokenInvalid) {
		t.Fatalf("second use of the invitation = %+v, %v", again, err)
	}

	// Their first sign-in gives no session until they set up an
	// authenticator app.
	login, err := manager.AuthenticatePassword(t.Context(), PasswordLoginOptions{
		Identifier: "odile.marsh", Password: "a long invented passphrase for odile", RequiredUserType: UserTypeManagement, Source: "test",
	})
	if err != nil || login.Session != nil || !login.MFAEnrollmentRequired || login.PreAuthChallenge == nil {
		t.Fatalf("first sign-in = %+v, %v", login, err)
	}
}
