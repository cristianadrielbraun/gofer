package auth

import (
	"database/sql"
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
		Name: "  Invited Person  ", Username: "Invited.Person",
	})
	if err != nil {
		t.Fatal(err)
	}
	if invitation.User.ID != "invited-user-id" || invitation.User.Username != "Invited.Person" ||
		invitation.User.Status != UserStatusPending ||
		invitation.User.IsAdmin || invitation.Name != "Invited Person" ||
		invitation.Token.Token != "private-invitation-token" ||
		invitation.Token.ExpiresAt.Sub(invitation.Token.CreatedAt) != defaultEnrollmentTokenLifetime {
		t.Fatalf("CreateAdministratorUserInvitation() = %#v", invitation)
	}

	var username, usernameNormalized, name, status string
	var authVersion, mfaRequired, isAdmin int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT username, username_normalized, name, status,
		       auth_version, mfa_required, is_admin
		FROM users WHERE id = ?`, invitation.User.ID).Scan(
		&username, &usernameNormalized, &name, &status,
		&authVersion, &mfaRequired, &isAdmin,
	); err != nil {
		t.Fatal(err)
	}
	if username != invitation.User.Username || usernameNormalized != "invited.person" ||
		name != invitation.Name || status != string(UserStatusPending) || authVersion != 1 ||
		mfaRequired != 0 || isAdmin != 0 {
		t.Fatalf("stored invited user = username:%q usernameNormalized:%q name:%q status:%q auth:%d mfa:%d admin:%d",
			username, usernameNormalized, name, status, authVersion, mfaRequired, isAdmin)
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
		Name: "\x00", Username: "x",
	})
	var validationErr *AdministratorUserInvitationValidationError
	if result != nil || !errors.As(err, &validationErr) || len(validationErr.Fields) != 2 ||
		validationErr.Fields["name"] == "" || validationErr.Fields["username"] == "" {
		t.Fatalf("invalid invitation = %#v, %#v", result, err)
	}

	result, err = manager.CreateAdministratorUserInvitation(t.Context(), CreateAdministratorUserInvitationOptions{
		ActorUserID: "administrator", ActorSessionID: sessionID,
		Name: "Another Person", Username: "taken.user",
	})
	validationErr = nil
	if result != nil || !errors.As(err, &validationErr) || validationErr.Fields["username"] == "" {
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

	base := CreateAdministratorUserInvitationOptions{Name: "Person", Username: "person"}
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
		Name: "Rollback Person", Username: "rollback",
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

func TestAdministratorCanRotateAndRevokeInvitationThroughSessionBoundReference(t *testing.T) {
	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{
			"original-token", "original-event",
			"replacement-token", "replacement-event",
			"foreign-reference-event", "revocation-event", "repeat-revocation-event",
		},
		tokens: []string{"original-private-token", "replacement-private-token"},
	})
	insertEnrollmentTokenUser(t, manager, "administrator", UserStatusActive, true, now)
	insertEnrollmentTokenUser(t, manager, "pending", UserStatusPending, false, now)
	sessionID := insertEnrollmentStepUpSession(t, manager, "administrator", now, now)
	original, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
		UserID: "pending", CreatedBy: "administrator", ActorSessionID: sessionID,
		Purpose: EnrollmentTokenPurposeEnrollment,
	})
	if err != nil {
		t.Fatal(err)
	}
	users, err := manager.ListAdministratorUsersForSession(t.Context(), "administrator", sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var actionReference string
	for _, user := range users {
		if user.ID == "pending" {
			actionReference = user.InvitationActionReference
			if user.InvitationState != AdministratorUserInvitationActive {
				t.Fatalf("initial invitation state = %#v", user)
			}
		}
	}
	if !canonicalAdministratorUserInvitationActionReference(actionReference) {
		t.Fatalf("invitation action reference = %q", actionReference)
	}
	replacement, err := manager.RotateAdministratorUserInvitation(t.Context(), RotateAdministratorUserInvitationOptions{
		ActorUserID: "administrator", ActorSessionID: sessionID, ActionReference: actionReference,
	})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.User.ID != "pending" || replacement.Token.Token != "replacement-private-token" ||
		replacement.Token.ID != "replacement-token" || replacement.Token.ExpiresAt.Sub(now) != defaultEnrollmentTokenLifetime {
		t.Fatalf("rotated invitation = %#v", replacement)
	}
	var originalRevoked, replacementRevoked sql.NullTime
	var replacementHash string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT revoked_at FROM user_enrollment_tokens WHERE id = ?`, original.ID,
	).Scan(&originalRevoked); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT token_hash, revoked_at FROM user_enrollment_tokens WHERE id = ?`, replacement.Token.ID,
	).Scan(&replacementHash, &replacementRevoked); err != nil {
		t.Fatal(err)
	}
	if !originalRevoked.Valid || replacementRevoked.Valid ||
		replacementHash != hashToken(replacement.Token.Token) || replacementHash == replacement.Token.Token {
		t.Fatalf("rotated token storage = original revoked:%v replacement revoked:%v hash:%q",
			originalRevoked, replacementRevoked, replacementHash)
	}
	var activeTokens, replacedCount int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM user_enrollment_tokens
		WHERE user_id = 'pending' AND purpose = 'enrollment'
		  AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?`, now,
	).Scan(&activeTokens); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT CAST(json_extract(metadata_json, '$.replaced_count') AS INTEGER)
		FROM auth_events WHERE id = 'replacement-event'`,
	).Scan(&replacedCount); err != nil {
		t.Fatal(err)
	}
	if activeTokens != 1 || replacedCount != 1 {
		t.Fatalf("rotation state = active:%d replaced:%d", activeTokens, replacedCount)
	}

	foreignReference, err := manager.administratorUserInvitationActionReference("different-session", "pending")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RevokeAdministratorUserInvitation(t.Context(), "administrator", sessionID, foreignReference); !errors.Is(err, ErrAdministratorUserInvitationTargetInvalid) {
		t.Fatalf("foreign-session invitation reference = %v", err)
	}
	if err := manager.RevokeAdministratorUserInvitation(t.Context(), "administrator", sessionID, actionReference); err != nil {
		t.Fatal(err)
	}
	if err := manager.RevokeAdministratorUserInvitation(t.Context(), "administrator", sessionID, actionReference); !errors.Is(err, ErrAdministratorUserInvitationNotActive) {
		t.Fatalf("repeat invitation revocation = %v", err)
	}
	var revokedAt sql.NullTime
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT revoked_at FROM user_enrollment_tokens WHERE id = 'replacement-token'`,
	).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	var revokedCount int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT CAST(json_extract(metadata_json, '$.revoked_count') AS INTEGER)
		FROM auth_events WHERE id = 'revocation-event'`,
	).Scan(&revokedCount); err != nil {
		t.Fatal(err)
	}
	if !revokedAt.Valid || revokedCount != 1 {
		t.Fatalf("revoked invitation state = revoked:%v count:%d", revokedAt, revokedCount)
	}
}

func TestAdministratorInvitationLifecycleRequiresRecentManagementStepUp(t *testing.T) {
	now := time.Date(2026, time.August, 20, 12, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"blocked-token", "blocked-event", "blocked-revoke-event"},
		tokens: []string{"blocked-private-token"},
	})
	insertEnrollmentTokenUser(t, manager, "administrator", UserStatusActive, true, now)
	insertEnrollmentTokenUser(t, manager, "pending", UserStatusPending, false, now)
	staleSessionID := insertEnrollmentStepUpSession(t, manager, "administrator", now.Add(-11*time.Minute), now)
	actionReference, err := manager.administratorUserInvitationActionReference(staleSessionID, "pending")
	if err != nil {
		t.Fatal(err)
	}
	if invitation, err := manager.RotateAdministratorUserInvitation(t.Context(), RotateAdministratorUserInvitationOptions{
		ActorUserID: "administrator", ActorSessionID: staleSessionID, ActionReference: actionReference,
	}); invitation != nil || !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale rotation = %#v, %v", invitation, err)
	}
	if err := manager.RevokeAdministratorUserInvitation(t.Context(), "administrator", staleSessionID, actionReference); !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale revocation = %v", err)
	}
	var tokens, events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if tokens != 0 || events != 0 {
		t.Fatalf("stale invitation lifecycle mutated state = tokens:%d events:%d", tokens, events)
	}
}

func TestAdministratorInvitationLifecycleRejectsNonManagementActor(t *testing.T) {
	now := time.Date(2026, time.August, 20, 12, 45, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"blocked-token", "blocked-event", "blocked-revoke-event"},
		tokens: []string{"blocked-private-token"},
	})
	insertEnrollmentTokenUser(t, manager, "ordinary", UserStatusActive, false, now)
	insertEnrollmentTokenUser(t, manager, "pending", UserStatusPending, false, now)
	sessionID := insertEnrollmentStepUpSession(t, manager, "ordinary", now, now)
	actionReference, err := manager.administratorUserInvitationActionReference(sessionID, "pending")
	if err != nil {
		t.Fatal(err)
	}
	if invitation, err := manager.RotateAdministratorUserInvitation(t.Context(), RotateAdministratorUserInvitationOptions{
		ActorUserID: "ordinary", ActorSessionID: sessionID, ActionReference: actionReference,
	}); invitation != nil || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("ordinary-user rotation = %#v, %v", invitation, err)
	}
	if err := manager.RevokeAdministratorUserInvitation(t.Context(), "ordinary", sessionID, actionReference); !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("ordinary-user revocation = %v", err)
	}
	var tokens, events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if tokens != 0 || events != 0 {
		t.Fatalf("ordinary invitation lifecycle mutated state = tokens:%d events:%d", tokens, events)
	}
}

func TestAdministratorInvitationLifecycleRollsBackWhenAuditFails(t *testing.T) {
	now := time.Date(2026, time.August, 20, 13, 0, 0, 0, time.UTC)
	for _, action := range []string{"rotate", "revoke"} {
		t.Run(action, func(t *testing.T) {
			manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
				ids:    []string{"original-token", "original-event", "action-token", "action-event"},
				tokens: []string{"original-private-token", "action-private-token"},
			})
			insertEnrollmentTokenUser(t, manager, "administrator", UserStatusActive, true, now)
			insertEnrollmentTokenUser(t, manager, "pending", UserStatusPending, false, now)
			sessionID := insertEnrollmentStepUpSession(t, manager, "administrator", now, now)
			original, err := manager.IssueEnrollmentToken(t.Context(), IssueEnrollmentTokenOptions{
				UserID: "pending", CreatedBy: "administrator", ActorSessionID: sessionID,
				Purpose: EnrollmentTokenPurposeEnrollment,
			})
			if err != nil {
				t.Fatal(err)
			}
			actionReference, err := manager.administratorUserInvitationActionReference(sessionID, "pending")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.db.Write().ExecContext(t.Context(), `
				CREATE TRIGGER reject_invitation_lifecycle_audit BEFORE INSERT ON auth_events
				BEGIN SELECT RAISE(FAIL, 'audit unavailable'); END`); err != nil {
				t.Fatal(err)
			}
			if action == "rotate" {
				if invitation, err := manager.RotateAdministratorUserInvitation(t.Context(), RotateAdministratorUserInvitationOptions{
					ActorUserID: "administrator", ActorSessionID: sessionID, ActionReference: actionReference,
				}); invitation != nil || err == nil {
					t.Fatalf("audit-failing rotation = %#v, %v", invitation, err)
				}
			} else if err := manager.RevokeAdministratorUserInvitation(t.Context(), "administrator", sessionID, actionReference); err == nil {
				t.Fatal("audit-failing revocation unexpectedly succeeded")
			}
			var active, total int
			if err := manager.db.Read().QueryRowContext(t.Context(), `
				SELECT COUNT(*) FILTER (WHERE used_at IS NULL AND revoked_at IS NULL), COUNT(*)
				FROM user_enrollment_tokens WHERE user_id = 'pending'`,
			).Scan(&active, &total); err != nil {
				t.Fatal(err)
			}
			if active != 1 || total != 1 {
				t.Fatalf("audit rollback token state = active:%d total:%d", active, total)
			}
			var revokedAt sql.NullTime
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT revoked_at FROM user_enrollment_tokens WHERE id = ?`, original.ID).Scan(&revokedAt); err != nil {
				t.Fatal(err)
			}
			if revokedAt.Valid {
				t.Fatalf("audit rollback revoked original token at %v", revokedAt.Time)
			}
		})
	}
}
