package auth

import (
	"errors"
	"testing"
	"time"
)

func countActive(t *testing.T, manager *Manager, query, userID string) int {
	t.Helper()
	var count int
	if err := manager.db.Read().QueryRow(query, userID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

const (
	activeTOTPQuery     = `SELECT COUNT(*) FROM totp_credentials WHERE user_id = ? AND enabled = 1 AND revoked_at IS NULL`
	activePasskeyQuery  = `SELECT COUNT(*) FROM webauthn_credentials WHERE user_id = ? AND revoked_at IS NULL`
	activeRecoveryQuery = `SELECT COUNT(*) FROM recovery_codes WHERE user_id = ? AND used_at IS NULL AND revoked_at IS NULL`
	activeSessionQuery  = `SELECT COUNT(*) FROM sessions WHERE user_id = ? AND revoked_at IS NULL`
)

func insertTestRecoveryCode(t *testing.T, manager *Manager, userID string, now time.Time) {
	t.Helper()
	if _, err := manager.db.Write().Exec(`
		INSERT INTO recovery_codes (id, user_id, batch_id, code_hash, created_at)
		VALUES (?, ?, 'batch', ?, ?)`, "recovery-"+userID, userID, "hash-"+userID, now,
	); err != nil {
		t.Fatal(err)
	}
}

func TestResettingAnotherAdministratorsSignIn(t *testing.T) {
	now := time.Date(2026, time.November, 12, 18, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "keeper", true, now)
	insertActiveUser(t, manager, "ordinary", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	insertPolicyTestTOTP(t, manager, "keeper", now)
	insertPolicyTestPasskey(t, manager, "keeper", now)
	insertTestRecoveryCode(t, manager, "keeper", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	staleAdmin, err := manager.CreateAuthenticatedSession(t.Context(), "administrator", "Stale admin", AuthenticationMethodPassword, AssuranceLevelMultiFactor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateAuthenticatedSession(t.Context(), "keeper", "Keeper's laptop", AuthenticationMethodPassword, AssuranceLevelMultiFactor); err != nil {
		t.Fatal(err)
	}
	options := ResetAdministratorSignInOptions{ActorUserID: "administrator", ActorSessionID: adminSession.ID, TargetUserID: "keeper"}

	for _, refused := range []struct {
		name    string
		options ResetAdministratorSignInOptions
		want    error
	}{
		{"a session that hasn't verified again", ResetAdministratorSignInOptions{ActorUserID: "administrator", ActorSessionID: staleAdmin.ID, TargetUserID: "keeper"}, ErrRecentStepUpRequired},
		{"oneself", ResetAdministratorSignInOptions{ActorUserID: "administrator", ActorSessionID: adminSession.ID, TargetUserID: "administrator"}, ErrEnrollmentTokenTargetInvalid},
		{"a mail user", ResetAdministratorSignInOptions{ActorUserID: "administrator", ActorSessionID: adminSession.ID, TargetUserID: "ordinary"}, ErrEnrollmentTokenTargetInvalid},
	} {
		if token, err := manager.ResetAdministratorSignIn(t.Context(), refused.options); token != nil || !errors.Is(err, refused.want) {
			t.Fatalf("reset of %s = %+v, %v", refused.name, token, err)
		}
	}
	// A mail user's password reset doesn't reach an administrator.
	if token, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "keeper", CreatedBy: "administrator", ActorSessionID: adminSession.ID, Purpose: EnrollmentTokenPurposeCredentialReset,
	}); token != nil || !errors.Is(err, ErrEnrollmentTokenTargetInvalid) {
		t.Fatalf("password reset of an administrator = %+v, %v", token, err)
	}

	token, err := manager.ResetAdministratorSignIn(t.Context(), options)
	if err != nil || token.Token == "" || token.Purpose != EnrollmentTokenPurposeCredentialReset {
		t.Fatalf("ResetAdministratorSignIn() = %+v, %v", token, err)
	}
	records := auditRecords(t, manager)
	if len(records) != 1 || records[0].Action != "user.administrator_sign_in_reset" || records[0].Target.ID != "keeper" {
		t.Fatalf("audit records = %+v", records)
	}
	// Nothing changes until it's used.
	for query, want := range map[string]int{activeTOTPQuery: 1, activePasskeyQuery: 1, activeRecoveryQuery: 1, activeSessionQuery: 1} {
		if got := countActive(t, manager, query, "keeper"); got != want {
			t.Fatalf("before use, %q = %d", query, got)
		}
	}

	// The administrator reset page doesn't take a mail user's reset.
	userReset, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "ordinary", CreatedBy: "administrator", ActorSessionID: adminSession.ID, Purpose: EnrollmentTokenPurposeCredentialReset,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{
		Token: userReset.Token, NewPassword: "an invented passphrase for the ordinary user", Purpose: EnrollmentTokenPurposeCredentialReset, Administrator: true,
	}); result != nil || !errors.Is(err, ErrEnrollmentTokenInvalid) {
		t.Fatalf("administrator reset page with a mail user's token = %+v, %v", result, err)
	}

	result, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{
		Token: token.Token, NewPassword: "an invented passphrase for the keeper", Purpose: EnrollmentTokenPurposeCredentialReset, Administrator: true,
	})
	if err != nil || result.UserID != "keeper" || !result.FactorsReset || result.RevokedSessions != 1 {
		t.Fatalf("using the reset = %+v, %v", result, err)
	}
	for _, query := range []string{activeTOTPQuery, activePasskeyQuery, activeRecoveryQuery, activeSessionQuery} {
		if got := countActive(t, manager, query, "keeper"); got != 0 {
			t.Fatalf("after use, %q = %d", query, got)
		}
	}
	// Their next sign-in sets up two-factor authentication again, with no
	// session before it.
	login, err := manager.AuthenticatePassword(t.Context(), PasswordLoginOptions{
		Identifier: "keeper", Password: "an invented passphrase for the keeper", RequiredUserType: UserTypeManagement, Source: "test",
	})
	if err != nil || login.Session != nil || !login.MFAEnrollmentRequired || login.PreAuthChallenge == nil {
		t.Fatalf("sign-in after the reset = %+v, %v", login, err)
	}
}

// The command line's recovery resets a password only: recovery codes stay
// the way back from a lost authenticator.
func TestCommandLineRecoveryKeepsAnAdministratorsFactors(t *testing.T) {
	now := time.Date(2026, time.November, 12, 18, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "keeper", true, now)
	insertPolicyTestTOTP(t, manager, "keeper", now)
	insertTestRecoveryCode(t, manager, "keeper", now)
	recovery, err := manager.RecoverUserLocally(t.Context(), "keeper")
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{
		Token: recovery.Token.Token, NewPassword: "an invented passphrase for the keeper", Purpose: EnrollmentTokenPurposeCredentialReset,
	})
	if err != nil || result.FactorsReset {
		t.Fatalf("using the recovery = %+v, %v", result, err)
	}
	if countActive(t, manager, activeTOTPQuery, "keeper") != 1 || countActive(t, manager, activeRecoveryQuery, "keeper") != 1 {
		t.Fatal("the command line's recovery retired the factors")
	}
}
