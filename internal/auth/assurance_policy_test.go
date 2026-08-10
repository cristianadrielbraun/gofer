package auth

import (
	"bytes"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestAuthenticationPolicyRequiresStrongAssuranceAndStepUp(t *testing.T) {
	normal := resolveAuthenticationPolicy(7, false, false, false)
	administrator := resolveAuthenticationPolicy(7, false, true, false)
	perUserMFA := resolveAuthenticationPolicy(7, true, false, false)
	instanceMFA := resolveAuthenticationPolicy(7, false, false, true)

	for _, assurance := range []AssuranceLevel{
		AssuranceLevelLegacy,
		AssuranceLevelSingleFactor,
		AssuranceLevelMultiFactor,
		AssuranceLevelPhishingResistant,
	} {
		if !normal.allowsAssurance(assurance) {
			t.Fatalf("normal policy rejected valid assurance %q", assurance)
		}
	}
	for name, policy := range map[string]authenticationPolicy{
		"administrator": administrator,
		"per-user MFA":  perUserMFA,
		"instance MFA":  instanceMFA,
	} {
		if policy.allowsAssurance(AssuranceLevelLegacy) || policy.allowsAssurance(AssuranceLevelSingleFactor) ||
			!policy.allowsAssurance(AssuranceLevelMultiFactor) || !policy.allowsAssurance(AssuranceLevelPhishingResistant) {
			t.Fatalf("%s assurance policy = %#v", name, policy)
		}
		if policy.allowsStepUpMethod(AuthenticationMethodPassword) ||
			policy.allowsStepUpMethod(AuthenticationMethodFederatedGoogle) ||
			!policy.allowsStepUpMethod(AuthenticationMethodTOTP) ||
			!policy.allowsStepUpMethod(AuthenticationMethodRecoveryCode) ||
			!policy.allowsStepUpMethod(AuthenticationMethodPasskey) {
			t.Fatalf("%s step-up policy = %#v", name, policy)
		}
	}
	if normal.allowsStepUpMethod(AuthenticationMethodLegacy) ||
		!normal.allowsStepUpMethod(AuthenticationMethodPassword) ||
		!normal.allowsStepUpMethod(AuthenticationMethodFederatedGoogle) {
		t.Fatalf("normal step-up policy = %#v", normal)
	}
}

func TestAuthenticatedSessionIssuanceEnforcesCurrentPolicy(t *testing.T) {
	now := time.Date(2026, time.August, 9, 20, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		isAdmin     bool
		mfaRequired bool
		instanceMFA bool
		assurance   AssuranceLevel
		wantError   bool
	}{
		{name: "normal legacy session", assurance: AssuranceLevelLegacy},
		{name: "administrator legacy session", isAdmin: true, assurance: AssuranceLevelLegacy, wantError: true},
		{name: "administrator single-factor session", isAdmin: true, assurance: AssuranceLevelSingleFactor, wantError: true},
		{name: "administrator multi-factor session", isAdmin: true, assurance: AssuranceLevelMultiFactor},
		{name: "per-user single-factor session", mfaRequired: true, assurance: AssuranceLevelSingleFactor, wantError: true},
		{name: "per-user phishing-resistant session", mfaRequired: true, assurance: AssuranceLevelPhishingResistant},
		{name: "instance single-factor session", instanceMFA: true, assurance: AssuranceLevelSingleFactor, wantError: true},
		{name: "instance multi-factor session", instanceMFA: true, assurance: AssuranceLevelMultiFactor},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
				ids: []string{"policy-session"}, tokens: []string{"policy-session-token"},
			})
			insertActiveUser(t, manager, "person", test.isAdmin, now)
			if test.mfaRequired {
				if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE users SET mfa_required = 1 WHERE id = 'person'`); err != nil {
					t.Fatal(err)
				}
			}
			if test.instanceMFA {
				if _, err := manager.db.Write().ExecContext(t.Context(), `
					INSERT INTO auth_system_state (id, initialized, mfa_policy)
					VALUES (1, 1, 'all_users')`); err != nil {
					t.Fatal(err)
				}
			}
			method := AuthenticationMethodPassword
			if test.assurance == AssuranceLevelLegacy {
				method = AuthenticationMethodLegacy
			} else if test.assurance == AssuranceLevelPhishingResistant {
				method = AuthenticationMethodPasskey
			}
			session, err := manager.CreateAuthenticatedSession(t.Context(), "person", "Policy Browser", method, test.assurance)
			if test.wantError {
				if session != nil || !errors.Is(err, ErrAuthenticationPolicyNotSatisfied) {
					t.Fatalf("CreateAuthenticatedSession() = %#v, %v, want policy rejection", session, err)
				}
			} else if err != nil || session == nil || session.AssuranceLevel != test.assurance {
				t.Fatalf("CreateAuthenticatedSession() = %#v, %v", session, err)
			}
			var sessionCount int
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&sessionCount); err != nil {
				t.Fatal(err)
			}
			wantCount := 1
			if test.wantError {
				wantCount = 0
			}
			if sessionCount != wantCount {
				t.Fatalf("persisted sessions = %d, want %d", sessionCount, wantCount)
			}
		})
	}
}

func TestSessionLookupAppliesPolicyChangesImmediately(t *testing.T) {
	now := time.Date(2026, time.August, 9, 20, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "person", false, now)
	weak, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Password Browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE users SET mfa_required = 1 WHERE id = 'person'`); err != nil {
		t.Fatal(err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), weak.Token); err != nil || stored != nil {
		t.Fatalf("weak session after MFA policy change = %#v, %v", stored, err)
	}
	if rotated, err := manager.RotateSession(t.Context(), weak.Token, "Rotated Password Browser"); rotated != nil || !errors.Is(err, ErrSessionNotActive) {
		t.Fatalf("RotateSession(weak policy) = %#v, %v", rotated, err)
	}
	var weakStillActive int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sessions WHERE id = ? AND revoked_at IS NULL`, weak.ID,
	).Scan(&weakStillActive); err != nil {
		t.Fatal(err)
	}
	if weakStillActive != 1 {
		t.Fatalf("weak session active rows after rejected rotation = %d, want 1", weakStillActive)
	}
	strong, err := manager.CreateAuthenticatedSession(
		t.Context(), "person", "Passkey Browser", AuthenticationMethodPasskey, AssuranceLevelPhishingResistant,
	)
	if err != nil {
		t.Fatal(err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), strong.Token); err != nil || stored == nil || stored.ID != strong.ID {
		t.Fatalf("strong session under MFA policy = %#v, %v", stored, err)
	}
}

func TestRecentSecurityStepUpUsesCurrentPolicyAndRejectsClockSkew(t *testing.T) {
	now := time.Date(2026, time.August, 9, 21, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	session, err := manager.CreateAuthenticatedSession(
		t.Context(), "administrator", "Security Browser", AuthenticationMethodPassword, AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	sessionToken := session.Token
	if steppedUp, err := manager.RecordSessionStepUp(t.Context(), session.UserID, session.ID, AuthenticationMethodPassword); err != nil || !steppedUp {
		t.Fatalf("RecordSessionStepUp(password) = %t, %v", steppedUp, err)
	}
	session, err = manager.GetSessionByToken(t.Context(), sessionToken)
	if err != nil || session == nil {
		t.Fatalf("load password-stepped session = %#v, %v", session, err)
	}
	if err := manager.requireRecentSecurityStepUp(t.Context(), session, now); !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("required-account password step-up error = %v", err)
	}
	if steppedUp, err := manager.RecordSessionStepUp(t.Context(), session.UserID, session.ID, AuthenticationMethodTOTP); err != nil || !steppedUp {
		t.Fatalf("RecordSessionStepUp(TOTP) = %t, %v", steppedUp, err)
	}
	session, err = manager.GetSessionByToken(t.Context(), sessionToken)
	if err != nil || session == nil {
		t.Fatalf("load TOTP-stepped session = %#v, %v", session, err)
	}
	if err := manager.requireRecentSecurityStepUp(t.Context(), session, now); err != nil {
		t.Fatalf("required-account TOTP step-up error = %v", err)
	}

	policy := resolveAuthenticationPolicy(session.AuthVersion, true, false, false)
	for name, stepUpAt := range map[string]time.Time{
		"stale":  now.Add(-securityStepUpMaximumAge - time.Nanosecond),
		"future": now.Add(time.Nanosecond),
	} {
		t.Run(name, func(t *testing.T) {
			copySession := *session
			copySession.StepUpAt = &stepUpAt
			if hasRecentSecurityStepUp(&copySession, policy, now) {
				t.Fatalf("%s step-up was accepted at %v", name, stepUpAt)
			}
		})
	}
}

func TestMFAContinuationEncryptsAndBindsPrimaryAuthentication(t *testing.T) {
	now := time.Date(2026, time.August, 9, 21, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids:    []string{"first-mfa", "second-mfa"},
		tokens: []string{"first-mfa-token", "second-mfa-token"},
	})
	insertActiveUser(t, manager, "administrator", true, now)
	first, draft, err := manager.prepareMFAContinuation(
		"administrator", 1, AuthenticationMethodFederatedGoogle, "https://gofer.example", now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(first.PayloadCiphertext, []byte("federated_google")) ||
		bytes.Contains(first.PayloadCiphertext, []byte("auth_version")) {
		t.Fatalf("MFA continuation contains plaintext draft: %q", first.PayloadCiphertext)
	}
	insert := func(challenge *PreAuthChallenge, at time.Time) {
		t.Helper()
		if err := manager.runSecurityTransition(t.Context(), SecurityTransitionLoginCompletion, func(tx *sql.Tx) error {
			return manager.insertMFAContinuation(t.Context(), tx, challenge, at)
		}); err != nil {
			t.Fatalf("insertMFAContinuation() error = %v", err)
		}
	}
	insert(first, now)
	loaded, loadedDraft, err := manager.readMFAContinuation(t.Context(), first.Token, first.Origin)
	if err != nil || loaded == nil || loaded.ID != first.ID || !sameMFAContinuationDraft(loadedDraft, draft) ||
		loadedDraft.PrimaryMethod != AuthenticationMethodFederatedGoogle {
		t.Fatalf("readMFAContinuation() = %#v, %#v, %v", loaded, loadedDraft, err)
	}
	wrongBinding := *loaded
	wrongBinding.Origin = "https://other.example"
	if _, err := manager.decryptMFAContinuationDraft(&wrongBinding, loaded.PayloadCiphertext); !errors.Is(err, ErrMFAContinuationInvalid) {
		t.Fatalf("cross-origin draft decryption error = %v", err)
	}

	second, _, err := manager.prepareMFAContinuation(
		"administrator", 1, AuthenticationMethodPassword, "https://gofer.example", now.Add(time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	insert(second, now.Add(time.Second))
	var consumedAt sql.NullTime
	var retiredPayload []byte
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT consumed_at, payload_ciphertext FROM auth_challenges WHERE id = ?`, first.ID,
	).Scan(&consumedAt, &retiredPayload); err != nil {
		t.Fatal(err)
	}
	if !consumedAt.Valid || retiredPayload != nil {
		t.Fatalf("replaced MFA continuation = consumed:%v payload:%q", consumedAt, retiredPayload)
	}
	if active, err := manager.GetActiveMFAChallenge(t.Context(), first.Token, first.Origin); err != nil || active != nil {
		t.Fatalf("replaced GetActiveMFAChallenge() = %#v, %v", active, err)
	}
	active, err := manager.GetActiveMFAChallenge(t.Context(), second.Token, second.Origin)
	if err != nil || active == nil || active.ID != second.ID {
		t.Fatalf("current GetActiveMFAChallenge() = %#v, %v", active, err)
	}

	tampered := append([]byte(nil), second.PayloadCiphertext...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE auth_challenges SET payload_ciphertext = ? WHERE id = ?`, tampered, second.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.readMFAContinuation(t.Context(), second.Token, second.Origin); !errors.Is(err, ErrMFAContinuationInvalid) {
		t.Fatalf("tampered readMFAContinuation() error = %v", err)
	}
}

func TestFederatedPrimaryAuthenticationUsesCurrentAssurancePolicy(t *testing.T) {
	now := time.Date(2026, time.August, 9, 22, 0, 0, 0, time.UTC)
	t.Run("normal user receives a single-factor session", func(t *testing.T) {
		manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
		insertActiveUser(t, manager, "person", false, now)
		result, err := manager.completeFederatedPrimaryAuthentication(
			t.Context(), "person", "Google Browser", AuthenticationMethodFederatedGoogle,
		)
		if err != nil || result == nil || result.Session == nil || result.PreAuthChallenge != nil {
			t.Fatalf("completeFederatedPrimaryAuthentication() = %#v, %v", result, err)
		}
		if result.Session.AuthenticationMethod != AuthenticationMethodFederatedGoogle ||
			result.Session.AssuranceLevel != AssuranceLevelSingleFactor || result.Session.StepUpAt == nil ||
			result.Session.StepUpMethod != AuthenticationMethodFederatedGoogle {
			t.Fatalf("federated session = %#v", result.Session)
		}
		if stored, err := manager.GetSessionByToken(t.Context(), result.Session.Token); err != nil || stored == nil {
			t.Fatalf("stored federated session = %#v, %v", stored, err)
		}
	})

	for _, test := range []struct {
		name        string
		isAdmin     bool
		mfaRequired bool
		instanceMFA bool
	}{
		{name: "administrator", isAdmin: true},
		{name: "per-user MFA", mfaRequired: true},
		{name: "instance MFA", instanceMFA: true},
	} {
		t.Run(test.name+" continues to local MFA", func(t *testing.T) {
			manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
			insertActiveUser(t, manager, "person", test.isAdmin, now)
			if test.mfaRequired {
				if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE users SET mfa_required = 1 WHERE id = 'person'`); err != nil {
					t.Fatal(err)
				}
			}
			if test.instanceMFA {
				if _, err := manager.db.Write().ExecContext(t.Context(), `
					INSERT INTO auth_system_state (id, initialized, mfa_policy)
					VALUES (1, 1, 'all_users')`); err != nil {
					t.Fatal(err)
				}
			}
			result, err := manager.completeFederatedPrimaryAuthentication(
				t.Context(), "person", "Google Browser", AuthenticationMethodFederatedGoogle,
			)
			if err != nil || result == nil || result.Session != nil || result.PreAuthChallenge == nil {
				t.Fatalf("completeFederatedPrimaryAuthentication() = %#v, %v", result, err)
			}
			var sessionCount int
			var payload []byte
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&sessionCount); err != nil {
				t.Fatal(err)
			}
			if err := manager.db.Read().QueryRowContext(t.Context(), `
				SELECT payload_ciphertext FROM auth_challenges WHERE id = ?`, result.PreAuthChallenge.ID,
			).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			if sessionCount != 0 || len(payload) == 0 || bytes.Contains(payload, []byte("federated_google")) {
				t.Fatalf("required federated result = sessions:%d payload:%q", sessionCount, payload)
			}
		})
	}
}

func TestFederatedPrimaryCompletesTOTPWithBoundAuthenticationMethod(t *testing.T) {
	now := time.Date(2026, time.August, 9, 22, 30, 0, 0, time.UTC)
	manager, secret, _ := prepareTOTPLoginManager(t, now, secureTokenGenerator{})
	result, err := manager.completeFederatedPrimaryAuthentication(
		t.Context(), totpLoginTestUserID, "Google Browser", AuthenticationMethodFederatedGoogle,
	)
	if err != nil || result == nil || result.Session != nil || result.PreAuthChallenge == nil {
		t.Fatalf("completeFederatedPrimaryAuthentication() = %#v, %v", result, err)
	}
	session, err := manager.CompleteTOTPLogin(t.Context(), TOTPLoginOptions{
		Token: result.PreAuthChallenge.Token, Code: setupTOTPCode(t, secret, now),
		Origin: totpLoginTestOrigin, Source: "198.51.100.91", UserAgent: "Google plus TOTP Browser",
	})
	if err != nil || session == nil {
		t.Fatalf("CompleteTOTPLogin() = %#v, %v", session, err)
	}
	if session.AuthenticationMethod != AuthenticationMethodFederatedGoogle ||
		session.AssuranceLevel != AssuranceLevelMultiFactor || session.StepUpMethod != AuthenticationMethodTOTP {
		t.Fatalf("federated TOTP session = %#v", session)
	}
	var metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT metadata_json FROM auth_events
		WHERE subject_user_id = ? AND event_type = ? AND success = 1`,
		totpLoginTestUserID, AuthEventLoginSucceeded,
	).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	if metadata != `{"factor":"totp","primary":"federated_google"}` {
		t.Fatalf("federated TOTP event metadata = %q", metadata)
	}
}

func TestInstanceMFAPolicyCompletesOrdinaryPasswordLoginWithTOTP(t *testing.T) {
	now := time.Date(2026, time.August, 9, 22, 45, 0, 0, time.UTC)
	manager, secret, _ := prepareTOTPLoginManager(t, now, secureTokenGenerator{})
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE users SET is_admin = 0, mfa_required = 0 WHERE id = ?`, totpLoginTestUserID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (id, initialized, mfa_policy)
		VALUES (1, 1, 'all_users')`); err != nil {
		t.Fatal(err)
	}
	primary, err := manager.AuthenticatePassword(t.Context(), PasswordLoginOptions{
		Identifier: totpLoginTestUserID, Password: passwordLoginTestPassword,
		Source: "198.51.100.93", UserAgent: "Global MFA password browser",
	})
	if err != nil || primary == nil || primary.Session != nil || primary.PreAuthChallenge == nil {
		t.Fatalf("AuthenticatePassword(instance MFA) = %#v, %v", primary, err)
	}
	session, err := manager.CompleteTOTPLogin(t.Context(), TOTPLoginOptions{
		Token: primary.PreAuthChallenge.Token, Code: setupTOTPCode(t, secret, now),
		Origin: totpLoginTestOrigin, Source: "198.51.100.93", UserAgent: "Global MFA browser",
	})
	if err != nil || session == nil || session.AssuranceLevel != AssuranceLevelMultiFactor ||
		session.AuthenticationMethod != AuthenticationMethodPassword || session.StepUpMethod != AuthenticationMethodTOTP {
		t.Fatalf("CompleteTOTPLogin(instance MFA) = %#v, %v", session, err)
	}
}

func TestMFACompletionRejectsChangedAuthenticationPolicy(t *testing.T) {
	now := time.Date(2026, time.August, 9, 23, 0, 0, 0, time.UTC)
	manager, secret, _ := prepareTOTPLoginManager(t, now, secureTokenGenerator{})
	result, err := manager.completeFederatedPrimaryAuthentication(
		t.Context(), totpLoginTestUserID, "Google Browser", AuthenticationMethodFederatedGoogle,
	)
	if err != nil || result == nil || result.PreAuthChallenge == nil {
		t.Fatalf("completeFederatedPrimaryAuthentication() = %#v, %v", result, err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE users SET auth_version = auth_version + 1 WHERE id = ?`, totpLoginTestUserID,
	); err != nil {
		t.Fatal(err)
	}
	session, err := manager.CompleteTOTPLogin(t.Context(), TOTPLoginOptions{
		Token: result.PreAuthChallenge.Token, Code: setupTOTPCode(t, secret, now),
		Origin: totpLoginTestOrigin, Source: "198.51.100.92", UserAgent: "Changed Policy Browser",
	})
	if session != nil || !errors.Is(err, ErrTOTPLoginChallengeInvalid) {
		t.Fatalf("CompleteTOTPLogin(changed policy) = %#v, %v", session, err)
	}
	var sessionCount int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&sessionCount); err != nil {
		t.Fatal(err)
	}
	if sessionCount != 0 {
		t.Fatalf("sessions after changed-policy completion = %d", sessionCount)
	}
}
