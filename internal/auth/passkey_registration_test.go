package auth

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

type fakePasskeyRegistration struct {
	beginCreation []byte
	beginSession  []byte
	beginErr      error
	finishRecord  *passkeyCredentialRecord
	finishErr     error
	beginUsers    []passkeyUser
	finishUsers   []passkeyUser
}

func (fake *fakePasskeyRegistration) Begin(user passkeyUser) ([]byte, []byte, error) {
	fake.beginUsers = append(fake.beginUsers, user)
	return append([]byte(nil), fake.beginCreation...), append([]byte(nil), fake.beginSession...), fake.beginErr
}

func (fake *fakePasskeyRegistration) Finish(user passkeyUser, _, _ []byte) (*passkeyCredentialRecord, error) {
	fake.finishUsers = append(fake.finishUsers, user)
	return fake.finishRecord, fake.finishErr
}

type passkeyTestFixture struct {
	manager      *Manager
	clock        *fixedClock
	tokens       *deterministicTokenGenerator
	registration *fakePasskeyRegistration
	session      *Session
}

func newPasskeyTestFixture(t *testing.T, isAdmin bool) *passkeyTestFixture {
	t.Helper()
	now := time.Date(2026, time.August, 9, 14, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	tokens := &deterministicTokenGenerator{
		ids: []string{
			"passkey-challenge-1", "passkey-row-1", "passkey-event-1",
			"rotated-session", "removal-event", "second-session", "second-event",
			"passkey-challenge-2", "passkey-row-2", "passkey-event-2", "rejected-event",
		},
		tokens: []string{"passkey-challenge-token-1", "rotated-session-token", "passkey-challenge-token-2", "second-session-token"},
	}
	manager := newDeterministicManager(t, clock, tokens)
	insertActiveUser(t, manager, "passkey-user", isAdmin, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO password_credentials (user_id, password_hash, created_at, changed_at)
		VALUES ('passkey-user', 'password-hash', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert passkey test password: %v", err)
	}
	session := &Session{
		ID: "passkey-session", UserID: "passkey-user", Token: "passkey-session-token", AuthVersion: 1,
		AuthenticationMethod: AuthenticationMethodPassword, AssuranceLevel: AssuranceLevelSingleFactor,
		UserAgent: "Passkey Test/1.0", AuthenticatedAt: now, LastUsedAt: now,
		IdleExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(24 * time.Hour),
		StepUpAt: &now, StepUpMethod: AuthenticationMethodPassword, CreatedAt: now,
	}
	if isAdmin {
		session.AssuranceLevel = AssuranceLevelMultiFactor
		session.StepUpMethod = AuthenticationMethodTOTP
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO sessions (
			id, user_id, token_hash, auth_version, authentication_method, assurance_level,
			user_agent, authenticated_at, last_used_at, idle_expires_at, absolute_expires_at,
			step_up_at, step_up_method, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID, session.UserID, hashToken(session.Token), session.AuthVersion,
		session.AuthenticationMethod, session.AssuranceLevel, session.UserAgent,
		session.AuthenticatedAt, session.LastUsedAt, session.IdleExpiresAt, session.AbsoluteExpiresAt,
		session.StepUpAt, session.StepUpMethod, session.CreatedAt,
	); err != nil {
		t.Fatalf("insert passkey test session: %v", err)
	}
	registration := &fakePasskeyRegistration{
		beginCreation: []byte(`{"publicKey":{"challenge":"creation-challenge"}}`),
		beginSession:  []byte(`{"challenge":"server-session"}`),
		finishRecord:  testPasskeyCredentialRecord(t, []byte("credential-id-1")),
	}
	manager.passkeyRegistrationFactory = func(origin string) (passkeyRegistrationCeremony, error) {
		if origin != "https://gofer.example" {
			return nil, fmt.Errorf("unexpected origin %q", origin)
		}
		return registration, nil
	}
	return &passkeyTestFixture{manager: manager, clock: clock, tokens: tokens, registration: registration, session: session}
}

func testPasskeyCredentialRecord(t *testing.T, credentialID []byte) *passkeyCredentialRecord {
	t.Helper()
	flags := protocol.FlagUserPresent | protocol.FlagUserVerified | protocol.FlagBackupEligible | protocol.FlagBackupState
	credential := &webauthn.Credential{
		ID: credentialID, PublicKey: []byte("credential-public-key"),
		Transport: []protocol.AuthenticatorTransport{protocol.Internal},
		Flags:     webauthn.NewCredentialFlags(flags),
		Authenticator: webauthn.Authenticator{
			AAGUID: bytes.Repeat([]byte{0x42}, 16), SignCount: 7,
			Attachment: protocol.Platform,
		},
		AttestationType: "none", AttestationFormat: "none",
	}
	record, err := credential.MarshalMsg(nil)
	if err != nil {
		t.Fatalf("marshal passkey credential record: %v", err)
	}
	return &passkeyCredentialRecord{
		CredentialID: credential.ID, PublicKey: credential.PublicKey, SignCount: 7,
		AAGUID: credential.Authenticator.AAGUID, Transports: []string{"internal"},
		Attachment: "platform", Flags: byte(flags), BackupEligible: true, BackupState: true,
		Record: record,
	}
}

func TestPasskeyRegistrationIsStepUpAndSessionBound(t *testing.T) {
	fixture := newPasskeyTestFixture(t, false)
	options, err := fixture.manager.StartPasskeyRegistration(
		t.Context(), fixture.session.Token, "https://GOFER.example:443/", "  Work laptop  ",
	)
	if err != nil || options == nil || options.Challenge == nil {
		t.Fatalf("StartPasskeyRegistration() = %#v, %v", options, err)
	}
	if string(options.CreationJSON) != string(fixture.registration.beginCreation) ||
		options.Challenge.SessionID != fixture.session.ID || options.Challenge.Origin != "https://gofer.example" ||
		options.Challenge.MaxAttempts != 1 || len(fixture.registration.beginUsers) != 1 ||
		len(fixture.registration.beginUsers[0].ID) != 32 {
		t.Fatalf("passkey registration options = %#v users=%#v", options, fixture.registration.beginUsers)
	}
	var storedPayload []byte
	var attempts, maxAttempts int
	if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `
		SELECT payload_ciphertext, attempts, max_attempts FROM auth_challenges WHERE id = ?`, options.Challenge.ID,
	).Scan(&storedPayload, &attempts, &maxAttempts); err != nil {
		t.Fatalf("load stored passkey challenge: %v", err)
	}
	if attempts != 0 || maxAttempts != 1 || bytes.Contains(storedPayload, []byte("Work laptop")) ||
		bytes.Contains(storedPayload, fixture.registration.beginSession) {
		t.Fatalf("passkey challenge payload was not encrypted or bounded: attempts=%d/%d payload=%q", attempts, maxAttempts, storedPayload)
	}

	if _, err := fixture.manager.db.Write().ExecContext(t.Context(), `
		UPDATE sessions SET step_up_at = ? WHERE id = ?`,
		fixture.clock.now.Add(-securityStepUpMaximumAge-time.Second), fixture.session.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.manager.StartPasskeyRegistration(
		t.Context(), fixture.session.Token, "https://gofer.example", "Phone",
	); !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale passkey registration error = %v", err)
	}
	if _, err := fixture.manager.StartPasskeyRegistration(
		t.Context(), fixture.session.Token, "https://gofer.example", "\n",
	); !errors.Is(err, ErrRecentStepUpRequired) {
		t.Fatalf("stale session must be rejected before passkey name validation: %v", err)
	}
}

func TestPasskeyRegistrationPersistsEncryptedCompleteCredentialAndMultipleHandles(t *testing.T) {
	fixture := newPasskeyTestFixture(t, false)
	started, err := fixture.manager.StartPasskeyRegistration(
		t.Context(), fixture.session.Token, "https://gofer.example", "Laptop",
	)
	if err != nil {
		t.Fatal(err)
	}
	created, err := fixture.manager.FinishPasskeyRegistration(
		t.Context(), started.Challenge.Token, fixture.session.Token, "https://gofer.example",
		[]byte(`{"credential":"response"}`), "Passkey Browser/1.0",
	)
	if err != nil || created == nil || created.Name != "Laptop" {
		t.Fatalf("FinishPasskeyRegistration() = %#v, %v", created, err)
	}
	var credentialID, publicKey, ciphertext, handle []byte
	var keyVersion, rawFlags, cloneWarning int
	var rpID, name string
	if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `
		SELECT credential_id, public_key, credential_ciphertext, key_version, rp_id, flags, clone_warning, name
		FROM webauthn_credentials WHERE id = ?`, created.ID,
	).Scan(&credentialID, &publicKey, &ciphertext, &keyVersion, &rpID, &rawFlags, &cloneWarning, &name); err != nil {
		t.Fatalf("load stored passkey: %v", err)
	}
	if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `
		SELECT user_handle FROM webauthn_users WHERE user_id = ? AND rp_id = ?`, fixture.session.UserID, rpID,
	).Scan(&handle); err != nil {
		t.Fatalf("load WebAuthn user handle: %v", err)
	}
	if !bytes.Equal(credentialID, fixture.registration.finishRecord.CredentialID) ||
		!bytes.Equal(publicKey, fixture.registration.finishRecord.PublicKey) || keyVersion != 1 ||
		rpID != "gofer.example" || rawFlags != int(fixture.registration.finishRecord.Flags) ||
		cloneWarning != 0 || name != "Laptop" || len(handle) != 32 ||
		bytes.Contains(ciphertext, fixture.registration.finishRecord.Record) {
		t.Fatalf("stored passkey metadata = id:%q key:%q version:%d rp:%q flags:%d clone:%d name:%q handle:%d ciphertext:%q",
			credentialID, publicKey, keyVersion, rpID, rawFlags, cloneWarning, name, len(handle), ciphertext)
	}
	decrypted, err := fixture.manager.decryptPasskeyCredential(fixture.session.UserID, created.ID, ciphertext, keyVersion)
	if err != nil || !bytes.Equal(decrypted.ID, credentialID) ||
		byte(decrypted.Flags.ProtocolValue()) != fixture.registration.finishRecord.Flags {
		t.Fatalf("decrypted passkey = %#v, %v", decrypted, err)
	}
	var consumed bool
	var payload []byte
	if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `
		SELECT consumed_at IS NOT NULL, payload_ciphertext FROM auth_challenges WHERE id = ?`, started.Challenge.ID,
	).Scan(&consumed, &payload); err != nil || !consumed || payload != nil {
		t.Fatalf("consumed registration = consumed:%t payload:%q err:%v", consumed, payload, err)
	}
	if _, err := fixture.manager.FinishPasskeyRegistration(
		t.Context(), started.Challenge.Token, fixture.session.Token, "https://gofer.example", []byte(`{}`), "Browser",
	); !errors.Is(err, ErrPasskeyRegistrationInvalid) {
		t.Fatalf("replayed passkey registration error = %v", err)
	}

	second, err := fixture.manager.StartPasskeyRegistration(
		t.Context(), fixture.session.Token, "https://gofer.example", "Phone",
	)
	if err != nil || second == nil || len(fixture.registration.beginUsers) != 2 ||
		len(fixture.registration.beginUsers[1].Credentials) != 1 ||
		!bytes.Equal(fixture.registration.beginUsers[0].ID, fixture.registration.beginUsers[1].ID) {
		t.Fatalf("second passkey registration = %#v, %v users=%#v", second, err, fixture.registration.beginUsers)
	}
}

func TestRejectedAndDuplicatePasskeyRegistrationsAreSingleUse(t *testing.T) {
	t.Run("invalid response", func(t *testing.T) {
		fixture := newPasskeyTestFixture(t, false)
		fixture.registration.finishErr = errors.New("invalid attestation")
		started, err := fixture.manager.StartPasskeyRegistration(
			t.Context(), fixture.session.Token, "https://gofer.example", "Laptop",
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.manager.FinishPasskeyRegistration(
			t.Context(), started.Challenge.Token, fixture.session.Token, "https://gofer.example", []byte(`{}`), "Browser",
		); err == nil {
			t.Fatalf("invalid passkey response error = %v", err)
		} else {
			var validationError *PasskeyRegistrationValidationError
			if !errors.As(err, &validationError) {
				t.Fatalf("invalid passkey response error = %T %v", err, err)
			}
		}
		var active int
		if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM auth_challenges WHERE id = ? AND consumed_at IS NULL`, started.Challenge.ID,
		).Scan(&active); err != nil || active != 0 {
			t.Fatalf("invalid passkey challenge active=%d err=%v", active, err)
		}
	})

	t.Run("duplicate credential owned by another user", func(t *testing.T) {
		fixture := newPasskeyTestFixture(t, false)
		insertActiveUser(t, fixture.manager, "other-passkey-owner", false, fixture.clock.now)
		if _, err := fixture.manager.db.Write().ExecContext(t.Context(), `
			INSERT INTO webauthn_credentials (id, user_id, credential_id, public_key, name, rp_id)
			VALUES ('existing-passkey', ?, ?, ?, 'Existing', 'gofer.example')`,
			"other-passkey-owner", fixture.registration.finishRecord.CredentialID, fixture.registration.finishRecord.PublicKey,
		); err != nil {
			t.Fatal(err)
		}
		started, err := fixture.manager.StartPasskeyRegistration(
			t.Context(), fixture.session.Token, "https://gofer.example", "Duplicate",
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.manager.FinishPasskeyRegistration(
			t.Context(), started.Challenge.Token, fixture.session.Token, "https://gofer.example", []byte(`{}`), "Browser",
		); !errors.Is(err, ErrPasskeyDuplicate) {
			t.Fatalf("duplicate passkey error = %v", err)
		}
		var credentials, active int
		if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM webauthn_credentials`).Scan(&credentials); err != nil {
			t.Fatal(err)
		}
		if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM auth_challenges WHERE id = ? AND consumed_at IS NULL`, started.Challenge.ID,
		).Scan(&active); err != nil || credentials != 1 || active != 0 {
			t.Fatalf("duplicate passkey state credentials=%d active=%d err=%v", credentials, active, err)
		}
	})
}

func TestPasskeyRemovalRotatesSessionsAndProtectsLastAuthenticator(t *testing.T) {
	t.Run("removal", func(t *testing.T) {
		fixture := newPasskeyTestFixture(t, false)
		started, err := fixture.manager.StartPasskeyRegistration(t.Context(), fixture.session.Token, "https://gofer.example", "Laptop")
		if err != nil {
			t.Fatal(err)
		}
		created, err := fixture.manager.FinishPasskeyRegistration(
			t.Context(), started.Challenge.Token, fixture.session.Token, "https://gofer.example", []byte(`{}`), "Browser",
		)
		if err != nil {
			t.Fatal(err)
		}
		rotated, err := fixture.manager.RemovePasskey(t.Context(), fixture.session.Token, created.ID, "Removal Browser")
		if err != nil || rotated == nil || rotated.Token != "rotated-session-token" || rotated.AuthVersion != 2 {
			t.Fatalf("RemovePasskey() = %#v, %v", rotated, err)
		}
		if old, err := fixture.manager.GetSessionByToken(t.Context(), fixture.session.Token); err != nil || old != nil {
			t.Fatalf("old session after passkey removal = %#v, %v", old, err)
		}
		var revoked bool
		if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `
			SELECT revoked_at IS NOT NULL FROM webauthn_credentials WHERE id = ?`, created.ID,
		).Scan(&revoked); err != nil || !revoked {
			t.Fatalf("removed passkey revoked=%t err=%v", revoked, err)
		}
	})

	t.Run("last required strong authenticator", func(t *testing.T) {
		fixture := newPasskeyTestFixture(t, true)
		started, err := fixture.manager.StartPasskeyRegistration(t.Context(), fixture.session.Token, "https://gofer.example", "Only passkey")
		if err != nil {
			t.Fatal(err)
		}
		created, err := fixture.manager.FinishPasskeyRegistration(
			t.Context(), started.Challenge.Token, fixture.session.Token, "https://gofer.example", []byte(`{}`), "Browser",
		)
		if err != nil {
			t.Fatal(err)
		}
		listed, err := fixture.manager.ListPasskeys(t.Context(), fixture.session.Token)
		if err != nil || len(listed) != 1 || listed[0].CanRemove {
			t.Fatalf("admin passkey list = %#v, %v", listed, err)
		}
		if _, err := fixture.manager.RemovePasskey(t.Context(), fixture.session.Token, created.ID, "Browser"); !errors.Is(err, ErrLastAuthenticator) {
			t.Fatalf("last passkey removal error = %v", err)
		}
		if _, err := fixture.manager.RemovePasskey(t.Context(), fixture.session.Token, "foreign-passkey", "Browser"); !errors.Is(err, ErrPasskeyNotFound) {
			t.Fatalf("foreign passkey removal error = %v", err)
		}
	})
}

func TestCanonicalWebAuthnRelyingPartyValidation(t *testing.T) {
	for _, test := range []struct {
		origin string
		want   string
		ok     bool
	}{
		{origin: "https://Example.COM:443/", want: "example.com", ok: true},
		{origin: "http://local.localhost:8090", want: "local.localhost", ok: true},
		{origin: "http://127.0.0.1:8090", want: "127.0.0.1", ok: true},
		{origin: "http://example.com", ok: false},
		{origin: "https://example.com/account", ok: false},
		{origin: "https://user@example.com", ok: false},
	} {
		_, rpID, err := canonicalWebAuthnRelyingParty(test.origin)
		if test.ok && (err != nil || rpID != test.want) {
			t.Errorf("canonicalWebAuthnRelyingParty(%q) = %q, %v", test.origin, rpID, err)
		}
		if test.ok {
			if _, err := newPasskeyRegistrationCeremony(test.origin); err != nil {
				t.Errorf("newPasskeyRegistrationCeremony(%q) error = %v", test.origin, err)
			}
		}
		if !test.ok && err == nil {
			t.Errorf("canonicalWebAuthnRelyingParty(%q) unexpectedly accepted", test.origin)
		}
	}
}
