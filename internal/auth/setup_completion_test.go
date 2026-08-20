package auth

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func completePreparedSetup(t *testing.T, manager *Manager) *SetupCompletionResult {
	t.Helper()
	manager.tokens = secureTokenGenerator{}
	result, err := manager.CompleteSetup(t.Context(), CompleteSetupOptions{
		Token: setupOwnerTestToken, Origin: setupOwnerTestOrigin, UserAgent: "Setup Browser/1.0",
	})
	if err != nil {
		t.Fatalf("CompleteSetup() error = %v", err)
	}
	if result == nil || result.Session == nil || result.OwnerUserID == "" || result.Session.Token == "" {
		t.Fatalf("CompleteSetup() result = %#v", result)
	}
	return result
}

func TestCompleteSetupFreshOwnerCommitsCredentialsStateEventAndSession(t *testing.T) {
	manager, clock := setupOwnerTestManager(t)
	batch := prepareSetupReviewDraft(t, manager, clock, SetupOwnerDraftInput{
		Mode: SetupOwnerModeCreate, Name: "Cristian Braun", Username: "cristian",
	})
	prepared, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if err != nil || prepared.Draft == nil {
		t.Fatalf("prepared setup state = %#v, %v", prepared, err)
	}
	draft := *prepared.Draft
	draft.RecoveryCodeHashes = append([]string(nil), prepared.Draft.RecoveryCodeHashes...)

	result := completePreparedSetup(t, manager)
	if result.RevokedSessions != 0 || result.Session.UserID != result.OwnerUserID ||
		result.Session.AuthenticationMethod != AuthenticationMethodPassword ||
		result.Session.AssuranceLevel != AssuranceLevelMultiFactor ||
		result.Session.StepUpMethod != AuthenticationMethodTOTP || result.Session.StepUpAt == nil {
		t.Fatalf("fresh setup result = %#v", result)
	}

	var username, usernameNormalized, name, status, passwordHash string
	var authVersion int64
	var mfaRequired, isAdmin int
	if err := manager.db.Read().QueryRow(`
		SELECT username, username_normalized, name, status,
		       auth_version, mfa_required, is_admin
		FROM users WHERE id = ?`, result.OwnerUserID).Scan(
		&username, &usernameNormalized, &name, &status,
		&authVersion, &mfaRequired, &isAdmin,
	); err != nil {
		t.Fatal(err)
	}
	if username != draft.Username || usernameNormalized != draft.UsernameNormalized ||
		name != draft.Name || status != string(UserStatusActive) ||
		authVersion != 1 || mfaRequired != 1 || isAdmin != 1 {
		t.Fatalf("fresh owner = username:%q usernameNormalized:%q name:%q status:%q authVersion:%d mfa:%d admin:%d",
			username, usernameNormalized, name, status, authVersion, mfaRequired, isAdmin)
	}
	if err := manager.db.Read().QueryRow(`SELECT password_hash FROM password_credentials WHERE user_id = ?`, result.OwnerUserID).Scan(&passwordHash); err != nil {
		t.Fatal(err)
	}
	if passwordHash != draft.PasswordHash {
		t.Fatal("setup password hash was not persisted exactly")
	}

	var totpID string
	var encryptedSeed []byte
	var keyVersion, digits, period, enabled int
	var algorithm, issuer string
	var acceptedStep int64
	if err := manager.db.Read().QueryRow(`
		SELECT id, encrypted_seed, key_version, algorithm, digits, period, issuer,
		       last_accepted_step, enabled
		FROM totp_credentials WHERE user_id = ? AND revoked_at IS NULL`, result.OwnerUserID).Scan(
		&totpID, &encryptedSeed, &keyVersion, &algorithm, &digits, &period, &issuer, &acceptedStep, &enabled,
	); err != nil {
		t.Fatal(err)
	}
	decryptedSeed, err := manager.decryptTOTPSeed(result.OwnerUserID, totpID, encryptedSeed, keyVersion)
	if err != nil || decryptedSeed != draft.TOTPSecret || bytes.Contains(encryptedSeed, []byte(draft.TOTPSecret)) ||
		algorithm != "SHA1" || digits != 6 || period != 30 || issuer != setupTOTPIssuer ||
		acceptedStep != *draft.TOTPConfirmedStep || enabled != 1 {
		t.Fatalf("persisted TOTP = decrypted:%t error:%v algorithm:%q digits:%d period:%d issuer:%q step:%d enabled:%d",
			decryptedSeed == draft.TOTPSecret, err, algorithm, digits, period, issuer, acceptedStep, enabled)
	}
	if _, err := manager.decryptTOTPSeed("wrong-owner", totpID, encryptedSeed, keyVersion); err == nil {
		t.Fatal("TOTP credential decrypted with the wrong owner binding")
	}
	tamperedSeed := append([]byte(nil), encryptedSeed...)
	tamperedSeed[len(tamperedSeed)-1] ^= 0x01
	if _, err := manager.decryptTOTPSeed(result.OwnerUserID, totpID, tamperedSeed, keyVersion); err == nil {
		t.Fatal("tampered TOTP credential ciphertext was accepted")
	}
	otherManager := NewManager(manager.config, manager.db, Dependencies{
		Clock: manager.clock, BucketHashKey: []byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"),
	})
	if _, err := otherManager.decryptTOTPSeed(result.OwnerUserID, totpID, encryptedSeed, keyVersion); err == nil {
		t.Fatal("TOTP credential decrypted with the wrong derived key")
	}

	rows, err := manager.db.Read().Query(`SELECT batch_id, code_hash FROM recovery_codes WHERE user_id = ?`, result.OwnerUserID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	recoveryHashes := make(map[string]struct{}, setupRecoveryCodeCount)
	for rows.Next() {
		var batchID, codeHash string
		if err := rows.Scan(&batchID, &codeHash); err != nil {
			t.Fatal(err)
		}
		if batchID != draft.RecoveryBatchID {
			t.Fatalf("recovery batch ID = %q", batchID)
		}
		recoveryHashes[codeHash] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(recoveryHashes) != setupRecoveryCodeCount {
		t.Fatalf("persisted recovery code count = %d", len(recoveryHashes))
	}
	for _, codeHash := range draft.RecoveryCodeHashes {
		if _, found := recoveryHashes[codeHash]; !found {
			t.Fatalf("prepared recovery hash was not persisted: %q", codeHash)
		}
	}

	state, err := manager.SetupState(t.Context())
	if err != nil || !state.Initialized || state.OwnerUserID != result.OwnerUserID ||
		state.InitializedAt == nil || state.TokenConfigured || state.CutoverVersion != setupCutoverVersion {
		t.Fatalf("completed setup state = %#v, %v", state, err)
	}
	var consumedAt sql.NullTime
	var payload []byte
	if err := manager.db.Read().QueryRow(`SELECT consumed_at, payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&consumedAt, &payload); err != nil {
		t.Fatal(err)
	}
	if !consumedAt.Valid || payload != nil {
		t.Fatalf("consumed setup challenge = consumed:%t payload:%x", consumedAt.Valid, payload)
	}
	loadedSession, err := manager.GetSessionByToken(t.Context(), result.Session.Token)
	if err != nil || loadedSession == nil || loadedSession.ID != result.Session.ID || loadedSession.UserID != result.OwnerUserID {
		t.Fatalf("new owner session = %#v, %v", loadedSession, err)
	}

	var eventType, reason, userAgent, metadata string
	var actorID, subjectID, sessionID sql.NullString
	if err := manager.db.Read().QueryRow(`
		SELECT event_type, reason, user_agent, metadata_json,
		       actor_user_id, subject_user_id, session_id
		FROM auth_events WHERE event_type = ?`, AuthEventSetupCompleted).Scan(
		&eventType, &reason, &userAgent, &metadata, &actorID, &subjectID, &sessionID,
	); err != nil {
		t.Fatal(err)
	}
	if eventType != string(AuthEventSetupCompleted) || reason != string(AuthEventReasonSystemInitialization) ||
		userAgent != "Setup Browser/1.0" || !actorID.Valid || actorID.String != result.OwnerUserID ||
		!subjectID.Valid || subjectID.String != result.OwnerUserID || !sessionID.Valid || sessionID.String != result.Session.ID ||
		!strings.Contains(metadata, `"cutover_version":1`) || !strings.Contains(metadata, `"owner_mode":"create"`) ||
		!strings.Contains(metadata, `"topology":"fresh"`) || !strings.Contains(metadata, `"revoked_sessions":0`) {
		t.Fatalf("setup event = type:%q reason:%q agent:%q actor:%#v subject:%#v session:%#v metadata:%q",
			eventType, reason, userAgent, actorID, subjectID, sessionID, metadata)
	}
	for _, secret := range append([]string{draft.PasswordHash, draft.TOTPSecret, draft.RecoveryBatchID, batch.Codes[0]}, draft.RecoveryCodeHashes...) {
		if strings.Contains(metadata, secret) {
			t.Fatalf("setup completion event exposed secret %q", secret)
		}
	}
	if replay, err := manager.CompleteSetup(t.Context(), CompleteSetupOptions{
		Token: setupOwnerTestToken, Origin: setupOwnerTestOrigin,
	}); replay != nil || (!errors.Is(err, ErrSetupAccessInvalid) && !errors.Is(err, ErrSetupAlreadyInitialized)) {
		t.Fatalf("replayed setup completion = %#v, %v", replay, err)
	}
	var defaultUsers int
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM users WHERE id = 'default'`).Scan(&defaultUsers); err != nil || defaultUsers != 0 {
		t.Fatalf("fresh setup synthetic default users = %d, %v", defaultUsers, err)
	}
}

func TestCompleteSetupCreatesSeparateOwnerAndLeavesLegacyWebmailInPlace(t *testing.T) {
	manager, clock := setupOwnerTestManager(t)
	insertSetupOwnerUser(t, manager, "default", "local@gofer.local", "", "Local User", UserStatusActive, false)
	if _, err := manager.db.Write().Exec(`
		INSERT INTO accounts (id, user_id, email_address) VALUES ('legacy-mailbox', 'default', 'mail@example.com');
		DROP TRIGGER users_management_type_update;
		UPDATE users SET is_admin = 1 WHERE id = 'default';
		CREATE TRIGGER users_management_type_update
		BEFORE UPDATE OF is_admin, user_type ON users
		WHEN NEW.is_admin = 1 AND NEW.user_type != 'management'
		 AND (OLD.is_admin != NEW.is_admin OR OLD.user_type != NEW.user_type)
		BEGIN
			SELECT RAISE(ABORT, 'administrator must be a management user');
		END;
		INSERT INTO password_credentials (user_id, password_hash) VALUES ('default', 'old-password');
		INSERT INTO webauthn_credentials (id, user_id, credential_id, public_key, name)
		VALUES ('legacy-passkey', 'default', x'01', x'02', 'Legacy passkey');
		INSERT INTO totp_credentials (id, user_id, encrypted_seed, key_version, enabled)
		VALUES ('legacy-totp', 'default', x'01', 1, 1);
		INSERT INTO auth_identities (id, user_id, provider, issuer, subject, email_verified)
		VALUES ('legacy-identity', 'default', 'google', 'https://accounts.google.com', 'subject', 1);
		INSERT INTO recovery_codes (id, user_id, batch_id, code_hash)
		VALUES ('legacy-recovery', 'default', 'legacy-batch', 'legacy-hash');
		INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method, assurance_level,
			authenticated_at, last_used_at, idle_expires_at, absolute_expires_at, created_at
		) VALUES ('legacy-session', 'default', ?, 1, 'legacy', 'legacy', ?, ?, ?, ?, ?)`,
		hashToken("legacy-session-token"), clock.now, clock.now, clock.now.Add(time.Hour), clock.now.Add(time.Hour), clock.now,
	); err != nil {
		t.Fatal(err)
	}
	prepareSetupReviewDraft(t, manager, clock, SetupOwnerDraftInput{
		Mode: SetupOwnerModeCreate, Name: "Real Owner",
		Username: "owner",
	})
	result := completePreparedSetup(t, manager)
	if result.OwnerUserID == "default" || result.RevokedSessions != 1 {
		t.Fatalf("legacy completion = %#v", result)
	}

	var ownerID, name, username, status, userType, accountOwner string
	var legacyAdmin int
	var authVersion int64
	if err := manager.db.Read().QueryRow(`SELECT id, name, username, status, auth_version, user_type, is_admin FROM users WHERE id = 'default'`).Scan(
		&ownerID, &name, &username, &status, &authVersion, &userType, &legacyAdmin,
	); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT user_id FROM accounts WHERE id = 'legacy-mailbox'`).Scan(&accountOwner); err != nil {
		t.Fatal(err)
	}
	if ownerID != "default" || name != "Local User" || username != "local" ||
		status != string(UserStatusActive) || authVersion != 2 || userType != string(UserTypeWebmail) || legacyAdmin != 0 || accountOwner != "default" {
		t.Fatalf("separated legacy user = id:%q name:%q username:%q status:%q version:%d mailboxOwner:%q",
			ownerID, name, username, status, authVersion, accountOwner)
	}
	var passkeys, identities, activeTOTPs, revokedTOTPs, activeRecovery, revokedRecovery int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM webauthn_credentials WHERE user_id = 'default' AND revoked_at IS NULL`:                &passkeys,
		`SELECT COUNT(*) FROM auth_identities WHERE user_id = 'default'`:                                            &identities,
		`SELECT COUNT(*) FROM totp_credentials WHERE user_id = 'default' AND revoked_at IS NULL AND enabled = 1`:    &activeTOTPs,
		`SELECT COUNT(*) FROM totp_credentials WHERE id = 'legacy-totp' AND revoked_at IS NOT NULL AND enabled = 0`: &revokedTOTPs,
		`SELECT COUNT(*) FROM recovery_codes WHERE user_id = 'default' AND used_at IS NULL AND revoked_at IS NULL`:  &activeRecovery,
		`SELECT COUNT(*) FROM recovery_codes WHERE id = 'legacy-recovery' AND revoked_at IS NOT NULL`:               &revokedRecovery,
	} {
		if err := manager.db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if passkeys != 1 || identities != 1 || activeTOTPs != 1 || revokedTOTPs != 0 ||
		activeRecovery != 1 || revokedRecovery != 0 {
		t.Fatalf("legacy security retention = passkeys:%d identities:%d activeTOTP:%d revokedTOTP:%d activeRecovery:%d revokedRecovery:%d",
			passkeys, identities, activeTOTPs, revokedTOTPs, activeRecovery, revokedRecovery)
	}
	var revokedAt sql.NullTime
	var reason string
	if err := manager.db.Read().QueryRow(`SELECT revoked_at, revocation_reason FROM sessions WHERE id = 'legacy-session'`).Scan(&revokedAt, &reason); err != nil {
		t.Fatal(err)
	}
	if !revokedAt.Valid || reason != string(SessionRevocationAdminAction) {
		t.Fatalf("legacy session revocation = revoked:%t reason:%q", revokedAt.Valid, reason)
	}
}

func TestCompleteSetupCreatesSeparateOwnerWithoutMovingExistingUsers(t *testing.T) {
	manager, clock := setupOwnerTestManager(t)
	insertSetupOwnerUser(t, manager, "selected", "selected@example.com", "selected", "Selected", UserStatusDisabled, false)
	insertSetupOwnerUser(t, manager, "other", "other@example.com", "other", "Other", UserStatusActive, false)
	if _, err := manager.db.Write().Exec(`
		INSERT INTO accounts (id, user_id, email_address) VALUES
			('selected-mailbox', 'selected', 'selected-mail@example.com'),
			('other-mailbox', 'other', 'other-mail@example.com')`); err != nil {
		t.Fatal(err)
	}
	prepareSetupReviewDraft(t, manager, clock, SetupOwnerDraftInput{
		Mode: SetupOwnerModeCreate, Name: "Separate Owner",
		Username: "separate-owner",
	})
	result := completePreparedSetup(t, manager)
	if result.OwnerUserID == "selected" || result.OwnerUserID == "other" {
		t.Fatalf("selected owner ID = %q", result.OwnerUserID)
	}
	var selectedOwner, otherOwner, otherName, otherStatus string
	var otherAdmin int
	if err := manager.db.Read().QueryRow(`SELECT user_id FROM accounts WHERE id = 'selected-mailbox'`).Scan(&selectedOwner); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT user_id FROM accounts WHERE id = 'other-mailbox'`).Scan(&otherOwner); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT name, status, is_admin FROM users WHERE id = 'other'`).Scan(&otherName, &otherStatus, &otherAdmin); err != nil {
		t.Fatal(err)
	}
	if selectedOwner != "selected" || otherOwner != "other" || otherName != "Other" ||
		otherStatus != string(UserStatusActive) || otherAdmin != 0 {
		t.Fatalf("existing ownership changed = selected:%q other:%q otherName:%q otherStatus:%q otherAdmin:%d",
			selectedOwner, otherOwner, otherName, otherStatus, otherAdmin)
	}
}

func TestCompleteSetupBlocksInvalidOwnershipAndStaleTopologyWithoutMutation(t *testing.T) {
	t.Run("unassigned mailbox", func(t *testing.T) {
		manager, clock := setupOwnerTestManager(t)
		if _, err := manager.db.Write().Exec(`INSERT INTO accounts (id, user_id, email_address) VALUES ('orphan', NULL, 'orphan@example.com')`); err != nil {
			t.Fatal(err)
		}
		prepareSetupReviewDraft(t, manager, clock, SetupOwnerDraftInput{
			Mode: SetupOwnerModeCreate, Name: "Owner", Username: "owner",
		})
		manager.tokens = secureTokenGenerator{}
		if result, err := manager.CompleteSetup(t.Context(), CompleteSetupOptions{Token: setupOwnerTestToken, Origin: setupOwnerTestOrigin}); result != nil || !errors.Is(err, ErrSetupCompletionBlocked) {
			t.Fatalf("blocked completion = %#v, %v", result, err)
		}
		assertSetupCompletionDidNotMutate(t, manager, 0, 0)
	})

	t.Run("stale topology", func(t *testing.T) {
		manager, clock := setupOwnerTestManager(t)
		prepareSetupReviewDraft(t, manager, clock, SetupOwnerDraftInput{
			Mode: SetupOwnerModeCreate, Name: "Owner", Username: "owner",
		})
		insertSetupOwnerUser(t, manager, "appeared", "appeared@example.com", "appeared", "Appeared", UserStatusActive, false)
		manager.tokens = secureTokenGenerator{}
		if result, err := manager.CompleteSetup(t.Context(), CompleteSetupOptions{Token: setupOwnerTestToken, Origin: setupOwnerTestOrigin}); result != nil || !errors.Is(err, ErrSetupOwnerDraftRequired) {
			t.Fatalf("stale completion = %#v, %v", result, err)
		}
		assertSetupCompletionDidNotMutate(t, manager, 1, 0)
	})
}

func TestCompleteSetupRollsBackEveryPersistenceStage(t *testing.T) {
	tests := []struct {
		name    string
		trigger string
	}{
		{name: "owner", trigger: `CREATE TRIGGER reject_completion BEFORE INSERT ON users BEGIN SELECT RAISE(ABORT, 'reject owner'); END`},
		{name: "password", trigger: `CREATE TRIGGER reject_completion BEFORE INSERT ON password_credentials BEGIN SELECT RAISE(ABORT, 'reject password'); END`},
		{name: "TOTP", trigger: `CREATE TRIGGER reject_completion BEFORE INSERT ON totp_credentials BEGIN SELECT RAISE(ABORT, 'reject TOTP'); END`},
		{name: "recovery", trigger: `CREATE TRIGGER reject_completion BEFORE INSERT ON recovery_codes BEGIN SELECT RAISE(ABORT, 'reject recovery'); END`},
		{name: "session revocation", trigger: `CREATE TRIGGER reject_completion BEFORE UPDATE ON sessions BEGIN SELECT RAISE(ABORT, 'reject session revocation'); END`},
		{name: "new session", trigger: `CREATE TRIGGER reject_completion BEFORE INSERT ON sessions BEGIN SELECT RAISE(ABORT, 'reject new session'); END`},
		{name: "challenge", trigger: `CREATE TRIGGER reject_completion BEFORE UPDATE OF consumed_at ON auth_challenges BEGIN SELECT RAISE(ABORT, 'reject challenge'); END`},
		{name: "state", trigger: `CREATE TRIGGER reject_completion BEFORE UPDATE OF initialized ON auth_system_state BEGIN SELECT RAISE(ABORT, 'reject state'); END`},
		{name: "event", trigger: `CREATE TRIGGER reject_completion BEFORE INSERT ON auth_events BEGIN SELECT RAISE(ABORT, 'reject event'); END`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, clock := setupOwnerTestManager(t)
			insertSetupOwnerUser(t, manager, "existing", "existing@example.com", "existing", "Existing", UserStatusActive, false)
			if _, err := manager.db.Write().Exec(`
				INSERT INTO sessions (
					id, user_id, token_hash, auth_version, authentication_method, assurance_level,
					authenticated_at, last_used_at, idle_expires_at, absolute_expires_at, created_at
				) VALUES ('old-session', 'existing', ?, 1, 'legacy', 'legacy', ?, ?, ?, ?, ?)`,
				hashToken("old-session-token"), clock.now, clock.now, clock.now.Add(time.Hour), clock.now.Add(time.Hour), clock.now,
			); err != nil {
				t.Fatal(err)
			}
			prepareSetupReviewDraft(t, manager, clock, SetupOwnerDraftInput{
				Mode: SetupOwnerModeCreate, Name: "Owner", Username: "owner",
			})
			if _, err := manager.db.Write().Exec(test.trigger); err != nil {
				t.Fatal(err)
			}
			manager.tokens = secureTokenGenerator{}
			if result, err := manager.CompleteSetup(t.Context(), CompleteSetupOptions{Token: setupOwnerTestToken, Origin: setupOwnerTestOrigin}); result != nil || err == nil {
				t.Fatalf("rejected completion = %#v, %v", result, err)
			}
			assertSetupCompletionDidNotMutate(t, manager, 1, 1)
		})
	}
}

func TestCompleteSetupGenerationFailureDoesNotBeginCutover(t *testing.T) {
	manager, clock := setupOwnerTestManager(t)
	prepareSetupReviewDraft(t, manager, clock, SetupOwnerDraftInput{
		Mode: SetupOwnerModeCreate, Name: "Owner", Username: "owner",
	})
	manager.tokens = &deterministicTokenGenerator{idErr: errors.New("entropy unavailable")}
	if result, err := manager.CompleteSetup(t.Context(), CompleteSetupOptions{Token: setupOwnerTestToken, Origin: setupOwnerTestOrigin}); result != nil || err == nil || !strings.Contains(err.Error(), "entropy unavailable") {
		t.Fatalf("generation failure = %#v, %v", result, err)
	}
	assertSetupCompletionDidNotMutate(t, manager, 0, 0)
}

func TestCompleteSetupIsSingleUseUnderConcurrency(t *testing.T) {
	manager, clock := setupOwnerTestManager(t)
	prepareSetupReviewDraft(t, manager, clock, SetupOwnerDraftInput{
		Mode: SetupOwnerModeCreate, Name: "Owner", Username: "owner",
	})
	manager.tokens = secureTokenGenerator{}
	type outcome struct {
		result *SetupCompletionResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	for range 2 {
		go func() {
			<-start
			result, err := manager.CompleteSetup(t.Context(), CompleteSetupOptions{
				Token: setupOwnerTestToken, Origin: setupOwnerTestOrigin,
			})
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	var successes, rejected int
	for range 2 {
		outcome := <-outcomes
		if outcome.err == nil && outcome.result != nil && outcome.result.Session != nil {
			successes++
			continue
		}
		if outcome.result == nil && (errors.Is(outcome.err, ErrSetupAccessInvalid) || errors.Is(outcome.err, ErrSetupAlreadyInitialized)) {
			rejected++
			continue
		}
		t.Fatalf("concurrent completion outcome = %#v, %v", outcome.result, outcome.err)
	}
	if successes != 1 || rejected != 1 {
		t.Fatalf("concurrent completion = successes:%d rejected:%d", successes, rejected)
	}
	var users, passwords, totps, recoveryCodes, sessions, events int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM users`:                                            &users,
		`SELECT COUNT(*) FROM password_credentials`:                             &passwords,
		`SELECT COUNT(*) FROM totp_credentials WHERE revoked_at IS NULL`:        &totps,
		`SELECT COUNT(*) FROM recovery_codes WHERE revoked_at IS NULL`:          &recoveryCodes,
		`SELECT COUNT(*) FROM sessions WHERE revoked_at IS NULL`:                &sessions,
		`SELECT COUNT(*) FROM auth_events WHERE event_type = 'setup_completed'`: &events,
	} {
		if err := manager.db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if users != 1 || passwords != 1 || totps != 1 || recoveryCodes != setupRecoveryCodeCount || sessions != 1 || events != 1 {
		t.Fatalf("concurrent persistence = users:%d passwords:%d totps:%d recovery:%d sessions:%d events:%d",
			users, passwords, totps, recoveryCodes, sessions, events)
	}
}

func assertSetupCompletionDidNotMutate(t *testing.T, manager *Manager, wantUsers, wantSessions int) {
	t.Helper()
	var users, passwords, totps, recoveryCodes, sessions, completionEvents, initialized int
	var setupHash string
	var consumed int
	var payloadLength int
	queries := []struct {
		query  string
		target *int
	}{
		{`SELECT COUNT(*) FROM users`, &users},
		{`SELECT COUNT(*) FROM password_credentials`, &passwords},
		{`SELECT COUNT(*) FROM totp_credentials`, &totps},
		{`SELECT COUNT(*) FROM recovery_codes`, &recoveryCodes},
		{`SELECT COUNT(*) FROM sessions`, &sessions},
		{`SELECT COUNT(*) FROM auth_events WHERE event_type = 'setup_completed'`, &completionEvents},
	}
	for _, check := range queries {
		if err := manager.db.Read().QueryRow(check.query).Scan(check.target); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.db.Read().QueryRow(`SELECT initialized, setup_token_hash FROM auth_system_state WHERE id = 1`).Scan(&initialized, &setupHash); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`
		SELECT consumed_at IS NOT NULL, length(payload_ciphertext)
		FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&consumed, &payloadLength); err != nil {
		t.Fatal(err)
	}
	if users != wantUsers || passwords != 0 || totps != 0 || recoveryCodes != 0 || sessions != wantSessions ||
		completionEvents != 0 || initialized != 0 || setupHash == "" || consumed != 0 || payloadLength == 0 {
		t.Fatalf("completion partial mutation = users:%d passwords:%d totps:%d recovery:%d sessions:%d events:%d initialized:%d setupHash:%q consumed:%d payload:%d",
			users, passwords, totps, recoveryCodes, sessions, completionEvents, initialized, setupHash, consumed, payloadLength)
	}
	if wantSessions > 0 {
		var revoked int
		if err := manager.db.Read().QueryRow(`SELECT revoked_at IS NOT NULL FROM sessions WHERE id = 'old-session'`).Scan(&revoked); err != nil {
			t.Fatal(err)
		}
		if revoked != 0 {
			t.Fatal("pre-cutover session remained revoked after rollback")
		}
	}
}

func TestSetupCompletionEventJSONContainsOnlyBoundedPolicyMetadata(t *testing.T) {
	metadata, err := setupCompletionEventJSON(SetupOwnerTopology{Kind: SetupOwnerTopologyExisting}, &SetupOwnerDraft{Mode: SetupOwnerModeExisting}, 12)
	if err != nil {
		t.Fatal(err)
	}
	wantParts := []string{`"cutover_version":1`, `"owner_mode":"existing"`, `"topology":"existing"`, `"revoked_sessions":12`}
	for _, part := range wantParts {
		if !strings.Contains(metadata, part) {
			t.Fatalf("metadata %q omitted %q", metadata, part)
		}
	}
	if len(metadata) > 256 {
		t.Fatalf("metadata length = %d", len(metadata))
	}
	if fmt.Sprint(metadata) == "" {
		t.Fatal("metadata is empty")
	}
}
