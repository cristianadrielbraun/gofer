package auth

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRequiredMFAEnrollmentCreatesFactorAndSessionAndRevokesExistingSessions(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, secureTokenGenerator{})
	insertPasswordLoginUser(
		t, manager, "person", "person", UserStatusActive, false, false, false,
		currentPasswordLoginHash(t), now.Add(-time.Hour),
	)
	existing, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Existing browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE users SET mfa_required = 1 WHERE id = 'person'`); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO recovery_codes (id, user_id, batch_id, code_hash, created_at)
		VALUES ('existing-recovery', 'person', 'existing-batch', ?, ?)`,
		[]byte("existing-recovery-hash"), now.Add(-time.Hour),
	); err != nil {
		t.Fatal(err)
	}

	login, err := manager.AuthenticatePassword(t.Context(), PasswordLoginOptions{
		Identifier: "person", Password: passwordLoginTestPassword,
		Source: "198.51.100.60", UserAgent: "Enrollment browser",
	})
	if err != nil || login == nil || login.Session != nil || login.PreAuthChallenge == nil || !login.MFAEnrollmentRequired {
		t.Fatalf("AuthenticatePassword(required enrollment) = %#v, %v", login, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), existing.Token); err != nil || stored == nil {
		t.Fatalf("existing session during enrollment = %#v, %v", stored, err)
	}
	state, err := manager.GetMFAEnrollmentState(t.Context(), login.PreAuthChallenge.Token, "https://gofer.example")
	if err != nil || state == nil || state.Enrollment == nil || state.TOTPConfirmed || state.RecoveryGenerated {
		t.Fatalf("GetMFAEnrollmentState() = %#v, %v", state, err)
	}
	secret := strings.ReplaceAll(state.Enrollment.ManualKey, " ", "")
	state, err = manager.ConfirmMFAEnrollmentTOTP(
		t.Context(), login.PreAuthChallenge.Token, "https://gofer.example", setupTOTPCode(t, secret, now),
	)
	if err != nil || state == nil || !state.TOTPConfirmed || state.RecoveryGenerated {
		t.Fatalf("ConfirmMFAEnrollmentTOTP() = %#v, %v", state, err)
	}
	batch, err := manager.GenerateMFAEnrollmentRecoveryCodes(
		t.Context(), login.PreAuthChallenge.Token, "https://gofer.example", false,
	)
	if err != nil || batch == nil || !batch.RecoveryGenerated || len(batch.Codes) != setupRecoveryCodeCount {
		t.Fatalf("GenerateMFAEnrollmentRecoveryCodes() = %#v, %v", batch, err)
	}
	session, err := manager.CompleteMFAEnrollment(t.Context(), CompleteMFAEnrollmentOptions{
		Token: login.PreAuthChallenge.Token, Origin: "https://gofer.example",
		BatchID: batch.RecoveryBatchID, Saved: true,
		Source: "198.51.100.60", UserAgent: "Enrollment browser",
	})
	if err != nil || session == nil || session.AssuranceLevel != AssuranceLevelMultiFactor ||
		session.AuthenticationMethod != AuthenticationMethodPassword || session.StepUpMethod != AuthenticationMethodTOTP {
		t.Fatalf("CompleteMFAEnrollment() = %#v, %v", session, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || stored == nil || stored.ID != session.ID {
		t.Fatalf("enrolled session = %#v, %v", stored, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), existing.Token); err != nil || stored != nil {
		t.Fatalf("existing session after enrollment = %#v, %v", stored, err)
	}
	var activeTOTP, recoveryCodes, activeSessions, events int
	var consumedAt sql.NullTime
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM totp_credentials
		WHERE user_id = 'person' AND enabled = 1 AND revoked_at IS NULL`,
	).Scan(&activeTOTP); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM recovery_codes
		WHERE user_id = 'person' AND used_at IS NULL AND revoked_at IS NULL`,
	).Scan(&recoveryCodes); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sessions WHERE user_id = 'person' AND revoked_at IS NULL`,
	).Scan(&activeSessions); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT consumed_at FROM auth_challenges WHERE id = ?`, login.PreAuthChallenge.ID,
	).Scan(&consumedAt); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_events
		WHERE subject_user_id = 'person' AND event_type IN ('credential_changed', 'login_succeeded')`,
	).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if activeTOTP != 1 || recoveryCodes != setupRecoveryCodeCount+1 || activeSessions != 1 || !consumedAt.Valid || events != 2 {
		t.Fatalf("completed enrollment state = TOTP:%d recovery:%d sessions:%d consumed:%v events:%d",
			activeTOTP, recoveryCodes, activeSessions, consumedAt, events)
	}
}

func TestRequiredMFAEnrollmentBecomesInvalidWhenPolicyOrFactorStateChanges(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 30, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		change func(*testing.T, *Manager)
		err    error
	}{
		{
			name: "policy cleared",
			change: func(t *testing.T, manager *Manager) {
				_, err := manager.db.Write().ExecContext(t.Context(), `UPDATE users SET mfa_required = 0 WHERE id = 'person'`)
				if err != nil {
					t.Fatal(err)
				}
			},
			err: ErrMFAEnrollmentInvalid,
		},
		{
			name: "factor enrolled elsewhere",
			change: func(t *testing.T, manager *Manager) {
				insertPolicyTestTOTP(t, manager, "person", now)
			},
			err: ErrMFAEnrollmentStateChanged,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
			insertPasswordLoginUser(
				t, manager, "person", "person", UserStatusActive, false, true, false,
				currentPasswordLoginHash(t), now,
			)
			login, err := manager.AuthenticatePassword(t.Context(), PasswordLoginOptions{
				Identifier: "person", Password: passwordLoginTestPassword,
			})
			if err != nil || login == nil || !login.MFAEnrollmentRequired {
				t.Fatalf("AuthenticatePassword() = %#v, %v", login, err)
			}
			test.change(t, manager)
			if state, err := manager.GetMFAEnrollmentState(t.Context(), login.PreAuthChallenge.Token, "https://gofer.example"); state != nil || !errors.Is(err, test.err) {
				t.Fatalf("GetMFAEnrollmentState(changed) = %#v, %v", state, err)
			}
		})
	}
}

func TestFederatedPrimaryAuthenticationUsesRequiredMFAEnrollmentForFactorlessUser(t *testing.T) {
	now := time.Date(2026, time.August, 24, 13, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "person", false, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE users SET mfa_required = 1 WHERE id = 'person'`); err != nil {
		t.Fatal(err)
	}
	result, err := manager.completeFederatedPrimaryAuthentication(
		t.Context(), "person", "Federated browser", AuthenticationMethodFederatedGoogle,
	)
	if err != nil || result == nil || result.Session != nil || result.PreAuthChallenge == nil || !result.MFAEnrollmentRequired {
		t.Fatalf("completeFederatedPrimaryAuthentication(required enrollment) = %#v, %v", result, err)
	}
	if state, err := manager.GetMFAEnrollmentState(t.Context(), result.PreAuthChallenge.Token, "https://gofer.example"); err != nil || state == nil || state.UserID != "person" {
		t.Fatalf("federated MFA enrollment state = %#v, %v", state, err)
	}
}
