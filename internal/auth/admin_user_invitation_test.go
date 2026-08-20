package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCreateAdministratorUserInvitationIsAtomicHashOnlyAndAudited(t *testing.T) {
	now := time.Date(2026, time.August, 20, 10, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"invited-user-id", "invitation-token-id", "invitation-event-id"},
		tokens: []string{"private-invitation-token"},
	})
	insertEnrollmentTokenUser(t, manager, "administrator", UserStatusActive, true, now)
	sessionID := insertEnrollmentStepUpSession(t, manager, "administrator", now, now)

	invitation, err := manager.CreateAdministratorUserInvitation(t.Context(), CreateAdministratorUserInvitationOptions{
		ActorUserID: "administrator", ActorSessionID: sessionID,
		Name: "  Invited Person  ", Username: "Invited.Person", Email: " Invited@Example.COM ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if invitation.User.ID != "invited-user-id" || invitation.User.Username != "Invited.Person" ||
		invitation.User.Email != "Invited@Example.COM" || invitation.User.Status != UserStatusPending ||
		invitation.User.IsAdmin || invitation.Name != "Invited Person" ||
		invitation.Token.Token != "private-invitation-token" ||
		invitation.Token.ExpiresAt.Sub(invitation.Token.CreatedAt) != defaultEnrollmentTokenLifetime {
		t.Fatalf("CreateAdministratorUserInvitation() = %#v", invitation)
	}

	var email, emailNormalized, username, usernameNormalized, name, status string
	var authVersion, mfaRequired, isAdmin int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT email, email_normalized, username, username_normalized, name, status,
		       auth_version, mfa_required, is_admin
		FROM users WHERE id = ?`, invitation.User.ID).Scan(
		&email, &emailNormalized, &username, &usernameNormalized, &name, &status,
		&authVersion, &mfaRequired, &isAdmin,
	); err != nil {
		t.Fatal(err)
	}
	if email != invitation.User.Email || emailNormalized != "invited@example.com" ||
		username != invitation.User.Username || usernameNormalized != "invited.person" ||
		name != invitation.Name || status != string(UserStatusPending) || authVersion != 1 ||
		mfaRequired != 0 || isAdmin != 0 {
		t.Fatalf("stored invited user = email:%q normalized:%q username:%q usernameNormalized:%q name:%q status:%q auth:%d mfa:%d admin:%d",
			email, emailNormalized, username, usernameNormalized, name, status, authVersion, mfaRequired, isAdmin)
	}

	var tokenHash, eventType, reason, metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT token_hash FROM user_enrollment_tokens WHERE id = ?`, invitation.Token.ID,
	).Scan(&tokenHash); err != nil {
		t.Fatal(err)
	}
	if tokenHash != hashToken(invitation.Token.Token) || tokenHash == invitation.Token.Token {
		t.Fatalf("stored invitation token hash = %q", tokenHash)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT event_type, reason, metadata_json FROM auth_events WHERE id = 'invitation-event-id'`,
	).Scan(&eventType, &reason, &metadata); err != nil {
		t.Fatal(err)
	}
	if eventType != string(AuthEventEnrollmentIssued) || reason != string(AuthEventReasonAdministratorAction) ||
		!strings.Contains(metadata, invitation.Token.ID) || strings.Contains(metadata, invitation.Token.Token) ||
		strings.Contains(metadata, tokenHash) {
		t.Fatalf("stored invitation event = type:%q reason:%q metadata:%q", eventType, reason, metadata)
	}
}

func TestCreateAdministratorUserInvitationValidatesFieldsAndIdentifierCollisions(t *testing.T) {
	now := time.Date(2026, time.August, 20, 10, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"unused-user", "unused-token", "unused-event"},
		tokens: []string{"unused-secret"},
	})
	insertEnrollmentTokenUser(t, manager, "administrator", UserStatusActive, true, now)
	insertEnrollmentTokenUser(t, manager, "existing", UserStatusActive, false, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE users SET username = 'Taken.User', username_normalized = 'taken.user'
		WHERE id = 'existing'`); err != nil {
		t.Fatal(err)
	}
	sessionID := insertEnrollmentStepUpSession(t, manager, "administrator", now, now)

	result, err := manager.CreateAdministratorUserInvitation(t.Context(), CreateAdministratorUserInvitationOptions{
		ActorUserID: "administrator", ActorSessionID: sessionID,
		Name: "\x00", Username: "x", Email: "not-an-email",
	})
	var validationErr *AdministratorUserInvitationValidationError
	if result != nil || !errors.As(err, &validationErr) || len(validationErr.Fields) != 3 ||
		validationErr.Fields["name"] == "" || validationErr.Fields["username"] == "" || validationErr.Fields["email"] == "" {
		t.Fatalf("invalid invitation = %#v, %#v", result, err)
	}

	result, err = manager.CreateAdministratorUserInvitation(t.Context(), CreateAdministratorUserInvitationOptions{
		ActorUserID: "administrator", ActorSessionID: sessionID,
		Name: "Another Person", Username: "taken.user", Email: "EXISTING@example.com",
	})
	validationErr = nil
	if result != nil || !errors.As(err, &validationErr) || validationErr.Fields["username"] == "" || validationErr.Fields["email"] == "" {
		t.Fatalf("colliding invitation = %#v, %#v", result, err)
	}
	var invitedUsers, invitationTokens int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM users WHERE status = 'pending'`).Scan(&invitedUsers); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens`).Scan(&invitationTokens); err != nil {
		t.Fatal(err)
	}
	if invitedUsers != 0 || invitationTokens != 0 {
		t.Fatalf("invalid invitations mutated state = users:%d tokens:%d", invitedUsers, invitationTokens)
	}
}

func TestCreateAdministratorUserInvitationRequiresActiveAdminRecentStepUp(t *testing.T) {
	now := time.Date(2026, time.August, 20, 11, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{
			"ordinary-user", "ordinary-token", "ordinary-event",
			"stale-user", "stale-token", "stale-event",
		},
		tokens: []string{"ordinary-secret", "stale-secret"},
	})
	insertEnrollmentTokenUser(t, manager, "administrator", UserStatusActive, true, now)
	insertEnrollmentTokenUser(t, manager, "ordinary", UserStatusActive, false, now)
	ordinarySession := insertEnrollmentStepUpSession(t, manager, "ordinary", now, now)
	staleAdminSession := insertEnrollmentStepUpSession(t, manager, "administrator", now.Add(-11*time.Minute), now)

	base := CreateAdministratorUserInvitationOptions{Name: "Person", Username: "person", Email: "person@example.com"}
	ordinary := base
	ordinary.ActorUserID = "ordinary"
	ordinary.ActorSessionID = ordinarySession
	if result, err := manager.CreateAdministratorUserInvitation(t.Context(), ordinary); result != nil || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("ordinary invitation = %#v, %v", result, err)
	}
	stale := base
	stale.ActorUserID = "administrator"
	stale.ActorSessionID = staleAdminSession
	if result, err := manager.CreateAdministratorUserInvitation(t.Context(), stale); result != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale administrator invitation = %#v, %v", result, err)
	}
	var pending int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM users WHERE status = 'pending'`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("unauthorized invitation created %d pending users", pending)
	}
}

func TestCreateAdministratorUserInvitationRollsBackUserAndTokenWhenAuditFails(t *testing.T) {
	now := time.Date(2026, time.August, 20, 11, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"rollback-user", "rollback-token", "rollback-event"},
		tokens: []string{"rollback-secret"},
	})
	insertEnrollmentTokenUser(t, manager, "administrator", UserStatusActive, true, now)
	sessionID := insertEnrollmentStepUpSession(t, manager, "administrator", now, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_invitation_audit BEFORE INSERT ON auth_events
		BEGIN SELECT RAISE(FAIL, 'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}

	result, err := manager.CreateAdministratorUserInvitation(t.Context(), CreateAdministratorUserInvitationOptions{
		ActorUserID: "administrator", ActorSessionID: sessionID,
		Name: "Rollback Person", Username: "rollback", Email: "rollback@example.com",
	})
	if result != nil || err == nil {
		t.Fatalf("audit-failing invitation = %#v, %v", result, err)
	}
	var users, tokens int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM users WHERE id = 'rollback-user'`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens WHERE id = 'rollback-token'`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if users != 0 || tokens != 0 {
		t.Fatalf("audit rollback left user/token = %d/%d", users, tokens)
	}
}
