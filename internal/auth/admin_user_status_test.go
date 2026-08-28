package auth

import (
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSetAdministratorUserStatusDisablesAndEnablesWithoutChangingCredentials(t *testing.T) {
	now := time.Date(2026, time.August, 26, 9, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "person", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	insertPolicyTestTOTP(t, manager, "person", now)
	insertPolicyTestPasskey(t, manager, "person", now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO password_credentials (user_id, password_hash)
		VALUES ('person', 'preserved-password-hash');
		INSERT INTO recovery_codes (id, user_id, batch_id, code_hash)
		VALUES ('person-recovery', 'person', 'person-batch', 'preserved-recovery-hash');
		INSERT INTO auth_identities (id, user_id, provider, issuer, subject)
		VALUES ('person-identity', 'person', 'google', 'https://accounts.google.com', 'preserved-subject')`); err != nil {
		t.Fatal(err)
	}
	adminSession := createPolicyAdministratorSession(t, manager)
	first, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "First browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Second browser", AuthenticationMethodPasskey, AssuranceLevelPhishingResistant,
	)
	if err != nil {
		t.Fatal(err)
	}

	disabled, err := manager.SetAdministratorUserStatus(t.Context(), SetAdministratorUserStatusOptions{
		ActorUserID: adminSession.UserID, ActorSessionID: adminSession.ID,
		TargetUserID: "person", Status: UserStatusDisabled,
	})
	if err != nil || disabled == nil || !disabled.Changed || disabled.Status != UserStatusDisabled || disabled.RevokedSessions != 2 {
		t.Fatalf("SetAdministratorUserStatus(disabled) = %#v, %v", disabled, err)
	}
	user, err := manager.GetUserByID(t.Context(), "person")
	if err != nil || user == nil || user.Status != UserStatusDisabled || user.AuthVersion != 2 ||
		user.DisabledAt == nil || !user.DisabledAt.Equal(now) || user.DisabledBy != "administrator" {
		t.Fatalf("disabled user = %#v, %v", user, err)
	}
	for _, session := range []*Session{first, second} {
		if stored, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || stored != nil {
			t.Fatalf("disabled session %q = %#v, %v", session.ID, stored, err)
		}
	}
	assertAdministratorStatusCredentialCounts(t, manager, 1, 1, 1, 1, 1)
	assertAdministratorStatusEvent(
		t, manager, AuthEventUserDisabled,
		`{"from":"active","to":"disabled","revoked_sessions":2}`,
		adminSession.ID,
	)

	idempotent, err := manager.SetAdministratorUserStatus(t.Context(), SetAdministratorUserStatusOptions{
		ActorUserID: adminSession.UserID, ActorSessionID: adminSession.ID,
		TargetUserID: "person", Status: UserStatusDisabled,
	})
	if err != nil || idempotent == nil || idempotent.Changed || idempotent.RevokedSessions != 0 {
		t.Fatalf("SetAdministratorUserStatus(idempotent disabled) = %#v, %v", idempotent, err)
	}

	clock.now = now.Add(time.Minute)
	enabled, err := manager.SetAdministratorUserStatus(t.Context(), SetAdministratorUserStatusOptions{
		ActorUserID: adminSession.UserID, ActorSessionID: adminSession.ID,
		TargetUserID: "person", Status: UserStatusActive,
	})
	if err != nil || enabled == nil || !enabled.Changed || enabled.Status != UserStatusActive || enabled.RevokedSessions != 0 {
		t.Fatalf("SetAdministratorUserStatus(active) = %#v, %v", enabled, err)
	}
	user, err = manager.GetUserByID(t.Context(), "person")
	if err != nil || user == nil || user.Status != UserStatusActive || user.AuthVersion != 2 ||
		user.DisabledAt != nil || user.DisabledBy != "" {
		t.Fatalf("enabled user = %#v, %v", user, err)
	}
	var revokedSessions int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sessions
		WHERE user_id = 'person' AND revoked_at IS NOT NULL AND revocation_reason = 'user_disabled'`,
	).Scan(&revokedSessions); err != nil || revokedSessions != 2 {
		t.Fatalf("preserved revoked sessions = %d, %v", revokedSessions, err)
	}
	assertAdministratorStatusCredentialCounts(t, manager, 1, 1, 1, 1, 1)
	assertAdministratorStatusEvent(
		t, manager, AuthEventUserEnabled,
		`{"from":"disabled","to":"active","revoked_sessions":0}`,
		adminSession.ID,
	)
}

func assertAdministratorStatusCredentialCounts(
	t *testing.T, manager *Manager, passwords, totps, passkeys, recoveryCodes, identities int,
) {
	t.Helper()
	queries := map[string]int{
		`SELECT COUNT(*) FROM password_credentials WHERE user_id = 'person'`:                        passwords,
		`SELECT COUNT(*) FROM totp_credentials WHERE user_id = 'person' AND revoked_at IS NULL`:     totps,
		`SELECT COUNT(*) FROM webauthn_credentials WHERE user_id = 'person' AND revoked_at IS NULL`: passkeys,
		`SELECT COUNT(*) FROM recovery_codes WHERE user_id = 'person' AND revoked_at IS NULL`:       recoveryCodes,
		`SELECT COUNT(*) FROM auth_identities WHERE user_id = 'person'`:                             identities,
	}
	for query, want := range queries {
		var got int
		if err := manager.db.Read().QueryRowContext(t.Context(), query).Scan(&got); err != nil || got != want {
			t.Fatalf("credential count for %q = %d, %v, want %d", query, got, err, want)
		}
	}
}

func assertAdministratorStatusEvent(
	t *testing.T, manager *Manager, eventType AuthEventType, wantMetadata, wantSessionID string,
) {
	t.Helper()
	var actor, subject, sessionID, reason, metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COALESCE(actor_user_id, ''), COALESCE(subject_user_id, ''),
		       COALESCE(session_id, ''), reason, metadata_json
		FROM auth_events WHERE event_type = ?`, eventType,
	).Scan(&actor, &subject, &sessionID, &reason, &metadata); err != nil {
		t.Fatal(err)
	}
	if actor != "administrator" || subject != "person" || sessionID != wantSessionID ||
		reason != string(AuthEventReasonAdministratorAction) || metadata != wantMetadata {
		t.Fatalf("status event = actor:%q subject:%q session:%q reason:%q metadata:%q",
			actor, subject, sessionID, reason, metadata)
	}
}

func TestSetAdministratorUserStatusRequiresVerifiedAdministratorAndEligibleTarget(t *testing.T) {
	now := time.Date(2026, time.August, 26, 10, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "ordinary", false, now)
	insertActiveUser(t, manager, "target", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	staleAdmin, err := manager.CreateAuthenticatedSession(
		t.Context(), "administrator", "Stale admin", AuthenticationMethodPassword, AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	ordinarySession, err := manager.CreateAuthenticatedSession(
		t.Context(), "ordinary", "Ordinary browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}

	options := SetAdministratorUserStatusOptions{TargetUserID: "target", Status: UserStatusDisabled}
	options.ActorUserID, options.ActorSessionID = "ordinary", ordinarySession.ID
	if result, err := manager.SetAdministratorUserStatus(t.Context(), options); result != nil || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("ordinary status change = %#v, %v", result, err)
	}
	options.ActorUserID, options.ActorSessionID = "administrator", staleAdmin.ID
	if result, err := manager.SetAdministratorUserStatus(t.Context(), options); result != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale administrator status change = %#v, %v", result, err)
	}
	options.ActorSessionID = adminSession.ID
	for _, test := range []struct {
		name   string
		target string
		status UserStatus
		err    error
	}{
		{name: "self", target: "administrator", status: UserStatusDisabled, err: ErrAdministratorUserStatusTargetInvalid},
		{name: "missing", target: "missing", status: UserStatusDisabled, err: ErrAdministratorUserStatusTargetInvalid},
		{name: "invalid state", target: "target", status: UserStatusPending, err: ErrAdministratorUserStatusInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			options.TargetUserID, options.Status = test.target, test.status
			if result, err := manager.SetAdministratorUserStatus(t.Context(), options); result != nil || !errors.Is(err, test.err) {
				t.Fatalf("SetAdministratorUserStatus() = %#v, %v", result, err)
			}
		})
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE users SET status = 'pending' WHERE id = 'target'`); err != nil {
		t.Fatal(err)
	}
	options.TargetUserID, options.Status = "target", UserStatusDisabled
	if result, err := manager.SetAdministratorUserStatus(t.Context(), options); result != nil || !errors.Is(err, ErrAdministratorUserStatusTargetInvalid) {
		t.Fatalf("pending target status change = %#v, %v", result, err)
	}
}

func TestSetAdministratorUserStatusEnforcesEffectiveMFAOnEnable(t *testing.T) {
	now := time.Date(2026, time.August, 26, 10, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "management-target", true, now)
	insertActiveUser(t, manager, "webmail-target", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE users SET status = 'disabled', disabled_at = ?, disabled_by = 'administrator'
		WHERE id IN ('management-target', 'webmail-target');
		UPDATE users SET mfa_required = 1 WHERE id = 'webmail-target'`, now); err != nil {
		t.Fatal(err)
	}

	options := SetAdministratorUserStatusOptions{
		ActorUserID: "administrator", ActorSessionID: adminSession.ID,
		TargetUserID: "management-target", Status: UserStatusActive,
	}
	if result, err := manager.SetAdministratorUserStatus(t.Context(), options); result != nil || !errors.Is(err, ErrInstanceMFAEnrollmentNeeded) {
		t.Fatalf("factorless management enable = %#v, %v", result, err)
	}
	options.TargetUserID = "webmail-target"
	if result, err := manager.SetAdministratorUserStatus(t.Context(), options); err != nil || result == nil || !result.Changed {
		t.Fatalf("individual-policy webmail enable = %#v, %v", result, err)
	}
	insertPolicyTestTOTP(t, manager, "management-target", now)
	options.TargetUserID = "management-target"
	if result, err := manager.SetAdministratorUserStatus(t.Context(), options); err != nil || result == nil || !result.Changed {
		t.Fatalf("factor-ready management enable = %#v, %v", result, err)
	}
}

func TestSetAdministratorUserStatusRollsBackDisableWhenAuditFails(t *testing.T) {
	now := time.Date(2026, time.August, 26, 11, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "person", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	personSession, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Person browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_user_disabled_event
		BEFORE INSERT ON auth_events
		WHEN NEW.event_type = 'user_disabled'
		BEGIN SELECT RAISE(ABORT, 'reject user disabled event'); END`); err != nil {
		t.Fatal(err)
	}

	result, err := manager.SetAdministratorUserStatus(t.Context(), SetAdministratorUserStatusOptions{
		ActorUserID: "administrator", ActorSessionID: adminSession.ID,
		TargetUserID: "person", Status: UserStatusDisabled,
	})
	if result != nil || err == nil || !strings.Contains(err.Error(), "record user status change") {
		t.Fatalf("SetAdministratorUserStatus(audit failure) = %#v, %v", result, err)
	}
	var status UserStatus
	var authVersion int64
	var disabledAt sql.NullTime
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT status, auth_version, disabled_at FROM users WHERE id = 'person'`,
	).Scan(&status, &authVersion, &disabledAt); err != nil {
		t.Fatal(err)
	}
	if status != UserStatusActive || authVersion != 1 || disabledAt.Valid {
		t.Fatalf("rolled-back user = status:%q version:%d disabled:%v", status, authVersion, disabledAt)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), personSession.Token); err != nil || stored == nil {
		t.Fatalf("session after audit rollback = %#v, %v", stored, err)
	}
}

func TestSetAdministratorUserStatusConcurrentDisableEmitsOneTransition(t *testing.T) {
	now := time.Date(2026, time.August, 26, 11, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "person", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	options := SetAdministratorUserStatusOptions{
		ActorUserID: "administrator", ActorSessionID: adminSession.ID,
		TargetUserID: "person", Status: UserStatusDisabled,
	}

	start := make(chan struct{})
	results := make([]*SetAdministratorUserStatusResult, 2)
	errs := make([]error, 2)
	var wait sync.WaitGroup
	for index := range results {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			results[index], errs[index] = manager.SetAdministratorUserStatus(t.Context(), options)
		}()
	}
	close(start)
	wait.Wait()
	changed := 0
	for index := range results {
		if errs[index] != nil || results[index] == nil {
			t.Fatalf("concurrent disable %d = %#v, %v", index, results[index], errs[index])
		}
		if results[index].Changed {
			changed++
		}
	}
	var events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_events WHERE event_type = 'user_disabled' AND subject_user_id = 'person'`,
	).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if changed != 1 || events != 1 {
		t.Fatalf("concurrent status result = changed:%d events:%d", changed, events)
	}
}
