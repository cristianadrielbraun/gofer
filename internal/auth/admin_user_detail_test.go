package auth

import (
	"errors"
	"testing"
	"time"
)

func TestGetAdministratorUserSaysHowSomeoneSignsIn(t *testing.T) {
	now := time.Date(2026, time.November, 12, 10, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "administrator", true, now)
	insertActiveUser(t, manager, "wren", false, now)
	insertActiveUser(t, manager, "ordinary", false, now)
	insertPolicyTestTOTP(t, manager, "administrator", now)
	adminSession := createPolicyAdministratorSession(t, manager)
	ordinarySession, err := manager.CreateAuthenticatedSession(t.Context(), "ordinary", "Ordinary browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor)
	if err != nil {
		t.Fatal(err)
	}

	// Nothing set up yet: no password, passkey, app, codes or sessions.
	bare, err := manager.GetAdministratorUser(t.Context(), "administrator", adminSession.ID, "wren")
	if err != nil || bare.ID != "wren" || bare.Passkeys != 0 || bare.AuthenticatorAppSince != nil ||
		bare.RecoveryCodesLeft != 0 || bare.PasswordChangedAt != nil || bare.WorkingSessions != 0 || len(bare.Identities) != 0 {
		t.Fatalf("bare detail = %+v, %v", bare, err)
	}

	lastUsed := now.Add(-time.Hour)
	insertPolicyTestPasskey(t, manager, "wren", now.Add(-48*time.Hour))
	insertPolicyTestTOTP(t, manager, "wren", now.Add(-24*time.Hour))
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`UPDATE webauthn_credentials SET last_used_at = ? WHERE user_id = 'wren'`, []any{lastUsed}},
		// A revoked passkey doesn't count.
		{`INSERT INTO webauthn_credentials (id, user_id, credential_id, public_key, name, created_at, credential_ciphertext, key_version, rp_id, revoked_at)
			VALUES ('passkey-old', 'wren', x'0405', x'02', 'Old passkey', ?, x'03', 1, 'gofer.example', ?)`, []any{now, now}},
		{`INSERT INTO password_credentials (user_id, password_hash, changed_at) VALUES ('wren', 'hash', ?)`, []any{now.Add(-72 * time.Hour)}},
		{`INSERT INTO recovery_codes (id, user_id, batch_id, code_hash) VALUES ('code-1', 'wren', 'batch', 'h1')`, nil},
		{`INSERT INTO recovery_codes (id, user_id, batch_id, code_hash) VALUES ('code-2', 'wren', 'batch', 'h2')`, nil},
		{`INSERT INTO recovery_codes (id, user_id, batch_id, code_hash, used_at) VALUES ('code-3', 'wren', 'batch', 'h3', ?)`, []any{now}},
		{`INSERT INTO auth_identities (id, user_id, provider, issuer, subject, email, linked_at, last_used_at)
			VALUES ('identity-1', 'wren', 'google', 'https://accounts.google.com', 'subject-1', 'wren@example.test', ?, ?)`, []any{now.Add(-96 * time.Hour), lastUsed}},
	} {
		if _, err := manager.db.Write().ExecContext(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.CreateAuthenticatedSession(t.Context(), "wren", "Wren's laptop", AuthenticationMethodPassword, AssuranceLevelMultiFactor); err != nil {
		t.Fatal(err)
	}

	detail, err := manager.GetAdministratorUser(t.Context(), "administrator", adminSession.ID, "wren")
	if err != nil {
		t.Fatal(err)
	}
	if detail.Passkeys != 1 || detail.PasskeyLastUsedAt == nil || !detail.PasskeyLastUsedAt.Equal(lastUsed) ||
		detail.AuthenticatorAppSince == nil || !detail.AuthenticatorAppSince.Equal(now.Add(-24*time.Hour)) ||
		detail.RecoveryCodesLeft != 2 || detail.PasswordChangedAt == nil || !detail.HasPassword || detail.WorkingSessions != 1 {
		t.Fatalf("detail = %+v", detail)
	}
	if len(detail.Identities) != 1 || detail.Identities[0].Provider != "google" || detail.Identities[0].Email != "wren@example.test" ||
		detail.Identities[0].LastUsedAt == nil {
		t.Fatalf("identities = %+v", detail.Identities)
	}

	if _, err := manager.GetAdministratorUser(t.Context(), "administrator", adminSession.ID, "nobody"); !errors.Is(err, ErrAdministratorUserNotFound) {
		t.Fatalf("unknown person = %v", err)
	}
	if detail, err := manager.GetAdministratorUser(t.Context(), "ordinary", ordinarySession.ID, "wren"); detail != nil || !errors.Is(err, ErrAdministratorRequired) {
		t.Fatalf("ordinary reader = %+v, %v", detail, err)
	}
}
