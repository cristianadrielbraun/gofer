package auth

import (
	"bytes"
	"database/sql"
	"errors"
	"image/png"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

const setupTOTPTestPassword = "correct horse battery staple for owner"

func prepareSetupPasswordForTOTP(t *testing.T, manager *Manager) {
	t.Helper()
	saveFreshSetupOwnerProfile(t, manager)
	if _, err := manager.SaveSetupPasswordDraft(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, SetupPasswordDraftInput{
		Password: setupTOTPTestPassword, PasswordConfirmation: setupTOTPTestPassword,
	}); err != nil {
		t.Fatalf("save setup password prerequisite: %v", err)
	}
}

func setupTOTPCode(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{
		Period: totpPeriodSeconds, Skew: 0, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestSetupTOTPRequiresCurrentPasswordDraft(t *testing.T) {
	manager, _ := setupOwnerTestManager(t)
	if _, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); !errors.Is(err, ErrSetupPasswordDraftRequired) {
		t.Fatalf("TOTP without owner = %v", err)
	}
	saveFreshSetupOwnerProfile(t, manager)
	if _, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); !errors.Is(err, ErrSetupPasswordDraftRequired) {
		t.Fatalf("TOTP without password = %v", err)
	}
	if _, err := manager.GetSetupTOTPEnrollment(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin); !errors.Is(err, ErrSetupPasswordDraftRequired) {
		t.Fatalf("read TOTP without password = %v", err)
	}
}

func TestStartSetupTOTPStoresOnlyEncryptedDraftAndRendersLocalEnrollment(t *testing.T) {
	manager, _ := setupOwnerTestManager(t)
	prepareSetupPasswordForTOTP(t, manager)
	enrollment, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false)
	if err != nil {
		t.Fatalf("StartSetupTOTP() error = %v", err)
	}
	if enrollment == nil || enrollment.Confirmed || enrollment.ManualKey == "" || enrollment.Algorithm != "SHA1" || enrollment.Digits != 6 || enrollment.Period != 30 {
		t.Fatalf("TOTP enrollment = %#v", enrollment)
	}
	if _, err := png.Decode(bytes.NewReader(enrollment.QRPNG)); err != nil {
		t.Fatalf("decode local TOTP QR PNG: %v", err)
	}

	state, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if err != nil || state == nil || state.Draft == nil || !state.PasswordReady || !state.TOTPStarted || state.TOTPReady || state.Draft.TOTPSecret == "" || state.Draft.TOTPConfirmedStep != nil {
		t.Fatalf("started TOTP state = %#v, %v", state, err)
	}
	if strings.ReplaceAll(enrollment.ManualKey, " ", "") != state.Draft.TOTPSecret {
		t.Fatalf("manual key does not represent stored secret: %q", enrollment.ManualKey)
	}
	var payload []byte
	if err := manager.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte(state.Draft.TOTPSecret)) || bytes.Contains(payload, []byte(enrollment.ManualKey)) {
		t.Fatal("TOTP seed was stored outside the encrypted draft")
	}
	var users, passwords, totps, sessions, events, initialized int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM users`:                             &users,
		`SELECT COUNT(*) FROM password_credentials`:              &passwords,
		`SELECT COUNT(*) FROM totp_credentials`:                  &totps,
		`SELECT COUNT(*) FROM sessions`:                          &sessions,
		`SELECT COUNT(*) FROM auth_events`:                       &events,
		`SELECT initialized FROM auth_system_state WHERE id = 1`: &initialized,
	} {
		if err := manager.db.Read().QueryRow(query).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if users != 0 || passwords != 0 || totps != 0 || sessions != 0 || events != 0 || initialized != 0 {
		t.Fatalf("TOTP draft partially enrolled = users:%d passwords:%d totps:%d sessions:%d events:%d initialized:%d", users, passwords, totps, sessions, events, initialized)
	}
}

func TestStartSetupTOTPIsStableUntilExplicitReplacement(t *testing.T) {
	manager, _ := setupOwnerTestManager(t)
	prepareSetupPasswordForTOTP(t, manager)
	first, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false)
	if err != nil {
		t.Fatal(err)
	}
	var firstPayload []byte
	if err := manager.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&firstPayload); err != nil {
		t.Fatal(err)
	}
	second, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false)
	if err != nil || second.ManualKey != first.ManualKey {
		t.Fatalf("idempotent TOTP start = %#v, %v", second, err)
	}
	var secondPayload []byte
	if err := manager.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&secondPayload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstPayload, secondPayload) {
		t.Fatal("ordinary TOTP retry replaced the encrypted draft")
	}
	replaced, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, true)
	if err != nil || replaced.ManualKey == first.ManualKey || replaced.Confirmed {
		t.Fatalf("explicit TOTP replacement = %#v, %v", replaced, err)
	}
}

func TestConfirmSetupTOTPAcceptsNarrowWindowAndStoresMatchedStep(t *testing.T) {
	for _, offset := range []int{-1, 0, 1} {
		t.Run((time.Duration(offset*totpPeriodSeconds) * time.Second).String(), func(t *testing.T) {
			manager, clock := setupOwnerTestManager(t)
			prepareSetupPasswordForTOTP(t, manager)
			if _, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); err != nil {
				t.Fatal(err)
			}
			state, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
			if err != nil {
				t.Fatal(err)
			}
			codeTime := clock.now.Add(time.Duration(offset*totpPeriodSeconds) * time.Second)
			confirmed, err := manager.ConfirmSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, setupTOTPCode(t, state.Draft.TOTPSecret, codeTime))
			wantStep := codeTime.Unix() / totpPeriodSeconds
			if err != nil || confirmed == nil || !confirmed.TOTPReady || confirmed.Draft.TOTPConfirmedStep == nil || *confirmed.Draft.TOTPConfirmedStep != wantStep {
				t.Fatalf("confirmed offset %d = %#v, %v; want step %d", offset, confirmed, err, wantStep)
			}
			var attempts, totps int
			if err := manager.db.Read().QueryRow(`SELECT attempts FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&attempts); err != nil {
				t.Fatal(err)
			}
			if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM totp_credentials`).Scan(&totps); err != nil {
				t.Fatal(err)
			}
			if attempts != 0 || totps != 0 {
				t.Fatalf("confirmation persistence = attempts:%d totps:%d", attempts, totps)
			}
		})
	}
}

func TestConfirmSetupTOTPRejectsOutsideWindowReplayAndRepeatedFailures(t *testing.T) {
	t.Run("outside window and replay", func(t *testing.T) {
		manager, clock := setupOwnerTestManager(t)
		prepareSetupPasswordForTOTP(t, manager)
		if _, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); err != nil {
			t.Fatal(err)
		}
		state, _ := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
		outside := setupTOTPCode(t, state.Draft.TOTPSecret, clock.now.Add(-2*totpPeriodSeconds*time.Second))
		var validationErr *SetupTOTPValidationError
		if confirmed, err := manager.ConfirmSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, outside); confirmed != nil || !errors.As(err, &validationErr) {
			t.Fatalf("outside-window confirmation = %#v, %v", confirmed, err)
		}
		current := setupTOTPCode(t, state.Draft.TOTPSecret, clock.now)
		if _, err := manager.ConfirmSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, current); err != nil {
			t.Fatal(err)
		}
		validationErr = nil
		if confirmed, err := manager.ConfirmSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, current); confirmed != nil || !errors.As(err, &validationErr) {
			t.Fatalf("replayed confirmation = %#v, %v", confirmed, err)
		}
	})

	t.Run("consecutive failures terminate challenge", func(t *testing.T) {
		manager, _ := setupOwnerTestManager(t)
		prepareSetupPasswordForTOTP(t, manager)
		if _, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); err != nil {
			t.Fatal(err)
		}
		for attempt := 1; attempt <= 3; attempt++ {
			state, err := manager.ConfirmSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, "000000")
			if state != nil {
				t.Fatalf("invalid attempt %d returned state %#v", attempt, state)
			}
			if attempt < 3 {
				var validationErr *SetupTOTPValidationError
				if !errors.As(err, &validationErr) || strings.Contains(err.Error(), "000000") {
					t.Fatalf("invalid attempt %d error = %v", attempt, err)
				}
			} else if !errors.Is(err, ErrSetupAccessInvalid) {
				t.Fatalf("blocking attempt error = %v", err)
			}
			if attempt == 1 {
				if _, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); err != nil {
					t.Fatalf("idempotent restart after failure: %v", err)
				}
				if _, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, true); err != nil {
					t.Fatalf("replacement after failure: %v", err)
				}
				var attempts int
				if err := manager.db.Read().QueryRow(`SELECT attempts FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&attempts); err != nil {
					t.Fatal(err)
				}
				if attempts != 1 {
					t.Fatalf("TOTP restart reset failed-attempt budget to %d", attempts)
				}
			}
		}
		var attempts int
		var consumedAt sql.NullTime
		if err := manager.db.Read().QueryRow(`SELECT attempts, consumed_at FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&attempts, &consumedAt); err != nil {
			t.Fatal(err)
		}
		if attempts != 3 || !consumedAt.Valid {
			t.Fatalf("blocked TOTP challenge = attempts:%d consumed:%#v", attempts, consumedAt)
		}
	})
}

func TestConfirmSetupTOTPRejectsConcurrentReplay(t *testing.T) {
	manager, clock := setupOwnerTestManager(t)
	prepareSetupPasswordForTOTP(t, manager)
	if _, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); err != nil {
		t.Fatal(err)
	}
	state, _ := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	code := setupTOTPCode(t, state.Draft.TOTPSecret, clock.now)
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			ready.Done()
			<-start
			_, err := manager.ConfirmSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, code)
			results <- err
		}()
	}
	ready.Wait()
	close(start)
	first, second := <-results, <-results
	successes := 0
	replays := 0
	for _, err := range []error{first, second} {
		if err == nil {
			successes++
			continue
		}
		var validationErr *SetupTOTPValidationError
		if errors.As(err, &validationErr) {
			replays++
		}
	}
	if successes != 1 || replays != 1 {
		t.Fatalf("concurrent confirmation results = %v / %v", first, second)
	}
}

func TestSetupTOTPReplacementAndOwnerEditInvalidatePriorConfirmation(t *testing.T) {
	manager, clock := setupOwnerTestManager(t)
	prepareSetupPasswordForTOTP(t, manager)
	if _, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); err != nil {
		t.Fatal(err)
	}
	state, _ := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if _, err := manager.ConfirmSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, setupTOTPCode(t, state.Draft.TOTPSecret, clock.now)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, true); err != nil {
		t.Fatal(err)
	}
	replaced, _ := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if !replaced.TOTPStarted || replaced.TOTPReady || replaced.Draft.TOTPConfirmedStep != nil {
		t.Fatalf("replaced TOTP state = %#v", replaced)
	}
	if _, err := manager.SaveSetupOwnerDraft(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, SetupOwnerDraftInput{
		Mode: SetupOwnerModeCreate, Name: "Cristian Braun", Username: "new-cristian", Email: "new-cristian@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	edited, _ := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if edited.PasswordReady || edited.TOTPStarted || edited.TOTPReady || edited.Draft.TOTPSecret != "" || edited.Draft.TOTPConfirmedStep != nil {
		t.Fatalf("edited owner retained security drafts = %#v", edited)
	}
}

func TestSetupTOTPGenerationAndStorageFailuresDoNotReplaceCurrentDraft(t *testing.T) {
	t.Run("randomness", func(t *testing.T) {
		manager, _ := setupOwnerTestManager(t)
		prepareSetupPasswordForTOTP(t, manager)
		var original []byte
		if err := manager.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&original); err != nil {
			t.Fatal(err)
		}
		manager.tokens = &deterministicTokenGenerator{tokenErr: errors.New("random source unavailable")}
		if enrollment, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); enrollment != nil || err == nil || !strings.Contains(err.Error(), "random source unavailable") {
			t.Fatalf("randomness failure = %#v, %v", enrollment, err)
		}
		var stored []byte
		if err := manager.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(stored, original) {
			t.Fatal("randomness failure replaced password draft")
		}
	})

	t.Run("storage", func(t *testing.T) {
		manager, _ := setupOwnerTestManager(t)
		prepareSetupPasswordForTOTP(t, manager)
		var original []byte
		if err := manager.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&original); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.db.Write().Exec(`
			CREATE TRIGGER reject_setup_totp_payload
			BEFORE UPDATE OF payload_ciphertext ON auth_challenges
			BEGIN SELECT RAISE(ABORT, 'reject setup TOTP payload'); END`); err != nil {
			t.Fatal(err)
		}
		if enrollment, err := manager.StartSetupTOTP(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, false); enrollment != nil || err == nil {
			t.Fatalf("storage failure = %#v, %v", enrollment, err)
		}
		var stored []byte
		if err := manager.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(stored, original) {
			t.Fatal("storage failure replaced password draft")
		}
	})
}
