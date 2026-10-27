package auth

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestLocalInvitationIsRedeemableAndRecordedAsLocalOperator(t *testing.T) {
	now := time.Date(2026, time.October, 2, 9, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"invited-user-id", "invitation-token-id", "invitation-event-id", "enrollment-completed-event"},
		tokens: []string{"private-invitation-token"},
	})
	insertEnrollmentTokenUser(t, manager, "owner", UserStatusActive, true, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (id, initialized, owner_user_id, initialized_at, cutover_version)
		VALUES (1, 1, 'owner', ?, 1)`, now); err != nil {
		t.Fatal(err)
	}

	invitation, err := manager.CreateUserInvitationLocally(t.Context(), " Invited Person ", "Invited.Person", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if invitation.User.ID != "invited-user-id" || invitation.Name != "Invited Person" ||
		invitation.Token.CreatedBy != "" || invitation.Token.ExpiresAt != now.Add(2*time.Hour) {
		t.Fatalf("CreateUserInvitationLocally() = %#v", invitation)
	}
	var actorID, sessionID sql.NullString
	var reason string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT actor_user_id, session_id, reason FROM auth_events WHERE id = 'invitation-event-id'`,
	).Scan(&actorID, &sessionID, &reason); err != nil {
		t.Fatal(err)
	}
	if actorID.Valid || sessionID.Valid || reason != string(AuthEventReasonLocalOperator) {
		t.Fatalf("local invitation event = actor:%v session:%v reason:%q", actorID, sessionID, reason)
	}

	result, err := manager.RedeemEnrollmentToken(t.Context(), RedeemEnrollmentTokenOptions{
		Token: "private-invitation-token", NewPassword: redemptionTestPassword, Purpose: EnrollmentTokenPurposeEnrollment,
	})
	if err != nil {
		t.Fatalf("RedeemEnrollmentToken() error = %v", err)
	}
	if result.UserID != "invited-user-id" {
		t.Fatalf("redeemed user = %q", result.UserID)
	}
}

func TestLocalInvitationRequiresCompletedSetup(t *testing.T) {
	now := time.Date(2026, time.October, 2, 9, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"invited-user-id", "invitation-token-id", "invitation-event-id"},
		tokens: []string{"private-invitation-token"},
	})
	if _, err := manager.CreateUserInvitationLocally(t.Context(), "Invited Person", "invited", 0); !errors.Is(err, ErrLocalInvitationSetupIncomplete) {
		t.Fatalf("CreateUserInvitationLocally() before setup error = %v", err)
	}
	var users int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM users`).Scan(&users); err != nil || users != 0 {
		t.Fatalf("users after refused local invitation = %d, %v", users, err)
	}
}
