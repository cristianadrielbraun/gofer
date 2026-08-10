package auth

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func prepareMFAManagement(t *testing.T, now time.Time, freshStepUp bool) (*Manager, *fixedClock, string, *Session) {
	t.Helper()
	clock := &fixedClock{now: now}
	manager, secret, _ := prepareTOTPLoginManager(t, now, secureTokenGenerator{})
	manager.clock = clock
	session, err := manager.CreateAuthenticatedSession(
		t.Context(), totpLoginTestUserID, "Security Settings Browser/1.0",
		AuthenticationMethodPassword, AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatalf("CreateAuthenticatedSession() error = %v", err)
	}
	if freshStepUp {
		sessionToken := session.Token
		steppedUp, err := manager.RecordSessionStepUp(t.Context(), session.UserID, session.ID, AuthenticationMethodTOTP)
		if err != nil || !steppedUp {
			t.Fatalf("RecordSessionStepUp() = %t, %v", steppedUp, err)
		}
		session, err = manager.GetSessionByToken(t.Context(), session.Token)
		if err != nil || session == nil {
			t.Fatalf("reload stepped-up session = %#v, %v", session, err)
		}
		session.Token = sessionToken
	}
	insertManagedRecoveryBatch(t, manager, "existing recovery material", now.Add(-time.Hour))
	return manager, clock, secret, session
}

func insertManagedRecoveryBatch(t *testing.T, manager *Manager, material string, createdAt time.Time) *SetupRecoveryBatch {
	t.Helper()
	batch, hashes, err := deriveSetupRecoveryBatch(material)
	if err != nil {
		t.Fatal(err)
	}
	for index, codeHash := range hashes {
		if _, err := manager.db.Write().ExecContext(t.Context(), `
			INSERT INTO recovery_codes (id, user_id, batch_id, code_hash, created_at)
			VALUES (?, ?, ?, ?, ?)`,
			"existing-recovery-"+string(rune('a'+index)), totpLoginTestUserID,
			batch.BatchID, codeHash, createdAt,
		); err != nil {
			t.Fatalf("insert recovery code %d: %v", index, err)
		}
	}
	return batch
}

func activeManagedRecoveryCount(t *testing.T, manager *Manager) int {
	t.Helper()
	var count int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM recovery_codes
		WHERE user_id = ? AND used_at IS NULL AND revoked_at IS NULL`,
		totpLoginTestUserID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestSecurityTOTPStepUpIsReplaySafeThrottledSeparatelyAndRedacted(t *testing.T) {
	now := time.Date(2026, time.August, 9, 15, 0, 0, 0, time.UTC)
	manager, _, secret, session := prepareMFAManagement(t, now, false)
	summary, err := manager.GetSecurityFactorSummary(t.Context(), session.Token)
	if err != nil || summary.StepUpFresh || !summary.HasTOTP || summary.RecoveryCodesRemaining != setupRecoveryCodeCount {
		t.Fatalf("initial security summary = %#v, %v", summary, err)
	}
	validCode := setupTOTPCode(t, secret, now)
	invalidCode := invalidTOTPCode(validCode)
	err = manager.VerifySecurityTOTPStepUp(
		t.Context(), session.Token, invalidCode, "198.51.100.20", "Step-up Browser/1.0",
	)
	var validationError *TOTPManagementValidationError
	if !errors.As(err, &validationError) {
		t.Fatalf("invalid VerifySecurityTOTPStepUp() error = %v", err)
	}
	summary, err = manager.GetSecurityFactorSummary(t.Context(), session.Token)
	if err != nil || summary.StepUpFresh {
		t.Fatalf("summary after rejected step-up = %#v, %v", summary, err)
	}
	if err := manager.VerifySecurityTOTPStepUp(
		t.Context(), session.Token, validCode, "198.51.100.20", "  Step-up Browser/1.0  ",
	); err != nil {
		t.Fatalf("valid VerifySecurityTOTPStepUp() error = %v", err)
	}
	summary, err = manager.GetSecurityFactorSummary(t.Context(), session.Token)
	if err != nil || !summary.StepUpFresh || summary.StepUpExpiresAt == nil ||
		!summary.StepUpExpiresAt.Equal(now.Add(securityStepUpMaximumAge)) {
		t.Fatalf("summary after accepted step-up = %#v, %v", summary, err)
	}
	if err := manager.VerifySecurityTOTPStepUp(
		t.Context(), session.Token, validCode, "198.51.100.20", "Step-up Browser/1.0",
	); !errors.As(err, &validationError) {
		t.Fatalf("replayed VerifySecurityTOTPStepUp() error = %v", err)
	}
	var eventText string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT group_concat(event_type || ':' || reason || ':' || user_agent || ':' || metadata_json, '|')
		FROM auth_events WHERE subject_user_id = ? AND event_type IN (?, ?)`,
		totpLoginTestUserID, AuthEventStepUpFailed, AuthEventStepUpSucceeded,
	).Scan(&eventText); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(eventText, validCode) || strings.Contains(eventText, invalidCode) ||
		!strings.Contains(eventText, string(AuthEventStepUpSucceeded)) || !strings.Contains(eventText, "Step-up Browser/1.0") {
		t.Fatalf("step-up events are not redacted/bounded: %q", eventText)
	}
	var loginBuckets, managementBuckets int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_throttle WHERE action = ?`, totpLoginThrottleAction).Scan(&loginBuckets); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_throttle WHERE action = ?`, totpManagementThrottleAction).Scan(&managementBuckets); err != nil {
		t.Fatal(err)
	}
	if loginBuckets != 0 || managementBuckets == 0 {
		t.Fatalf("throttle buckets login=%d management=%d", loginBuckets, managementBuckets)
	}
}

func TestTOTPReplacementKeepsOldFactorUntilAtomicCompletion(t *testing.T) {
	now := time.Date(2026, time.August, 9, 16, 0, 0, 0, time.UTC)
	manager, _, oldSecret, session := prepareMFAManagement(t, now, true)
	other, err := manager.CreateAuthenticatedSession(
		t.Context(), session.UserID, "Other Browser", AuthenticationMethodPassword, AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	state, err := manager.StartTOTPReplacement(t.Context(), session.Token, totpLoginTestOrigin)
	if err != nil || state == nil || state.Challenge == nil || state.Enrollment == nil || len(state.Enrollment.QRPNG) == 0 {
		t.Fatalf("StartTOTPReplacement() = %#v, %v", state, err)
	}
	if _, err := manager.GetTOTPReplacement(
		t.Context(), state.Challenge.Token, session.Token, "https://other.example",
	); !errors.Is(err, ErrSecurityChallengeInvalid) {
		t.Fatalf("cross-origin GetTOTPReplacement() error = %v", err)
	}
	replacementSecret := strings.ReplaceAll(state.Enrollment.ManualKey, " ", "")
	validCode := setupTOTPCode(t, replacementSecret, now)
	if _, err := manager.ConfirmTOTPReplacement(
		t.Context(), state.Challenge.Token, session.Token, totpLoginTestOrigin,
		invalidTOTPCode(validCode), "198.51.100.30", "Replacement Browser",
	); err == nil {
		t.Fatal("invalid replacement code succeeded")
	}
	var activeID string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT id FROM totp_credentials WHERE user_id = ? AND enabled = 1 AND revoked_at IS NULL`,
		session.UserID,
	).Scan(&activeID); err != nil || activeID != "totp-credential" {
		t.Fatalf("active TOTP before completion = %q, %v", activeID, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), other.Token); err != nil || stored == nil {
		t.Fatalf("other session before replacement completion = %#v, %v", stored, err)
	}
	rotated, err := manager.ConfirmTOTPReplacement(
		t.Context(), state.Challenge.Token, session.Token, totpLoginTestOrigin,
		validCode, "198.51.100.30", "  Replacement Browser  ",
	)
	if err != nil || rotated == nil || rotated.Token == "" || rotated.Token == session.Token ||
		rotated.AuthVersion != session.AuthVersion+1 || rotated.StepUpMethod != AuthenticationMethodTOTP {
		t.Fatalf("ConfirmTOTPReplacement() = %#v, %v", rotated, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || stored != nil {
		t.Fatalf("old current session after replacement = %#v, %v", stored, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), other.Token); err != nil || stored != nil {
		t.Fatalf("other session after replacement = %#v, %v", stored, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), rotated.Token); err != nil || stored == nil {
		t.Fatalf("rotated session after replacement = %#v, %v", stored, err)
	}
	var encryptedSeed []byte
	var keyVersion int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT id, encrypted_seed, key_version FROM totp_credentials
		WHERE user_id = ? AND enabled = 1 AND revoked_at IS NULL`, session.UserID,
	).Scan(&activeID, &encryptedSeed, &keyVersion); err != nil {
		t.Fatal(err)
	}
	decrypted, err := manager.decryptTOTPSeed(session.UserID, activeID, encryptedSeed, keyVersion)
	if err != nil || decrypted != replacementSecret || decrypted == oldSecret || activeID == "totp-credential" {
		t.Fatalf("replacement credential id=%q secret changed=%t error=%v", activeID, decrypted != oldSecret, err)
	}
	var oldEnabled int
	var oldRevoked time.Time
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT enabled, revoked_at FROM totp_credentials WHERE id = 'totp-credential'`,
	).Scan(&oldEnabled, &oldRevoked); err != nil || oldEnabled != 0 || !oldRevoked.Equal(now) {
		t.Fatalf("old TOTP lifecycle = enabled=%d revoked=%v error=%v", oldEnabled, oldRevoked, err)
	}
	if count := activeManagedRecoveryCount(t, manager); count != setupRecoveryCodeCount {
		t.Fatalf("recovery codes after TOTP replacement = %d", count)
	}
	var consumedAt time.Time
	var payload []byte
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT consumed_at, payload_ciphertext FROM auth_challenges WHERE id = ?`, state.Challenge.ID,
	).Scan(&consumedAt, &payload); err != nil || !consumedAt.Equal(now) || payload != nil {
		t.Fatalf("replacement challenge lifecycle = consumed=%v payload=%x error=%v", consumedAt, payload, err)
	}
}

func TestRecoveryCodeReplacementActivatesOnlyAcknowledgedBatch(t *testing.T) {
	now := time.Date(2026, time.August, 9, 17, 0, 0, 0, time.UTC)
	manager, clock, _, session := prepareMFAManagement(t, now, true)
	batch, err := manager.StartRecoveryCodeReplacement(t.Context(), session.Token, totpLoginTestOrigin)
	if err != nil || batch == nil || batch.Challenge == nil || len(batch.Codes) != setupRecoveryCodeCount {
		t.Fatalf("StartRecoveryCodeReplacement() = %#v, %v", batch, err)
	}
	if count := activeManagedRecoveryCount(t, manager); count != setupRecoveryCodeCount {
		t.Fatalf("old codes after draft generation = %d", count)
	}
	if err := manager.CompleteRecoveryCodeReplacement(
		t.Context(), batch.Challenge.Token, session.Token, totpLoginTestOrigin,
		batch.BatchID, "Recovery Browser", false,
	); !errors.Is(err, ErrRecoveryManagementRequired) {
		t.Fatalf("unacknowledged replacement error = %v", err)
	}
	if count := activeManagedRecoveryCount(t, manager); count != setupRecoveryCodeCount {
		t.Fatalf("old codes after rejected acknowledgement = %d", count)
	}
	state, err := manager.GetRecoveryCodeReplacement(
		t.Context(), batch.Challenge.Token, session.Token, totpLoginTestOrigin,
	)
	if err != nil || state.BatchID != batch.BatchID {
		t.Fatalf("GetRecoveryCodeReplacement() = %#v, %v", state, err)
	}
	if err := manager.CompleteRecoveryCodeReplacement(
		t.Context(), batch.Challenge.Token, session.Token, totpLoginTestOrigin,
		batch.BatchID, "  Recovery Browser  ", true,
	); err != nil {
		t.Fatalf("CompleteRecoveryCodeReplacement() error = %v", err)
	}
	if count := activeManagedRecoveryCount(t, manager); count != setupRecoveryCodeCount {
		t.Fatalf("new active recovery codes = %d", count)
	}
	var activeBatch string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT DISTINCT batch_id FROM recovery_codes
		WHERE user_id = ? AND used_at IS NULL AND revoked_at IS NULL`, session.UserID,
	).Scan(&activeBatch); err != nil || activeBatch != batch.BatchID {
		t.Fatalf("active recovery batch = %q, %v", activeBatch, err)
	}
	for _, plaintext := range batch.Codes {
		if strings.Contains(activeBatch, plaintext) {
			t.Fatalf("plaintext recovery code leaked into batch metadata: %q", plaintext)
		}
	}
	if _, err := manager.GetRecoveryCodeReplacement(
		t.Context(), batch.Challenge.Token, session.Token, totpLoginTestOrigin,
	); !errors.Is(err, ErrSecurityChallengeInvalid) {
		t.Fatalf("consumed recovery challenge error = %v", err)
	}

	second, err := manager.StartRecoveryCodeReplacement(t.Context(), session.Token, totpLoginTestOrigin)
	if err != nil {
		t.Fatal(err)
	}
	clock.now = now.Add(securityStepUpMaximumAge + time.Second)
	if err := manager.CompleteRecoveryCodeReplacement(
		t.Context(), second.Challenge.Token, session.Token, totpLoginTestOrigin,
		second.BatchID, "Recovery Browser", true,
	); !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale-step-up recovery replacement error = %v", err)
	}
	if count := activeManagedRecoveryCount(t, manager); count != setupRecoveryCodeCount {
		t.Fatalf("active codes after stale completion = %d", count)
	}
}

func TestTOTPDisableProtectsRequiredLastFactorAndRevokesRecovery(t *testing.T) {
	now := time.Date(2026, time.August, 9, 18, 0, 0, 0, time.UTC)
	manager, _, _, session := prepareMFAManagement(t, now, true)
	if _, err := manager.DisableTOTP(t.Context(), session.Token, "Disable Browser"); !errors.Is(err, ErrLastAuthenticator) {
		t.Fatalf("DisableTOTP() without alternate strong factor error = %v", err)
	}
	summary, err := manager.GetSecurityFactorSummary(t.Context(), session.Token)
	if err != nil || !summary.HasTOTP || summary.CanDisableTOTP || summary.DisableTOTPReason == "" {
		t.Fatalf("protected factor summary = %#v, %v", summary, err)
	}
	if count := activeManagedRecoveryCount(t, manager); count != setupRecoveryCodeCount {
		t.Fatalf("recovery codes after protected disable = %d", count)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE users SET is_admin = 0, mfa_required = 0 WHERE id = ?`,
		session.UserID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (id, initialized, mfa_policy)
		VALUES (1, 1, 'all_users')`); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO webauthn_credentials (
			id, user_id, credential_id, public_key, name, created_at, rp_id
		) VALUES ('incomplete-alternative', ?, x'0b', x'02', 'Incomplete', ?, 'gofer.example')`,
		session.UserID, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.DisableTOTP(t.Context(), session.Token, "Disable Browser"); !errors.Is(err, ErrLastAuthenticator) {
		t.Fatalf("DisableTOTP() under instance policy error = %v", err)
	}
	summary, err = manager.GetSecurityFactorSummary(t.Context(), session.Token)
	if err != nil || !summary.RequiresMFA || summary.CanDisableTOTP {
		t.Fatalf("instance-policy factor summary = %#v, %v", summary, err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE auth_system_state SET mfa_policy = 'administrators' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	other, err := manager.CreateAuthenticatedSession(
		t.Context(), session.UserID, "Other Browser", AuthenticationMethodPassword, AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := manager.DisableTOTP(t.Context(), session.Token, "  Disable Browser  ")
	if err != nil || rotated == nil || rotated.AuthVersion != session.AuthVersion+1 || rotated.UserAgent != "Disable Browser" {
		t.Fatalf("DisableTOTP() = %#v, %v", rotated, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || stored != nil {
		t.Fatalf("old session after disable = %#v, %v", stored, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), other.Token); err != nil || stored != nil {
		t.Fatalf("other session after disable = %#v, %v", stored, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), rotated.Token); err != nil || stored == nil {
		t.Fatalf("rotated session after disable = %#v, %v", stored, err)
	}
	var activeTOTP int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM totp_credentials WHERE user_id = ? AND enabled = 1 AND revoked_at IS NULL`,
		session.UserID,
	).Scan(&activeTOTP); err != nil {
		t.Fatal(err)
	}
	if activeTOTP != 0 || activeManagedRecoveryCount(t, manager) != 0 {
		t.Fatalf("factor state after disable: TOTP=%d recovery=%d", activeTOTP, activeManagedRecoveryCount(t, manager))
	}
}

func TestRecoveryCodeRevocationRequiresFreshStepUpAndClearsOnlyUnusedCodes(t *testing.T) {
	now := time.Date(2026, time.August, 9, 19, 0, 0, 0, time.UTC)
	manager, clock, _, session := prepareMFAManagement(t, now, true)
	clock.now = now.Add(securityStepUpMaximumAge + time.Second)
	if _, err := manager.RevokeRecoveryCodes(t.Context(), session.Token, "Browser"); !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale RevokeRecoveryCodes() error = %v", err)
	}
	if count := activeManagedRecoveryCount(t, manager); count != setupRecoveryCodeCount {
		t.Fatalf("codes after stale revocation = %d", count)
	}
	clock.now = now
	revoked, err := manager.RevokeRecoveryCodes(t.Context(), session.Token, "Browser")
	if err != nil || revoked != setupRecoveryCodeCount || activeManagedRecoveryCount(t, manager) != 0 {
		t.Fatalf("RevokeRecoveryCodes() = %d, %v active=%d", revoked, err, activeManagedRecoveryCount(t, manager))
	}
	if stored, err := manager.GetSessionByToken(t.Context(), session.Token); err != nil || stored == nil {
		t.Fatalf("current session after recovery revocation = %#v, %v", stored, err)
	}
}

func TestTOTPReplacementRollsBackCredentialSessionsAndChallengeOnAuditFailure(t *testing.T) {
	now := time.Date(2026, time.August, 9, 20, 0, 0, 0, time.UTC)
	manager, _, _, session := prepareMFAManagement(t, now, true)
	other, err := manager.CreateAuthenticatedSession(
		t.Context(), session.UserID, "Other Browser", AuthenticationMethodPassword, AssuranceLevelMultiFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	state, err := manager.StartTOTPReplacement(t.Context(), session.Token, totpLoginTestOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER fail_managed_totp_event
		BEFORE INSERT ON auth_events
		WHEN NEW.metadata_json = '{"action":"replaced","factor":"totp"}'
		BEGIN SELECT RAISE(ABORT, 'injected managed TOTP event failure'); END`); err != nil {
		t.Fatal(err)
	}
	secret := strings.ReplaceAll(state.Enrollment.ManualKey, " ", "")
	if _, err := manager.ConfirmTOTPReplacement(
		t.Context(), state.Challenge.Token, session.Token, totpLoginTestOrigin,
		setupTOTPCode(t, secret, now), "198.51.100.31", "Rollback Browser",
	); err == nil || !strings.Contains(err.Error(), "injected managed TOTP event failure") {
		t.Fatalf("ConfirmTOTPReplacement() audit failure = %v", err)
	}
	var activeTOTP, authVersion int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM totp_credentials
		WHERE user_id = ? AND enabled = 1 AND revoked_at IS NULL`, session.UserID,
	).Scan(&activeTOTP); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT auth_version FROM users WHERE id = ?`, session.UserID).Scan(&authVersion); err != nil {
		t.Fatal(err)
	}
	if activeTOTP != 1 || authVersion != int(session.AuthVersion) {
		t.Fatalf("rolled-back factor state: active=%d authVersion=%d", activeTOTP, authVersion)
	}
	for _, candidate := range []*Session{session, other} {
		if stored, err := manager.GetSessionByToken(t.Context(), candidate.Token); err != nil || stored == nil {
			t.Fatalf("session %q after rollback = %#v, %v", candidate.ID, stored, err)
		}
	}
	var consumedAt any
	var payload []byte
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT consumed_at, payload_ciphertext FROM auth_challenges WHERE id = ?`, state.Challenge.ID,
	).Scan(&consumedAt, &payload); err != nil || consumedAt != nil || len(payload) == 0 {
		t.Fatalf("challenge after rollback = consumed:%v payload:%d error:%v", consumedAt, len(payload), err)
	}
}

func TestConcurrentTOTPReplacementHasOneWinner(t *testing.T) {
	now := time.Date(2026, time.August, 9, 21, 0, 0, 0, time.UTC)
	manager, _, _, session := prepareMFAManagement(t, now, true)
	state, err := manager.StartTOTPReplacement(t.Context(), session.Token, totpLoginTestOrigin)
	if err != nil {
		t.Fatal(err)
	}
	code := setupTOTPCode(t, strings.ReplaceAll(state.Enrollment.ManualKey, " ", ""), now)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := manager.ConfirmTOTPReplacement(
				t.Context(), state.Challenge.Token, session.Token, totpLoginTestOrigin,
				code, "198.51.100.32", "Concurrent Browser",
			)
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent TOTP replacement successes = %d", successes)
	}
	var activeTOTP, activeSessions int
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM totp_credentials
		WHERE user_id = ? AND enabled = 1 AND revoked_at IS NULL`, session.UserID,
	).Scan(&activeTOTP); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sessions WHERE user_id = ? AND revoked_at IS NULL`, session.UserID,
	).Scan(&activeSessions); err != nil {
		t.Fatal(err)
	}
	if activeTOTP != 1 || activeSessions != 1 {
		t.Fatalf("concurrent replacement state: TOTP=%d sessions=%d", activeTOTP, activeSessions)
	}
}
