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
		WHERE id = 'ordinary'`); err != nil {
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
		users[2].Username != "Bravo" || users[2].Email != "ordinary@example.com" || users[2].IsAdmin ||
		users[3].Username != "Zulu" || users[3].Status != UserStatusDisabled || !users[3].IsAdmin {
		t.Fatalf("administrator user projection = %#v", users)
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
