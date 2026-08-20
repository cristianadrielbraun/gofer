package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func prepareSetupReviewDraft(t *testing.T, manager *Manager, clock *fixedClock, input SetupOwnerDraftInput) *SetupRecoveryBatch {
	t.Helper()
	if _, err := manager.SaveSetupOwnerDraft(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, input); err != nil {
		t.Fatalf("save setup review owner: %v", err)
	}
	if _, err := manager.SaveSetupPasswordDraft(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, SetupPasswordDraftInput{
		Password: setupTOTPTestPassword, PasswordConfirmation: setupTOTPTestPassword,
	}); err != nil {
		t.Fatalf("save setup review password: %v", err)
	}
	if _, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); err != nil {
		t.Fatalf("start setup review TOTP: %v", err)
	}
	state, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ConfirmSetupTOTP(
		t.Context(), setupOwnerTestToken, setupOwnerTestOrigin,
		setupTOTPCode(t, state.Draft.TOTPSecret, clock.now),
	); err != nil {
		t.Fatalf("confirm setup review TOTP: %v", err)
	}
	batch, err := manager.GenerateSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false)
	if err != nil {
		t.Fatalf("generate setup review recovery codes: %v", err)
	}
	if _, err := manager.AcknowledgeSetupRecoveryCodes(
		t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, batch.BatchID, true,
	); err != nil {
		t.Fatalf("acknowledge setup review recovery codes: %v", err)
	}
	return batch
}

func TestSetupReviewRequiresEveryPreparedSecurityStep(t *testing.T) {
	manager, clock := setupOwnerTestManager(t)
	if _, err := manager.GetSetupReview(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin); !errors.Is(err, ErrSetupOwnerDraftRequired) {
		t.Fatalf("review without owner = %v", err)
	}
	if _, err := manager.SaveSetupOwnerDraft(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, SetupOwnerDraftInput{
		Mode: SetupOwnerModeCreate, Name: "Owner", Username: "owner", Email: "owner@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.GetSetupReview(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin); !errors.Is(err, ErrSetupPasswordDraftRequired) {
		t.Fatalf("review without password = %v", err)
	}
	if _, err := manager.SaveSetupPasswordDraft(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, SetupPasswordDraftInput{
		Password: setupTOTPTestPassword, PasswordConfirmation: setupTOTPTestPassword,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.GetSetupReview(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin); !errors.Is(err, ErrSetupTOTPConfirmationRequired) {
		t.Fatalf("review without TOTP = %v", err)
	}
	if _, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); err != nil {
		t.Fatal(err)
	}
	state, _ := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if _, err := manager.ConfirmSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, setupTOTPCode(t, state.Draft.TOTPSecret, clock.now)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.GetSetupReview(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin); !errors.Is(err, ErrSetupRecoveryAcknowledgementRequired) {
		t.Fatalf("review without recovery batch = %v", err)
	}
	batch, err := manager.GenerateSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.GetSetupReview(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin); !errors.Is(err, ErrSetupRecoveryAcknowledgementRequired) {
		t.Fatalf("review without recovery acknowledgement = %v", err)
	}
	if _, err := manager.AcknowledgeSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, batch.BatchID, true); err != nil {
		t.Fatal(err)
	}
	if review, err := manager.GetSetupReview(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin); err != nil || review == nil {
		t.Fatalf("complete setup review = %#v, %v", review, err)
	}
}

func TestSetupReviewFreshOwnerIsSecretFreeReadOnlyAndExact(t *testing.T) {
	manager, clock := setupOwnerTestManager(t)
	batch := prepareSetupReviewDraft(t, manager, clock, SetupOwnerDraftInput{
		Mode: SetupOwnerModeCreate, Name: "Cristian Braun", Username: "cristian", Email: "cristian@example.com",
	})
	state, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if err != nil {
		t.Fatal(err)
	}
	var originalPayload []byte
	if err := manager.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&originalPayload); err != nil {
		t.Fatal(err)
	}
	review, err := manager.GetSetupReview(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if err != nil || review == nil || review.TopologyKind != SetupOwnerTopologyFresh || review.Mode != SetupOwnerModeCreate ||
		!review.CreatesNewOwner || review.ClaimsLegacyDefault || review.ClaimsExistingUser || review.TargetUserID != "" ||
		review.OwnerName != "Cristian Braun" || review.OwnerUsername != "cristian" || review.OwnerEmail != "cristian@example.com" ||
		review.ExistingUserCount != 0 || review.TotalMailboxCount != 0 || review.UnrevokedSessionCount != 0 || review.BlockedMessage != "" {
		t.Fatalf("fresh setup review = %#v, %v", review, err)
	}
	var storedPayload []byte
	var users, passwords, totps, recoveryCodes, sessions, initialized int
	if err := manager.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&storedPayload); err != nil {
		t.Fatal(err)
	}
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM users`:                             &users,
		`SELECT COUNT(*) FROM password_credentials`:              &passwords,
		`SELECT COUNT(*) FROM totp_credentials`:                  &totps,
		`SELECT COUNT(*) FROM recovery_codes`:                    &recoveryCodes,
		`SELECT COUNT(*) FROM sessions`:                          &sessions,
		`SELECT initialized FROM auth_system_state WHERE id = 1`: &initialized,
	} {
		if err := manager.db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(storedPayload, originalPayload) || users != 0 || passwords != 0 || totps != 0 || recoveryCodes != 0 || sessions != 0 || initialized != 0 {
		t.Fatalf("review mutated setup = payloadChanged:%t users:%d passwords:%d totps:%d recovery:%d sessions:%d initialized:%d",
			!bytes.Equal(storedPayload, originalPayload), users, passwords, totps, recoveryCodes, sessions, initialized)
	}
	reviewJSON, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{state.Draft.PasswordHash, state.Draft.TOTPSecret, state.Draft.RecoveryBatchID, batch.Codes[0], state.Draft.RecoveryCodeHashes[0]} {
		if bytes.Contains(reviewJSON, []byte(secret)) {
			t.Fatalf("review exposed prepared secret %q", secret)
		}
	}
}

func TestSetupReviewLegacyDefaultCreatesSeparateOwnerAndPreservesMail(t *testing.T) {
	manager, clock := setupOwnerTestManager(t)
	insertSetupOwnerUser(t, manager, "default", "local@gofer.local", "", "Local User", UserStatusActive, false)
	if _, err := manager.db.Write().Exec(`
		INSERT INTO accounts (id, user_id, email_address) VALUES
			('mailbox-a', 'default', 'a@example.com'),
			('mailbox-b', 'default', 'b@example.com');
		INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method, assurance_level,
			authenticated_at, last_used_at, idle_expires_at, absolute_expires_at, created_at
		) VALUES ('legacy-session', 'default', ?, 1, 'legacy', 'legacy', ?, ?, ?, ?, ?)`,
		hashToken("legacy-session-token"), clock.now, clock.now, clock.now.Add(time.Hour), clock.now.Add(24*time.Hour), clock.now,
	); err != nil {
		t.Fatal(err)
	}
	prepareSetupReviewDraft(t, manager, clock, SetupOwnerDraftInput{
		Mode: SetupOwnerModeCreate, Name: "Cristian Braun",
		Username: "cristian", Email: "cristian@example.com",
	})
	review, err := manager.GetSetupReview(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if err != nil || review == nil || review.ClaimsLegacyDefault || !review.CreatesNewOwner || review.ClaimsExistingUser ||
		review.TargetUserID != "" || review.CurrentName != "" || review.CurrentEmail != "" ||
		review.OwnerName != "Cristian Braun" || review.OwnerUsername != "cristian" || review.OwnerEmail != "cristian@example.com" ||
		review.TargetMailboxCount != 0 || review.TotalMailboxCount != 2 || review.TargetLegacySessions != 0 ||
		review.UnrevokedSessionCount != 1 || review.ExistingUserCount != 1 || review.BlockedMessage != "" {
		t.Fatalf("legacy setup review = %#v, %v", review, err)
	}
	var name, email, accountOwners string
	var revokedAt any
	if err := manager.db.Read().QueryRow(`SELECT name, email FROM users WHERE id = 'default'`).Scan(&name, &email); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT group_concat(user_id, ',') FROM (SELECT user_id FROM accounts ORDER BY id)`).Scan(&accountOwners); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT revoked_at FROM sessions WHERE id = 'legacy-session'`).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if name != "Local User" || email != "local@gofer.local" || accountOwners != "default,default" || revokedAt != nil {
		t.Fatalf("legacy review mutated data = name:%q email:%q owners:%q revoked:%#v", name, email, accountOwners, revokedAt)
	}
}

func TestSetupReviewExistingUsersCreatesSeparateOwnerWithoutCredentialReplacement(t *testing.T) {
	manager, clock := setupOwnerTestManager(t)
	insertSetupOwnerUser(t, manager, "existing", "existing@example.com", "existing", "Existing Person", UserStatusDisabled, false)
	insertSetupOwnerUser(t, manager, "other", "other@example.com", "other", "Other Person", UserStatusActive, true)
	if _, err := manager.db.Write().Exec(`
		INSERT INTO accounts (id, user_id, email_address) VALUES ('owned-mailbox', 'existing', 'mail@example.com');
		INSERT INTO password_credentials (user_id, password_hash) VALUES ('existing', 'old-password-hash');
		INSERT INTO webauthn_credentials (id, user_id, credential_id, public_key, name)
		VALUES ('passkey', 'existing', x'01', x'02', 'Existing passkey');
		INSERT INTO totp_credentials (id, user_id, encrypted_seed, key_version, enabled)
		VALUES ('old-totp', 'existing', x'01', 1, 1);
		INSERT INTO auth_identities (id, user_id, provider, issuer, subject)
		VALUES ('identity', 'existing', 'google', 'https://accounts.google.com', 'subject');
		INSERT INTO recovery_codes (id, user_id, batch_id, code_hash)
		VALUES ('old-recovery', 'existing', 'old-batch', 'old-code-hash')`); err != nil {
		t.Fatal(err)
	}
	prepareSetupReviewDraft(t, manager, clock, SetupOwnerDraftInput{
		Mode: SetupOwnerModeCreate, Name: "New Owner Name",
		Username: "new-owner", Email: "new-owner@example.com",
	})
	review, err := manager.GetSetupReview(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if err != nil || review == nil || review.ClaimsExistingUser || review.ClaimsLegacyDefault || !review.CreatesNewOwner ||
		review.TargetUserID != "" || review.CurrentStatus != "" || review.CurrentIsAdmin ||
		review.ExistingUserCount != 2 || review.TargetMailboxCount != 0 || review.TotalMailboxCount != 1 ||
		review.ExistingPasswordCredentials != 0 || review.RetainedPasskeys != 0 || review.ReplacedTOTPs != 0 ||
		review.RetainedIdentities != 0 || review.ReplacedRecoveryCodes != 0 {
		t.Fatalf("existing-user setup review = %#v, %v", review, err)
	}
	var status UserStatus
	var admin, passwordRows, passkeys, totps, identities, recoveryCodes int
	if err := manager.db.Read().QueryRow(`SELECT status, is_admin FROM users WHERE id = 'existing'`).Scan(&status, &admin); err != nil {
		t.Fatal(err)
	}
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM password_credentials WHERE user_id = 'existing'`:                                      &passwordRows,
		`SELECT COUNT(*) FROM webauthn_credentials WHERE user_id = 'existing' AND revoked_at IS NULL`:               &passkeys,
		`SELECT COUNT(*) FROM totp_credentials WHERE user_id = 'existing' AND revoked_at IS NULL`:                   &totps,
		`SELECT COUNT(*) FROM auth_identities WHERE user_id = 'existing'`:                                           &identities,
		`SELECT COUNT(*) FROM recovery_codes WHERE user_id = 'existing' AND used_at IS NULL AND revoked_at IS NULL`: &recoveryCodes,
	} {
		if err := manager.db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if status != UserStatusDisabled || admin != 0 || passwordRows != 1 || passkeys != 1 || totps != 1 || identities != 1 || recoveryCodes != 1 {
		t.Fatalf("existing review mutated security state = status:%q admin:%d password:%d passkeys:%d totps:%d identities:%d recovery:%d",
			status, admin, passwordRows, passkeys, totps, identities, recoveryCodes)
	}
}

func TestSetupReviewBlocksUnassignedMailboxesAndRejectsStaleTopology(t *testing.T) {
	t.Run("unassigned mailbox", func(t *testing.T) {
		manager, clock := setupOwnerTestManager(t)
		if _, err := manager.db.Write().Exec(`INSERT INTO accounts (id, user_id, email_address) VALUES ('orphan', NULL, 'orphan@example.com')`); err != nil {
			t.Fatal(err)
		}
		prepareSetupReviewDraft(t, manager, clock, SetupOwnerDraftInput{
			Mode: SetupOwnerModeCreate, Name: "Owner", Username: "owner", Email: "owner@example.com",
		})
		review, err := manager.GetSetupReview(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
		if err != nil || review == nil || review.UnassignedMailboxCount != 1 || review.TotalMailboxCount != 1 || review.BlockedMessage == "" {
			t.Fatalf("blocked setup review = %#v, %v", review, err)
		}
		var owner any
		if err := manager.db.Read().QueryRow(`SELECT user_id FROM accounts WHERE id = 'orphan'`).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		if owner != nil {
			t.Fatalf("review assigned orphan mailbox to %#v", owner)
		}
	})

	t.Run("stale topology", func(t *testing.T) {
		manager, clock := setupOwnerTestManager(t)
		prepareSetupReviewDraft(t, manager, clock, SetupOwnerDraftInput{
			Mode: SetupOwnerModeCreate, Name: "Owner", Username: "owner", Email: "owner@example.com",
		})
		insertSetupOwnerUser(t, manager, "appeared", "appeared@example.com", "appeared", "Appeared", UserStatusActive, false)
		if review, err := manager.GetSetupReview(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin); review != nil || !errors.Is(err, ErrSetupOwnerDraftRequired) {
			t.Fatalf("stale setup review = %#v, %v", review, err)
		}
	})
}
