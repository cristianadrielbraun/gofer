package auth

import (
	"encoding/json"
	"testing"
	"time"
)

// An administrator's individual requirement can accept a verified passkey as
// the second step. Each row signs in with the passkey and checks what follows.
func TestPasskeyLoginUnderAnIndividualRequirement(t *testing.T) {
	for _, tt := range []struct {
		name          string
		passkeyCounts bool
		everyone      bool
		hasApp        bool
		askAfterKey   bool
		want          string // "session", "code" or "set up the app"
	}{
		{"passkey counts, no app", true, false, false, true, "session"},
		{"passkey counts, app, person asks for the code too", true, false, true, true, "code"},
		{"passkey counts, app, person doesn't", true, false, true, false, "session"},
		{"app only, no app yet", false, false, false, false, "set up the app"},
		{"app only, app, person doesn't ask", false, false, true, false, "code"},
		{"everyone must use the app", true, true, true, false, "code"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newPasskeyAuthenticationFixture(t, 7)
			db := fixture.manager.db.Write()
			if _, err := db.ExecContext(t.Context(), `UPDATE users SET mfa_required = 1, mfa_passkey_counts = ?, passkey_totp_required = ? WHERE id = ?`,
				boolInt(tt.passkeyCounts), boolInt(tt.askAfterKey), fixture.userID); err != nil {
				t.Fatal(err)
			}
			if tt.hasApp {
				if _, err := db.ExecContext(t.Context(), `INSERT INTO totp_credentials (id, user_id, encrypted_seed, key_version, enabled, created_at) VALUES ('second-step-totp', ?, ?, 1, 1, ?)`,
					fixture.userID, []byte{1}, time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)); err != nil {
					t.Fatal(err)
				}
			}
			if tt.everyone {
				if _, err := db.ExecContext(t.Context(), `INSERT INTO auth_system_state (id, initialized, mfa_policy) VALUES (1, 1, 'all_users')`); err != nil {
					t.Fatal(err)
				}
			}
			started, err := fixture.manager.StartPasskeyLogin(t.Context(), "", "https://gofer.example", "source")
			if err != nil {
				t.Fatal(err)
			}
			result, err := fixture.manager.FinishPasskeyLogin(t.Context(), started.Challenge.Token, "https://gofer.example", []byte(`{}`), "source", "Browser")
			if err != nil {
				t.Fatal(err)
			}
			got := "session"
			if result.PreAuthChallenge != nil {
				got = "code"
				if result.MFAEnrollmentRequired {
					got = "set up the app"
				}
			}
			if got != tt.want {
				t.Fatalf("after the passkey: %s, want %s (%#v)", got, tt.want, result)
			}
			if got == "session" {
				if result.Session == nil || result.Session.AssuranceLevel != AssuranceLevelPhishingResistant {
					t.Fatalf("passkey session = %#v", result.Session)
				}
				// The session must also pass every later check, not only sign-in.
				if saved, err := fixture.manager.GetSessionByToken(t.Context(), result.Session.Token); err != nil || saved == nil {
					t.Fatalf("passkey session no longer valid: %#v, %v", saved, err)
				}
			}
		})
	}
}

func TestAdministratorChoosesWhetherAPasskeyCounts(t *testing.T) {
	now := time.Date(2026, time.August, 24, 10, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "person", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	admin := createPolicyAdministratorSession(t, manager)
	set := func(required, passkey bool) *SetAdministratorUserMFAPolicyResult {
		t.Helper()
		result, err := manager.SetAdministratorUserMFAPolicy(t.Context(), SetAdministratorUserMFAPolicyOptions{
			ActorUserID: admin.UserID, ActorSessionID: admin.ID, TargetUserID: "person",
			Required: required, PasskeyCounts: passkey,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	stored := func() (int, int) {
		t.Helper()
		var required, passkey int
		if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT mfa_required, mfa_passkey_counts FROM users WHERE id = 'person'`).Scan(&required, &passkey); err != nil {
			t.Fatal(err)
		}
		return required, passkey
	}

	if result := set(true, true); !result.Changed || !result.Required || !result.PasskeyCounts {
		t.Fatalf("require with passkey = %#v", result)
	}
	if required, passkey := stored(); required != 1 || passkey != 1 {
		t.Fatalf("stored = %d, %d", required, passkey)
	}
	if result := set(true, true); result.Changed {
		t.Fatalf("same choice again changed something: %#v", result)
	}
	// Switching only the passkey choice is a change of its own.
	if result := set(true, false); !result.Changed || result.PasskeyCounts {
		t.Fatalf("require the app only = %#v", result)
	}
	if required, passkey := stored(); required != 1 || passkey != 0 {
		t.Fatalf("stored after app only = %d, %d", required, passkey)
	}
	set(true, true)
	// Clearing the requirement also clears the passkey choice, so a later
	// requirement starts from what the administrator picks then.
	if result := set(false, true); !result.Changed || result.Required || result.PasskeyCounts {
		t.Fatalf("clear = %#v", result)
	}
	if required, passkey := stored(); required != 0 || passkey != 0 {
		t.Fatalf("stored after clear = %d, %d", required, passkey)
	}

	var metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT metadata_json FROM auth_events
		WHERE subject_user_id = 'person' AND event_type = ? ORDER BY rowid LIMIT 1 OFFSET 1`,
		AuthEventSecurityPolicyChanged).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	var change struct {
		From, To, PasskeyFrom, PasskeyTo bool
	}
	if err := json.Unmarshal([]byte(metadata), &struct {
		From        *bool `json:"from"`
		To          *bool `json:"to"`
		PasskeyFrom *bool `json:"passkey_from"`
		PasskeyTo   *bool `json:"passkey_to"`
	}{&change.From, &change.To, &change.PasskeyFrom, &change.PasskeyTo}); err != nil {
		t.Fatal(err)
	}
	if !change.From || !change.To || !change.PasskeyFrom || change.PasskeyTo {
		t.Fatalf("audit of the switch to app only = %s", metadata)
	}
}

func TestAppCanGoWhenTheRequirementAcceptsAKeptPasskey(t *testing.T) {
	for _, tt := range []struct {
		name          string
		passkeyCounts bool
		want          bool
	}{
		{"app only", false, false},
		{"passkey counts", true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newPasskeyAuthenticationFixture(t, 7)
			db := fixture.manager.db.Write()
			if _, err := db.ExecContext(t.Context(), `UPDATE users SET mfa_required = 1, mfa_passkey_counts = ? WHERE id = ?`, boolInt(tt.passkeyCounts), fixture.userID); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), `INSERT INTO totp_credentials (id, user_id, encrypted_seed, key_version, enabled, created_at) VALUES ('second-step-totp', ?, ?, 1, 1, ?)`,
				fixture.userID, []byte{1}, time.Date(2026, 8, 9, 18, 0, 0, 0, time.UTC)); err != nil {
				t.Fatal(err)
			}
			tx, err := db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			canRemove, err := canRemoveAuthenticatorInTransaction(t.Context(), tx, fixture.userID, "gofer.example", federatedLoginAvailability{}, authenticatorRemoval{TOTP: true})
			if err != nil || canRemove != tt.want {
				t.Fatalf("can remove the app = %t, %v; want %t", canRemove, err, tt.want)
			}
		})
	}
}
