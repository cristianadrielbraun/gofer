package auth

import (
	"errors"
	"testing"
	"time"
)

func TestPasskeyTOTPPreferenceAppliesOnlyToPasskeyAssurance(t *testing.T) {
	normal := resolveAuthenticationPolicy(1, false, false, false)
	normal.RequiresMFA = true // enrolled TOTP, but no administrator mandate
	if normal.allowsAssurance(AssuranceLevelPhishingResistant) {
		t.Fatal("default allowed passkey without required TOTP")
	}
	normal.PasskeyTOTPRequired = false
	if !normal.allowsAssurance(AssuranceLevelPhishingResistant) {
		t.Fatal("verified passkey not accepted when extra TOTP disabled")
	}
	if normal.allowsAssurance(AssuranceLevelSingleFactor) {
		t.Fatal("password-only assurance incorrectly accepted")
	}
	if !normal.allowsAssurance(AssuranceLevelMultiFactor) {
		t.Fatal("TOTP-authenticated session rejected")
	}
	managed := resolveAuthenticationPolicy(1, false, false, true)
	managed.PasskeyTOTPRequired = false
	if managed.allowsAssurance(AssuranceLevelPhishingResistant) {
		t.Fatal("administrator-enforced TOTP was bypassed")
	}
}

func TestPasskeyTOTPPreferenceChangeInvalidatesSessionsAndRespectsAdminPolicy(t *testing.T) {
	now := time.Date(2026, 8, 9, 15, 0, 0, 0, time.UTC)
	m, _, _, session := prepareMFAManagement(t, now, true)
	if _, err := m.db.Write().ExecContext(t.Context(), `UPDATE users SET user_type='webmail', is_admin=0, mfa_required=0 WHERE id=?`, session.UserID); err != nil {
		t.Fatal(err)
	}
	previousToken := session.Token
	rotated, err := m.SetPasskeyTOTPRequired(t.Context(), previousToken, false, "Settings Browser")
	if err != nil || rotated == nil {
		t.Fatalf("SetPasskeyTOTPRequired() = %#v, %v", rotated, err)
	}
	if old, err := m.GetSessionByToken(t.Context(), previousToken); err != nil || old != nil {
		t.Fatalf("old session retained: %#v, %v", old, err)
	}
	summary, err := m.GetSecurityFactorSummary(t.Context(), rotated.Token)
	if err != nil || summary.PasskeyTOTPRequired || summary.PasskeyTOTPLocked {
		t.Fatalf("security settings = %#v, %v", summary, err)
	}
	var events int
	if err := m.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events WHERE subject_user_id=? AND event_type=? AND metadata_json LIKE '%passkey_totp_required%'`, session.UserID, AuthEventSecurityPolicyChanged).Scan(&events); err != nil || events != 1 {
		t.Fatalf("policy change audit = %d, %v", events, err)
	}
	if _, err := m.db.Write().ExecContext(t.Context(), `UPDATE users SET mfa_required=1 WHERE id=?`, session.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetPasskeyTOTPRequired(t.Context(), rotated.Token, false, "Settings Browser"); !errors.Is(err, ErrPasskeyTOTPPolicyEnforced) {
		t.Fatalf("admin TOTP requirement not enforced: %v", err)
	}
	locked, err := m.GetSecurityFactorSummary(t.Context(), rotated.Token)
	if err != nil || !locked.PasskeyTOTPLocked || !locked.PasskeyTOTPRequired {
		t.Fatalf("admin-enforced policy = %#v, %v", locked, err)
	}
}

func TestPasskeyLoginWithTOTPEnrolledHonorsExtraFactorSetting(t *testing.T) {
	for _, tt := range []struct {
		name       string
		required   bool
		expectCode bool
	}{
		{"extra TOTP enabled", true, true},
		{"extra TOTP disabled", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newPasskeyAuthenticationFixture(t, 7)
			enabled := 0
			if tt.required {
				enabled = 1
			}
			if _, err := fixture.manager.db.Write().ExecContext(t.Context(), `UPDATE users SET passkey_totp_required=? WHERE id=?`, enabled, fixture.userID); err != nil {
				t.Fatal(err)
			}
			if _, err := fixture.manager.db.Write().ExecContext(t.Context(), `INSERT INTO totp_credentials (id,user_id,encrypted_seed,key_version,enabled,created_at) VALUES ('test-totp',?,?,1,1,?)`, fixture.userID, []byte{1}, time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)); err != nil {
				t.Fatal(err)
			}
			started, err := fixture.manager.StartPasskeyLogin(t.Context(), "", "https://gofer.example", "source")
			if err != nil {
				t.Fatal(err)
			}
			result, err := fixture.manager.FinishPasskeyLogin(t.Context(), started.Challenge.Token, "https://gofer.example", []byte(`{}`), "source", "Browser")
			if err != nil {
				t.Fatal(err)
			}
			if (result.PreAuthChallenge != nil) != tt.expectCode {
				t.Fatalf("TOTP continuation = %#v; expected %v", result.PreAuthChallenge, tt.expectCode)
			}
			if !tt.expectCode {
				if result.Session == nil || result.Session.AssuranceLevel != AssuranceLevelPhishingResistant {
					t.Fatalf("passkey session = %#v", result.Session)
				}
				saved, err := fixture.manager.GetSessionByToken(t.Context(), result.Session.Token)
				if err != nil || saved == nil {
					t.Fatalf("passkey session invalid: %v", err)
				}
			}
		})
	}
}
