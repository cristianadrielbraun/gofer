package auth

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestEnsureSetupTokenCreatesHashOnlyStateAndDoesNotReissueOnRestart(t *testing.T) {
	now := time.Date(2026, time.August, 8, 10, 0, 0, 0, time.UTC)
	rawToken := "generated-setup-token"
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"setup-issued-event"}, tokens: []string{rawToken},
	})

	before, err := manager.SetupState(t.Context())
	if err != nil || before.Initialized || before.TokenConfigured {
		t.Fatalf("SetupState(before) = %#v, %v", before, err)
	}
	result, err := manager.EnsureSetupToken(t.Context(), "")
	if err != nil {
		t.Fatalf("EnsureSetupToken() error = %v", err)
	}
	if !result.Created || result.Configured || result.Token != rawToken ||
		result.State.Initialized || !result.State.TokenConfigured ||
		result.State.TokenExpiresAt == nil || !result.State.TokenExpiresAt.Equal(now.Add(defaultSetupTokenLifetime)) {
		t.Fatalf("EnsureSetupToken() = %#v", result)
	}

	var storedHash string
	var expiresAt time.Time
	var attempts int64
	var rotatedAt sql.NullTime
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT setup_token_hash, setup_expires_at, setup_attempts, setup_rotated_at
		FROM auth_system_state WHERE id = 1`,
	).Scan(&storedHash, &expiresAt, &attempts, &rotatedAt); err != nil {
		t.Fatal(err)
	}
	if storedHash != hashToken(rawToken) || storedHash == rawToken ||
		!expiresAt.Equal(now.Add(defaultSetupTokenLifetime)) || attempts != 0 || rotatedAt.Valid {
		t.Fatalf("persisted setup state = hash:%q expiry:%v attempts:%d rotated:%#v", storedHash, expiresAt, attempts, rotatedAt)
	}
	var eventType, reason, metadata string
	var actorID, subjectID, sessionID sql.NullString
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT event_type, reason, actor_user_id, subject_user_id, session_id, metadata_json
		FROM auth_events WHERE id = 'setup-issued-event'`,
	).Scan(&eventType, &reason, &actorID, &subjectID, &sessionID, &metadata); err != nil {
		t.Fatal(err)
	}
	if eventType != string(AuthEventSetupTokenIssued) || reason != string(AuthEventReasonSystemInitialization) ||
		actorID.Valid || subjectID.Valid || sessionID.Valid ||
		strings.Contains(metadata, rawToken) || strings.Contains(metadata, storedHash) ||
		!strings.Contains(metadata, `"source":"generated"`) {
		t.Fatalf("setup issuance event = type:%q reason:%q actor:%#v subject:%#v session:%#v metadata:%q", eventType, reason, actorID, subjectID, sessionID, metadata)
	}

	restarted := NewManager(&Config{Enabled: true}, manager.db, Dependencies{
		Clock: &fixedClock{now: now.Add(time.Hour)},
		Tokens: &deterministicTokenGenerator{
			tokenErr: errors.New("must not generate replacement token"),
			idErr:    errors.New("must not generate replacement event"),
		},
	})
	again, err := restarted.EnsureSetupToken(t.Context(), "short stale environment value")
	if err != nil || again.Created || again.Token != "" || !again.State.TokenConfigured ||
		again.State.TokenExpiresAt == nil || !again.State.TokenExpiresAt.Equal(expiresAt) {
		t.Fatalf("EnsureSetupToken(restart) = %#v, %v", again, err)
	}
	var events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("event count after restart = %d, %v", events, err)
	}
}

func TestEnsureSetupTokenAcceptsStrongOperatorTokenWithoutReturningIt(t *testing.T) {
	now := time.Date(2026, time.August, 8, 11, 0, 0, 0, time.UTC)
	configuredToken := strings.Repeat("operator-secret-", 3)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"configured-setup-event"},
	})
	result, err := manager.EnsureSetupToken(t.Context(), configuredToken)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created || !result.Configured || result.Token != "" {
		t.Fatalf("configured setup provision = %#v", result)
	}
	var storedHash, metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT setup_token_hash FROM auth_system_state WHERE id = 1`).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT metadata_json FROM auth_events WHERE id = 'configured-setup-event'`).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	if storedHash != hashToken(configuredToken) || strings.Contains(metadata, configuredToken) || strings.Contains(metadata, storedHash) ||
		!strings.Contains(metadata, `"source":"environment"`) {
		t.Fatalf("configured setup persistence = hash:%q metadata:%q", storedHash, metadata)
	}
}

func TestEnsureSetupTokenRejectsWeakConfiguredTokenWithoutMutation(t *testing.T) {
	manager := newDeterministicManager(t, &fixedClock{now: time.Now()}, &deterministicTokenGenerator{})
	result, err := manager.EnsureSetupToken(t.Context(), "too-short")
	if result != nil || !errors.Is(err, ErrConfiguredSetupTokenLength) {
		t.Fatalf("EnsureSetupToken(short) = %#v, %v", result, err)
	}
	var states, events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_system_state`).Scan(&states); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if states != 0 || events != 0 {
		t.Fatalf("weak configured token mutated state = states:%d events:%d", states, events)
	}
}

func TestSetupStateIsIndependentOfUserRows(t *testing.T) {
	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{})
	insertActiveUser(t, manager, "legacy-default", true, now)
	state, err := manager.SetupState(t.Context())
	if err != nil || state.Initialized || state.OwnerUserID != "" || state.TokenConfigured {
		t.Fatalf("SetupState(with user but no singleton) = %#v, %v", state, err)
	}
}

func TestRotateSetupTokenLocallyReplacesSecretAndResetsAttempts(t *testing.T) {
	now := time.Date(2026, time.August, 8, 13, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"rotation-event"}, tokens: []string{"replacement-setup-secret"},
	})
	oldToken := "old-setup-secret"
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (
			id, initialized, setup_token_hash, setup_expires_at, setup_attempts, cutover_version
		) VALUES (1, 0, ?, ?, 7, 0)`, hashToken(oldToken), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_challenges (
			id, challenge_hash, purpose, origin, attempts, max_attempts, created_at, expires_at
		) VALUES ('old-setup-access', ?, 'enrollment', 'https://gofer.example', 0, 3, ?, ?)`,
		hashToken("old-setup-access-secret"), now.Add(-time.Minute), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	result, err := manager.RotateSetupTokenLocally(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Token != "replacement-setup-secret" || !result.Created || result.Configured ||
		result.State.TokenAttempts != 0 || result.State.TokenRotatedAt == nil || !result.State.TokenRotatedAt.Equal(now) ||
		result.State.TokenExpiresAt == nil || !result.State.TokenExpiresAt.Equal(now.Add(defaultSetupTokenLifetime)) {
		t.Fatalf("RotateSetupTokenLocally() = %#v", result)
	}
	var storedHash string
	var expiresAt, rotatedAt time.Time
	var attempts int64
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT setup_token_hash, setup_expires_at, setup_attempts, setup_rotated_at
		FROM auth_system_state WHERE id = 1`,
	).Scan(&storedHash, &expiresAt, &attempts, &rotatedAt); err != nil {
		t.Fatal(err)
	}
	if storedHash != hashToken(result.Token) || storedHash == result.Token || storedHash == hashToken(oldToken) ||
		!expiresAt.Equal(now.Add(defaultSetupTokenLifetime)) || attempts != 0 || !rotatedAt.Equal(now) {
		t.Fatalf("rotated setup state = hash:%q expiry:%v attempts:%d rotated:%v", storedHash, expiresAt, attempts, rotatedAt)
	}
	var eventType, reason, metadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT event_type, reason, metadata_json FROM auth_events WHERE id = 'rotation-event'`,
	).Scan(&eventType, &reason, &metadata); err != nil {
		t.Fatal(err)
	}
	if eventType != string(AuthEventSetupTokenRotated) || reason != string(AuthEventReasonLocalOperator) ||
		strings.Contains(metadata, result.Token) || strings.Contains(metadata, storedHash) ||
		!strings.Contains(metadata, `"source":"local_operator"`) {
		t.Fatalf("rotation event = type:%q reason:%q metadata:%q", eventType, reason, metadata)
	}
	var accessConsumed sql.NullTime
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT consumed_at FROM auth_challenges WHERE id = 'old-setup-access'`,
	).Scan(&accessConsumed); err != nil || !accessConsumed.Valid || !accessConsumed.Time.Equal(now) {
		t.Fatalf("setup access after rotation = %#v, %v", accessConsumed, err)
	}
}

func TestRotateSetupTokenLocallyCanProvisionMissingSingleton(t *testing.T) {
	now := time.Date(2026, time.August, 8, 13, 30, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"first-rotation-event"}, tokens: []string{"first-local-setup-secret"},
	})
	result, err := manager.RotateSetupTokenLocally(t.Context())
	if err != nil || result == nil || result.Token != "first-local-setup-secret" {
		t.Fatalf("RotateSetupTokenLocally(missing singleton) = %#v, %v", result, err)
	}
	state, err := manager.SetupState(t.Context())
	if err != nil || state.Initialized || !state.TokenConfigured || state.TokenRotatedAt == nil || !state.TokenRotatedAt.Equal(now) {
		t.Fatalf("SetupState(after first local rotation) = %#v, %v", state, err)
	}
}

func TestRotateSetupTokenLocallyRejectsCompletedSetupWithoutGeneratingSecret(t *testing.T) {
	now := time.Date(2026, time.August, 8, 14, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		tokenErr: errors.New("must not generate"), idErr: errors.New("must not generate"),
	})
	insertActiveUser(t, manager, "owner", true, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (id, initialized, owner_user_id, initialized_at, cutover_version)
		VALUES (1, 1, 'owner', ?, 1)`, now); err != nil {
		t.Fatal(err)
	}
	result, err := manager.RotateSetupTokenLocally(t.Context())
	if result != nil || !errors.Is(err, ErrSetupAlreadyInitialized) {
		t.Fatalf("RotateSetupTokenLocally(initialized) = %#v, %v", result, err)
	}
	var events int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&events); err != nil || events != 0 {
		t.Fatalf("completed setup event count = %d, %v", events, err)
	}
}

func TestRotateSetupTokenLocallyRollsBackWhenAuditFails(t *testing.T) {
	now := time.Date(2026, time.August, 8, 15, 0, 0, 0, time.UTC)
	oldHash := hashToken("old-setup-secret")
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"rejected-rotation-event"}, tokens: []string{"unused-replacement-secret"},
	})
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_system_state (
			id, initialized, setup_token_hash, setup_expires_at, setup_attempts, cutover_version
		) VALUES (1, 0, ?, ?, 4, 0)`, oldHash, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_setup_rotation_event
		BEFORE INSERT ON auth_events
		WHEN NEW.event_type = 'setup_token_rotated'
		BEGIN SELECT RAISE(ABORT, 'reject setup rotation'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_challenges (
			id, challenge_hash, purpose, origin, attempts, max_attempts, created_at, expires_at
		) VALUES ('rollback-setup-access', ?, 'enrollment', 'https://gofer.example', 0, 3, ?, ?)`,
		hashToken("rollback-setup-access-secret"), now.Add(-time.Minute), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	result, err := manager.RotateSetupTokenLocally(t.Context())
	if result != nil || err == nil {
		t.Fatalf("RotateSetupTokenLocally(audit failure) = %#v, %v", result, err)
	}
	var storedHash string
	var attempts int64
	var rotatedAt sql.NullTime
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT setup_token_hash, setup_attempts, setup_rotated_at
		FROM auth_system_state WHERE id = 1`,
	).Scan(&storedHash, &attempts, &rotatedAt); err != nil {
		t.Fatal(err)
	}
	if storedHash != oldHash || attempts != 4 || rotatedAt.Valid {
		t.Fatalf("failed rotation state = hash:%q attempts:%d rotated:%#v", storedHash, attempts, rotatedAt)
	}
	var accessConsumed sql.NullTime
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT consumed_at FROM auth_challenges WHERE id = 'rollback-setup-access'`,
	).Scan(&accessConsumed); err != nil || accessConsumed.Valid {
		t.Fatalf("failed rotation consumed setup access = %#v, %v", accessConsumed, err)
	}
}

func TestEnsureSetupTokenRollsBackWhenAuditFails(t *testing.T) {
	now := time.Date(2026, time.August, 8, 16, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, &deterministicTokenGenerator{
		ids: []string{"rejected-issuance-event"}, tokens: []string{"unused-initial-secret"},
	})
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		CREATE TRIGGER reject_setup_issuance_event
		BEFORE INSERT ON auth_events
		WHEN NEW.event_type = 'setup_token_issued'
		BEGIN SELECT RAISE(ABORT, 'reject setup issuance'); END`); err != nil {
		t.Fatal(err)
	}
	result, err := manager.EnsureSetupToken(t.Context(), "")
	if result != nil || err == nil {
		t.Fatalf("EnsureSetupToken(audit failure) = %#v, %v", result, err)
	}
	var states int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_system_state`).Scan(&states); err != nil || states != 0 {
		t.Fatalf("failed issuance state count = %d, %v", states, err)
	}
}

func TestSetupTokenGenerationFailuresDoNotMutateState(t *testing.T) {
	now := time.Date(2026, time.August, 8, 17, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		tokens *deterministicTokenGenerator
		call   func(*Manager) (*SetupTokenProvision, error)
	}{
		{
			name: "initial secret",
			tokens: &deterministicTokenGenerator{
				tokenErr: errors.New("random source failed"),
			},
			call: func(manager *Manager) (*SetupTokenProvision, error) {
				return manager.EnsureSetupToken(t.Context(), "")
			},
		},
		{
			name: "initial event ID",
			tokens: &deterministicTokenGenerator{
				tokens: []string{"unused-secret"}, idErr: errors.New("random source failed"),
			},
			call: func(manager *Manager) (*SetupTokenProvision, error) {
				return manager.EnsureSetupToken(t.Context(), "")
			},
		},
		{
			name: "rotation secret",
			tokens: &deterministicTokenGenerator{
				tokenErr: errors.New("random source failed"),
			},
			call: func(manager *Manager) (*SetupTokenProvision, error) {
				return manager.RotateSetupTokenLocally(t.Context())
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := newDeterministicManager(t, &fixedClock{now: now}, test.tokens)
			result, err := test.call(manager)
			if result != nil || err == nil {
				t.Fatalf("setup token call = %#v, %v", result, err)
			}
			var states, events int
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_system_state`).Scan(&states); err != nil {
				t.Fatal(err)
			}
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_events`).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if states != 0 || events != 0 {
				t.Fatalf("generation failure mutated state = states:%d events:%d", states, events)
			}
		})
	}
}
