package auth

import (
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSetAdministratorUserMFAPolicyPreservesSessionsAndAuthenticators(t *testing.T) {
	now := time.Date(2026, time.August, 24, 10, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "person", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	insertPolicyTestTOTP(t, manager, "person", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	weak, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Weak browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	strong, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Strong browser", AuthenticationMethodTOTP, AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.SetAdministratorUserMFAPolicy(t.Context(), SetAdministratorUserMFAPolicyOptions{
		ActorUserID: adminSession.UserID, ActorSessionID: adminSession.ID,
		TargetUserID: "person", Required: true,
	})
	if err != nil || result == nil || !result.Changed || !result.Required {
		t.Fatalf("SetAdministratorUserMFAPolicy(require) = %#v, %v", result, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), weak.Token); err != nil || stored == nil || stored.ID != weak.ID {
		t.Fatalf("weak session after individual policy = %#v, %v", stored, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), strong.Token); err != nil || stored == nil || stored.ID != strong.ID {
		t.Fatalf("strong session after individual policy = %#v, %v", stored, err)
	}
	var required int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT mfa_required FROM users WHERE id = 'person'`).Scan(&required); err != nil || required != 1 {
		t.Fatalf("stored individual MFA requirement = %d, %v", required, err)
	}
	var revokedAt sql.NullTime
	var activeTOTP int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT revoked_at FROM sessions WHERE id = ?`, weak.ID,
	).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM totp_credentials
		WHERE user_id = 'person' AND enabled = 1 AND revoked_at IS NULL`,
	).Scan(&activeTOTP); err != nil {
		t.Fatal(err)
	}
	if revokedAt.Valid || activeTOTP != 1 {
		t.Fatalf("preserved authentication state = revoked:%v active TOTP:%d", revokedAt, activeTOTP)
	}
	var actor, subject, sessionID, metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COALESCE(actor_user_id, ''), COALESCE(subject_user_id, ''),
		       COALESCE(session_id, ''), metadata_json
		FROM auth_events WHERE event_type = ?`, AuthEventSecurityPolicyChanged,
	).Scan(&actor, &subject, &sessionID, &metadata); err != nil {
		t.Fatal(err)
	}
	if actor != adminSession.UserID || subject != "person" || sessionID != adminSession.ID ||
		metadata != `{"from":false,"to":true}` {
		t.Fatalf("individual policy event = actor:%q subject:%q session:%q metadata:%q", actor, subject, sessionID, metadata)
	}

	idempotent, err := manager.SetAdministratorUserMFAPolicy(t.Context(), SetAdministratorUserMFAPolicyOptions{
		ActorUserID: adminSession.UserID, ActorSessionID: adminSession.ID,
		TargetUserID: "person", Required: true,
	})
	if err != nil || idempotent == nil || idempotent.Changed || !idempotent.Required {
		t.Fatalf("SetAdministratorUserMFAPolicy(idempotent) = %#v, %v", idempotent, err)
	}
	var events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_events WHERE event_type = ? AND subject_user_id = 'person'`, AuthEventSecurityPolicyChanged,
	).Scan(&events); err != nil || events != 1 {
		t.Fatalf("individual policy events after idempotent call = %d, %v", events, err)
	}

	clock.now = now.Add(time.Minute)
	cleared, err := manager.SetAdministratorUserMFAPolicy(t.Context(), SetAdministratorUserMFAPolicyOptions{
		ActorUserID: adminSession.UserID, ActorSessionID: adminSession.ID,
		TargetUserID: "person", Required: false,
	})
	if err != nil || cleared == nil || !cleared.Changed || cleared.Required {
		t.Fatalf("SetAdministratorUserMFAPolicy(clear) = %#v, %v", cleared, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), weak.Token); err != nil || stored == nil || stored.ID != weak.ID {
		t.Fatalf("preserved weak session after clearing = %#v, %v", stored, err)
	}
	newWeak, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "New weak browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil || newWeak == nil {
		t.Fatalf("new weak session after clearing = %#v, %v", newWeak, err)
	}
}

func TestSetAdministratorUserMFAPolicyAllowsFactorlessTargetAndRequiresVerifiedManagementAdministrator(t *testing.T) {
	now := time.Date(2026, time.August, 24, 10, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "ordinary", false, now)
	insertActiveUser(t, manager, "target", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	insertPolicyTestTOTP(t, manager, "ordinary", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	staleAdminSession, err := manager.CreateAuthenticatedSession(
		t.Context(), "administrator", "Unverified admin", AuthenticationMethodPassword, AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	ordinarySession, err := manager.CreateAuthenticatedSession(
		t.Context(), "ordinary", "Verified ordinary user", AuthenticationMethodPassword, AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if steppedUp, err := manager.RecordSessionStepUp(t.Context(), "ordinary", ordinarySession.ID, AuthenticationMethodTOTP); err != nil || !steppedUp {
		t.Fatalf("RecordSessionStepUp(ordinary) = %t, %v", steppedUp, err)
	}

	options := SetAdministratorUserMFAPolicyOptions{TargetUserID: "target", Required: true}
	options.ActorUserID, options.ActorSessionID = "ordinary", ordinarySession.ID
	if result, err := manager.SetAdministratorUserMFAPolicy(t.Context(), options); result != nil || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("ordinary policy change = %#v, %v", result, err)
	}
	options.ActorUserID, options.ActorSessionID = "administrator", staleAdminSession.ID
	if result, err := manager.SetAdministratorUserMFAPolicy(t.Context(), options); result != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("unverified admin policy change = %#v, %v", result, err)
	}
	options.ActorSessionID = adminSession.ID
	if result, err := manager.SetAdministratorUserMFAPolicy(t.Context(), options); err != nil || result == nil || !result.Changed || !result.Required {
		t.Fatalf("factorless target policy change = %#v, %v", result, err)
	}
	options.TargetUserID = "administrator"
	if result, err := manager.SetAdministratorUserMFAPolicy(t.Context(), options); result != nil || !errors.Is(err, ErrAdministratorUserMFATargetInvalid) {
		t.Fatalf("management target policy change = %#v, %v", result, err)
	}
	var required, events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT mfa_required FROM users WHERE id = 'target'`).Scan(&required); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events WHERE event_type = ?`, AuthEventSecurityPolicyChanged).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if required != 1 || events != 1 {
		t.Fatalf("factorless policy state = required:%d events:%d", required, events)
	}
}

func TestSetAdministratorUserMFAPolicyRollsBackPolicyAndSessionsWhenAuditFails(t *testing.T) {
	now := time.Date(2026, time.August, 24, 11, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "person", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	insertPolicyTestTOTP(t, manager, "person", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	weak, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Weak browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_user_policy_event
		BEFORE INSERT ON auth_events
		WHEN NEW.event_type = 'security_policy_changed'
		BEGIN SELECT RAISE(ABORT, 'reject user policy event'); END`); err != nil {
		t.Fatal(err)
	}

	result, err := manager.SetAdministratorUserMFAPolicy(t.Context(), SetAdministratorUserMFAPolicyOptions{
		ActorUserID: adminSession.UserID, ActorSessionID: adminSession.ID,
		TargetUserID: "person", Required: true,
	})
	if result != nil || err == nil || !strings.Contains(err.Error(), "record user MFA policy change") {
		t.Fatalf("SetAdministratorUserMFAPolicy(audit failure) = %#v, %v", result, err)
	}
	var required int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT mfa_required FROM users WHERE id = 'person'`).Scan(&required); err != nil || required != 0 {
		t.Fatalf("rolled-back individual requirement = %d, %v", required, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), weak.Token); err != nil || stored == nil {
		t.Fatalf("weak session after rollback = %#v, %v", stored, err)
	}
}

func TestAdministratorUserMFAPolicyCannotRaceAWeakSessionIntoPersistence(t *testing.T) {
	now := time.Date(2026, time.August, 24, 11, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "person", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	insertPolicyTestTOTP(t, manager, "person", now)
	adminSession := createPolicyAdministratorSession(t, manager)

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	var policyResult *SetAdministratorUserMFAPolicyResult
	var policyErr error
	go func() {
		defer wait.Done()
		<-start
		policyResult, policyErr = manager.SetAdministratorUserMFAPolicy(t.Context(), SetAdministratorUserMFAPolicyOptions{
			ActorUserID: adminSession.UserID, ActorSessionID: adminSession.ID,
			TargetUserID: "person", Required: true,
		})
	}()
	var weak *Session
	var weakErr error
	go func() {
		defer wait.Done()
		<-start
		weak, weakErr = manager.CreateAuthenticatedSession(
			t.Context(), "person", "Racing weak browser",
			AuthenticationMethodPassword, AssuranceLevelSingleFactor,
		)
	}()
	close(start)
	wait.Wait()

	if policyErr != nil || policyResult == nil || !policyResult.Changed {
		t.Fatalf("SetAdministratorUserMFAPolicy() = %#v, %v", policyResult, policyErr)
	}
	if weakErr != nil && !errors.Is(weakErr, ErrAuthenticationPolicyNotSatisfied) {
		t.Fatalf("CreateAuthenticatedSession() racing error = %v", weakErr)
	}
	if weak != nil {
		if stored, err := manager.GetSessionByToken(t.Context(), weak.Token); err != nil || stored == nil || stored.ID != weak.ID {
			t.Fatalf("weak session created before policy commit was not preserved = %#v, %v", stored, err)
		}
	}
	var activeWeak int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sessions
		WHERE user_id = 'person' AND revoked_at IS NULL
		  AND assurance_level IN ('legacy', 'single_factor')`,
	).Scan(&activeWeak); err != nil || activeWeak != boolInt(weak != nil) {
		t.Fatalf("active weak sessions after individual policy race = %d, want %d, %v", activeWeak, boolInt(weak != nil), err)
	}
}
