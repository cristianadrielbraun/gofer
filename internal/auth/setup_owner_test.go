package auth

import (
	"bytes"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	setupOwnerTestToken  = "setup-owner-access-token"
	setupOwnerTestOrigin = "https://gofer.example"
)

func setupOwnerTestManager(t *testing.T) (*Manager, *fixedClock) {
	t.Helper()
	now := time.Date(2026, time.August, 8, 12, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	manager := newDeterministicManager(t, clock, &deterministicTokenGenerator{})
	insertSetupAccessState(t, manager, "setup-token-remains-active", now.Add(time.Hour), 0)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO auth_challenges (
			id, challenge_hash, purpose, origin, attempts, max_attempts, created_at, expires_at
		) VALUES ('setup-owner-challenge', ?, 'enrollment', ?, 0, 3, ?, ?)`,
		hashToken(setupOwnerTestToken), setupOwnerTestOrigin, now, now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	return manager, clock
}

func insertSetupOwnerUser(t *testing.T, manager *Manager, id, email, username, name string, status UserStatus, admin bool) {
	t.Helper()
	adminValue := 0
	if admin {
		adminValue = 1
	}
	var normalizedUsername any
	if username != "" {
		normalizedUsername = strings.ToLower(username)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, email, email_normalized, username, username_normalized, name,
			status, is_admin, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, email, strings.ToLower(email), nullableIdentifier(username), normalizedUsername,
		name, status, adminValue, manager.clock.Now(), manager.clock.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestSetupOwnerTopologyDistinguishesFreshLegacyAndExistingUsers(t *testing.T) {
	t.Run("fresh", func(t *testing.T) {
		manager, _ := setupOwnerTestManager(t)
		state, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
		if err != nil || state.Topology.Kind != SetupOwnerTopologyFresh || len(state.Topology.Candidates) != 0 {
			t.Fatalf("fresh topology = %#v, %v", state, err)
		}
	})

	t.Run("legacy default", func(t *testing.T) {
		manager, _ := setupOwnerTestManager(t)
		insertSetupOwnerUser(t, manager, "default", "local@gofer.local", "", "Local User", UserStatusActive, true)
		if _, err := manager.db.Write().ExecContext(t.Context(), `
			INSERT INTO accounts (id, user_id, email_address) VALUES ('mailbox', 'default', 'mail@example.com')`); err != nil {
			t.Fatal(err)
		}
		state, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
		if err != nil || state.Topology.Kind != SetupOwnerTopologyLegacyDefault || len(state.Topology.Candidates) != 1 ||
			state.Topology.Candidates[0].ID != "default" || state.Topology.Candidates[0].MailboxCount != 1 {
			t.Fatalf("legacy topology = %#v, %v", state, err)
		}
	})

	t.Run("existing users require explicit choice", func(t *testing.T) {
		manager, _ := setupOwnerTestManager(t)
		insertSetupOwnerUser(t, manager, "person-a", "a@example.com", "person.a", "Person A", UserStatusActive, false)
		insertSetupOwnerUser(t, manager, "person-b", "b@example.com", "person.b", "Person B", UserStatusPending, true)
		state, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
		if err != nil || state.Topology.Kind != SetupOwnerTopologyExisting || len(state.Topology.Candidates) != 2 {
			t.Fatalf("existing topology = %#v, %v", state, err)
		}
		if state.Topology.Candidates[0].ID != "person-a" || state.Topology.Candidates[1].ID != "person-b" {
			t.Fatalf("candidate order = %#v", state.Topology.Candidates)
		}
	})
}

func TestSaveSetupOwnerDraftIsEncryptedChallengeBoundAndNonMutating(t *testing.T) {
	manager, _ := setupOwnerTestManager(t)
	state, err := manager.SaveSetupOwnerDraft(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, SetupOwnerDraftInput{
		Mode: SetupOwnerModeCreate, Name: "  Cristian Braun  ", Username: "Cristian.B", Email: "Cristian@Example.COM",
	})
	if err != nil {
		t.Fatalf("SaveSetupOwnerDraft() error = %v", err)
	}
	if state.Draft == nil || state.Draft.Name != "Cristian Braun" || state.Draft.UsernameNormalized != "cristian.b" || state.Draft.EmailNormalized != "cristian@example.com" {
		t.Fatalf("saved setup owner draft = %#v", state.Draft)
	}
	var payload []byte
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) == 0 || bytes.Contains(payload, []byte("Cristian")) || bytes.Contains(payload, []byte("cristian@example.com")) {
		t.Fatalf("owner draft was absent or stored plaintext: %q", payload)
	}
	var users, credentials, events, initialized int
	var setupHash string
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM password_credentials`).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRow(`SELECT initialized, setup_token_hash FROM auth_system_state WHERE id = 1`).Scan(&initialized, &setupHash); err != nil {
		t.Fatal(err)
	}
	if users != 0 || credentials != 0 || events != 0 || initialized != 0 || setupHash == "" {
		t.Fatalf("draft mutated setup = users:%d credentials:%d events:%d initialized:%d setupHash:%q", users, credentials, events, initialized, setupHash)
	}

	reloaded, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if err != nil || reloaded.Draft == nil || reloaded.Draft.Email != "Cristian@Example.COM" || reloaded.DraftStale {
		t.Fatalf("GetSetupOwnerState(saved) = %#v, %v", reloaded, err)
	}

	otherManager := NewManager(manager.config, manager.db, Dependencies{Clock: manager.clock, BucketHashKey: []byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")})
	if _, err := otherManager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin); err == nil || !strings.Contains(err.Error(), "authenticate setup owner draft") {
		t.Fatalf("wrong-key draft read error = %v", err)
	}
}

func TestSetupOwnerExistingSelectionPreservesOwnershipAndRejectsCollisions(t *testing.T) {
	manager, _ := setupOwnerTestManager(t)
	insertSetupOwnerUser(t, manager, "existing", "existing@example.com", "existing", "Existing User", UserStatusActive, false)
	insertSetupOwnerUser(t, manager, "other", "other@example.com", "other", "Other User", UserStatusActive, true)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO accounts (id, user_id, email_address) VALUES ('owned-mailbox', 'existing', 'mail@example.com')`); err != nil {
		t.Fatal(err)
	}

	_, err := manager.SaveSetupOwnerDraft(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, SetupOwnerDraftInput{
		Mode: SetupOwnerModeExisting, TargetUserID: "existing", Name: "Real Owner",
		Username: "existing", Email: "existing@example.com",
	})
	if err != nil {
		t.Fatalf("save existing owner draft: %v", err)
	}
	var userID, name, email, username string
	if err := manager.db.Read().QueryRow(`SELECT id, name, email, username FROM users WHERE id = 'existing'`).Scan(&userID, &name, &email, &username); err != nil {
		t.Fatal(err)
	}
	var accountOwner string
	if err := manager.db.Read().QueryRow(`SELECT user_id FROM accounts WHERE id = 'owned-mailbox'`).Scan(&accountOwner); err != nil {
		t.Fatal(err)
	}
	if userID != "existing" || name != "Existing User" || email != "existing@example.com" || username != "existing" || accountOwner != "existing" {
		t.Fatalf("existing selection mutated ownership/profile = user:%q name:%q email:%q username:%q account:%q", userID, name, email, username, accountOwner)
	}

	_, err = manager.SaveSetupOwnerDraft(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, SetupOwnerDraftInput{
		Mode: SetupOwnerModeExisting, TargetUserID: "missing", Name: "Owner", Username: "new-owner", Email: "new@example.com",
	})
	var validationErr *SetupOwnerValidationError
	if !errors.As(err, &validationErr) || validationErr.Fields["target"] == "" {
		t.Fatalf("forged target error = %v", err)
	}

	_, err = manager.SaveSetupOwnerDraft(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, SetupOwnerDraftInput{
		Mode: SetupOwnerModeCreate, Name: "New Owner", Username: "other", Email: "other@example.com",
	})
	validationErr = nil
	if !errors.As(err, &validationErr) || validationErr.Fields["username"] == "" || validationErr.Fields["email"] == "" {
		t.Fatalf("identifier collision error = %#v, %v", validationErr, err)
	}
}

func TestSetupOwnerDraftBecomesStaleWhenUserTopologyChanges(t *testing.T) {
	manager, _ := setupOwnerTestManager(t)
	insertSetupOwnerUser(t, manager, "existing", "existing@example.com", "existing", "Existing", UserStatusActive, false)
	if _, err := manager.SaveSetupOwnerDraft(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, SetupOwnerDraftInput{
		Mode: SetupOwnerModeExisting, TargetUserID: "existing", Name: "Owner", Username: "owner", Email: "owner@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	insertSetupOwnerUser(t, manager, "new-user", "new@example.com", "new-user", "New User", UserStatusActive, false)
	state, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if err != nil || state.Draft == nil || !state.DraftStale {
		t.Fatalf("stale setup owner draft = %#v, %v", state, err)
	}
}

func TestSetupOwnerAccessExpiryAndPayloadTamperingFailClosed(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		manager, clock := setupOwnerTestManager(t)
		clock.now = clock.now.Add(11 * time.Minute)
		if _, err := manager.SaveSetupOwnerDraft(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, SetupOwnerDraftInput{
			Mode: SetupOwnerModeCreate, Name: "Owner", Username: "owner", Email: "owner@example.com",
		}); !errors.Is(err, ErrSetupAccessInvalid) {
			t.Fatalf("expired save error = %v", err)
		}
		if _, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin); !errors.Is(err, ErrSetupAccessInvalid) {
			t.Fatalf("expired read error = %v", err)
		}
	})

	t.Run("tampered", func(t *testing.T) {
		manager, _ := setupOwnerTestManager(t)
		if _, err := manager.SaveSetupOwnerDraft(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, SetupOwnerDraftInput{
			Mode: SetupOwnerModeCreate, Name: "Owner", Username: "owner", Email: "owner@example.com",
		}); err != nil {
			t.Fatal(err)
		}
		var payload []byte
		if err := manager.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&payload); err != nil {
			t.Fatal(err)
		}
		payload[len(payload)-1] ^= 0xff
		if _, err := manager.db.Write().Exec(`UPDATE auth_challenges SET payload_ciphertext = ? WHERE id = 'setup-owner-challenge'`, payload); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin); err == nil || !strings.Contains(err.Error(), "authenticate setup owner draft") {
			t.Fatalf("tampered draft read error = %v", err)
		}
	})
}

func TestSetupOwnerTopologyRejectsAmbiguousExistingIdentifiers(t *testing.T) {
	manager, _ := setupOwnerTestManager(t)
	insertSetupOwnerUser(t, manager, "first", "first@example.com", "first", "First", UserStatusActive, false)
	insertSetupOwnerUser(t, manager, "second", "second@example.com", "second", "Second", UserStatusActive, false)
	if _, err := manager.db.Write().Exec(`UPDATE users SET username = 'first@example.com', username_normalized = 'first@example.com' WHERE id = 'second'`); err != nil {
		t.Fatal(err)
	}
	state, err := manager.GetSetupOwnerState(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin)
	if state != nil || !errors.Is(err, ErrSetupOwnerBlocked) {
		t.Fatalf("ambiguous topology = %#v, %v", state, err)
	}
}

func TestSetupOwnerDraftUpdateRollsBackOnStorageFailure(t *testing.T) {
	manager, _ := setupOwnerTestManager(t)
	if _, err := manager.db.Write().Exec(`
		CREATE TRIGGER reject_setup_owner_payload
		BEFORE UPDATE OF payload_ciphertext ON auth_challenges
		BEGIN SELECT RAISE(ABORT, 'reject setup owner payload'); END`); err != nil {
		t.Fatal(err)
	}
	state, err := manager.SaveSetupOwnerDraft(t.Context(), setupOwnerTestToken, setupOwnerTestOrigin, SetupOwnerDraftInput{
		Mode: SetupOwnerModeCreate, Name: "Owner", Username: "owner", Email: "owner@example.com",
	})
	if state != nil || err == nil {
		t.Fatalf("rollback save = %#v, %v", state, err)
	}
	var payload []byte
	if err := manager.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id = 'setup-owner-challenge'`).Scan(&payload); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if len(payload) != 0 {
		t.Fatalf("rollback retained payload: %x", payload)
	}
	var users int
	if err := manager.db.Read().QueryRow(`SELECT COUNT(*) FROM users`).Scan(&users); err != nil || users != 0 {
		t.Fatalf("rollback user count = %d, %v", users, err)
	}
}
