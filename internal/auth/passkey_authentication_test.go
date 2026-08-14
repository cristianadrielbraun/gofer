package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

type fakePasskeyAssertion struct {
	mu                 sync.Mutex
	requestJSON        []byte
	sessionJSON        []byte
	finishRecord       *passkeyCredentialRecord
	finishErr          error
	discoverableRawID  []byte
	discoverableHandle []byte
	beginUsers         []*passkeyUser
	finishUsers        []passkeyUser
}

func (fake *fakePasskeyAssertion) Begin(user *passkeyUser) ([]byte, []byte, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if user == nil {
		fake.beginUsers = append(fake.beginUsers, nil)
	} else {
		copyUser := *user
		fake.beginUsers = append(fake.beginUsers, &copyUser)
	}
	return append([]byte(nil), fake.requestJSON...), append([]byte(nil), fake.sessionJSON...), nil
}

func (fake *fakePasskeyAssertion) Finish(user passkeyUser, _, _ []byte) (*passkeyCredentialRecord, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.finishUsers = append(fake.finishUsers, user)
	return fake.finishRecord, fake.finishErr
}

func (fake *fakePasskeyAssertion) FinishDiscoverable(lookup passkeyAssertionUserLookup, _, _ []byte) (*passkeyCredentialRecord, error) {
	user, err := lookup(fake.discoverableRawID, fake.discoverableHandle)
	if err != nil {
		return nil, err
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.finishUsers = append(fake.finishUsers, user)
	return fake.finishRecord, fake.finishErr
}

type passkeyAuthenticationFixture struct {
	manager      *Manager
	clock        *fixedClock
	assertion    *fakePasskeyAssertion
	credentialID []byte
	userHandle   []byte
	rowID        string
	userID       string
}

func newPasskeyAuthenticationFixture(t *testing.T, signCount uint32) *passkeyAuthenticationFixture {
	t.Helper()
	now := time.Date(2026, time.August, 9, 18, 0, 0, 0, time.UTC)
	clock := &fixedClock{now: now}
	tokens := &deterministicTokenGenerator{
		ids: []string{
			"assertion-challenge-1", "assertion-session-1", "assertion-event-1",
			"assertion-challenge-2", "assertion-event-2", "assertion-rejected-1",
			"assertion-challenge-3", "assertion-session-2", "assertion-event-3",
		},
		tokens: []string{
			"assertion-challenge-token-1", "assertion-session-token-1",
			"assertion-challenge-token-2", "assertion-challenge-token-3", "assertion-session-token-2",
		},
	}
	manager := newDeterministicManager(t, clock, tokens)
	userID := "passkey-login-user"
	insertActiveUser(t, manager, userID, false, now)
	credentialID := []byte("passkey-login-credential")
	userHandle := bytes.Repeat([]byte{0x31}, 32)
	rowID := "passkey-login-row"
	credential := passkeyAuthenticationCredential(credentialID, signCount, false)
	record, err := credential.MarshalMsg(nil)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := manager.encryptPasskeyCredential(userID, rowID, record)
	if err != nil {
		t.Fatal(err)
	}
	transports := `["internal"]`
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO webauthn_users (user_id, rp_id, user_handle, created_at)
		VALUES (?, 'gofer.example', ?, ?)`, userID, userHandle, now); err != nil {
		t.Fatalf("insert passkey user handle: %v", err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO webauthn_credentials (
			id, user_id, credential_id, public_key, credential_ciphertext, key_version,
			sign_count, aaguid, transports, attachment, backup_eligible, backup_state,
			name, created_at, rp_id, flags, clone_warning
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'platform', 1, 1, 'Laptop', ?, 'gofer.example', ?, 0)`,
		rowID, userID, credential.ID, credential.PublicKey, ciphertext, passkeyCredentialKeyVersion,
		credential.Authenticator.SignCount, credential.Authenticator.AAGUID, transports, now,
		byte(credential.Flags.ProtocolValue()),
	); err != nil {
		t.Fatalf("insert passkey login credential: %v", err)
	}
	finishCredential := passkeyAuthenticationCredential(credentialID, signCount+1, false)
	if signCount == 0 {
		finishCredential.Authenticator.SignCount = 0
	}
	finishRecord, err := passkeyCredentialRecordFromCredential(&finishCredential)
	if err != nil {
		t.Fatal(err)
	}
	assertion := &fakePasskeyAssertion{
		requestJSON:  []byte(`{"publicKey":{"challenge":"assertion"}}`),
		sessionJSON:  []byte(`{"challenge":"assertion-session"}`),
		finishRecord: finishRecord, discoverableRawID: credentialID, discoverableHandle: userHandle,
	}
	manager.passkeyAssertionFactory = func(origin string) (passkeyAssertionCeremony, error) {
		if origin != "https://gofer.example" {
			return nil, fmt.Errorf("unexpected assertion origin %q", origin)
		}
		return assertion, nil
	}
	return &passkeyAuthenticationFixture{
		manager: manager, clock: clock, assertion: assertion, credentialID: credentialID,
		userHandle: userHandle, rowID: rowID, userID: userID,
	}
}

func passkeyAuthenticationCredential(credentialID []byte, signCount uint32, cloneWarning bool) webauthn.Credential {
	flags := protocol.FlagUserPresent | protocol.FlagUserVerified | protocol.FlagBackupEligible | protocol.FlagBackupState
	return webauthn.Credential{
		ID: append([]byte(nil), credentialID...), PublicKey: []byte("passkey-login-public-key"),
		Transport: []protocol.AuthenticatorTransport{protocol.Internal},
		Flags:     webauthn.NewCredentialFlags(flags), AttestationType: "none", AttestationFormat: "none",
		Authenticator: webauthn.Authenticator{
			AAGUID: bytes.Repeat([]byte{0x41}, 16), SignCount: signCount,
			CloneWarning: cloneWarning, Attachment: protocol.Platform,
		},
	}
}

func TestSecurityFactorSummaryOnlyOffersCompletePasskeys(t *testing.T) {
	fixture := newPasskeyAuthenticationFixture(t, 7)
	session, err := fixture.manager.CreateAuthenticatedSession(
		t.Context(), fixture.userID, "Security Browser", AuthenticationMethodPassword, AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := fixture.manager.GetSecurityFactorSummary(t.Context(), session.Token)
	if err != nil || !summary.HasPasskey {
		t.Fatalf("complete passkey summary = %#v, %v", summary, err)
	}
	if _, err := fixture.manager.db.Write().ExecContext(t.Context(), `
		UPDATE webauthn_credentials
		SET credential_ciphertext = NULL, key_version = NULL
		WHERE id = ?`, fixture.rowID,
	); err != nil {
		t.Fatal(err)
	}
	summary, err = fixture.manager.GetSecurityFactorSummary(t.Context(), session.Token)
	if err != nil || summary.HasPasskey {
		t.Fatalf("incomplete passkey summary = %#v, %v", summary, err)
	}
	if _, err := fixture.manager.StartPasskeyStepUp(
		t.Context(), session.Token, "https://gofer.example", "198.51.100.20",
	); !errors.Is(err, ErrPasskeyAuthenticationUnavailable) {
		t.Fatalf("StartPasskeyStepUp() with incomplete passkey error = %v", err)
	}
}

func TestPasskeyLoginSupportsIdentifierAndDiscoverableAssertions(t *testing.T) {
	for _, test := range []struct {
		name         string
		identifier   string
		discoverable bool
		requireMFA   bool
		instanceMFA  bool
	}{
		{name: "identifier first with required MFA", identifier: " PASSKEY-LOGIN-USER@EXAMPLE.COM ", requireMFA: true},
		{name: "discoverable", discoverable: true},
		{name: "discoverable with instance MFA", discoverable: true, instanceMFA: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPasskeyAuthenticationFixture(t, 7)
			if test.requireMFA {
				if _, err := fixture.manager.db.Write().ExecContext(t.Context(), `
					UPDATE users SET mfa_required = 1 WHERE id = ?`, fixture.userID,
				); err != nil {
					t.Fatal(err)
				}
			}
			if test.instanceMFA {
				if _, err := fixture.manager.db.Write().ExecContext(t.Context(), `
					INSERT INTO auth_system_state (id, initialized, mfa_policy)
					VALUES (1, 1, 'all_users')`); err != nil {
					t.Fatal(err)
				}
			}
			started, err := fixture.manager.StartPasskeyLogin(
				t.Context(), test.identifier, "https://GOFER.example:443/", "198.51.100.20",
			)
			if err != nil || started == nil || started.Challenge == nil {
				t.Fatalf("StartPasskeyLogin() = %#v, %v", started, err)
			}
			if started.Challenge.Purpose != ChallengePurposeLogin || started.Challenge.MaxAttempts != 1 ||
				len(fixture.assertion.beginUsers) != 1 || (fixture.assertion.beginUsers[0] == nil) != test.discoverable {
				t.Fatalf("passkey login start = %#v users=%#v", started, fixture.assertion.beginUsers)
			}
			var payload []byte
			if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `
				SELECT payload_ciphertext FROM auth_challenges WHERE id = ?`, started.Challenge.ID,
			).Scan(&payload); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(payload, fixture.assertion.sessionJSON) || bytes.Contains(payload, []byte("passkey-login-user")) {
				t.Fatalf("passkey login draft was not encrypted: %q", payload)
			}

			session, err := fixture.manager.FinishPasskeyLogin(
				t.Context(), started.Challenge.Token, "https://gofer.example", []byte(`{"assertion":true}`),
				"198.51.100.20", "Passkey Login Browser/1.0",
			)
			if err != nil || session == nil {
				t.Fatalf("FinishPasskeyLogin() = %#v, %v", session, err)
			}
			if session.AuthenticationMethod != AuthenticationMethodPasskey ||
				session.AssuranceLevel != AssuranceLevelPhishingResistant || session.StepUpAt == nil ||
				session.StepUpMethod != AuthenticationMethodPasskey || session.UserID != fixture.userID {
				t.Fatalf("passkey login session = %#v", session)
			}
			stored, err := fixture.manager.GetSessionByToken(t.Context(), session.Token)
			if err != nil || stored == nil || stored.ID != session.ID {
				t.Fatalf("stored passkey session = %#v, %v", stored, err)
			}
			var signCount int
			var lastUsedAt, consumedAt any
			var challengePayload []byte
			if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `
				SELECT sign_count, last_used_at FROM webauthn_credentials WHERE id = ?`, fixture.rowID,
			).Scan(&signCount, &lastUsedAt); err != nil || signCount != 8 || lastUsedAt == nil {
				t.Fatalf("passkey use metadata = count:%d used:%v err:%v", signCount, lastUsedAt, err)
			}
			if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `
				SELECT consumed_at, payload_ciphertext FROM auth_challenges WHERE id = ?`, started.Challenge.ID,
			).Scan(&consumedAt, &challengePayload); err != nil || consumedAt == nil || challengePayload != nil {
				t.Fatalf("passkey challenge completion = consumed:%v payload:%q err:%v", consumedAt, challengePayload, err)
			}
			if _, err := fixture.manager.FinishPasskeyLogin(
				t.Context(), started.Challenge.Token, "https://gofer.example", []byte(`{}`), "198.51.100.20", "Browser",
			); !errors.Is(err, ErrPasskeyAuthenticationInvalid) {
				t.Fatalf("passkey login replay error = %v", err)
			}
		})
	}
}

func TestPasskeyStepUpIsSessionBoundAndRefreshesOnlyCurrentSession(t *testing.T) {
	fixture := newPasskeyAuthenticationFixture(t, 11)
	staleAt := fixture.clock.now.Add(-securityStepUpMaximumAge - time.Second)
	session := &Session{
		ID: "step-up-session", UserID: fixture.userID, Token: "step-up-session-token", AuthVersion: 1,
		AuthenticationMethod: AuthenticationMethodPassword, AssuranceLevel: AssuranceLevelMultiFactor,
		UserAgent: "Original Browser", AuthenticatedAt: fixture.clock.now.Add(-time.Hour), LastUsedAt: fixture.clock.now,
		IdleExpiresAt: fixture.clock.now.Add(time.Hour), AbsoluteExpiresAt: fixture.clock.now.Add(24 * time.Hour),
		StepUpAt: &staleAt, StepUpMethod: AuthenticationMethodTOTP, CreatedAt: fixture.clock.now.Add(-time.Hour),
	}
	if _, err := fixture.manager.db.Write().ExecContext(t.Context(), `
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
		t.Fatal(err)
	}
	started, err := fixture.manager.StartPasskeyStepUp(
		t.Context(), session.Token, "https://gofer.example", "198.51.100.30",
	)
	if err != nil || started == nil || started.Challenge.SessionID != session.ID || started.Challenge.Purpose != ChallengePurposeStepUp {
		t.Fatalf("StartPasskeyStepUp() = %#v, %v", started, err)
	}
	if err := fixture.manager.FinishPasskeyStepUp(
		t.Context(), started.Challenge.Token, session.Token, "https://gofer.example",
		[]byte(`{"assertion":true}`), "198.51.100.30", "Step-up Browser/1.0",
	); err != nil {
		t.Fatalf("FinishPasskeyStepUp() error = %v", err)
	}
	refreshed, err := fixture.manager.GetSessionByToken(t.Context(), session.Token)
	if err != nil || refreshed == nil || refreshed.StepUpAt == nil || !refreshed.StepUpAt.Equal(fixture.clock.now) || refreshed.StepUpMethod != AuthenticationMethodPasskey {
		t.Fatalf("passkey stepped-up session = %#v, %v", refreshed, err)
	}
	var sessions int
	if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("passkey step-up session count = %d, %v", sessions, err)
	}
}

func TestPasskeyCounterPolicyAcceptsSyncedZeroAndRejectsRegression(t *testing.T) {
	t.Run("synced zero counter", func(t *testing.T) {
		fixture := newPasskeyAuthenticationFixture(t, 0)
		started, err := fixture.manager.StartPasskeyLogin(t.Context(), "", "https://gofer.example", "source")
		if err != nil {
			t.Fatal(err)
		}
		session, err := fixture.manager.FinishPasskeyLogin(
			t.Context(), started.Challenge.Token, "https://gofer.example", []byte(`{}`), "source", "Browser",
		)
		if err != nil || session == nil {
			t.Fatalf("zero-counter passkey login = %#v, %v", session, err)
		}
		var signCount, cloneWarning int
		if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `
			SELECT sign_count, clone_warning FROM webauthn_credentials WHERE id = ?`, fixture.rowID,
		).Scan(&signCount, &cloneWarning); err != nil || signCount != 0 || cloneWarning != 0 {
			t.Fatalf("zero-counter state = count:%d clone:%d err:%v", signCount, cloneWarning, err)
		}
	})

	t.Run("meaningful regression", func(t *testing.T) {
		fixture := newPasskeyAuthenticationFixture(t, 12)
		regressed := passkeyAuthenticationCredential(fixture.credentialID, 12, true)
		record, err := passkeyCredentialRecordFromCredential(&regressed)
		if err != nil {
			t.Fatal(err)
		}
		fixture.assertion.finishRecord = record
		started, err := fixture.manager.StartPasskeyLogin(t.Context(), "", "https://gofer.example", "source")
		if err != nil {
			t.Fatal(err)
		}
		if session, err := fixture.manager.FinishPasskeyLogin(
			t.Context(), started.Challenge.Token, "https://gofer.example", []byte(`{}`), "source", "Browser",
		); !errors.Is(err, ErrPasskeyCloneWarning) || session != nil {
			t.Fatalf("regressed passkey login = %#v, %v", session, err)
		}
		var signCount, cloneWarning, sessions int
		if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `
			SELECT sign_count, clone_warning FROM webauthn_credentials WHERE id = ?`, fixture.rowID,
		).Scan(&signCount, &cloneWarning); err != nil || signCount != 12 || cloneWarning != 1 {
			t.Fatalf("regressed passkey state = count:%d clone:%d err:%v", signCount, cloneWarning, err)
		}
		if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil || sessions != 0 {
			t.Fatalf("regressed passkey sessions = %d, %v", sessions, err)
		}
		var failures int
		if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM auth_events WHERE event_type = ? AND success = 0 AND reason = ?`,
			AuthEventLoginFailed, AuthEventReasonPolicyRequired,
		).Scan(&failures); err != nil || failures != 1 {
			t.Fatalf("regressed passkey event count = %d, %v", failures, err)
		}
	})
}

func TestPasskeyAssertionFailureIsSingleUseAndDiscoverableOwnershipIsExact(t *testing.T) {
	fixture := newPasskeyAuthenticationFixture(t, 4)
	otherHandle := bytes.Repeat([]byte{0x72}, 32)
	insertActiveUser(t, fixture.manager, "other-passkey-user", false, fixture.clock.now)
	if _, err := fixture.manager.db.Write().ExecContext(t.Context(), `
		INSERT INTO webauthn_users (user_id, rp_id, user_handle, created_at)
		VALUES ('other-passkey-user', 'gofer.example', ?, ?)`, otherHandle, fixture.clock.now,
	); err != nil {
		t.Fatalf("insert other passkey user handle: %v", err)
	}
	fixture.assertion.discoverableHandle = otherHandle
	started, err := fixture.manager.StartPasskeyLogin(t.Context(), "", "https://gofer.example", "source")
	if err != nil {
		t.Fatal(err)
	}
	if session, err := fixture.manager.FinishPasskeyLogin(
		t.Context(), started.Challenge.Token, "https://gofer.example", []byte(`{}`), "source", "Browser",
	); session != nil {
		t.Fatalf("cross-handle passkey login returned session %#v", session)
	} else {
		var validationError *PasskeyAuthenticationValidationError
		if !errors.As(err, &validationError) {
			t.Fatalf("cross-handle passkey error = %T %v", err, err)
		}
	}
	var active, sessions int
	if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_challenges WHERE id = ? AND consumed_at IS NULL`, started.Challenge.ID,
	).Scan(&active); err != nil || active != 0 {
		t.Fatalf("invalid discoverable challenge active = %d, %v", active, err)
	}
	if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("invalid discoverable sessions = %d, %v", sessions, err)
	}
}

func TestPasskeyLoginCompletionIsAtomicAndSingleWinner(t *testing.T) {
	t.Run("audit failure rolls back every security mutation", func(t *testing.T) {
		fixture := newPasskeyAuthenticationFixture(t, 6)
		started, err := fixture.manager.StartPasskeyLogin(t.Context(), "", "https://gofer.example", "source")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.manager.db.Write().ExecContext(t.Context(), `
			CREATE TRIGGER reject_passkey_login_event
			BEFORE INSERT ON auth_events
			WHEN NEW.event_type = 'login_succeeded'
			BEGIN SELECT RAISE(ABORT, 'reject passkey login audit'); END`); err != nil {
			t.Fatal(err)
		}
		if session, err := fixture.manager.FinishPasskeyLogin(
			t.Context(), started.Challenge.Token, "https://gofer.example", []byte(`{}`), "source", "Browser",
		); err == nil || session != nil {
			t.Fatalf("audit-failed passkey login = %#v, %v", session, err)
		}
		var signCount, active, sessions int
		if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `SELECT sign_count FROM webauthn_credentials WHERE id = ?`, fixture.rowID).Scan(&signCount); err != nil {
			t.Fatal(err)
		}
		if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_challenges WHERE id = ? AND consumed_at IS NULL`, started.Challenge.ID).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
			t.Fatal(err)
		}
		if signCount != 6 || active != 1 || sessions != 0 {
			t.Fatalf("audit rollback state = count:%d active:%d sessions:%d", signCount, active, sessions)
		}
	})

	t.Run("one assertion challenge creates one session", func(t *testing.T) {
		fixture := newPasskeyAuthenticationFixture(t, 9)
		started, err := fixture.manager.StartPasskeyLogin(t.Context(), "", "https://gofer.example", "source")
		if err != nil {
			t.Fatal(err)
		}
		type result struct {
			session *Session
			err     error
		}
		ready := make(chan struct{})
		results := make(chan result, 2)
		var wait sync.WaitGroup
		for range 2 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-ready
				session, err := fixture.manager.FinishPasskeyLogin(
					t.Context(), started.Challenge.Token, "https://gofer.example", []byte(`{}`), "source", "Browser",
				)
				results <- result{session: session, err: err}
			}()
		}
		close(ready)
		wait.Wait()
		close(results)
		successes := 0
		for result := range results {
			if result.err == nil && result.session != nil {
				successes++
			}
		}
		var sessions int
		if err := fixture.manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil {
			t.Fatal(err)
		}
		if successes != 1 || sessions != 1 {
			t.Fatalf("concurrent passkey completions = successes:%d sessions:%d", successes, sessions)
		}
	})
}

func TestRealPasskeyAssertionAdapterBindsRequiredUserVerification(t *testing.T) {
	assertion, err := newPasskeyAssertionCeremony("https://gofer.example")
	if err != nil {
		t.Fatal(err)
	}
	credential := passkeyAuthenticationCredential([]byte("adapter-credential"), 0, false)
	user := passkeyUser{
		ID: bytes.Repeat([]byte{0x22}, 32), Name: "person", DisplayName: "Person",
		Credentials: []webauthn.Credential{credential},
	}
	requestJSON, sessionJSON, err := assertion.Begin(&user)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(requestJSON, []byte(`"userVerification":"required"`)) ||
		!bytes.Contains(requestJSON, []byte(`"allowCredentials"`)) || len(sessionJSON) == 0 {
		t.Fatalf("identifier assertion options=%s session=%s", requestJSON, sessionJSON)
	}
	discoverableJSON, discoverableSession, err := assertion.Begin(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(discoverableJSON, []byte(`"userVerification":"required"`)) ||
		bytes.Contains(discoverableJSON, []byte(`"allowCredentials":[{`)) || len(discoverableSession) == 0 {
		t.Fatalf("discoverable assertion options=%s session=%s", discoverableJSON, discoverableSession)
	}
	if _, err := assertion.Finish(user, sessionJSON, []byte(`{"invalid":true}`)); err == nil {
		t.Fatal("real assertion adapter accepted an invalid response")
	}
}

func TestRealPasskeyAssertionAdapterRejectsOriginRPChallengeSignatureAndUserHandleFailures(t *testing.T) {
	validUser, validSession, validResponse, validSignature := realPasskeyAssertionTestVector(t)
	assertion, err := newPasskeyAssertionCeremony("https://example.org")
	if err != nil {
		t.Fatal(err)
	}
	if record, err := assertion.Finish(validUser, validSession, validResponse); err != nil || record == nil {
		t.Fatalf("valid assertion vector = %#v, %v", record, err)
	}

	tests := []struct {
		name             string
		configureAdapter func(*testing.T, passkeyAssertionCeremony)
		mutateResponse   func(*testing.T, []byte, []byte) ([]byte, []byte)
	}{
		{
			name: "origin mismatch",
			configureAdapter: func(t *testing.T, adapter passkeyAssertionCeremony) {
				configured, ok := adapter.(*goWebAuthnAssertion)
				if !ok {
					t.Fatalf("assertion adapter type = %T", adapter)
				}
				configured.webAuthn.Config.RPOrigins = []string{"https://other.example"}
			},
		},
		{
			name: "relying party mismatch",
			configureAdapter: func(t *testing.T, adapter passkeyAssertionCeremony) {
				configured, ok := adapter.(*goWebAuthnAssertion)
				if !ok {
					t.Fatalf("assertion adapter type = %T", adapter)
				}
				configured.webAuthn.Config.RPID = "other.example"
			},
		},
		{
			name: "challenge mismatch",
			mutateResponse: func(t *testing.T, sessionJSON, responseJSON []byte) ([]byte, []byte) {
				var session webauthn.SessionData
				if err := json.Unmarshal(sessionJSON, &session); err != nil {
					t.Fatal(err)
				}
				session.Challenge = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x44}, 32))
				return mustMarshalPasskeyTestJSON(t, session), responseJSON
			},
		},
		{
			name: "invalid signature",
			mutateResponse: func(t *testing.T, sessionJSON, responseJSON []byte) ([]byte, []byte) {
				var response map[string]any
				if err := json.Unmarshal(responseJSON, &response); err != nil {
					t.Fatal(err)
				}
				tampered := append([]byte(nil), validSignature...)
				tampered[len(tampered)-1] ^= 0x01
				response["response"].(map[string]any)["signature"] = base64.RawURLEncoding.EncodeToString(tampered)
				return sessionJSON, mustMarshalPasskeyTestJSON(t, response)
			},
		},
		{
			name: "user handle mismatch",
			mutateResponse: func(t *testing.T, sessionJSON, responseJSON []byte) ([]byte, []byte) {
				var response map[string]any
				if err := json.Unmarshal(responseJSON, &response); err != nil {
					t.Fatal(err)
				}
				response["response"].(map[string]any)["userHandle"] = base64.RawURLEncoding.EncodeToString([]byte("wrong-user-handle"))
				return sessionJSON, mustMarshalPasskeyTestJSON(t, response)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			user, sessionJSON, responseJSON, _ := realPasskeyAssertionTestVector(t)
			if test.mutateResponse != nil {
				sessionJSON, responseJSON = test.mutateResponse(t, sessionJSON, responseJSON)
			}
			assertion, err := newPasskeyAssertionCeremony("https://example.org")
			if err != nil {
				t.Fatal(err)
			}
			if test.configureAdapter != nil {
				test.configureAdapter(t, assertion)
			}
			if record, err := assertion.Finish(user, sessionJSON, responseJSON); err == nil || record != nil {
				t.Fatalf("rejected assertion = %#v, %v", record, err)
			}
		})
	}
}

func realPasskeyAssertionTestVector(t *testing.T) (passkeyUser, []byte, []byte, []byte) {
	t.Helper()
	// W3C WebAuthn Level 3 packed ES256 authentication vector.
	const (
		authenticatorDataHex = "bfabc37432958b063360d3ad6461c9c4735ae7f8edd46592a5e0f01452b2e4b50d00000000"
		clientDataJSONHex    = "7b2274797065223a22776562617574686e2e676574222c226368616c6c656e6765223a2273524276704770587676463446524841565833496d4b4130453958773858306b526a44426c4d6668726255222c226f726967696e223a2268747470733a2f2f6578616d706c652e6f7267222c2263726f73734f726967696e223a66616c73652c22657874726144617461223a22636c69656e74446174614a534f4e206d617920626520657874656e6465642077697468206164646974696f6e616c206669656c647320696e20746865206675747572652c207375636820617320746869733a20415a4d77794d78496244382d756775464e7036723851227d"
		signatureHex         = "30450220694969d3ee928de6f02ef23a9c644d7d779916451734a94b432542f498a1ebe90221008b0819c824218a97152cd099c55bfb1477b29d900a49a64018314f9bfccda163"
		credentialIDHex      = "c9a6f5b3462d02873fea0c56862234f99f081728084e511bb7760201a89054a5"
		challengeHex         = "b1106fa46a57bef1781511c0557dc898a03413d5f0f17d244630c194c7e1adb5"
		credentialPubKeyHex  = "a50102032620012158201cf27f25da591208a4239c2e324f104f585525479a29edeedd830f48e77aeae522582059e4b7da6c0106e206ce390c93ab98a15a5ec3887e57f0cc2bece803b920c423"
	)
	decode := func(value string) []byte {
		decoded, err := hex.DecodeString(value)
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	credentialID := decode(credentialIDHex)
	publicKey := decode(credentialPubKeyHex)
	signature := decode(signatureHex)
	userID := []byte("test-user-id")
	user := passkeyUser{
		ID: userID, Name: "test-user", DisplayName: "Test User",
		Credentials: []webauthn.Credential{{
			ID: credentialID, PublicKey: publicKey,
			Flags: webauthn.CredentialFlags{UserPresent: true, BackupEligible: true},
		}},
	}
	session := webauthn.SessionData{
		Challenge:            base64.RawURLEncoding.EncodeToString(decode(challengeHex)),
		RelyingPartyID:       "example.org",
		UserID:               userID,
		AllowedCredentialIDs: [][]byte{credentialID},
		Expires:              time.Now().Add(time.Minute),
		UserVerification:     protocol.VerificationRequired,
	}
	id := base64.RawURLEncoding.EncodeToString(credentialID)
	response := map[string]any{
		"id": id, "rawId": id, "type": "public-key",
		"response": map[string]any{
			"authenticatorData": base64.RawURLEncoding.EncodeToString(decode(authenticatorDataHex)),
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(decode(clientDataJSONHex)),
			"signature":         base64.RawURLEncoding.EncodeToString(signature),
		},
	}
	return user, mustMarshalPasskeyTestJSON(t, session), mustMarshalPasskeyTestJSON(t, response), signature
}

func mustMarshalPasskeyTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
