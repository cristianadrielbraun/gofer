package auth

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func requiredChangeFixture(t *testing.T) (*Manager, *fixedClock, string, *Session) {
	t.Helper()
	clock := &fixedClock{now: time.Now().UTC()}
	m := newDeterministicManager(t, clock, secureTokenGenerator{})
	insertEnrollmentTokenUser(t, m, "admin", UserStatusActive, true, clock.now)
	actor := insertEnrollmentStepUpSession(t, m, "admin", clock.now, clock.now)
	insertPasswordLoginUser(t, m, "person", "person", UserStatusActive, false, false, false, currentPasswordLoginHash(t), clock.now)
	session, err := m.CreateAuthenticatedSession(t.Context(), "person", "browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor)
	if err != nil {
		t.Fatal(err)
	}
	return m, clock, actor, session
}

func TestRequiredPasswordChangeRequestRollsBackWithAudit(t *testing.T) {
	m, _, actor, session := requiredChangeFixture(t)
	original, err := m.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{UserID: "person", CreatedBy: "admin", ActorSessionID: actor, Purpose: EnrollmentTokenPurposeCredentialReset})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Write().ExecContext(t.Context(), `CREATE TRIGGER reject_forced_reset BEFORE INSERT ON auth_events WHEN NEW.event_type = 'password_change_required' BEGIN SELECT RAISE(ABORT, 'audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	err = m.RequirePasswordChange(t.Context(), RequirePasswordChangeOptions{UserID: "person", CreatedBy: "admin", ActorSessionID: actor})
	if err == nil {
		t.Fatal("issuance did not roll back")
	}
	var must, version, activeTokens int
	if err := m.db.Read().QueryRowContext(t.Context(), `SELECT p.must_change, u.auth_version FROM users u JOIN password_credentials p ON p.user_id = u.id WHERE u.id = 'person'`).Scan(&must, &version); err != nil {
		t.Fatal(err)
	}
	if must != 0 || version != 1 {
		t.Fatal("failed issuance changed credential state")
	}
	if s, err := m.GetSessionByToken(t.Context(), session.Token); err != nil || s == nil {
		t.Fatal("failed issuance revoked session")
	}
	if err := m.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens WHERE id = ? AND revoked_at IS NULL`, original.ID).Scan(&activeTokens); err != nil || activeTokens != 1 {
		t.Fatal("failed issuance revoked original token")
	}
}

func TestRequiredPasswordChangePreservesMFAAndRejectsIneligibleTargets(t *testing.T) {
	m, clock, actor, _ := requiredChangeFixture(t)
	for _, target := range []string{"admin", "missing", "no-password"} {
		if target == "no-password" {
			insertEnrollmentTokenUser(t, m, target, UserStatusActive, false, clock.now)
		}
		if err := m.RequirePasswordChange(t.Context(), RequirePasswordChangeOptions{UserID: target, CreatedBy: "admin", ActorSessionID: actor}); !errors.Is(err, ErrEnrollmentTokenTargetInvalid) {
			t.Fatalf("ineligible %s: %v", target, err)
		}
	}
	if _, err := m.db.Write().ExecContext(t.Context(), `UPDATE users SET mfa_required = 1 WHERE id = 'person'`); err != nil {
		t.Fatal(err)
	}
	if err := m.RequirePasswordChange(t.Context(), RequirePasswordChangeOptions{UserID: "person", CreatedBy: "admin", ActorSessionID: actor}); err != nil {
		t.Fatal(err)
	}
	// The requirement is persistent, independent of reset-token lifetimes.
	clock.now = clock.now.Add(31 * time.Minute)
	result, err := m.AuthenticatePassword(t.Context(), PasswordLoginOptions{Identifier: "person", Password: passwordLoginTestPassword, Source: "test"})
	if err != nil || result.Session != nil || result.PreAuthChallenge == nil || !result.MFAEnrollmentRequired {
		t.Fatalf("required change bypassed MFA: %v", err)
	}
}

func TestRequiredPasswordChangeCompletionRollsBackAndInvalidatesResetTokens(t *testing.T) {
	m, _, actor, _ := requiredChangeFixture(t)
	token, err := m.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{UserID: "person", CreatedBy: "admin", ActorSessionID: actor, Purpose: EnrollmentTokenPurposeCredentialReset})
	if err != nil {
		t.Fatal(err)
	}
	err = m.RequirePasswordChange(t.Context(), RequirePasswordChangeOptions{UserID: "person", CreatedBy: "admin", ActorSessionID: actor})
	if err != nil {
		t.Fatal(err)
	}
	login, err := m.AuthenticatePassword(t.Context(), PasswordLoginOptions{Identifier: "person", Password: passwordLoginTestPassword, Source: "test"})
	if err != nil || login.Session == nil {
		t.Fatalf("restricted login: %v", err)
	}
	if _, err := m.db.Write().ExecContext(t.Context(), `CREATE TRIGGER reject_forced_change BEFORE INSERT ON auth_events WHEN NEW.event_type = 'credential_changed' BEGIN SELECT RAISE(ABORT, 'audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	opts := PasswordChangeOptions{SessionToken: login.Session.Token, CurrentPassword: passwordLoginTestPassword, NewPassword: changedPasswordTestValue}
	if result, err := m.ChangePassword(t.Context(), opts); err == nil || result != nil {
		t.Fatal("completion did not roll back")
	}
	var must, version, live int
	if err := m.db.Read().QueryRowContext(t.Context(), `SELECT p.must_change, u.auth_version FROM users u JOIN password_credentials p ON p.user_id = u.id WHERE u.id = 'person'`).Scan(&must, &version); err != nil || must != 1 || version != 2 {
		t.Fatal("rollback lost requirement or authentication version")
	}
	if err := m.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens WHERE id = ? AND revoked_at IS NULL`, token.ID).Scan(&live); err != nil || live != 1 {
		t.Fatal("rollback revoked recovery token")
	}
	if _, err := m.db.Write().ExecContext(t.Context(), `DROP TRIGGER reject_forced_change`); err != nil {
		t.Fatal(err)
	}
	result, err := m.ChangePassword(t.Context(), opts)
	if err != nil || result.Session.AuthVersion != 3 {
		t.Fatalf("complete forced change: %v", err)
	}
	var metadata string
	if err := m.db.Read().QueryRowContext(t.Context(), `SELECT metadata_json FROM auth_events WHERE subject_user_id = 'person' AND event_type = 'enrollment_token_issued'`).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{token.Token, login.Session.Token, passwordLoginTestPassword, changedPasswordTestValue} {
		if strings.Contains(metadata, secret) {
			t.Fatal("audit leaked private data")
		}
	}
}

func TestConcurrentRequiredResetCannotBeClearedByExistingSession(t *testing.T) {
	m, _, actor, session := requiredChangeFixture(t)
	var wg sync.WaitGroup
	var issueErr, changeErr error
	var changed *PasswordChangeResult
	wg.Add(2)
	go func() {
		defer wg.Done()
		issueErr = m.RequirePasswordChange(t.Context(), RequirePasswordChangeOptions{UserID: "person", CreatedBy: "admin", ActorSessionID: actor})
	}()
	go func() {
		defer wg.Done()
		changed, changeErr = m.ChangePassword(t.Context(), PasswordChangeOptions{SessionToken: session.Token, CurrentPassword: passwordLoginTestPassword, NewPassword: changedPasswordTestValue})
	}()
	wg.Wait()
	if issueErr != nil {
		t.Fatal(issueErr)
	}
	if changeErr != nil && !errors.Is(changeErr, ErrSessionNotActive) && !errors.Is(changeErr, ErrCurrentPasswordInvalid) {
		t.Fatal(changeErr)
	}
	var must int
	if err := m.db.Read().QueryRowContext(t.Context(), `SELECT must_change FROM password_credentials WHERE user_id = 'person'`).Scan(&must); err != nil || must != 1 {
		t.Fatal("concurrent change removed administrator requirement")
	}
	if changed != nil {
		if found, err := m.GetSessionByToken(t.Context(), changed.Session.Token); err != nil || found != nil {
			t.Fatal("concurrent change retained unrestricted access")
		}
	}
}

func TestRequiredPasswordChangeRequestIsIdempotentAndCreatesNoToken(t *testing.T) {
	m, _, actor, _ := requiredChangeFixture(t)
	options := RequirePasswordChangeOptions{UserID: "person", CreatedBy: "admin", ActorSessionID: actor}
	for range 2 {
		if err := m.RequirePasswordChange(t.Context(), options); err != nil {
			t.Fatal(err)
		}
	}
	var tokens, events int
	if err := m.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens`).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatal("requirement generated a token")
	}
	if err := m.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events WHERE subject_user_id = 'person' AND event_type = 'password_change_required' AND actor_user_id = 'admin' AND session_id = ? AND reason = 'administrator_action' AND success = 1`, actor).Scan(&events); err != nil || events != 1 {
		t.Fatalf("requirement event attribution/count: %d %v", events, err)
	}
}
