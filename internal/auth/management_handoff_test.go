package auth

import (
	"database/sql"
	"errors"
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
			id, email, email_normalized, username, username_normalized, name,
			status, user_type, is_admin, created_at, updated_at
		) VALUES ('legacy-admin', 'legacy@example.com', 'legacy@example.com',
			'legacy', 'legacy', 'Legacy Admin', 'active', 'webmail', 0, ?, ?);
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
			id, email, email_normalized, username, username_normalized, name,
			status, user_type, is_admin, created_at, updated_at
		) VALUES ('legacy-federated-admin', 'federated@example.com', 'federated@example.com',
			'federated-admin', 'federated-admin', 'Federated Admin', 'active', 'webmail', 0, ?, ?);
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
		Name: "Management Owner", Username: "management", Email: "management@example.com",
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
		Name: "Management Owner", Username: "management", Email: "management@example.com",
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
