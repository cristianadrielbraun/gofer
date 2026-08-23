package auth

import (
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type managementHandoffFixture struct {
	manager            *Manager
	clock              *fixedClock
	handoff            *ManagementHandoff
	sourceSessionToken string
	targetSessionToken string
}

func insertManagementHandoffSession(t *testing.T, manager *Manager, id, userID, token string, now time.Time) {
	t.Helper()
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method, assurance_level,
			user_agent, authenticated_at, last_used_at, idle_expires_at, absolute_expires_at,
			step_up_at, step_up_method, created_at
		) VALUES (?, ?, ?, 1, 'password', 'multi_factor', 'Handoff Browser', ?, ?, ?, ?, ?, 'totp', ?)`,
		id, userID, hashToken(token), now, now, now.Add(time.Hour), now.Add(24*time.Hour), now, now,
	); err != nil {
		t.Fatalf("insert handoff session %q: %v", id, err)
	}
}

func insertLegacyMixedAdministrator(t *testing.T, manager *Manager, now time.Time) {
	t.Helper()
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, username, username_normalized, name,
			status, user_type, is_admin, created_at, updated_at
		) VALUES ('legacy-admin', 'legacy', 'legacy', 'Legacy Admin', 'active', 'webmail', 0, ?, ?);
		INSERT INTO accounts (id, user_id, email_address)
		VALUES ('legacy-mailbox', 'legacy-admin', 'mailbox@example.com');
		DROP TRIGGER users_management_type_update;
		UPDATE users SET is_admin = 1 WHERE id = 'legacy-admin';
		CREATE TRIGGER users_management_type_update
		BEFORE UPDATE OF is_admin, user_type ON users
		WHEN NEW.is_admin = 1 AND NEW.user_type != 'management'
		 AND (OLD.is_admin != NEW.is_admin OR OLD.user_type != NEW.user_type)
		BEGIN
			SELECT RAISE(ABORT, 'administrator must be a management user');
		END;
	`, now, now); err != nil {
		t.Fatalf("insert grandfathered mixed administrator: %v", err)
	}
	insertManagementHandoffSession(t, manager, "legacy-session", "legacy-admin", "legacy-session-token", now)
}

func insertLegacyFederatedAdministrator(t *testing.T, manager *Manager, now time.Time) {
	t.Helper()
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, username, username_normalized, name,
			status, user_type, is_admin, created_at, updated_at
		) VALUES ('legacy-federated-admin', 'federated-admin', 'federated-admin', 'Federated Admin', 'active', 'webmail', 0, ?, ?);
		INSERT INTO auth_identities (id, user_id, provider, issuer, subject)
		VALUES ('legacy-federated-identity', 'legacy-federated-admin', 'google',
			'https://accounts.google.com', 'legacy-federated-subject');
		DROP TRIGGER users_management_type_update;
		UPDATE users SET is_admin = 1 WHERE id = 'legacy-federated-admin';
		CREATE TRIGGER users_management_type_update
		BEFORE UPDATE OF is_admin, user_type ON users
		WHEN NEW.is_admin = 1 AND NEW.user_type != 'management'
		 AND (OLD.is_admin != NEW.is_admin OR OLD.user_type != NEW.user_type)
		BEGIN
			SELECT RAISE(ABORT, 'administrator must be a management user');
		END;
	`, now, now); err != nil {
		t.Fatalf("insert grandfathered federated administrator: %v", err)
	}
	insertManagementHandoffSession(t, manager, "legacy-federated-session", "legacy-federated-admin", "legacy-federated-session-token", now)
}

func TestManagementHandoffAllowsLegacyFederatedAdministratorWithoutMailbox(t *testing.T) {
	now := time.Date(2026, time.August, 20, 8, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"handoff-id", "management-user", "enrollment-token-id", "handoff-start-event"},
		tokens: []string{"management-enrollment-token"},
	})
	insertLegacyFederatedAdministrator(t, manager, now)

	handoff, err := manager.CreateManagementHandoff(t.Context(), CreateManagementHandoffOptions{
		ActorUserID: "legacy-federated-admin", ActorSessionID: "legacy-federated-session",
		Name: "Management Owner", Username: "management",
	})
	if err != nil {
		t.Fatalf("CreateManagementHandoff() error = %v", err)
	}
	if handoff == nil || handoff.Target.UserType != UserTypeManagement || handoff.Target.IsAdmin {
		t.Fatalf("federated administrator handoff = %#v", handoff)
	}
}

func newManagementHandoffFixture(t *testing.T, withRecoveryCodes bool) managementHandoffFixture {
	t.Helper()
	now := time.Date(2026, time.August, 20, 9, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{
		ids: []string{
			"handoff-id", "management-user", "enrollment-token-id", "handoff-start-event",
			"management-session", "handoff-complete-event",
		},
		tokens: []string{"management-enrollment-token", "management-session-token"},
	})
	insertLegacyMixedAdministrator(t, manager, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (id, initialized, owner_user_id, initialized_at)
		VALUES (1, 1, 'legacy-admin', ?)`, now); err != nil {
		t.Fatal(err)
	}
	handoff, err := manager.CreateManagementHandoff(t.Context(), CreateManagementHandoffOptions{
		ActorUserID: "legacy-admin", ActorSessionID: "legacy-session",
		Name: "Management Owner", Username: "management",
	})
	if err != nil {
		t.Fatalf("CreateManagementHandoff() error = %v", err)
	}
	if handoff.ID != "handoff-id" || handoff.Target.ID != "management-user" ||
		handoff.Target.UserType != UserTypeManagement || handoff.Target.IsAdmin ||
		handoff.Token.Token != "management-enrollment-token" {
		t.Fatalf("created handoff = %#v", handoff)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE users SET status = 'active', mfa_required = 0 WHERE id = 'management-user';
		INSERT INTO totp_credentials (id, user_id, encrypted_seed, key_version, enabled)
		VALUES ('management-totp', 'management-user', x'01', 1, 1);
	`); err != nil {
		t.Fatal(err)
	}
	if withRecoveryCodes {
		if _, err := manager.db.Write().ExecContext(t.Context(), `
			INSERT INTO recovery_codes (id, user_id, batch_id, code_hash)
			VALUES ('management-recovery', 'management-user', 'management-batch', 'management-code-hash')`); err != nil {
			t.Fatal(err)
		}
	}
	insertManagementHandoffSession(t, manager, "management-enrollment-session", "management-user", "management-enrollment-session-token", now)
	return managementHandoffFixture{
		manager: manager, clock: clock, handoff: handoff,
		sourceSessionToken: "legacy-session-token",
		targetSessionToken: "management-enrollment-session-token",
	}
}

func TestManagementHandoffCompletesAtomically(t *testing.T) {
	fixture := newManagementHandoffFixture(t, true)
	session, err := fixture.manager.CompleteManagementHandoff(
		t.Context(), fixture.targetSessionToken, "Activated Management Browser",
	)
	if err != nil {
		t.Fatalf("CompleteManagementHandoff() error = %v", err)
	}
	if session == nil || session.ID != "management-session" || session.UserID != "management-user" ||
		session.Token != "management-session-token" || session.AuthVersion != 2 ||
		session.AssuranceLevel != AssuranceLevelMultiFactor || session.StepUpMethod != AuthenticationMethodTOTP {
		t.Fatalf("replacement management session = %#v", session)
	}

	for userID, want := range map[string]struct {
		userType    UserType
		isAdmin     int
		mfa         int
		authVersion int64
	}{
		"legacy-admin":    {userType: UserTypeWebmail, isAdmin: 0, mfa: 0, authVersion: 2},
		"management-user": {userType: UserTypeManagement, isAdmin: 1, mfa: 1, authVersion: 2},
	} {
		var userType UserType
		var isAdmin, mfa int
		var authVersion int64
		if err := fixture.manager.db.Read().QueryRow(`
			SELECT user_type, is_admin, mfa_required, auth_version FROM users WHERE id = ?`, userID,
		).Scan(&userType, &isAdmin, &mfa, &authVersion); err != nil {
			t.Fatal(err)
		}
		if userType != want.userType || isAdmin != want.isAdmin || mfa != want.mfa || authVersion != want.authVersion {
			t.Fatalf("%s state = type:%q admin:%d mfa:%d version:%d; want %#v", userID, userType, isAdmin, mfa, authVersion, want)
		}
	}

	var mailboxOwner, ownerUserID, handoffStatus string
	var completedAt sql.NullTime
	if err := fixture.manager.db.Read().QueryRow(`SELECT user_id FROM accounts WHERE id = 'legacy-mailbox'`).Scan(&mailboxOwner); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.db.Read().QueryRow(`SELECT owner_user_id FROM auth_system_state WHERE id = 1`).Scan(&ownerUserID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.db.Read().QueryRow(`SELECT status, completed_at FROM management_handoffs WHERE id = 'handoff-id'`).Scan(&handoffStatus, &completedAt); err != nil {
		t.Fatal(err)
	}
	if mailboxOwner != "legacy-admin" || ownerUserID != "management-user" || handoffStatus != "completed" || !completedAt.Valid {
		t.Fatalf("handoff ownership = mailbox:%q owner:%q status:%q completed:%t", mailboxOwner, ownerUserID, handoffStatus, completedAt.Valid)
	}
	for _, sessionID := range []string{"legacy-session", "management-enrollment-session"} {
		var reason string
		var revokedAt sql.NullTime
		if err := fixture.manager.db.Read().QueryRow(`SELECT revocation_reason, revoked_at FROM sessions WHERE id = ?`, sessionID).Scan(&reason, &revokedAt); err != nil {
			t.Fatal(err)
		}
		if reason != string(SessionRevocationRoleChanged) || !revokedAt.Valid {
			t.Fatalf("revoked session %s = reason:%q revoked:%t", sessionID, reason, revokedAt.Valid)
		}
	}
	if loaded, err := fixture.manager.GetSessionByToken(t.Context(), session.Token); err != nil || loaded == nil || loaded.ID != session.ID {
		t.Fatalf("new management session lookup = %#v, %v", loaded, err)
	}
	var started, completed int
	if err := fixture.manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE event_type = ?`, AuthEventManagementHandoffStarted).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE event_type = ?`, AuthEventManagementHandoffCompleted).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if started != 1 || completed != 1 {
		t.Fatalf("handoff event counts = started:%d completed:%d", started, completed)
	}
}

func TestManagementHandoffCompletesWhenSourceRetainsOnlyFederatedIdentity(t *testing.T) {
	fixture := newManagementHandoffFixture(t, true)
	if _, err := fixture.manager.db.Write().ExecContext(t.Context(), `
		DELETE FROM accounts WHERE id = 'legacy-mailbox';
		INSERT INTO auth_identities (id, user_id, provider, issuer, subject)
		VALUES ('legacy-identity', 'legacy-admin', 'google',
			'https://accounts.google.com', 'legacy-subject')`); err != nil {
		t.Fatal(err)
	}

	session, err := fixture.manager.CompleteManagementHandoff(
		t.Context(), fixture.targetSessionToken, "Activated Management Browser",
	)
	if err != nil || session == nil {
		t.Fatalf("CompleteManagementHandoff() = %#v, %v", session, err)
	}
	var sourceAdmin, retainedIdentities int
	if err := fixture.manager.db.Read().QueryRow(`SELECT is_admin FROM users WHERE id = 'legacy-admin'`).Scan(&sourceAdmin); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_identities WHERE user_id = 'legacy-admin'`).Scan(&retainedIdentities); err != nil {
		t.Fatal(err)
	}
	if sourceAdmin != 0 || retainedIdentities != 1 {
		t.Fatalf("separated federated source = admin:%d identities:%d", sourceAdmin, retainedIdentities)
	}
}

func TestManagementHandoffDoesNotChangeRolesBeforeEnrollmentIsComplete(t *testing.T) {
	fixture := newManagementHandoffFixture(t, false)
	session, err := fixture.manager.CompleteManagementHandoff(t.Context(), fixture.targetSessionToken, "Incomplete Browser")
	if session != nil || !errors.Is(err, ErrManagementEnrollmentIncomplete) {
		t.Fatalf("CompleteManagementHandoff() = %#v, %v", session, err)
	}
	var sourceAdmin, targetAdmin int
	var sourceVersion, targetVersion int64
	var status, ownerID string
	var completedAt sql.NullTime
	if err := fixture.manager.db.Read().QueryRow(`SELECT is_admin, auth_version FROM users WHERE id = 'legacy-admin'`).Scan(&sourceAdmin, &sourceVersion); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.db.Read().QueryRow(`SELECT is_admin, auth_version FROM users WHERE id = 'management-user'`).Scan(&targetAdmin, &targetVersion); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.db.Read().QueryRow(`SELECT status, completed_at FROM management_handoffs WHERE id = 'handoff-id'`).Scan(&status, &completedAt); err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.db.Read().QueryRow(`SELECT owner_user_id FROM auth_system_state WHERE id = 1`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if sourceAdmin != 1 || targetAdmin != 0 || sourceVersion != 1 || targetVersion != 1 ||
		status != "pending" || completedAt.Valid || ownerID != "legacy-admin" {
		t.Fatalf("incomplete handoff mutated state = source:%d/%d target:%d/%d status:%q completed:%t owner:%q",
			sourceAdmin, sourceVersion, targetAdmin, targetVersion, status, completedAt.Valid, ownerID)
	}
}

func TestLegacyMixedAdministratorCannotUseRegularManagementOperations(t *testing.T) {
	now := time.Date(2026, time.August, 20, 10, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertLegacyMixedAdministrator(t, manager, now)
	if users, err := manager.ListAdministratorUsers(t.Context(), "legacy-admin"); users != nil || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("ListAdministratorUsers() = %#v, %v", users, err)
	}
}

func newPendingManagementHandoffRecoveryFixture(t *testing.T) (*Manager, *fixedClock, *ManagementHandoff) {
	t.Helper()
	now := time.Date(2026, time.August, 23, 8, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{
		ids: []string{
			"handoff-id", "management-user", "initial-token-id", "handoff-start-event",
			"reissued-token-one", "reissue-event-one",
			"reissued-token-two", "reissue-event-two",
			"reissued-token-three", "reissue-event-three",
			"cancel-event",
		},
		tokens: []string{
			"initial-management-invitation", "replacement-management-invitation-one",
			"replacement-management-invitation-two", "replacement-management-invitation-three",
		},
	})
	insertLegacyMixedAdministrator(t, manager, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (id, initialized, owner_user_id, initialized_at)
		VALUES (1, 1, 'legacy-admin', ?)`, now); err != nil {
		t.Fatal(err)
	}
	handoff, err := manager.CreateManagementHandoff(t.Context(), CreateManagementHandoffOptions{
		ActorUserID: "legacy-admin", ActorSessionID: "legacy-session",
		Name: "Management Owner", Username: "management",
	})
	if err != nil {
		t.Fatalf("CreateManagementHandoff() error = %v", err)
	}
	return manager, clock, handoff
}

func TestManagementHandoffInvitationReissueRevokesPriorBearerAndRejectsReplay(t *testing.T) {
	manager, _, handoff := newPendingManagementHandoffRecoveryFixture(t)
	status, err := manager.GetPendingManagementHandoff(t.Context(), "legacy-admin", "legacy-session")
	if err != nil {
		t.Fatalf("GetPendingManagementHandoff() error = %v", err)
	}
	if status == nil || status.InvitationState != AdministratorUserInvitationActive ||
		status.InvitationActionReference == "" ||
		strings.Contains(status.InvitationActionReference, handoff.ID) ||
		strings.Contains(status.InvitationActionReference, handoff.Target.ID) {
		t.Fatalf("pending management handoff status = %#v", status)
	}

	replacement, err := manager.ReissueManagementHandoffInvitation(t.Context(), ReissueManagementHandoffInvitationOptions{
		ActorUserID: "legacy-admin", ActorSessionID: "legacy-session",
		ActionReference: status.InvitationActionReference,
	})
	if err != nil {
		t.Fatalf("ReissueManagementHandoffInvitation() error = %v", err)
	}
	if replacement.ID != handoff.ID || replacement.Target.ID != handoff.Target.ID ||
		replacement.Token.Token != "replacement-management-invitation-one" {
		t.Fatalf("reissued management invitation = %#v", replacement)
	}
	if replayed, err := manager.ReissueManagementHandoffInvitation(t.Context(), ReissueManagementHandoffInvitationOptions{
		ActorUserID: "legacy-admin", ActorSessionID: "legacy-session",
		ActionReference: status.InvitationActionReference,
	}); replayed != nil || !errors.Is(err, ErrManagementHandoffUnavailable) {
		t.Fatalf("replayed management invitation reissue = %#v, %v", replayed, err)
	}

	var oldRevokedAt sql.NullTime
	var replacementHash string
	if err := manager.db.Read().QueryRow(`SELECT revoked_at FROM user_enrollment_tokens WHERE id = 'initial-token-id'`).Scan(&oldRevokedAt); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT token_hash FROM user_enrollment_tokens WHERE id = 'reissued-token-one'`).Scan(&replacementHash); err != nil {
		t.Fatal(err)
	}
	if !oldRevokedAt.Valid || replacementHash != hashToken(replacement.Token.Token) || replacementHash == replacement.Token.Token {
		t.Fatalf("reissued token storage = old-revoked:%t replacement-hash:%q", oldRevokedAt.Valid, replacementHash)
	}
	if redeemed, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{
		Token: handoff.Token.Token, NewPassword: "Correct Horse Battery Staple! 2026",
	}); redeemed != nil || !errors.Is(err, ErrEnrollmentTokenInvalid) {
		t.Fatalf("redeem replaced management invitation = %#v, %v", redeemed, err)
	}

	var sourceAdmin, targetAdmin, mailboxCount, eventCount int
	if err := manager.db.Read().QueryRow(`SELECT is_admin FROM users WHERE id = 'legacy-admin'`).Scan(&sourceAdmin); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT is_admin FROM users WHERE id = 'management-user'`).Scan(&targetAdmin); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM accounts WHERE user_id = 'legacy-admin'`).Scan(&mailboxCount); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE event_type = ?`, AuthEventManagementHandoffReissued).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if sourceAdmin != 1 || targetAdmin != 0 || mailboxCount != 1 || eventCount != 1 {
		t.Fatalf("reissue boundaries = source-admin:%d target-admin:%d mailboxes:%d events:%d", sourceAdmin, targetAdmin, mailboxCount, eventCount)
	}
}

func TestExpiredManagementHandoffInvitationCanBeReissued(t *testing.T) {
	manager, clock, _ := newPendingManagementHandoffRecoveryFixture(t)
	clock.now = clock.now.Add(defaultEnrollmentTokenLifetime + time.Minute)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE sessions
		SET step_up_at = ?, last_used_at = ?, idle_expires_at = ?, absolute_expires_at = ?
		WHERE id = 'legacy-session'`,
		clock.now, clock.now, clock.now.Add(time.Hour), clock.now.Add(24*time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	status, err := manager.GetPendingManagementHandoff(t.Context(), "legacy-admin", "legacy-session")
	if err != nil {
		t.Fatal(err)
	}
	if status.InvitationState != AdministratorUserInvitationExpired || status.InvitationExpiresAt == nil {
		t.Fatalf("expired management invitation state = %#v", status)
	}
	replacement, err := manager.ReissueManagementHandoffInvitation(t.Context(), ReissueManagementHandoffInvitationOptions{
		ActorUserID: "legacy-admin", ActorSessionID: "legacy-session",
		ActionReference: status.InvitationActionReference,
	})
	if err != nil || replacement == nil || replacement.Token.Token != "replacement-management-invitation-one" {
		t.Fatalf("expired management invitation reissue = %#v, %v", replacement, err)
	}
	var oldRevokedAt sql.NullTime
	if err := manager.db.Read().QueryRow(`SELECT revoked_at FROM user_enrollment_tokens WHERE id = 'initial-token-id'`).Scan(&oldRevokedAt); err != nil {
		t.Fatal(err)
	}
	if !oldRevokedAt.Valid {
		t.Fatal("expired management invitation bearer was not explicitly revoked")
	}
}

func TestManagementHandoffReissueIsSingleWinnerUnderConcurrency(t *testing.T) {
	manager, _, _ := newPendingManagementHandoffRecoveryFixture(t)
	status, err := manager.GetPendingManagementHandoff(t.Context(), "legacy-admin", "legacy-session")
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		invitation *ManagementHandoff
		err        error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			invitation, err := manager.ReissueManagementHandoffInvitation(t.Context(), ReissueManagementHandoffInvitationOptions{
				ActorUserID: "legacy-admin", ActorSessionID: "legacy-session",
				ActionReference: status.InvitationActionReference,
			})
			outcomes <- outcome{invitation: invitation, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(outcomes)
	succeeded, stale := 0, 0
	for result := range outcomes {
		switch {
		case result.err == nil && result.invitation != nil:
			succeeded++
		case result.invitation == nil && errors.Is(result.err, ErrManagementHandoffUnavailable):
			stale++
		default:
			t.Fatalf("concurrent reissue outcome = %#v, %v", result.invitation, result.err)
		}
	}
	var activeTokens int
	if err := manager.db.Read().QueryRow(`
		SELECT COUNT(*) FROM user_enrollment_tokens
		WHERE user_id = 'management-user' AND used_at IS NULL AND revoked_at IS NULL`).Scan(&activeTokens); err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || stale != 1 || activeTokens != 1 {
		t.Fatalf("concurrent reissue = success:%d stale:%d active-tokens:%d", succeeded, stale, activeTokens)
	}
}

func TestManagementHandoffRecoveryActionsRequireRecentBoundAdministratorSession(t *testing.T) {
	manager, clock, _ := newPendingManagementHandoffRecoveryFixture(t)
	status, err := manager.GetPendingManagementHandoff(t.Context(), "legacy-admin", "legacy-session")
	if err != nil {
		t.Fatal(err)
	}
	insertManagementHandoffSession(t, manager, "other-source-session", "legacy-admin", "other-source-token", clock.now)
	if invitation, err := manager.ReissueManagementHandoffInvitation(t.Context(), ReissueManagementHandoffInvitationOptions{
		ActorUserID: "legacy-admin", ActorSessionID: "other-source-session",
		ActionReference: status.InvitationActionReference,
	}); invitation != nil || !errors.Is(err, ErrManagementHandoffUnavailable) {
		t.Fatalf("cross-session management invitation reissue = %#v, %v", invitation, err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE sessions SET step_up_at = ? WHERE id = 'legacy-session'`,
		clock.now.Add(-securityStepUpMaximumAge-time.Second),
	); err != nil {
		t.Fatal(err)
	}
	if invitation, err := manager.ReissueManagementHandoffInvitation(t.Context(), ReissueManagementHandoffInvitationOptions{
		ActorUserID: "legacy-admin", ActorSessionID: "legacy-session",
		ActionReference: status.InvitationActionReference,
	}); invitation != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale-step-up management invitation reissue = %#v, %v", invitation, err)
	}
	if err := manager.CancelManagementHandoff(t.Context(), CancelManagementHandoffOptions{
		ActorUserID: "legacy-admin", ActorSessionID: "legacy-session",
		ActionReference: status.InvitationActionReference,
	}); !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale-step-up management handoff cancellation = %v", err)
	}
	var handoffStatus string
	var revokedAt sql.NullTime
	if err := manager.db.Read().QueryRow(`SELECT status FROM management_handoffs WHERE id = 'handoff-id'`).Scan(&handoffStatus); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT revoked_at FROM user_enrollment_tokens WHERE id = 'initial-token-id'`).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if handoffStatus != "pending" || revokedAt.Valid {
		t.Fatalf("rejected recovery action state = handoff:%s token-revoked:%t", handoffStatus, revokedAt.Valid)
	}
}

func TestCancelManagementHandoffDisablesReplacementWithoutChangingRolesOrOwnership(t *testing.T) {
	manager, clock, _ := newPendingManagementHandoffRecoveryFixture(t)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE users SET status = 'active' WHERE id = 'management-user';
		INSERT INTO password_credentials (user_id, password_hash)
		VALUES ('management-user', 'preserved-password-hash');
		INSERT INTO totp_credentials (id, user_id, encrypted_seed, key_version, enabled)
		VALUES ('preserved-totp', 'management-user', x'01', 1, 1);
		INSERT INTO recovery_codes (id, user_id, batch_id, code_hash)
		VALUES ('preserved-recovery', 'management-user', 'preserved-batch', 'preserved-hash')`); err != nil {
		t.Fatal(err)
	}
	insertManagementHandoffSession(t, manager, "target-session", "management-user", "target-session-token", clock.now)
	status, err := manager.GetPendingManagementHandoff(t.Context(), "legacy-admin", "legacy-session")
	if err != nil {
		t.Fatal(err)
	}
	if status.Target.Status != UserStatusActive {
		t.Fatalf("redeemed management target status = %q", status.Target.Status)
	}
	if err := manager.CancelManagementHandoff(t.Context(), CancelManagementHandoffOptions{
		ActorUserID: "legacy-admin", ActorSessionID: "legacy-session",
		ActionReference: status.InvitationActionReference,
	}); err != nil {
		t.Fatalf("CancelManagementHandoff() error = %v", err)
	}

	var sourceAdmin, targetAdmin, targetVersion int
	var targetStatus, handoffStatus, ownerID, mailboxOwner string
	var canceledAt, targetSessionRevoked sql.NullTime
	var targetSessionReason string
	if err := manager.db.Read().QueryRow(`SELECT is_admin FROM users WHERE id = 'legacy-admin'`).Scan(&sourceAdmin); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT status, is_admin, auth_version FROM users WHERE id = 'management-user'`).Scan(&targetStatus, &targetAdmin, &targetVersion); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT status, canceled_at FROM management_handoffs WHERE id = 'handoff-id'`).Scan(&handoffStatus, &canceledAt); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT owner_user_id FROM auth_system_state WHERE id = 1`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT user_id FROM accounts WHERE id = 'legacy-mailbox'`).Scan(&mailboxOwner); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT revoked_at, revocation_reason FROM sessions WHERE id = 'target-session'`).Scan(&targetSessionRevoked, &targetSessionReason); err != nil {
		t.Fatal(err)
	}
	var sourceSessionRevoked sql.NullTime
	if err := manager.db.Read().QueryRow(`SELECT revoked_at FROM sessions WHERE id = 'legacy-session'`).Scan(&sourceSessionRevoked); err != nil {
		t.Fatal(err)
	}
	var preservedFactors, eventCount int
	if err := manager.db.Read().QueryRow(`
		SELECT (SELECT COUNT(*) FROM password_credentials WHERE user_id = 'management-user') +
		       (SELECT COUNT(*) FROM totp_credentials WHERE user_id = 'management-user') +
		       (SELECT COUNT(*) FROM recovery_codes WHERE user_id = 'management-user')`).Scan(&preservedFactors); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE event_type = ?`, AuthEventManagementHandoffCanceled).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if sourceAdmin != 1 || targetAdmin != 0 || targetStatus != string(UserStatusDisabled) || targetVersion != 2 ||
		handoffStatus != "canceled" || !canceledAt.Valid || ownerID != "legacy-admin" || mailboxOwner != "legacy-admin" ||
		!targetSessionRevoked.Valid || targetSessionReason != string(SessionRevocationUserDisabled) || sourceSessionRevoked.Valid ||
		preservedFactors != 3 || eventCount != 1 {
		t.Fatalf("canceled handoff = source-admin:%d target:%s/%d/v%d handoff:%s/%t owner:%s mailbox:%s target-session:%t/%s source-session:%t factors:%d events:%d",
			sourceAdmin, targetStatus, targetAdmin, targetVersion, handoffStatus, canceledAt.Valid,
			ownerID, mailboxOwner, targetSessionRevoked.Valid, targetSessionReason, sourceSessionRevoked.Valid,
			preservedFactors, eventCount)
	}
	if pending, err := manager.GetPendingManagementHandoff(t.Context(), "legacy-admin", "legacy-session"); pending != nil || err != nil {
		t.Fatalf("pending handoff after cancellation = %#v, %v", pending, err)
	}
}

func TestCancelManagementHandoffRollsBackWhenAuditFails(t *testing.T) {
	manager, _, _ := newPendingManagementHandoffRecoveryFixture(t)
	status, err := manager.GetPendingManagementHandoff(t.Context(), "legacy-admin", "legacy-session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_management_handoff_cancel_audit
		BEFORE INSERT ON auth_events
		WHEN NEW.event_type = 'management_handoff_canceled'
		BEGIN SELECT RAISE(ABORT, 'reject handoff cancellation audit'); END`); err != nil {
		t.Fatal(err)
	}
	if err := manager.CancelManagementHandoff(t.Context(), CancelManagementHandoffOptions{
		ActorUserID: "legacy-admin", ActorSessionID: "legacy-session",
		ActionReference: status.InvitationActionReference,
	}); err == nil {
		t.Fatal("CancelManagementHandoff() error = nil, want audit failure")
	}
	var targetStatus, handoffStatus string
	var targetVersion int
	var revokedAt sql.NullTime
	if err := manager.db.Read().QueryRow(`SELECT status, auth_version FROM users WHERE id = 'management-user'`).Scan(&targetStatus, &targetVersion); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT status FROM management_handoffs WHERE id = 'handoff-id'`).Scan(&handoffStatus); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT revoked_at FROM user_enrollment_tokens WHERE id = 'initial-token-id'`).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if targetStatus != string(UserStatusPending) || targetVersion != 1 || handoffStatus != "pending" || revokedAt.Valid {
		t.Fatalf("cancellation rollback = target:%s/v%d handoff:%s token-revoked:%t", targetStatus, targetVersion, handoffStatus, revokedAt.Valid)
	}
}

func TestReissueManagementHandoffInvitationRollsBackWhenAuditFails(t *testing.T) {
	manager, _, _ := newPendingManagementHandoffRecoveryFixture(t)
	status, err := manager.GetPendingManagementHandoff(t.Context(), "legacy-admin", "legacy-session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_management_handoff_reissue_audit
		BEFORE INSERT ON auth_events
		WHEN NEW.event_type = 'management_handoff_reissued'
		BEGIN SELECT RAISE(ABORT, 'reject handoff reissue audit'); END`); err != nil {
		t.Fatal(err)
	}
	if invitation, err := manager.ReissueManagementHandoffInvitation(t.Context(), ReissueManagementHandoffInvitationOptions{
		ActorUserID: "legacy-admin", ActorSessionID: "legacy-session",
		ActionReference: status.InvitationActionReference,
	}); invitation != nil || err == nil {
		t.Fatalf("ReissueManagementHandoffInvitation() = %#v, %v, want audit failure", invitation, err)
	}
	var tokenCount int
	var revokedAt sql.NullTime
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM user_enrollment_tokens WHERE user_id = 'management-user'`).Scan(&tokenCount); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT revoked_at FROM user_enrollment_tokens WHERE id = 'initial-token-id'`).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if tokenCount != 1 || revokedAt.Valid {
		t.Fatalf("reissue rollback = token-count:%d initial-revoked:%t", tokenCount, revokedAt.Valid)
	}
}
