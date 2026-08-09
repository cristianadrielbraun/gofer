package auth

import (
	"bytes"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
)

func prepareVerifiedSetupTOTPForRecovery(t *testing.T, manager *Manager, clock *fixedClock) *SetupOwnerState {
	t.Helper()
	prepareSetupPasswordForTOTP(t, manager)
	if _, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); err != nil {
		t.Fatalf("start setup TOTP prerequisite: %v", err)
	}
	state, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := manager.ConfirmSetupTOTP(
		t.Context(), setupOwnerTestToken, setupOwnerTestOrigin,
		setupTOTPCode(t, state.Draft.TOTPSecret, clock.now),
	)
	if err != nil {
		t.Fatalf("confirm setup TOTP prerequisite: %v", err)
	}
	return confirmed
}

func TestSetupRecoveryRequiresVerifiedTOTP(t *testing.T) {
	manager, _ := setupOwnerTestManager(t)
	if _, err := manager.GetSetupRecoveryState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin); !errors.Is(err, ErrSetupPasswordDraftRequired) {
		t.Fatalf("recovery state without owner = %v", err)
	}
	prepareSetupPasswordForTOTP(t, manager)
	if _, err := manager.GenerateSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); !errors.Is(err, ErrSetupTOTPConfirmationRequired) {
		t.Fatalf("recovery generation without TOTP = %v", err)
	}
	if _, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.GenerateSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); !errors.Is(err, ErrSetupTOTPConfirmationRequired) {
		t.Fatalf("recovery generation before TOTP confirmation = %v", err)
	}
}

func TestGenerateSetupRecoveryCodesReturnsPlaintextOnceAndStoresOnlyEncryptedHashes(t *testing.T) {
	manager, clock := setupOwnerTestManager(t)
	prepareVerifiedSetupTOTPForRecovery(t, manager, clock)
	batch, err := manager.GenerateSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false)
	if err != nil {
		t.Fatalf("GenerateSetupRecoveryCodes() error = %v", err)
	}
	if batch == nil || !batch.Generated || batch.Acknowledged || !isLowerHexHash(batch.BatchID) || len(batch.Codes) != setupRecoveryCodeCount {
		t.Fatalf("generated recovery batch = %#v", batch)
	}

	seen := make(map[string]struct{}, setupRecoveryCodeCount)
	state, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if err != nil || state.Draft == nil || !state.RecoveryGenerated || state.RecoveryReady || state.Draft.RecoveryAcknowledged ||
		state.Draft.RecoveryBatchID != batch.BatchID || len(state.Draft.RecoveryCodeHashes) != setupRecoveryCodeCount {
		t.Fatalf("stored recovery draft = %#v, %v", state, err)
	}
	for index, displayed := range batch.Codes {
		parts := strings.Split(displayed, "-")
		if len(parts) != 6 {
			t.Fatalf("recovery code %d groups = %q", index, displayed)
		}
		for _, part := range parts {
			if len(part) != setupRecoveryCodeGroupSize {
				t.Fatalf("recovery code %d group = %q", index, part)
			}
		}
		canonical := strings.Join(parts, "")
		if len(canonical) != setupRecoveryCodeByteCount*8/5 || strings.ContainsAny(canonical, "ILOU") {
			t.Fatalf("recovery code %d canonical form = %q", index, canonical)
		}
		if _, duplicate := seen[canonical]; duplicate {
			t.Fatalf("duplicate recovery code %q", displayed)
		}
		seen[canonical] = struct{}{}
		if state.Draft.RecoveryCodeHashes[index] != hashToken(canonical) {
			t.Fatalf("recovery code %d hash mismatch", index)
		}
	}

	var payload []byte
	if err := manager.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	for _, code := range batch.Codes {
		if bytes.Contains(payload, []byte(code)) || bytes.Contains(payload, []byte(strings.ReplaceAll(code, "-", ""))) {
			t.Fatal("encrypted setup payload exposed a plaintext recovery code")
		}
	}
	for _, codeHash := range state.Draft.RecoveryCodeHashes {
		if bytes.Contains(payload, []byte(codeHash)) {
			t.Fatal("encrypted setup payload exposed a recovery-code hash")
		}
	}

	readState, err := manager.GetSetupRecoveryState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if err != nil || readState == nil || !readState.Generated || readState.Acknowledged || readState.BatchID != batch.BatchID {
		t.Fatalf("recovery state = %#v, %v", readState, err)
	}
	if _, err := manager.GenerateSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); !errors.Is(err, ErrSetupRecoveryBatchAlreadyGenerated) {
		t.Fatalf("repeated recovery generation = %v", err)
	}

	var users, passwords, totps, recoveryCodes, sessions, events, initialized int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM users`:                             &users,
		`SELECT COUNT(*) FROM password_credentials`:              &passwords,
		`SELECT COUNT(*) FROM totp_credentials`:                  &totps,
		`SELECT COUNT(*) FROM recovery_codes`:                    &recoveryCodes,
		`SELECT COUNT(*) FROM sessions`:                          &sessions,
		`SELECT COUNT(*) FROM auth_events`:                       &events,
		`SELECT initialized FROM auth_system_state WHERE id = 1`: &initialized,
	} {
		if err := manager.db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if users != 0 || passwords != 0 || totps != 0 || recoveryCodes != 0 || sessions != 0 || events != 0 || initialized != 0 {
		t.Fatalf("recovery draft partially enrolled = users:%d passwords:%d totps:%d recovery:%d sessions:%d events:%d initialized:%d",
			users, passwords, totps, recoveryCodes, sessions, events, initialized)
	}
}

func TestSetupRecoveryAcknowledgementIsExactIdempotentAndBatchBound(t *testing.T) {
	manager, clock := setupOwnerTestManager(t)
	prepareVerifiedSetupTOTPForRecovery(t, manager, clock)
	first, err := manager.GenerateSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false)
	if err != nil {
		t.Fatal(err)
	}

	var validationErr *SetupRecoveryValidationError
	if state, err := manager.AcknowledgeSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, first.BatchID, false); state != nil || !errors.As(err, &validationErr) {
		t.Fatalf("unchecked recovery acknowledgement = %#v, %v", state, err)
	}
	validationErr = nil
	if state, err := manager.AcknowledgeSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, strings.ToUpper(first.BatchID), true); state != nil || !errors.As(err, &validationErr) {
		t.Fatalf("non-exact recovery acknowledgement = %#v, %v", state, err)
	}

	acknowledged, err := manager.AcknowledgeSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, first.BatchID, true)
	if err != nil || acknowledged == nil || !acknowledged.Generated || !acknowledged.Acknowledged {
		t.Fatalf("recovery acknowledgement = %#v, %v", acknowledged, err)
	}
	acknowledgedAgain, err := manager.AcknowledgeSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, first.BatchID, true)
	if err != nil || acknowledgedAgain == nil || !acknowledgedAgain.Acknowledged {
		t.Fatalf("idempotent recovery acknowledgement = %#v, %v", acknowledgedAgain, err)
	}

	replaced, err := manager.GenerateSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, true)
	if err != nil || replaced == nil || replaced.BatchID == first.BatchID || replaced.Acknowledged || len(replaced.Codes) != setupRecoveryCodeCount {
		t.Fatalf("replacement recovery batch = %#v, %v", replaced, err)
	}
	validationErr = nil
	if state, err := manager.AcknowledgeSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, first.BatchID, true); state != nil || !errors.As(err, &validationErr) {
		t.Fatalf("stale recovery acknowledgement = %#v, %v", state, err)
	}
	current, err := manager.GetSetupRecoveryState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if err != nil || current.BatchID != replaced.BatchID || current.Acknowledged {
		t.Fatalf("current replacement state = %#v, %v", current, err)
	}
}

func TestSetupRecoveryIsInvalidatedByTOTPReplacementAndOwnerEdit(t *testing.T) {
	manager, clock := setupOwnerTestManager(t)
	prepareVerifiedSetupTOTPForRecovery(t, manager, clock)
	batch, err := manager.GenerateSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AcknowledgeSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, batch.BatchID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, true); err != nil {
		t.Fatal(err)
	}
	state, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if err != nil || state.RecoveryGenerated || state.RecoveryReady || state.Draft.RecoveryBatchID != "" || len(state.Draft.RecoveryCodeHashes) != 0 || state.Draft.RecoveryAcknowledged {
		t.Fatalf("TOTP replacement retained recovery state = %#v, %v", state, err)
	}
	if _, err := manager.GetSetupRecoveryState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin); !errors.Is(err, ErrSetupTOTPConfirmationRequired) {
		t.Fatalf("recovery state after TOTP replacement = %v", err)
	}
	if _, err := manager.SaveSetupOwnerDraft(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, SetupOwnerDraftInput{
		Mode: SetupOwnerModeCreate, Name: "Cristian Braun", Username: "recovery-owner", Email: "recovery-owner@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	edited, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if err != nil || edited.PasswordReady || edited.TOTPReady || edited.RecoveryGenerated || edited.RecoveryReady {
		t.Fatalf("owner edit retained security drafts = %#v, %v", edited, err)
	}
}

func TestGenerateSetupRecoveryCodesAllowsOnlyOneConcurrentInitialBatch(t *testing.T) {
	manager, clock := setupOwnerTestManager(t)
	prepareVerifiedSetupTOTPForRecovery(t, manager, clock)
	results := make(chan struct {
		batch *SetupRecoveryBatch
		err   error
	}, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			ready.Done()
			<-start
			batch, err := manager.GenerateSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false)
			results <- struct {
				batch *SetupRecoveryBatch
				err   error
			}{batch: batch, err: err}
		}()
	}
	ready.Wait()
	close(start)
	first, second := <-results, <-results
	successes, rejected := 0, 0
	for _, result := range []struct {
		batch *SetupRecoveryBatch
		err   error
	}{first, second} {
		if result.err == nil && result.batch != nil {
			successes++
			continue
		}
		if result.batch == nil && (errors.Is(result.err, ErrSetupRecoveryStateChanged) || errors.Is(result.err, ErrSetupRecoveryBatchAlreadyGenerated)) {
			rejected++
		}
	}
	if successes != 1 || rejected != 1 {
		t.Fatalf("concurrent recovery generation = %#v / %#v", first, second)
	}
}

func TestSetupRecoveryFailuresDoNotExposeOrReplaceDraft(t *testing.T) {
	t.Run("randomness", func(t *testing.T) {
		manager, clock := setupOwnerTestManager(t)
		prepareVerifiedSetupTOTPForRecovery(t, manager, clock)
		var original []byte
		if err := manager.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&original); err != nil {
			t.Fatal(err)
		}
		manager.tokens = &deterministicTokenGenerator{tokenErr: errors.New("random source unavailable")}
		if batch, err := manager.GenerateSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); batch != nil || err == nil || !strings.Contains(err.Error(), "random source unavailable") {
			t.Fatalf("recovery randomness failure = %#v, %v", batch, err)
		}
		var stored []byte
		if err := manager.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(stored, original) {
			t.Fatal("recovery randomness failure replaced setup draft")
		}
	})

	t.Run("storage", func(t *testing.T) {
		manager, clock := setupOwnerTestManager(t)
		prepareVerifiedSetupTOTPForRecovery(t, manager, clock)
		var original []byte
		if err := manager.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&original); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.db.Write().Exec(`
			CREATE TRIGGER reject_setup_recovery_payload
			BEFORE UPDATE OF payload_ciphertext ON auth_challenges
			BEGIN SELECT RAISE(ABORT, 'reject setup recovery payload'); END`); err != nil {
			t.Fatal(err)
		}
		if batch, err := manager.GenerateSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); batch != nil || err == nil {
			t.Fatalf("recovery storage failure = %#v, %v", batch, err)
		}
		var stored []byte
		if err := manager.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(stored, original) {
			t.Fatal("recovery storage failure replaced setup draft")
		}
	})

	t.Run("acknowledgement storage", func(t *testing.T) {
		manager, clock := setupOwnerTestManager(t)
		prepareVerifiedSetupTOTPForRecovery(t, manager, clock)
		batch, err := manager.GenerateSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.db.Write().Exec(`
			CREATE TRIGGER reject_setup_recovery_acknowledgement
			BEFORE UPDATE OF payload_ciphertext ON auth_challenges
			BEGIN SELECT RAISE(ABORT, 'reject setup recovery acknowledgement'); END`); err != nil {
			t.Fatal(err)
		}
		if state, err := manager.AcknowledgeSetupRecoveryCodes(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, batch.BatchID, true); state != nil || err == nil {
			t.Fatalf("recovery acknowledgement storage failure = %#v, %v", state, err)
		}
		var acknowledged bool
		var recoveryRows int
		ownerState, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
		if err != nil {
			t.Fatal(err)
		}
		acknowledged = ownerState.Draft.RecoveryAcknowledged
		if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM recovery_codes`).Scan(&recoveryRows); err != nil {
			t.Fatal(err)
		}
		if acknowledged || recoveryRows != 0 {
			t.Fatalf("failed acknowledgement persisted state = acknowledged:%t rows:%d", acknowledged, recoveryRows)
		}
	})
}

func TestSetupRecoveryDraftRejectsMalformedBatchMetadata(t *testing.T) {
	manager, clock := setupOwnerTestManager(t)
	prepareVerifiedSetupTOTPForRecovery(t, manager, clock)
	challenge, err := manager.GetActiveSetupAccess(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if err != nil || challenge == nil {
		t.Fatalf("setup challenge = %#v, %v", challenge, err)
	}
	state, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if err != nil {
		t.Fatal(err)
	}
	state.Draft.RecoveryBatchID = strings.Repeat("a", 64)
	state.Draft.RecoveryCodeHashes = []string{strings.Repeat("b", 64), strings.Repeat("b", 64)}
	payload, err := manager.encryptSetupOwnerDraft(challenge, state.Draft)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().Exec(`UPDATE auth_challenges SET payload_ciphertext = ? WHERE id = ?`, payload, challenge.ID); err != nil {
		t.Fatal(err)
	}
	if recoveryState, err := manager.GetSetupRecoveryState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin); recoveryState != nil || err == nil {
		t.Fatalf("malformed recovery state = %#v, %v", recoveryState, err)
	}
	var recoveryRows int
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM recovery_codes`).Scan(&recoveryRows); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if recoveryRows != 0 {
		t.Fatalf("malformed recovery draft created %d persistent rows", recoveryRows)
	}
}
