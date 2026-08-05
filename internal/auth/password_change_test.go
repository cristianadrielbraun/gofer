package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const changedPasswordTestValue = "a different excellent local passphrase"

func newPasswordChangeFixture(t *testing.T) (*Manager, *fixedClock, *Session, *Session) {
	t.Helper()
	now := time.Date(2026, time.August, 5, 20, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{
		ids:    []string{"current-session-id", "other-session-id", "changed-session-id", "password-event-id"},
		tokens: []string{"current-session-token", "other-session-token", "changed-session-token"},
	})
	insertPasswordLoginUser(
		t, manager, "person", "Person@Example.com", "Person.Name", UserStatusActive,
		false, false, true, currentPasswordLoginHash(t), now.Add(-time.Hour),
	)
	current, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "original browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatalf("CreateAuthenticatedSession(current) error = %v", err)
	}
	other, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "other browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatalf("CreateAuthenticatedSession(other) error = %v", err)
	}
	clock.now = now.Add(10 * time.Minute)
	return manager, clock, current, other
}

func TestChangePasswordReplacesCredentialAndSessionAtomically(t *testing.T) {
	manager, clock, current, other := newPasswordChangeFixture(t)
	result, err := manager.ChangePassword(t.Context(), PasswordChangeOptions{
		SessionToken:    current.Token,
		CurrentPassword: passwordLoginTestPassword,
		NewPassword:     changedPasswordTestValue,
		UserAgent:       "  changed browser  ",
	})
	if err != nil {
		t.Fatalf("ChangePassword() error = %v", err)
	}
	if result == nil || result.Session == nil || result.RevokedSessions != 1 {
		t.Fatalf("ChangePassword() result = %#v", result)
	}
	changed := result.Session
	if changed.ID != "changed-session-id" || changed.Token != "changed-session-token" || changed.UserID != "person" || changed.UserAgent != "changed browser" {
		t.Fatalf("changed session = %#v", changed)
	}
	if changed.StepUpAt == nil || !changed.StepUpAt.Equal(clock.now) || changed.StepUpMethod != AuthenticationMethodPassword {
		t.Fatalf("changed session step-up = %v/%q, want %v/password", changed.StepUpAt, changed.StepUpMethod, clock.now)
	}
	if changed.AuthenticationMethod != current.AuthenticationMethod || changed.AssuranceLevel != current.AssuranceLevel || !changed.AuthenticatedAt.Equal(current.AuthenticatedAt) || !changed.AbsoluteExpiresAt.Equal(current.AbsoluteExpiresAt) {
		t.Fatalf("changed session did not preserve authentication metadata: %#v", changed)
	}
	if found, err := manager.GetSessionByToken(t.Context(), current.Token); err != nil || found != nil {
		t.Fatalf("old current session = %#v, %v", found, err)
	}
	if found, err := manager.GetSessionByToken(t.Context(), other.Token); err != nil || found != nil {
		t.Fatalf("other session = %#v, %v", found, err)
	}
	if found, err := manager.GetSessionByToken(t.Context(), changed.Token); err != nil || found == nil || found.ID != changed.ID {
		t.Fatalf("rotated session = %#v, %v", found, err)
	}

	var storedHash string
	var mustChange int
	var changedAt time.Time
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT password_hash, must_change, changed_at
		FROM password_credentials WHERE user_id = 'person'`,
	).Scan(&storedHash, &mustChange, &changedAt); err != nil {
		t.Fatalf("query changed credential: %v", err)
	}
	if mustChange != 0 || !changedAt.Equal(clock.now) {
		t.Fatalf("changed credential = hash:%q must_change:%d changed_at:%v", storedHash, mustChange, changedAt)
	}
	if matches, _, err := VerifyPassword(storedHash, changedPasswordTestValue); err != nil || !matches {
		t.Fatalf("new credential verification = %t, %v", matches, err)
	}
	if matches, _, err := VerifyPassword(storedHash, passwordLoginTestPassword); err != nil || matches {
		t.Fatalf("old credential verification = %t, %v", matches, err)
	}

	sessions, err := manager.ListSessions(t.Context(), "person")
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	reasons := map[string]SessionRevocationReason{}
	for _, session := range sessions {
		reasons[session.ID] = session.RevocationReason
	}
	if reasons[current.ID] != SessionRevocationRotation || reasons[other.ID] != SessionRevocationCredentialReset || reasons[changed.ID] != "" {
		t.Fatalf("password-change session reasons = %#v", reasons)
	}

	var eventType string
	var success int
	var actorID, subjectID, sessionID, userAgent, metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT event_type, success, actor_user_id, subject_user_id, session_id,
		       user_agent, metadata_json
		FROM auth_events WHERE id = 'password-event-id'`,
	).Scan(&eventType, &success, &actorID, &subjectID, &sessionID, &userAgent, &metadata); err != nil {
		t.Fatalf("query password-change event: %v", err)
	}
	if eventType != string(AuthEventCredentialChanged) || success != 1 || actorID != "person" || subjectID != "person" || sessionID != changed.ID || userAgent != "changed browser" || metadata != "{}" {
		t.Fatalf("password-change event = type:%q success:%d actor:%q subject:%q session:%q agent:%q metadata:%q", eventType, success, actorID, subjectID, sessionID, userAgent, metadata)
	}
	if strings.Contains(metadata, passwordLoginTestPassword) || strings.Contains(metadata, changedPasswordTestValue) {
		t.Fatal("password-change event exposed credential contents")
	}
}

func TestChangePasswordRejectsInvalidCurrentAndNewPasswordsWithoutSideEffects(t *testing.T) {
	tests := []struct {
		name            string
		currentPassword string
		newPassword     string
		wantError       error
	}{
		{name: "incorrect current password", currentPassword: "incorrect current password", newPassword: changedPasswordTestValue, wantError: ErrCurrentPasswordInvalid},
		{name: "same password", currentPassword: passwordLoginTestPassword, newPassword: passwordLoginTestPassword, wantError: ErrPasswordUnchanged},
		{name: "short new password", currentPassword: passwordLoginTestPassword, newPassword: "too short", wantError: ErrPasswordTooShort},
		{name: "context-derived new password", currentPassword: passwordLoginTestPassword, newPassword: "person.namepassword", wantError: ErrPasswordCommon},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, _, current, other := newPasswordChangeFixture(t)
			var originalHash string
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT password_hash FROM password_credentials WHERE user_id = 'person'`).Scan(&originalHash); err != nil {
				t.Fatal(err)
			}
			result, err := manager.ChangePassword(t.Context(), PasswordChangeOptions{
				SessionToken: current.Token, CurrentPassword: test.currentPassword,
				NewPassword: test.newPassword, UserAgent: "changed browser",
			})
			if result != nil || !errors.Is(err, test.wantError) {
				t.Fatalf("ChangePassword() = %#v, %v, want nil/%v", result, err, test.wantError)
			}
			var storedHash string
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT password_hash FROM password_credentials WHERE user_id = 'person'`).Scan(&storedHash); err != nil {
				t.Fatal(err)
			}
			if storedHash != originalHash {
				t.Fatal("rejected password change replaced the credential")
			}
			for _, session := range []*Session{current, other} {
				if found, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || found == nil {
					t.Fatalf("session %q after rejection = %#v, %v", session.ID, found, err)
				}
			}
			var eventCount int
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&eventCount); err != nil || eventCount != 0 {
				t.Fatalf("auth events after rejection = %d, %v", eventCount, err)
			}
		})
	}
}

func TestChangePasswordRollsBackCredentialAndSessionsWhenEventWriteFails(t *testing.T) {
	manager, _, current, other := newPasswordChangeFixture(t)
	var originalHash string
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT password_hash FROM password_credentials WHERE user_id = 'person'`).Scan(&originalHash); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_password_change_event
		BEFORE INSERT ON auth_events
		BEGIN
			SELECT RAISE(ABORT, 'forced password event failure');
		END`); err != nil {
		t.Fatalf("create event failure trigger: %v", err)
	}

	result, err := manager.ChangePassword(t.Context(), PasswordChangeOptions{
		SessionToken: current.Token, CurrentPassword: passwordLoginTestPassword,
		NewPassword: changedPasswordTestValue, UserAgent: "changed browser",
	})
	if result != nil || err == nil {
		t.Fatalf("ChangePassword() = %#v, %v, want rollback error", result, err)
	}
	var storedHash string
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT password_hash FROM password_credentials WHERE user_id = 'person'`).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash != originalHash {
		t.Fatal("rolled-back password change replaced the credential")
	}
	for _, session := range []*Session{current, other} {
		if found, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || found == nil {
			t.Fatalf("session %q after rollback = %#v, %v", session.ID, found, err)
		}
	}
	var totalSessions int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&totalSessions); err != nil || totalSessions != 2 {
		t.Fatalf("sessions after rollback = %d, %v", totalSessions, err)
	}
}

func TestChangePasswordRequiresActiveSessionWithLocalCredential(t *testing.T) {
	now := time.Date(2026, time.August, 5, 21, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{})
	insertActiveUser(t, manager, "federated", false, now)
	sessionManager := NewManager(manager.config, manager.db)
	session, err := sessionManager.CreateAuthenticatedSession(t.Context(), "federated", "browser", AuthenticationMethodFederatedGoogle, AssuranceLevelSingleFactor)
	if err != nil {
		t.Fatalf("CreateAuthenticatedSession() error = %v", err)
	}
	if hasPassword, err := manager.HasPasswordCredential(t.Context(), "federated"); err != nil || hasPassword {
		t.Fatalf("HasPasswordCredential() = %t, %v", hasPassword, err)
	}
	result, err := manager.ChangePassword(t.Context(), PasswordChangeOptions{
		SessionToken: session.Token, CurrentPassword: passwordLoginTestPassword,
		NewPassword: changedPasswordTestValue,
	})
	if result != nil || !errors.Is(err, ErrCurrentPasswordInvalid) {
		t.Fatalf("ChangePassword() = %#v, %v", result, err)
	}
}
