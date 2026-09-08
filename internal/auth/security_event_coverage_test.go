package auth

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type coverageEvent struct {
	actor, subject, session, kind, reason, metadata string
	success                                         bool
}

func coverageEvents(t *testing.T, m *Manager) []coverageEvent {
	t.Helper()
	rows, err := m.db.Read().QueryContext(t.Context(), `SELECT COALESCE(actor_user_id,''), COALESCE(subject_user_id,''), COALESCE(session_id,''), event_type, success, reason, metadata_json FROM auth_events ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []coverageEvent
	for rows.Next() {
		var e coverageEvent
		if err := rows.Scan(&e.actor, &e.subject, &e.session, &e.kind, &e.success, &e.reason, &e.metadata); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}
func failCoverageEventWrites(t *testing.T, m *Manager) {
	t.Helper()
	if _, err := m.db.Write().ExecContext(t.Context(), `CREATE TRIGGER coverage_reject_event BEFORE INSERT ON auth_events BEGIN SELECT RAISE(ABORT, 'forced audit failure'); END`); err != nil {
		t.Fatal(err)
	}
}
func assertCoveragePrivateDataAbsent(t *testing.T, m *Manager, forbidden ...string) {
	t.Helper()
	rows, err := m.db.Read().QueryContext(t.Context(), `SELECT id, COALESCE(actor_user_id,''), COALESCE(subject_user_id,''), COALESCE(session_id,''), request_id, event_type, reason, user_agent, source_hash, metadata_json FROM auth_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		fields := make([]string, 10)
		dest := make([]any, 10)
		for i := range fields {
			dest[i] = &fields[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		for _, secret := range forbidden {
			for _, field := range fields {
				if secret != "" && strings.Contains(field, secret) {
					t.Fatalf("private data persisted in audit: %q", secret)
				}
			}
		}
		var metadata map[string]any
		if err := json.Unmarshal([]byte(fields[9]), &metadata); err != nil {
			t.Fatal(err)
		}
		for key := range metadata {
			switch key {
			case "method", "stage", "revocation_reason":
			default:
				t.Fatalf("unexpected metadata key %q", key)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestCoveragePasswordLoginEventsAndThrottleAtomicity(t *testing.T) {
	now := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	hash := currentPasswordLoginHash(t)
	for _, scenario := range []string{"success", "mfa", "wrong", "unknown", "disabled", "wrong surface", "throttled", "threshold", "failure rollback", "success rollback", "mfa rollback"} {
		t.Run(scenario, func(t *testing.T) {
			m := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
			status := UserStatusActive
			if scenario == "disabled" {
				status = UserStatusDisabled
			}
			insertPasswordLoginUser(t, m, "person", "private-user", status, false, strings.HasPrefix(scenario, "mfa"), false, hash, now.Add(-time.Hour))
			opts := PasswordLoginOptions{Identifier: "private-user", Password: passwordLoginTestPassword, Source: "192.0.2.123", UserAgent: "private-header"}
			if scenario == "wrong" || scenario == "failure rollback" || scenario == "threshold" {
				opts.Password = "incorrect-secret"
			}
			if scenario == "wrong surface" {
				opts.RequiredUserType = UserTypeManagement
			}
			if scenario == "unknown" {
				opts.Identifier = "unknown-private-user"
			}
			if scenario == "throttled" || scenario == "threshold" {
				attempts := 5
				if scenario == "threshold" {
					attempts = 4
				}
				for i := 0; i < attempts; i++ {
					if _, err := m.RecordLoginFailure(t.Context(), opts.Identifier, opts.Source); err != nil {
						t.Fatal(err)
					}
				}
			}
			if strings.HasSuffix(scenario, "rollback") {
				failCoverageEventWrites(t, m)
			}
			result, err := m.AuthenticatePassword(t.Context(), opts)
			events := coverageEvents(t, m)
			if strings.HasSuffix(scenario, "rollback") {
				if err == nil || result != nil || len(events) != 0 {
					t.Fatalf("rollback result=%v err=%v events=%v", result, err, events)
				}
				for _, table := range []string{"sessions", "auth_throttle", "auth_challenges"} {
					var count int
					if err := m.db.Read().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != 0 {
						t.Fatalf("%s count=%d err=%v", table, count, err)
					}
				}
				var last sql.NullTime
				if err := m.db.Read().QueryRowContext(t.Context(), `SELECT last_login_at FROM users WHERE id='person'`).Scan(&last); err != nil || last.Valid {
					t.Fatalf("last login changed: %v %v", last, err)
				}
				return
			}
			if len(events) != 1 {
				t.Fatalf("events=%v", events)
			}
			e := events[0]
			switch scenario {
			case "success":
				if err != nil || result.Session == nil || e.kind != "login_succeeded" || !e.success || e.actor != "person" || e.subject != "person" || e.session != result.Session.ID || e.reason != "challenge_verified" {
					t.Fatalf("success=%v %v event=%v", result, err, e)
				}
			case "mfa":
				if err != nil || result.Session != nil || result.PreAuthChallenge == nil || e.kind != "primary_verified" || !e.success || e.actor != "" || e.subject != "person" || e.session != "" || e.reason != "policy_required" {
					t.Fatalf("mfa=%v %v event=%v", result, err, e)
				}
			default:
				reason := "invalid_credentials"
				if scenario == "throttled" || scenario == "threshold" {
					reason = "throttled"
				}
				if err == nil || result != nil || e.kind != "login_failed" || e.success || e.actor != "" || e.subject != "" || e.session != "" || e.reason != reason {
					t.Fatalf("failure=%v %v event=%v", result, err, e)
				}
			}
			assertCoveragePrivateDataAbsent(t, m, opts.Identifier, opts.Password, opts.Source, opts.UserAgent, hash, hashToken(opts.Password))
		})
	}
}

func TestCoverageLogoutSingleWinnerAndRollback(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "concurrent", true: "rollback"}[rollback], func(t *testing.T) {
			now := time.Now().UTC()
			m := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
			insertActiveUser(t, m, "person", false, now)
			session, err := m.CreateSession(t.Context(), "person", "private-browser")
			if err != nil {
				t.Fatal(err)
			}
			if rollback {
				failCoverageEventWrites(t, m)
				if changed, err := m.RevokeSessionByToken(t.Context(), session.Token, "person", SessionRevocationLogout); err == nil || changed {
					t.Fatalf("revoke=%v %v", changed, err)
				}
				active, err := m.GetSessionByToken(t.Context(), session.Token)
				if err != nil || active == nil {
					t.Fatalf("session lost: %v %v", active, err)
				}
				if len(coverageEvents(t, m)) != 0 {
					t.Fatal("rollback emitted event")
				}
				return
			}
			var wg sync.WaitGroup
			results := make(chan bool, 2)
			errs := make(chan error, 2)
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					changed, err := m.RevokeSessionByToken(t.Context(), session.Token, "", SessionRevocationLogout)
					results <- changed
					errs <- err
				}()
			}
			wg.Wait()
			close(results)
			close(errs)
			count := 0
			for changed := range results {
				if changed {
					count++
				}
			}
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			events := coverageEvents(t, m)
			if count != 1 || len(events) != 1 {
				t.Fatalf("winners=%d events=%v", count, events)
			}
			e := events[0]
			if e.actor != "person" || e.subject != "person" || e.session != session.ID || e.kind != "session_revoked" || !e.success || e.reason != "user_action" || e.metadata != `{"revocation_reason":"logout"}` {
				t.Fatalf("event=%v", e)
			}
			if changed, err := m.RevokeSessionByToken(t.Context(), "foreign-private-token", "", SessionRevocationLogout); changed || err != nil {
				t.Fatalf("unknown=%v %v", changed, err)
			}
			if len(coverageEvents(t, m)) != 1 {
				t.Fatal("unknown bearer emitted event")
			}
			assertCoveragePrivateDataAbsent(t, m, session.Token, hashToken(session.Token), "private-browser", "foreign-private-token")
		})
	}
}

func TestCoverageFederatedPrimaryAuthenticationAtomicity(t *testing.T) {
	for _, method := range []AuthenticationMethod{AuthenticationMethodFederatedGoogle, AuthenticationMethodFederatedMicrosoft, AuthenticationMethodFederatedOIDC} {
		for _, rollback := range []bool{false, true} {
			t.Run(string(method)+map[bool]string{false: " success", true: " rollback"}[rollback], func(t *testing.T) {
				now := time.Now().UTC()
				m := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
				insertActiveUser(t, m, "person", false, now)
				challenge, err := m.CreatePreAuthChallenge(t.Context(), PreAuthChallengeOptions{Purpose: ChallengePurposeFederatedLogin, Origin: m.config.BaseURL, IssueNonce: true})
				if err != nil {
					t.Fatal(err)
				}
				if rollback {
					failCoverageEventWrites(t, m)
				}
				result, err := m.completeFederatedPrimaryAuthenticationWithTransition(t.Context(), "person", "private-header", method, m.consumeFederatedLoginForEvent(t.Context(), challenge.Token, challenge.Nonce))
				if rollback {
					if err == nil || result != nil {
						t.Fatalf("result=%v err=%v", result, err)
					}
					active, err := m.GetActivePreAuthChallenge(t.Context(), challenge.Token, challenge.Purpose, challenge.Origin)
					if err != nil || active == nil {
						t.Fatalf("challenge consumed on rollback: %v %v", active, err)
					}
					var sessions int
					if err := m.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil || sessions != 0 {
						t.Fatalf("sessions=%d err=%v", sessions, err)
					}
					if len(coverageEvents(t, m)) != 0 {
						t.Fatal("rollback emitted event")
					}
					return
				}
				if err != nil || result.Session == nil {
					t.Fatalf("result=%v err=%v", result, err)
				}
				events := coverageEvents(t, m)
				if len(events) != 1 {
					t.Fatalf("events=%v", events)
				}
				e := events[0]
				if e.kind != "login_succeeded" || e.actor != "person" || e.subject != "person" || e.session != result.Session.ID || !e.success || e.reason != "challenge_verified" {
					t.Fatalf("event=%v", e)
				}
				if _, err := m.completeFederatedPrimaryAuthenticationWithTransition(t.Context(), "person", "", method, m.consumeFederatedLoginForEvent(t.Context(), challenge.Token, challenge.Nonce)); !errors.Is(err, ErrPreAuthChallengeInvalid) {
					t.Fatalf("replay err=%v", err)
				}
				if len(coverageEvents(t, m)) != 1 {
					t.Fatal("replay duplicated success")
				}
				assertCoveragePrivateDataAbsent(t, m, challenge.Token, challenge.Nonce, result.Session.Token, hashToken(result.Session.Token), "private-header")
			})
		}
	}
}

func TestCoverageFederatedCallbackFailuresAreAtomicAndRedacted(t *testing.T) {
	for _, method := range []AuthenticationMethod{AuthenticationMethodFederatedGoogle, AuthenticationMethodFederatedMicrosoft, AuthenticationMethodFederatedOIDC} {
		for _, purpose := range []ChallengePurpose{ChallengePurposeFederatedLogin, ChallengePurposeFederatedLink, ChallengePurposeFederatedEnrollment} {
			for _, rollback := range []bool{false, true} {
				t.Run(string(method)+"/"+string(purpose)+map[bool]string{false: "", true: " rollback"}[rollback], func(t *testing.T) {
					now := time.Now().UTC()
					m := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
					insertActiveUser(t, m, "person", false, now)
					insertGoogleLinkSession(t, m, "person", "session", "private-session-token", now, now)
					user, session := "", ""
					kind := "login_failed"
					if purpose == ChallengePurposeFederatedLink {
						user, session, kind = "person", "session", "identity_linked"
					}
					if purpose == ChallengePurposeFederatedEnrollment {
						user, kind = "person", "enrollment_completed"
					}
					c, err := m.CreatePreAuthChallenge(t.Context(), PreAuthChallengeOptions{UserID: user, SessionID: session, Purpose: purpose, Origin: m.config.BaseURL, IssueNonce: true})
					if err != nil {
						t.Fatal(err)
					}
					if rollback {
						failCoverageEventWrites(t, m)
					}
					var reject func() error
					switch method {
					case AuthenticationMethodFederatedGoogle:
						reject = func() error {
							return m.rejectGoogleAuthorization(t.Context(), c.Token, purpose, FederatedLoginFailureNonceInvalid)
						}
					case AuthenticationMethodFederatedMicrosoft:
						reject = func() error {
							return m.rejectMicrosoftAuthorization(t.Context(), c.Token, purpose, FederatedLoginFailureNonceInvalid)
						}
					case AuthenticationMethodFederatedOIDC:
						reject = func() error {
							return m.rejectOIDCAuthorization(t.Context(), c.Token, purpose, FederatedLoginFailureNonceInvalid)
						}
					}
					err = reject()
					want := FederatedLoginFailureNonceInvalid
					if rollback {
						want = FederatedLoginFailureInternal
					}
					if FederatedLoginReason(err) != want {
						t.Fatalf("error=%v", err)
					}
					active, err := m.GetActivePreAuthChallenge(t.Context(), c.Token, purpose, c.Origin)
					if err != nil {
						t.Fatal(err)
					}
					events := coverageEvents(t, m)
					if rollback {
						if active == nil || len(events) != 0 {
							t.Fatalf("rollback active=%v events=%v", active, events)
						}
						return
					}
					if active != nil || len(events) != 1 {
						t.Fatalf("active=%v events=%v", active, events)
					}
					e := events[0]
					if e.actor != "" || e.subject != user || e.session != session || e.kind != kind || e.success || e.reason != "invalid_credentials" {
						t.Fatalf("event=%v", e)
					}
					assertCoveragePrivateDataAbsent(t, m, c.Token, c.Nonce, hashToken(c.Token), "private-session-token")
				})
			}
		}
	}
}

func TestCoverageEarlyThrottleEvents(t *testing.T) {
	now := time.Now().UTC()
	for _, scenario := range []string{"totp login", "recovery", "totp step up", "passkey step up", "totp replacement", "passkey login", "passkey unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			m, _, _, session := prepareMFAManagement(t, now, true)
			action := totpManagementThrottleAction
			identifier := session.UserID
			kind := AuthEventStepUpFailed
			method := AuthenticationMethodTOTP
			actor, subject, sessionID := session.UserID, session.UserID, session.ID
			var attempt func() error
			switch scenario {
			case "totp login", "recovery":
				insertTOTPLoginChallenge(t, m, "challenge", "private-challenge", now, 3)
				actor, sessionID = "", ""
				kind = AuthEventLoginFailed
				if scenario == "totp login" {
					action = totpLoginThrottleAction
					attempt = func() error {
						_, err := m.CompleteTOTPLogin(t.Context(), TOTPLoginOptions{Token: "private-challenge", Origin: totpLoginTestOrigin, Code: "private-code", Source: "private-source"})
						return err
					}
				} else {
					action = recoveryCodeThrottleAction
					method = AuthenticationMethodRecoveryCode
					attempt = func() error {
						_, err := m.StartRecoveryCodeRepair(t.Context(), RecoveryCodeLoginOptions{Token: "private-challenge", Origin: totpLoginTestOrigin, Code: "private-code", Source: "private-source"})
						return err
					}
				}
			case "totp step up":
				attempt = func() error {
					return m.VerifySecurityTOTPStepUp(t.Context(), session.Token, "private-code", "private-source", "private-header")
				}
			case "passkey step up":
				action = passkeyStepUpThrottleAction
				method = AuthenticationMethodPasskey
				attempt = func() error {
					_, err := m.StartPasskeyStepUp(t.Context(), session.Token, totpLoginTestOrigin, "private-source")
					return err
				}
			case "totp replacement":
				state, err := m.StartTOTPManagement(t.Context(), session.Token, totpLoginTestOrigin)
				if err != nil {
					t.Fatal(err)
				}
				kind = AuthEventCredentialChanged
				attempt = func() error {
					_, err := m.ConfirmTOTPManagement(t.Context(), state.Challenge.Token, session.Token, totpLoginTestOrigin, "private-code", "private-source", "private-header")
					return err
				}
			case "passkey login", "passkey unavailable":
				action = passkeyLoginThrottleAction
				method = AuthenticationMethodPasskey
				identifier = "private-unknown"
				actor, subject, sessionID = "", "", ""
				kind = AuthEventLoginFailed
				attempt = func() error {
					_, err := m.StartPasskeyLogin(t.Context(), identifier, totpLoginTestOrigin, "private-source")
					return err
				}
			}
			buckets, err := m.authenticationThrottleBuckets(action, identifier, "private-source")
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "passkey unavailable" {
				for i := 0; i < 5; i++ {
					if _, err := m.recordLoginThrottleBuckets(t.Context(), buckets); err != nil {
						t.Fatal(err)
					}
				}
			}
			err = attempt()
			if scenario == "passkey unavailable" {
				if !errors.Is(err, ErrPasskeyAuthenticationUnavailable) {
					t.Fatalf("error=%v", err)
				}
			} else if !errors.Is(err, ErrLoginThrottled) {
				t.Fatalf("error=%v", err)
			}
			events := coverageEvents(t, m)
			if len(events) != 1 {
				t.Fatalf("events=%v", events)
			}
			e := events[0]
			reason := "throttled"
			if scenario == "passkey unavailable" {
				reason = "invalid_credentials"
			}
			if e.kind != string(kind) || e.success || e.actor != actor || e.subject != subject || e.session != sessionID || e.reason != reason || !strings.Contains(e.metadata, string(method)) {
				t.Fatalf("event=%v", e)
			}
			assertCoveragePrivateDataAbsent(t, m, "private-code", "private-source", "private-header", "private-challenge", "private-unknown", session.Token)
		})
	}
}

func TestCoverageAuthorizationEndingDoesNotDuplicateEvents(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "ending and replay", true: "rollback"}[rollback], func(t *testing.T) {
			now := time.Now().UTC()
			m := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
			configureGoogleOAuthTest(m)
			start, err := m.BeginGoogleLogin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if rollback {
				failCoverageEventWrites(t, m)
			}
			err = m.TerminateGoogleAuthorizationChallenge(t.Context(), start.Challenge.Token)
			if rollback {
				if err == nil {
					t.Fatal("expected rollback")
				}
				active, err := m.GetActivePreAuthChallenge(t.Context(), start.Challenge.Token, ChallengePurposeFederatedLogin, m.config.BaseURL)
				if err != nil || active == nil {
					t.Fatalf("active=%v err=%v", active, err)
				}
				if len(coverageEvents(t, m)) != 0 {
					t.Fatal("rollback emitted event")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := m.TerminateGoogleAuthorizationChallenge(t.Context(), start.Challenge.Token); err != nil {
				t.Fatal(err)
			}
			events := coverageEvents(t, m)
			if len(events) != 1 || events[0].success || events[0].reason != "user_action" {
				t.Fatalf("events=%v", events)
			}
			if events[0].actor != "" || events[0].subject != "" || events[0].session != "" {
				t.Fatalf("invented attribution: %v", events[0])
			}
			assertCoveragePrivateDataAbsent(t, m, start.Challenge.Token, start.Challenge.Nonce, hashToken(start.Challenge.Token))
		})
	}
}

func TestCoverageRejectedPasswordChangeAttributionAndRedaction(t *testing.T) {
	m, _, session, _ := newPasswordChangeFixture(t)
	result, err := m.ChangePassword(t.Context(), PasswordChangeOptions{SessionToken: session.Token, CurrentPassword: "incorrect-private-password", NewPassword: "another-private-password", UserAgent: "private-header"})
	if result != nil || !errors.Is(err, ErrCurrentPasswordInvalid) {
		t.Fatalf("result=%v err=%v", result, err)
	}
	events := coverageEvents(t, m)
	if len(events) != 1 {
		t.Fatalf("events=%v", events)
	}
	e := events[0]
	if e.kind != "credential_changed" || e.success || e.reason != "invalid_credentials" || e.actor != session.UserID || e.subject != session.UserID || e.session != session.ID {
		t.Fatalf("event=%v", e)
	}
	assertCoveragePrivateDataAbsent(t, m, session.Token, hashToken(session.Token), "incorrect-private-password", "another-private-password", "private-header")
}
