package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const recoveryLoginTestToken = "recovery-login-mfa-token"

func prepareRecoveryLoginManager(t *testing.T, now time.Time, tokens TokenGenerator) (*Manager, *SetupRecoveryBatch) {
	t.Helper()
	manager, _, _ := prepareTOTPLoginManager(t, now, tokens)
	batch, hashes, err := deriveSetupRecoveryBatch("stable old recovery-code batch material")
	if err != nil {
		t.Fatal(err)
	}
	for index, codeHash := range hashes {
		if _, err := manager.db.Write().ExecContext(t.Context(), `
			INSERT INTO recovery_codes (id, user_id, batch_id, code_hash, created_at)
			VALUES (?, ?, ?, ?, ?)`,
			fmt.Sprintf("old-recovery-%d", index), totpLoginTestUserID,
			batch.BatchID, codeHash, now.Add(-time.Hour),
		); err != nil {
			t.Fatal(err)
		}
	}
	insertTOTPLoginChallenge(t, manager, "recovery-mfa-challenge", recoveryLoginTestToken, now, 3)
	return manager, batch
}

func startRecoveryRepair(t *testing.T, manager *Manager, token, code string) (*PreAuthChallenge, error) {
	t.Helper()
	return manager.StartRecoveryCodeRepair(t.Context(), RecoveryCodeLoginOptions{
		Token: token, Code: code, Origin: totpLoginTestOrigin,
		Source: "198.51.100.77", UserAgent: " Recovery Browser/1.0 ",
	})
}

func recoveryCompletionIDs() []string {
	ids := []string{"replacement-totp"}
	for index := 0; index < setupRecoveryCodeCount; index++ {
		ids = append(ids, fmt.Sprintf("replacement-recovery-%d", index))
	}
	return append(ids, "recovery-session", "credential-event", "login-event")
}

func TestStartRecoveryCodeRepairConsumesOneCodeWithoutCreatingSessionOrReplacingFactors(t *testing.T) {
	now := time.Date(2026, time.August, 9, 13, 0, 0, 0, time.UTC)
	manager, batch := prepareRecoveryLoginManager(t, now, &deterministicTokenGenerator{
		ids:    []string{"recovery-used-event", "recovery-repair-challenge"},
		tokens: []string{"recovery-repair-token", "replacement-totp-material"},
	})
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

	repair, err := startRecoveryRepair(t, manager, recoveryLoginTestToken, strings.ToLower(batch.Codes[0]))
	if err != nil {
		t.Fatalf("StartRecoveryCodeRepair() error = %v", err)
	}
	if repair == nil || repair.Token != "recovery-repair-token" || repair.Purpose != ChallengePurposeRecovery ||
		repair.UserID != totpLoginTestUserID || !repair.ExpiresAt.Equal(now.Add(recoveryRepairLifetime)) {
		t.Fatalf("recovery repair challenge = %#v", repair)
	}
	state, err := manager.GetRecoveryRepairState(t.Context(), repair.Token, totpLoginTestOrigin)
	if err != nil || state == nil || state.Enrollment == nil || state.TOTPConfirmed || state.RecoveryGenerated {
		t.Fatalf("recovery repair state = %#v, %v", state, err)
	}

	var usedAt sql.NullTime
	var unused, activeTOTP, sessions, activeMFA, activeRecovery int
	var payload []byte
	if err := manager.db.Read().QueryRow(`SELECT used_at FROM recovery_codes WHERE id = 'old-recovery-0'`).Scan(&usedAt); err != nil {
		t.Fatal(err)
	}
	queries := map[string]*int{
		`SELECT COUNT(*) FROM recovery_codes WHERE user_id = 'totp-person' AND used_at IS NULL AND revoked_at IS NULL`: &unused,
		`SELECT COUNT(*) FROM totp_credentials WHERE user_id = 'totp-person' AND enabled = 1 AND revoked_at IS NULL`:   &activeTOTP,
		`SELECT COUNT(*) FROM sessions`: &sessions,
		`SELECT COUNT(*) FROM auth_challenges WHERE purpose = 'mfa' AND consumed_at IS NULL`:      &activeMFA,
		`SELECT COUNT(*) FROM auth_challenges WHERE purpose = 'recovery' AND consumed_at IS NULL`: &activeRecovery,
	}
	for query, target := range queries {
		if err := manager.db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id = 'recovery-repair-challenge'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !usedAt.Valid || unused != setupRecoveryCodeCount-1 || activeTOTP != 1 || sessions != 0 || activeMFA != 0 || activeRecovery != 1 || len(payload) == 0 {
		t.Fatalf("recovery start state = used:%t unused:%d totp:%d sessions:%d mfa:%d recovery:%d payload:%d", usedAt.Valid, unused, activeTOTP, sessions, activeMFA, activeRecovery, len(payload))
	}
	for _, secret := range []string{batch.Codes[0], "recovery-repair-token", state.Enrollment.ManualKey} {
		if strings.Contains(string(payload), secret) {
			t.Fatalf("encrypted recovery payload exposed %q", secret)
		}
	}

	var eventType, reason, agent, sourceHash, metadata string
	if err := manager.db.Read().QueryRow(`
		SELECT event_type, reason, user_agent, source_hash, metadata_json
		FROM auth_events WHERE id = 'recovery-used-event'`,
	).Scan(&eventType, &reason, &agent, &sourceHash, &metadata); err != nil {
		t.Fatal(err)
	}
	if eventType != string(AuthEventRecoveryUsed) || reason != string(AuthEventReasonPolicyRequired) ||
		agent != "Recovery Browser/1.0" || sourceHash == "" || sourceHash == "198.51.100.77" ||
		metadata != `{"factor":"recovery_code","primary":"password","repair_required":true}` {
		t.Fatalf("recovery event = type:%q reason:%q agent:%q source:%q metadata:%q", eventType, reason, agent, sourceHash, metadata)
	}
}

func TestStartRecoveryCodeRepairRejectsInvalidCodesWithBoundedAttemptsAndRollback(t *testing.T) {
	now := time.Date(2026, time.August, 9, 13, 5, 0, 0, time.UTC)
	manager, _ := prepareRecoveryLoginManager(t, now, &deterministicTokenGenerator{
		ids: []string{"invalid-event-1", "invalid-event-2", "invalid-event-3"},
	})
	for attempt := 1; attempt <= 3; attempt++ {
		repair, err := startRecoveryRepair(t, manager, recoveryLoginTestToken, "not-a-recovery-code")
		var validationError *RecoveryCodeLoginValidationError
		if repair != nil || !errors.As(err, &validationError) || validationError.Terminal != (attempt == 3) {
			t.Fatalf("invalid recovery attempt %d = %#v, %T %v", attempt, repair, err, err)
		}
	}
	var attempts, used, repairs, events int
	var consumedAt sql.NullTime
	if err := manager.db.Read().QueryRow(`SELECT attempts, consumed_at FROM auth_challenges WHERE id = 'recovery-mfa-challenge'`).Scan(&attempts, &consumedAt); err != nil {
		t.Fatal(err)
	}
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM recovery_codes WHERE used_at IS NOT NULL`:      &used,
		`SELECT COUNT(*) FROM auth_challenges WHERE purpose = 'recovery'`:    &repairs,
		`SELECT COUNT(*) FROM auth_events WHERE event_type = 'login_failed'`: &events,
	} {
		if err := manager.db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if attempts != 3 || !consumedAt.Valid || used != 0 || repairs != 0 || events != 3 {
		t.Fatalf("bounded recovery failures = attempts:%d consumed:%t used:%d repairs:%d events:%d", attempts, consumedAt.Valid, used, repairs, events)
	}
}

func TestStartRecoveryCodeRepairRejectsUsedRevokedAndForeignCodes(t *testing.T) {
	now := time.Date(2026, time.August, 9, 13, 7, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*testing.T, *Manager, *SetupRecoveryBatch) string
	}{
		{
			name: "used",
			mutate: func(t *testing.T, manager *Manager, batch *SetupRecoveryBatch) string {
				if _, err := manager.db.Write().Exec(`UPDATE recovery_codes SET used_at = ? WHERE id = 'old-recovery-0'`, now.Add(-time.Minute)); err != nil {
					t.Fatal(err)
				}
				return batch.Codes[0]
			},
		},
		{
			name: "revoked",
			mutate: func(t *testing.T, manager *Manager, batch *SetupRecoveryBatch) string {
				if _, err := manager.db.Write().Exec(`UPDATE recovery_codes SET revoked_at = ? WHERE id = 'old-recovery-0'`, now.Add(-time.Minute)); err != nil {
					t.Fatal(err)
				}
				return batch.Codes[0]
			},
		},
		{
			name: "foreign user",
			mutate: func(t *testing.T, manager *Manager, _ *SetupRecoveryBatch) string {
				insertActiveUser(t, manager, "foreign-recovery-user", false, now)
				foreignBatch, hashes, err := deriveSetupRecoveryBatch("foreign recovery-code batch material")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := manager.db.Write().Exec(`
					INSERT INTO recovery_codes (id, user_id, batch_id, code_hash, created_at)
					VALUES ('foreign-code', 'foreign-recovery-user', ?, ?, ?)`,
					foreignBatch.BatchID, hashes[0], now,
				); err != nil {
					t.Fatal(err)
				}
				return foreignBatch.Codes[0]
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager, batch := prepareRecoveryLoginManager(t, now, &deterministicTokenGenerator{ids: []string{"rejected-event"}})
			code := test.mutate(t, manager, batch)
			repair, err := startRecoveryRepair(t, manager, recoveryLoginTestToken, code)
			var validationError *RecoveryCodeLoginValidationError
			if repair != nil || !errors.As(err, &validationError) || validationError.Terminal {
				t.Fatalf("ineligible recovery code = %#v, %T %v", repair, err, err)
			}
			var repairs int
			if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_challenges WHERE purpose = 'recovery'`).Scan(&repairs); err != nil {
				t.Fatal(err)
			}
			if repairs != 0 {
				t.Fatalf("ineligible recovery code created %d repair challenges", repairs)
			}
		})
	}
}

func TestRecoveryCodeThrottlePersistsWithoutAdvancingBlockedChallenge(t *testing.T) {
	now := time.Date(2026, time.August, 9, 13, 8, 0, 0, time.UTC)
	manager, _ := prepareRecoveryLoginManager(t, now, &deterministicTokenGenerator{
		ids: []string{"failure-1", "failure-2", "failure-3", "failure-4", "failure-5"},
	})
	if _, err := manager.db.Write().Exec(`UPDATE auth_challenges SET max_attempts = 10 WHERE id = 'recovery-mfa-challenge'`); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 4; attempt++ {
		if repair, err := startRecoveryRepair(t, manager, recoveryLoginTestToken, "AAAA-BBBB-CCCC-DDDD-EEEE-FFFF"); repair != nil {
			t.Fatalf("invalid attempt %d created repair %#v", attempt, repair)
		} else {
			var validationError *RecoveryCodeLoginValidationError
			if !errors.As(err, &validationError) {
				t.Fatalf("invalid attempt %d error = %T %v", attempt, err, err)
			}
		}
	}
	if repair, err := startRecoveryRepair(t, manager, recoveryLoginTestToken, "AAAA-BBBB-CCCC-DDDD-EEEE-FFFF"); repair != nil {
		t.Fatalf("throttling attempt created repair %#v", repair)
	} else {
		var throttleError *LoginThrottleError
		if !errors.As(err, &throttleError) || throttleError.RetryAfter != time.Second {
			t.Fatalf("fifth recovery failure = %T %v", err, err)
		}
	}
	var attempts int
	if err := manager.db.Read().QueryRow(`SELECT attempts FROM auth_challenges WHERE id = 'recovery-mfa-challenge'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 5 {
		t.Fatalf("recovery throttle attempts = %d, want 5", attempts)
	}
	if repair, err := startRecoveryRepair(t, manager, recoveryLoginTestToken, "AAAA-BBBB-CCCC-DDDD-EEEE-FFFF"); repair != nil || !errors.Is(err, ErrLoginThrottled) {
		t.Fatalf("blocked recovery attempt = %#v, %T %v", repair, err, err)
	}
	if err := manager.db.Read().QueryRow(`SELECT attempts FROM auth_challenges WHERE id = 'recovery-mfa-challenge'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 5 {
		t.Fatalf("blocked recovery attempt advanced challenge to %d", attempts)
	}
}

func TestStartRecoveryCodeRepairHasOneWinnerUnderConcurrency(t *testing.T) {
	now := time.Date(2026, time.August, 9, 13, 10, 0, 0, time.UTC)
	manager, batch := prepareRecoveryLoginManager(t, now, &deterministicTokenGenerator{
		ids:    []string{"event-a", "event-b", "repair-a", "repair-b"},
		tokens: []string{"repair-token", "repair-seed"},
	})
	insertTOTPLoginChallenge(t, manager, "parallel-recovery-mfa", "parallel-recovery-token", now, 3)

	type outcome struct {
		challenge *PreAuthChallenge
		err       error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, token := range []string{recoveryLoginTestToken, "parallel-recovery-token"} {
		go func(token string) {
			ready.Done()
			<-start
			challenge, err := manager.StartRecoveryCodeRepair(t.Context(), RecoveryCodeLoginOptions{
				Token: token, Code: batch.Codes[0], Origin: totpLoginTestOrigin,
				Source: "198.51.100.77", UserAgent: "parallel recovery",
			})
			results <- outcome{challenge: challenge, err: err}
		}(token)
	}
	ready.Wait()
	close(start)
	first, second := <-results, <-results
	winners, losers := 0, 0
	for _, result := range []outcome{first, second} {
		if result.err == nil && result.challenge != nil {
			winners++
		} else if result.challenge == nil && errors.Is(result.err, ErrRecoveryCodeLoginChallengeInvalid) {
			losers++
		} else {
			t.Fatalf("concurrent recovery outcome = %#v, %T %v", result.challenge, result.err, result.err)
		}
	}
	var used, activeRepair, activeMFA int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM recovery_codes WHERE used_at IS NOT NULL`:                           &used,
		`SELECT COUNT(*) FROM auth_challenges WHERE purpose = 'recovery' AND consumed_at IS NULL`: &activeRepair,
		`SELECT COUNT(*) FROM auth_challenges WHERE purpose = 'mfa' AND consumed_at IS NULL`:      &activeMFA,
	} {
		if err := manager.db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if winners != 1 || losers != 1 || used != 1 || activeRepair != 1 || activeMFA != 0 {
		t.Fatalf("concurrent recovery = winners:%d losers:%d used:%d repair:%d mfa:%d", winners, losers, used, activeRepair, activeMFA)
	}
}

func TestRecoveryRepairCompletionReplacesFactorsRevokesSessionsAndCreatesMFAState(t *testing.T) {
	now := time.Date(2026, time.August, 9, 13, 15, 0, 0, time.UTC)
	manager, oldBatch := prepareRecoveryLoginManager(t, now, &deterministicTokenGenerator{
		ids:    []string{"recovery-used-event", "recovery-repair-challenge"},
		tokens: []string{"recovery-repair-token", "replacement-totp-material"},
	})
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method, assurance_level,
			user_agent, authenticated_at, last_used_at, idle_expires_at, absolute_expires_at, created_at
		) VALUES ('old-session', ?, ?, 1, 'password', 'multi_factor', 'old', ?, ?, ?, ?, ?)`,
		totpLoginTestUserID, hashToken("old-session-token"), now.Add(-time.Hour), now.Add(-time.Hour),
		now.Add(time.Hour), now.Add(24*time.Hour), now.Add(-time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	repair, err := startRecoveryRepair(t, manager, recoveryLoginTestToken, oldBatch.Codes[0])
	if err != nil {
		t.Fatal(err)
	}
	_, draft, _, err := manager.readRecoveryRepairDraft(t.Context(), repair.Token, totpLoginTestOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ConfirmRecoveryRepairTOTP(
		t.Context(), repair.Token, totpLoginTestOrigin, setupTOTPCode(t, draft.TOTPSecret, now),
	); err != nil {
		t.Fatalf("ConfirmRecoveryRepairTOTP() error = %v", err)
	}
	manager.tokens = &deterministicTokenGenerator{tokens: []string{"fresh recovery batch material"}}
	newBatch, err := manager.GenerateRecoveryRepairCodes(t.Context(), repair.Token, totpLoginTestOrigin, false)
	if err != nil || newBatch == nil || len(newBatch.Codes) != setupRecoveryCodeCount {
		t.Fatalf("GenerateRecoveryRepairCodes() = %#v, %v", newBatch, err)
	}
	manager.tokens = &deterministicTokenGenerator{
		ids: recoveryCompletionIDs(), tokens: []string{"recovery-session-token"},
	}
	session, err := manager.CompleteRecoveryRepair(t.Context(), CompleteRecoveryRepairOptions{
		Token: repair.Token, Origin: totpLoginTestOrigin, BatchID: newBatch.RecoveryBatchID,
		Saved: true, Source: "198.51.100.77", UserAgent: " Recovery Browser/1.0 ",
	})
	if err != nil {
		t.Fatalf("CompleteRecoveryRepair() error = %v", err)
	}
	if session == nil || session.Token != "recovery-session-token" || session.AuthVersion != 2 ||
		session.AuthenticationMethod != AuthenticationMethodPassword || session.AssuranceLevel != AssuranceLevelMultiFactor ||
		session.StepUpAt == nil || session.StepUpMethod != AuthenticationMethodRecoveryCode {
		t.Fatalf("recovery session = %#v", session)
	}
	stored, err := manager.GetSessionByToken(t.Context(), session.Token)
	if err != nil || stored == nil || stored.ID != session.ID {
		t.Fatalf("stored recovery session = %#v, %v", stored, err)
	}

	var activeTOTP, revokedTOTP, activeNewCodes, revokedOldCodes, usedOldCodes int
	var oldSessionRevoked sql.NullTime
	var oldSessionReason string
	var challengeConsumed sql.NullTime
	var challengePayload []byte
	queries := map[string]*int{
		`SELECT COUNT(*) FROM totp_credentials WHERE user_id = 'totp-person' AND enabled = 1 AND revoked_at IS NULL`:                         &activeTOTP,
		`SELECT COUNT(*) FROM totp_credentials WHERE user_id = 'totp-person' AND enabled = 0 AND revoked_at IS NOT NULL`:                     &revokedTOTP,
		`SELECT COUNT(*) FROM recovery_codes WHERE batch_id = '` + newBatch.RecoveryBatchID + `' AND used_at IS NULL AND revoked_at IS NULL`: &activeNewCodes,
		`SELECT COUNT(*) FROM recovery_codes WHERE batch_id = '` + oldBatch.BatchID + `' AND used_at IS NULL AND revoked_at IS NOT NULL`:     &revokedOldCodes,
		`SELECT COUNT(*) FROM recovery_codes WHERE batch_id = '` + oldBatch.BatchID + `' AND used_at IS NOT NULL AND revoked_at IS NULL`:     &usedOldCodes,
	}
	for query, target := range queries {
		if err := manager.db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.db.Read().QueryRow(`SELECT revoked_at, revocation_reason FROM sessions WHERE id = 'old-session'`).Scan(&oldSessionRevoked, &oldSessionReason); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT consumed_at, payload_ciphertext FROM auth_challenges WHERE id = 'recovery-repair-challenge'`).Scan(&challengeConsumed, &challengePayload); err != nil {
		t.Fatal(err)
	}
	if activeTOTP != 1 || revokedTOTP != 1 || activeNewCodes != setupRecoveryCodeCount ||
		revokedOldCodes != setupRecoveryCodeCount-1 || usedOldCodes != 1 || !oldSessionRevoked.Valid ||
		oldSessionReason != string(SessionRevocationCredentialReset) || !challengeConsumed.Valid || challengePayload != nil {
		t.Fatalf("completed repair state = activeTOTP:%d revokedTOTP:%d new:%d oldRevoked:%d oldUsed:%d sessionRevoked:%t reason:%q challenge:%t payload:%d",
			activeTOTP, revokedTOTP, activeNewCodes, revokedOldCodes, usedOldCodes,
			oldSessionRevoked.Valid, oldSessionReason, challengeConsumed.Valid, len(challengePayload))
	}
	var recoveryEvents, credentialEvents, loginEvents int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM auth_events WHERE event_type = 'recovery_used'`:      &recoveryEvents,
		`SELECT COUNT(*) FROM auth_events WHERE event_type = 'credential_changed'`: &credentialEvents,
		`SELECT COUNT(*) FROM auth_events WHERE event_type = 'login_succeeded'`:    &loginEvents,
	} {
		if err := manager.db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if recoveryEvents != 1 || credentialEvents != 1 || loginEvents != 1 {
		t.Fatalf("recovery events = used:%d credential:%d login:%d", recoveryEvents, credentialEvents, loginEvents)
	}
}

func TestRecoveryRepairTOTPFailuresTerminateRepairWithoutReplacingCurrentFactors(t *testing.T) {
	now := time.Date(2026, time.August, 9, 13, 18, 0, 0, time.UTC)
	manager, batch := prepareRecoveryLoginManager(t, now, &deterministicTokenGenerator{
		ids:    []string{"recovery-used-event", "recovery-repair-challenge"},
		tokens: []string{"recovery-repair-token", "replacement-totp-material"},
	})
	repair, err := startRecoveryRepair(t, manager, recoveryLoginTestToken, batch.Codes[0])
	if err != nil {
		t.Fatal(err)
	}
	_, draft, _, err := manager.readRecoveryRepairDraft(t.Context(), repair.Token, totpLoginTestOrigin)
	if err != nil {
		t.Fatal(err)
	}
	invalidCode := invalidTOTPCode(setupTOTPCode(t, draft.TOTPSecret, now))
	for attempt := 1; attempt <= recoveryRepairMaxAttempts; attempt++ {
		state, err := manager.ConfirmRecoveryRepairTOTP(t.Context(), repair.Token, totpLoginTestOrigin, invalidCode)
		var validationError *RecoveryRepairTOTPValidationError
		if state != nil || !errors.As(err, &validationError) || validationError.Terminal != (attempt == recoveryRepairMaxAttempts) {
			t.Fatalf("invalid repaired TOTP attempt %d = %#v, %T %v", attempt, state, err, err)
		}
	}
	var activeTOTP, unrevokedOldCodes, sessions int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM totp_credentials WHERE id = 'totp-credential' AND enabled = 1 AND revoked_at IS NULL`: &activeTOTP,
		`SELECT COUNT(*) FROM recovery_codes WHERE batch_id = '` + batch.BatchID + `' AND revoked_at IS NULL`:       &unrevokedOldCodes,
		`SELECT COUNT(*) FROM sessions`: &sessions,
	} {
		if err := manager.db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if activeTOTP != 1 || unrevokedOldCodes != setupRecoveryCodeCount || sessions != 0 {
		t.Fatalf("terminated repair changed factors = totp:%d oldCodes:%d sessions:%d", activeTOTP, unrevokedOldCodes, sessions)
	}
	if _, err := manager.GetRecoveryRepairState(t.Context(), repair.Token, totpLoginTestOrigin); !errors.Is(err, ErrRecoveryRepairInvalid) {
		t.Fatalf("terminated repair state error = %v", err)
	}
}

func TestRecoveryRepairCodeBatchIsShownOnceReplaceableAndExact(t *testing.T) {
	now := time.Date(2026, time.August, 9, 13, 19, 0, 0, time.UTC)
	manager, batch := prepareRecoveryLoginManager(t, now, &deterministicTokenGenerator{
		ids:    []string{"recovery-used-event", "recovery-repair-challenge"},
		tokens: []string{"recovery-repair-token", "replacement-totp-material"},
	})
	repair, err := startRecoveryRepair(t, manager, recoveryLoginTestToken, batch.Codes[0])
	if err != nil {
		t.Fatal(err)
	}
	_, draft, _, err := manager.readRecoveryRepairDraft(t.Context(), repair.Token, totpLoginTestOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ConfirmRecoveryRepairTOTP(t.Context(), repair.Token, totpLoginTestOrigin, setupTOTPCode(t, draft.TOTPSecret, now)); err != nil {
		t.Fatal(err)
	}
	manager.tokens = &deterministicTokenGenerator{tokens: []string{"first fresh recovery material"}}
	first, err := manager.GenerateRecoveryRepairCodes(t.Context(), repair.Token, totpLoginTestOrigin, false)
	if err != nil {
		t.Fatal(err)
	}
	if repeated, err := manager.GenerateRecoveryRepairCodes(t.Context(), repair.Token, totpLoginTestOrigin, false); repeated != nil || !errors.Is(err, ErrRecoveryRepairBatchExists) {
		t.Fatalf("repeated recovery generation = %#v, %v", repeated, err)
	}
	state, err := manager.GetRecoveryRepairState(t.Context(), repair.Token, totpLoginTestOrigin)
	if err != nil || state == nil || !state.RecoveryGenerated || state.RecoveryBatchID != first.RecoveryBatchID {
		t.Fatalf("persisted first recovery batch = %#v, %v", state, err)
	}
	manager.tokens = &deterministicTokenGenerator{tokens: []string{"second fresh recovery material"}}
	second, err := manager.GenerateRecoveryRepairCodes(t.Context(), repair.Token, totpLoginTestOrigin, true)
	if err != nil {
		t.Fatal(err)
	}
	if second.RecoveryBatchID == first.RecoveryBatchID || second.Codes[0] == first.Codes[0] {
		t.Fatalf("replacement recovery batch did not change: first:%#v second:%#v", first, second)
	}
	if session, err := manager.CompleteRecoveryRepair(t.Context(), CompleteRecoveryRepairOptions{
		Token: repair.Token, Origin: totpLoginTestOrigin, BatchID: first.RecoveryBatchID, Saved: true,
	}); session != nil || !errors.Is(err, ErrRecoveryRepairStateChanged) {
		t.Fatalf("stale recovery batch completion = %#v, %v", session, err)
	}
}

func TestRecoveryTransitionsRollbackWhenAuditPersistenceFails(t *testing.T) {
	now := time.Date(2026, time.August, 9, 13, 20, 0, 0, time.UTC)
	t.Run("code consumption", func(t *testing.T) {
		manager, batch := prepareRecoveryLoginManager(t, now, &deterministicTokenGenerator{
			ids:    []string{"rejected-event", "rejected-repair"},
			tokens: []string{"rejected-token", "rejected-seed"},
		})
		if _, err := manager.db.Write().Exec(`
			CREATE TRIGGER reject_recovery_event BEFORE INSERT ON auth_events
			BEGIN SELECT RAISE(ABORT, 'reject recovery event'); END`); err != nil {
			t.Fatal(err)
		}
		if repair, err := startRecoveryRepair(t, manager, recoveryLoginTestToken, batch.Codes[0]); repair != nil || err == nil {
			t.Fatalf("audit-rejected recovery start = %#v, %v", repair, err)
		}
		var used, activeMFA, repairs int
		for query, target := range map[string]*int{
			`SELECT COUNT(*) FROM recovery_codes WHERE used_at IS NOT NULL`:                      &used,
			`SELECT COUNT(*) FROM auth_challenges WHERE purpose = 'mfa' AND consumed_at IS NULL`: &activeMFA,
			`SELECT COUNT(*) FROM auth_challenges WHERE purpose = 'recovery'`:                    &repairs,
		} {
			if err := manager.db.Read().QueryRow(query).Scan(target); err != nil {
				t.Fatal(err)
			}
		}
		if used != 0 || activeMFA != 1 || repairs != 0 {
			t.Fatalf("rolled-back recovery start = used:%d mfa:%d repairs:%d", used, activeMFA, repairs)
		}
	})

	t.Run("factor replacement", func(t *testing.T) {
		manager, batch := prepareRecoveryLoginManager(t, now, &deterministicTokenGenerator{
			ids:    []string{"recovery-used-event", "recovery-repair-challenge"},
			tokens: []string{"recovery-repair-token", "replacement-totp-material"},
		})
		repair, err := startRecoveryRepair(t, manager, recoveryLoginTestToken, batch.Codes[0])
		if err != nil {
			t.Fatal(err)
		}
		_, draft, _, err := manager.readRecoveryRepairDraft(t.Context(), repair.Token, totpLoginTestOrigin)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.ConfirmRecoveryRepairTOTP(t.Context(), repair.Token, totpLoginTestOrigin, setupTOTPCode(t, draft.TOTPSecret, now)); err != nil {
			t.Fatal(err)
		}
		manager.tokens = &deterministicTokenGenerator{tokens: []string{"fresh recovery batch material"}}
		newBatch, err := manager.GenerateRecoveryRepairCodes(t.Context(), repair.Token, totpLoginTestOrigin, false)
		if err != nil {
			t.Fatal(err)
		}
		manager.tokens = &deterministicTokenGenerator{ids: recoveryCompletionIDs(), tokens: []string{"recovery-session-token"}}
		if _, err := manager.db.Write().Exec(`
			CREATE TRIGGER reject_recovery_login_event BEFORE INSERT ON auth_events
			WHEN NEW.event_type = 'login_succeeded'
			BEGIN SELECT RAISE(ABORT, 'reject recovery login event'); END`); err != nil {
			t.Fatal(err)
		}
		if session, err := manager.CompleteRecoveryRepair(t.Context(), CompleteRecoveryRepairOptions{
			Token: repair.Token, Origin: totpLoginTestOrigin, BatchID: newBatch.RecoveryBatchID,
			Saved: true, Source: "198.51.100.77", UserAgent: "recovery rollback",
		}); session != nil || err == nil {
			t.Fatalf("audit-rejected recovery completion = %#v, %v", session, err)
		}
		var authVersion int64
		var activeOldTOTP, activeOldCodes, newCodes, sessions int
		if err := manager.db.Read().QueryRow(`SELECT auth_version FROM users WHERE id = 'totp-person'`).Scan(&authVersion); err != nil {
			t.Fatal(err)
		}
		for query, target := range map[string]*int{
			`SELECT COUNT(*) FROM totp_credentials WHERE id = 'totp-credential' AND enabled = 1 AND revoked_at IS NULL`: &activeOldTOTP,
			`SELECT COUNT(*) FROM recovery_codes WHERE batch_id = '` + batch.BatchID + `' AND revoked_at IS NULL`:       &activeOldCodes,
			`SELECT COUNT(*) FROM recovery_codes WHERE batch_id = '` + newBatch.RecoveryBatchID + `'`:                   &newCodes,
			`SELECT COUNT(*) FROM sessions`: &sessions,
		} {
			if err := manager.db.Read().QueryRow(query).Scan(target); err != nil {
				t.Fatal(err)
			}
		}
		state, stateErr := manager.GetRecoveryRepairState(t.Context(), repair.Token, totpLoginTestOrigin)
		if authVersion != 1 || activeOldTOTP != 1 || activeOldCodes != setupRecoveryCodeCount || newCodes != 0 || sessions != 0 ||
			stateErr != nil || state == nil || !state.RecoveryGenerated {
			t.Fatalf("rolled-back completion = auth:%d totp:%d old:%d new:%d sessions:%d state:%#v err:%v",
				authVersion, activeOldTOTP, activeOldCodes, newCodes, sessions, state, stateErr)
		}
	})
}
