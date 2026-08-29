package auth

import (
	"errors"
	"testing"
	"time"
)

func TestListAdministratorUsersRequiresActiveAdministratorAndReturnsOnlySafeMetadata(t *testing.T) {
	now := time.Date(2026, time.August, 20, 8, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "ordinary", false, now)
	insertActiveUser(t, manager, "pending", false, now)
	insertActiveUser(t, manager, "disabled-administrator", true, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE users SET username = 'Zulu', username_normalized = 'zulu', status = 'disabled',
			disabled_by = 'administrator', auth_version = 9, mfa_required = 1,
			name = 'private-profile-name', avatar_url = 'private-avatar-url'
		WHERE id = 'disabled-administrator';
		UPDATE users SET username = 'Alpha', username_normalized = 'alpha', status = 'pending'
		WHERE id = 'pending';
		UPDATE users SET username = 'Bravo', username_normalized = 'bravo'
		WHERE id = 'ordinary';
		UPDATE users SET mfa_required = 1, password_reset_requested_at = ? WHERE id = 'ordinary'`, now); err != nil {
		t.Fatal(err)
	}

	users, err := manager.ListAdministratorUsers(t.Context(), "administrator")
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 4 {
		t.Fatalf("ListAdministratorUsers() = %#v", users)
	}
	for index, wantID := range []string{"administrator", "pending", "ordinary", "disabled-administrator"} {
		if users[index].ID != wantID {
			t.Fatalf("administrator user ordering = %#v", users)
		}
	}
	if users[0].Status != UserStatusActive || !users[0].IsAdmin ||
		users[1].Username != "Alpha" || users[1].Status != UserStatusPending || users[1].IsAdmin ||
		users[2].Username != "Bravo" || users[2].IsAdmin || !users[2].MFARequired || users[2].PasswordResetRequestedAt == nil || !users[2].PasswordResetRequestedAt.Equal(now) ||
		users[3].Username != "Zulu" || users[3].Status != UserStatusDisabled || !users[3].IsAdmin || !users[3].MFARequired {
		t.Fatalf("administrator user projection = %#v", users)
	}
	if users[1].InvitationState != AdministratorUserInvitationNotIssued || users[1].InvitationActionReference != "" {
		t.Fatalf("non-interactive administrator invitation projection = %#v", users[1])
	}
	administratorSessionID := insertEnrollmentStepUpSession(t, manager, "administrator", now, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO user_enrollment_tokens (
			id, user_id, created_by, token_hash, purpose, created_at, expires_at
		) VALUES ('pending-token', 'pending', 'administrator', 'pending-token-hash', 'enrollment', ?, ?)`,
		now, now.Add(time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	interactive, err := manager.ListAdministratorUsersForSession(t.Context(), "administrator", administratorSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if interactive[1].InvitationState != AdministratorUserInvitationActive ||
		interactive[1].InvitationExpiresAt == nil ||
		!interactive[1].InvitationExpiresAt.Equal(now.Add(time.Hour)) ||
		!canonicalAdministratorUserInvitationActionReference(interactive[1].InvitationActionReference) ||
		interactive[0].InvitationActionReference != "" || interactive[2].InvitationActionReference != "" {
		t.Fatalf("interactive administrator invitation projection = %#v", interactive)
	}
	if result, err := manager.ListAdministratorUsers(t.Context(), "ordinary"); result != nil || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("ordinary ListAdministratorUsers() = %#v, %v", result, err)
	}
	if result, err := manager.ListAdministratorUsers(t.Context(), "disabled-administrator"); result != nil || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("disabled ListAdministratorUsers() = %#v, %v", result, err)
	}
	if result, err := manager.ListAdministratorUsers(t.Context(), "missing"); result != nil || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("missing ListAdministratorUsers() = %#v, %v", result, err)
	}
}
