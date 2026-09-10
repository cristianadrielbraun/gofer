package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func initializePolicyTestInstance(t *testing.T, manager *Manager, ownerID string, now time.Time) {
	t.Helper()
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (
			id, initialized, owner_user_id, initialized_at, cutover_version
		) VALUES (1, 1, ?, ?, 1)`, ownerID, now,
	); err != nil {
		t.Fatalf("initialize policy test instance: %v", err)
	}
}

func insertPolicyTestTOTP(t *testing.T, manager *Manager, userID string, now time.Time) {
	t.Helper()
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO totp_credentials (
			id, user_id, encrypted_seed, key_version, algorithm, digits,
			period, issuer, enabled, created_at
		) VALUES (?, ?, x'01', 1, 'SHA1', 6, 30, 'Gofer', 1, ?)`,
		"totp-"+userID, userID, now,
	); err != nil {
		t.Fatalf("insert policy TOTP for %q: %v", userID, err)
	}
}

func insertPolicyTestPasskey(t *testing.T, manager *Manager, userID string, now time.Time) {
	t.Helper()
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO webauthn_credentials (
			id, user_id, credential_id, public_key, name, created_at,
			credential_ciphertext, key_version, rp_id
		) VALUES (?, ?, ?, x'02', 'Policy passkey', ?, x'03', 1, 'gofer.example')`,
		"passkey-"+userID, userID, []byte("credential-"+userID), now,
	); err != nil {
		t.Fatalf("insert policy passkey for %q: %v", userID, err)
	}
}

func createPolicyAdministratorSession(t *testing.T, manager *Manager) *Session {
	t.Helper()
	session, err := manager.CreateAuthenticatedSession(
		t.Context(), "administrator", "Policy administrator",
		AuthenticationMethodPassword, AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if steppedUp, err := manager.RecordSessionStepUp(
		t.Context(), session.UserID, session.ID, AuthenticationMethodTOTP,
	); err != nil || !steppedUp {
		t.Fatalf("RecordSessionStepUp() = %t, %v", steppedUp, err)
	}
	return session
}

func TestInstanceSecurityPolicyUsesAdministratorDefaultBeforeProvisioning(t *testing.T) {
	now := time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	policy, err := manager.InstanceSecurityPolicy(t.Context())
	if err != nil || policy.MFA != InstanceMFAPolicyAdministrators || policy.UpdatedAt != nil || policy.UpdatedBy != "" {
		t.Fatalf("InstanceSecurityPolicy() = %#v, %v", policy, err)
	}
	insertActiveUser(t, manager, "administrator", true, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	if _, err := manager.SetInstanceMFAPolicy(t.Context(), SetInstanceMFAPolicyOptions{
		ActorUserID: adminSession.UserID, ActorSessionID: adminSession.ID, Policy: InstanceMFAPolicyAllUsers,
	}); !errors.Is(err, ErrInstanceMFAPolicyUnavailable) {
		t.Fatalf("SetInstanceMFAPolicy(unprovisioned) error = %v", err)
	}
}

func TestSetInstanceMFAPolicyRejectsFactorlessActiveUsersWithoutSideEffects(t *testing.T) {
	now := time.Date(2026, time.August, 10, 0, 15, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "factorless", false, now)
	insertActiveUser(t, manager, "disabled-factorless", false, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE users SET status = 'disabled' WHERE id = 'disabled-factorless'`); err != nil {
		t.Fatal(err)
	}
	initializePolicyTestInstance(t, manager, "administrator", now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO webauthn_credentials (
			id, user_id, credential_id, public_key, name, created_at,
			credential_ciphertext, key_version, rp_id
		) VALUES ('wrong-rp', 'factorless', x'09', x'02', 'Other site', ?, x'03', 1, 'other.example')`, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO webauthn_credentials (
			id, user_id, credential_id, public_key, name, created_at, rp_id
		) VALUES ('incomplete-passkey', 'factorless', x'0a', x'02', 'Incomplete', ?, 'gofer.example')`, now,
	); err != nil {
		t.Fatal(err)
	}
	adminSession := createPolicyAdministratorSession(t, manager)
	weak, err := manager.CreateAuthenticatedSession(
		t.Context(), "factorless", "Factorless browser",
		AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.SetInstanceMFAPolicy(t.Context(), SetInstanceMFAPolicyOptions{
		ActorUserID: adminSession.UserID, ActorSessionID: adminSession.ID,
		Policy: InstanceMFAPolicyAllUsers,
	})
	var enrollmentErr *InstanceMFAPolicyEnrollmentError
	if result != nil || !errors.As(err, &enrollmentErr) || enrollmentErr.ActiveUsersWithoutFactor != 1 {
		t.Fatalf("SetInstanceMFAPolicy() = %#v, %v", result, err)
	}
	policy, err := manager.InstanceSecurityPolicy(t.Context())
	if err != nil || policy.MFA != InstanceMFAPolicyAdministrators || policy.UpdatedAt != nil {
		t.Fatalf("policy after rejected activation = %#v, %v", policy, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), weak.Token); err != nil || stored == nil {
		t.Fatalf("weak session after rejected activation = %#v, %v", stored, err)
	}
	var events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_events WHERE event_type = ?`, AuthEventSecurityPolicyChanged,
	).Scan(&events); err != nil || events != 0 {
		t.Fatalf("policy events after rejected activation = %d, %v", events, err)
	}
}

func TestSetInstanceMFAPolicyRequiresAdministratorAndRecentStrongVerification(t *testing.T) {
	now := time.Date(2026, time.August, 10, 0, 20, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "person", false, now)
	initializePolicyTestInstance(t, manager, "administrator", now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	insertPolicyTestTOTP(t, manager, "person", now)
	adminSession, err := manager.CreateAuthenticatedSession(
		t.Context(), "administrator", "Unverified administrator",
		AuthenticationMethodPassword, AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	personSession, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Strong non-administrator",
		AuthenticationMethodPassword, AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if steppedUp, err := manager.RecordSessionStepUp(
		t.Context(), personSession.UserID, personSession.ID, AuthenticationMethodTOTP,
	); err != nil || !steppedUp {
		t.Fatalf("RecordSessionStepUp(person) = %t, %v", steppedUp, err)
	}

	if _, err := manager.SetInstanceMFAPolicy(t.Context(), SetInstanceMFAPolicyOptions{
		ActorUserID: personSession.UserID, ActorSessionID: personSession.ID,
		Policy: InstanceMFAPolicyAllUsers,
	}); !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("non-administrator policy change error = %v", err)
	}
	if _, err := manager.SetInstanceMFAPolicy(t.Context(), SetInstanceMFAPolicyOptions{
		ActorUserID: adminSession.UserID, ActorSessionID: adminSession.ID,
		Policy: InstanceMFAPolicyAllUsers,
	}); !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("unverified administrator policy change error = %v", err)
	}
	if steppedUp, err := manager.RecordSessionStepUp(
		t.Context(), adminSession.UserID, adminSession.ID, AuthenticationMethodPassword,
	); err != nil || !steppedUp {
		t.Fatalf("RecordSessionStepUp(admin password) = %t, %v", steppedUp, err)
	}
	if _, err := manager.SetInstanceMFAPolicy(t.Context(), SetInstanceMFAPolicyOptions{
		ActorUserID: adminSession.UserID, ActorSessionID: adminSession.ID,
		Policy: InstanceMFAPolicyAllUsers,
	}); !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("password-only administrator policy change error = %v", err)
	}
	policy, err := manager.InstanceSecurityPolicy(t.Context())
	if err != nil || policy.MFA != InstanceMFAPolicyAdministrators || policy.UpdatedAt != nil {
		t.Fatalf("policy after authorization rejections = %#v, %v", policy, err)
	}
}

func TestSetInstanceMFAPolicyRevokesWeakSessionsAndDoesNotReviveThem(t *testing.T) {
	now := time.Date(2026, time.August, 10, 0, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "totp-user", false, now)
	insertActiveUser(t, manager, "passkey-user", false, now)
	initializePolicyTestInstance(t, manager, "administrator", now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	insertPolicyTestTOTP(t, manager, "totp-user", now)
	insertPolicyTestPasskey(t, manager, "passkey-user", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	weak, err := createLegacyPolicySession(t, manager,
		t.Context(), "totp-user", "Weak browser",
		AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	strong, err := manager.CreateAuthenticatedSession(
		t.Context(), "passkey-user", "Passkey browser",
		AuthenticationMethodPasskey, AssuranceLevelPhishingResistant,
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := manager.SetInstanceMFAPolicy(t.Context(), SetInstanceMFAPolicyOptions{
		ActorUserID: adminSession.UserID, ActorSessionID: adminSession.ID,
		Policy: InstanceMFAPolicyAllUsers,
	})
	if err != nil || result == nil || !result.Changed || result.RevokedSessions != 1 ||
		result.Policy.MFA != InstanceMFAPolicyAllUsers || result.Policy.UpdatedAt == nil ||
		!result.Policy.UpdatedAt.Equal(now) || result.Policy.UpdatedBy != adminSession.UserID {
		t.Fatalf("SetInstanceMFAPolicy(all users) = %#v, %v", result, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), weak.Token); err != nil || stored != nil {
		t.Fatalf("weak session after activation = %#v, %v", stored, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), strong.Token); err != nil || stored == nil || stored.ID != strong.ID {
		t.Fatalf("strong session after activation = %#v, %v", stored, err)
	}
	var revokedAt sql.NullTime
	var revokedBy string
	var reason SessionRevocationReason
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT revoked_at, COALESCE(revoked_by, ''), revocation_reason
		FROM sessions WHERE id = ?`, weak.ID,
	).Scan(&revokedAt, &revokedBy, &reason); err != nil {
		t.Fatal(err)
	}
	if !revokedAt.Valid || revokedBy != adminSession.UserID || reason != SessionRevocationAdminAction {
		t.Fatalf("weak session revocation = at:%v by:%q reason:%q", revokedAt, revokedBy, reason)
	}
	var eventType AuthEventType
	var eventActor, eventSession, metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT event_type, COALESCE(actor_user_id, ''), COALESCE(session_id, ''), metadata_json
		FROM auth_events WHERE event_type = ?`, AuthEventSecurityPolicyChanged,
	).Scan(&eventType, &eventActor, &eventSession, &metadata); err != nil {
		t.Fatal(err)
	}
	if eventType != AuthEventSecurityPolicyChanged || eventActor != adminSession.UserID ||
		eventSession != adminSession.ID || metadata != `{"from":"administrators","to":"all_users","revoked_sessions":1}` {
		t.Fatalf("policy event = type:%q actor:%q session:%q metadata:%q", eventType, eventActor, eventSession, metadata)
	}
	same, err := manager.SetInstanceMFAPolicy(t.Context(), SetInstanceMFAPolicyOptions{
		ActorUserID: adminSession.UserID, ActorSessionID: adminSession.ID,
		Policy: InstanceMFAPolicyAllUsers,
	})
	if err != nil || same == nil || same.Changed || same.RevokedSessions != 0 || same.Policy.MFA != InstanceMFAPolicyAllUsers {
		t.Fatalf("SetInstanceMFAPolicy(idempotent) = %#v, %v", same, err)
	}
	var events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_events WHERE event_type = ?`, AuthEventSecurityPolicyChanged,
	).Scan(&events); err != nil || events != 1 {
		t.Fatalf("policy events after idempotent update = %d, %v", events, err)
	}

	now = now.Add(time.Minute)
	manager.clock.(*fixedClock).now = now
	result, err = manager.SetInstanceMFAPolicy(t.Context(), SetInstanceMFAPolicyOptions{
		ActorUserID: adminSession.UserID, ActorSessionID: adminSession.ID,
		Policy: InstanceMFAPolicyAdministrators,
	})
	if err != nil || result == nil || !result.Changed || result.RevokedSessions != 0 ||
		result.Policy.MFA != InstanceMFAPolicyAdministrators {
		t.Fatalf("SetInstanceMFAPolicy(administrators) = %#v, %v", result, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), weak.Token); err != nil || stored != nil {
		t.Fatalf("revoked weak session after relaxation = %#v, %v", stored, err)
	}
	newWeak, err := manager.CreateAuthenticatedSession(
		t.Context(), "totp-user", "New weak browser",
		AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if !errors.Is(err, ErrAuthenticationPolicyNotSatisfied) || newWeak != nil {
		t.Fatalf("new weak session after relaxation = %#v, %v", newWeak, err)
	}
}

func TestInstanceMFAPolicyAllUsersRejectsImplicitActiveUserCreation(t *testing.T) {
	now := time.Date(2026, time.August, 10, 1, 15, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"administrator", "administrator-session-id", "policy-event"},
		tokens: []string{"administrator-session"},
	})
	administrator, err := manager.CreateOrUpdateUser(t.Context(), "administrator", "Administrator", "")
	if err != nil {
		t.Fatal(err)
	}
	insertPolicyTestTOTP(t, manager, administrator.ID, now)
	initializePolicyTestInstance(t, manager, administrator.ID, now)
	adminSession := createPolicyAdministratorSession(t, manager)
	if _, err := manager.SetInstanceMFAPolicy(t.Context(), SetInstanceMFAPolicyOptions{
		ActorUserID: administrator.ID, ActorSessionID: adminSession.ID, Policy: InstanceMFAPolicyAllUsers,
	}); err != nil {
		t.Fatal(err)
	}

	created, err := manager.CreateOrUpdateUser(t.Context(), "new-user", "New User", "")
	if created != nil || !errors.Is(err, ErrInstanceMFAEnrollmentNeeded) {
		t.Fatalf("CreateOrUpdateUser(global MFA) = %#v, %v", created, err)
	}
	var newUsers int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM users WHERE username_normalized = 'new-user'`,
	).Scan(&newUsers); err != nil || newUsers != 0 {
		t.Fatalf("implicitly created users = %d, %v", newUsers, err)
	}

	updated, err := manager.CreateOrUpdateUser(t.Context(), administrator.Username, "Updated Administrator", "")
	if err != nil || updated == nil || updated.ID != administrator.ID || updated.Name != "Updated Administrator" {
		t.Fatalf("CreateOrUpdateUser(existing global-MFA user) = %#v, %v", updated, err)
	}
}

func TestSetInstanceMFAPolicyRollsBackStateAndRevocationWhenAuditFails(t *testing.T) {
	now := time.Date(2026, time.August, 10, 0, 45, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "person", false, now)
	initializePolicyTestInstance(t, manager, "administrator", now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	insertPolicyTestTOTP(t, manager, "person", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	weak, err := createLegacyPolicySession(t, manager,
		t.Context(), "person", "Weak browser",
		AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_security_policy_event
		BEFORE INSERT ON auth_events
		WHEN NEW.event_type = 'security_policy_changed'
		BEGIN SELECT RAISE(ABORT, 'reject security policy event'); END`,
	); err != nil {
		t.Fatal(err)
	}

	result, err := manager.SetInstanceMFAPolicy(t.Context(), SetInstanceMFAPolicyOptions{
		ActorUserID: adminSession.UserID, ActorSessionID: adminSession.ID,
		Policy: InstanceMFAPolicyAllUsers,
	})
	if result != nil || err == nil || !strings.Contains(err.Error(), "record instance MFA policy change") {
		t.Fatalf("SetInstanceMFAPolicy(audit failure) = %#v, %v", result, err)
	}
	policy, err := manager.InstanceSecurityPolicy(t.Context())
	if err != nil || policy.MFA != InstanceMFAPolicyAdministrators || policy.UpdatedAt != nil {
		t.Fatalf("policy after rolled-back activation = %#v, %v", policy, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), weak.Token); err != nil || stored == nil {
		t.Fatalf("weak session after rolled-back activation = %#v, %v", stored, err)
	}
}

func TestInstanceMFAPolicyActivationCannotRaceAWeakSessionIntoPersistence(t *testing.T) {
	now := time.Date(2026, time.August, 10, 1, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "person", false, now)
	initializePolicyTestInstance(t, manager, "administrator", now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	insertPolicyTestTOTP(t, manager, "person", now)
	adminSession := createPolicyAdministratorSession(t, manager)

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	var policyResult *SetInstanceMFAPolicyResult
	var policyErr error
	go func() {
		defer wait.Done()
		<-start
		policyResult, policyErr = manager.SetInstanceMFAPolicy(t.Context(), SetInstanceMFAPolicyOptions{
			ActorUserID: adminSession.UserID, ActorSessionID: adminSession.ID,
			Policy: InstanceMFAPolicyAllUsers,
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
		t.Fatalf("SetInstanceMFAPolicy() = %#v, %v", policyResult, policyErr)
	}
	if weakErr != nil && !errors.Is(weakErr, ErrAuthenticationPolicyNotSatisfied) {
		t.Fatalf("CreateAuthenticatedSession() racing error = %v", weakErr)
	}
	if weak != nil {
		if stored, err := manager.GetSessionByToken(t.Context(), weak.Token); err != nil || stored != nil {
			t.Fatalf("racing weak session remained active = %#v, %v", stored, err)
		}
	}
	var activeWeak int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sessions
		WHERE user_id = 'person' AND revoked_at IS NULL
		  AND assurance_level IN ('legacy', 'single_factor')`,
	).Scan(&activeWeak); err != nil || activeWeak != 0 {
		t.Fatalf("active weak sessions after policy race = %d, %v", activeWeak, err)
	}
}

// Seed sessions created before enrolled MFA was enforced. Normal issuance must
// now reject these, but policy-transition tests still exercise legacy rows.
func createLegacyPolicySession(t *testing.T, m *Manager, ctx context.Context, userID, agent string, method AuthenticationMethod, assurance AssuranceLevel) (*Session, error) {
	t.Helper()
	session, err := m.CreateAuthenticatedSession(ctx, userID, agent, AuthenticationMethodTOTP, AssuranceLevelMultiFactor)
	if err != nil {
		return nil, err
	}
	_, err = m.db.Write().ExecContext(ctx, `UPDATE sessions SET authentication_method=?, assurance_level=? WHERE id=?`, method, assurance, session.ID)
	session.AuthenticationMethod = method
	session.AssuranceLevel = assurance
	return session, err
}
