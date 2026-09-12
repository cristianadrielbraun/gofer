package auth

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestEnrolledMFAAppliesToPasswordAndEveryFederatedPrimary(t *testing.T) {
	for _, required := range []bool{false, true} {
		for _, factor := range []string{"none", "totp", "passkey", "both", "foreign-passkey"} {
			for _, method := range []AuthenticationMethod{AuthenticationMethodPassword, AuthenticationMethodFederatedGoogle, AuthenticationMethodFederatedMicrosoft, AuthenticationMethodFederatedOIDC} {
				t.Run(fmt.Sprintf("required=%t/%s/%s", required, factor, method), func(t *testing.T) {
					now := time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC)
					m := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
					insertPasswordLoginUser(t, m, "person", "person", UserStatusActive, false, required, false, currentPasswordLoginHash(t), now)
					if factor == "totp" || factor == "both" {
						insertPolicyTestTOTP(t, m, "person", now)
					}
					if factor == "passkey" || factor == "both" || factor == "foreign-passkey" {
						insertPolicyTestPasskey(t, m, "person", now)
					}
					if factor == "foreign-passkey" {
						if _, err := m.db.Write().Exec(`UPDATE webauthn_credentials SET rp_id='foreign.example'`); err != nil {
							t.Fatal(err)
						}
					}
					var session *Session
					var challenge *PreAuthChallenge
					var enrollment bool
					if method == AuthenticationMethodPassword {
						result, err := m.AuthenticatePassword(t.Context(), PasswordLoginOptions{Identifier: "person", Password: passwordLoginTestPassword})
						if err != nil {
							t.Fatal(err)
						}
						session = result.Session
						challenge = result.PreAuthChallenge
						enrollment = result.MFAEnrollmentRequired
					} else {
						result, err := m.completeFederatedPrimaryAuthentication(t.Context(), "person", "Test browser", method)
						if err != nil {
							t.Fatal(err)
						}
						session = result.Session
						challenge = result.PreAuthChallenge
						enrollment = result.MFAEnrollmentRequired
					}
					enrolled := factor != "none" && factor != "foreign-passkey"
					if required || enrolled {
						if session != nil || challenge == nil || enrollment != (required && !enrolled) {
							t.Fatal("primary login bypassed verification or selected wrong enrollment flow")
						}
						if enrolled {
							factors, err := m.GetMFAContinuationFactors(t.Context(), challenge.Token, m.config.BaseURL)
							if err != nil || factors.PrimaryMethod != method {
								t.Fatalf("verified primary method not preserved: %v %v", factors, err)
							}
						}
						var sessions int
						if err := m.db.Read().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&sessions); err != nil || sessions != 0 {
							t.Fatal("session exists before MFA")
						}
						var eventType string
						if err := m.db.Read().QueryRow(`SELECT event_type FROM auth_events ORDER BY occurred_at DESC LIMIT 1`).Scan(&eventType); err != nil || eventType != string(AuthEventPrimaryVerified) {
							t.Fatalf("primary verification event: %s %v", eventType, err)
						}
					} else if session == nil || challenge != nil {
						t.Fatal("optional factorless account cannot sign in")
					}
					policy, err := m.loadAuthenticationPolicy(t.Context(), m.db.Read(), "person", 1)
					if err != nil || policy.MFAEnrollmentRequired != required || policy.RequiresMFA != (required || enrolled) {
						t.Fatalf("incorrect enrollment/verification policy: %#v %v", policy, err)
					}
				})
			}
		}
	}
}

func TestOptionalEnrolledTOTPCompletesEveryPrimaryMethod(t *testing.T) {
	for _, method := range []AuthenticationMethod{AuthenticationMethodPassword, AuthenticationMethodFederatedGoogle, AuthenticationMethodFederatedMicrosoft, AuthenticationMethodFederatedOIDC} {
		t.Run(string(method), func(t *testing.T) {
			now := time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC)
			m, secret, _ := prepareTOTPLoginManager(t, now, secureTokenGenerator{})
			if _, err := m.db.Write().Exec(`UPDATE users SET user_type='webmail',is_admin=0,mfa_required=0 WHERE id=?`, totpLoginTestUserID); err != nil {
				t.Fatal(err)
			}
			var challenge *PreAuthChallenge
			if method == AuthenticationMethodPassword {
				r, err := m.AuthenticatePassword(t.Context(), PasswordLoginOptions{Identifier: "totp-person", Password: passwordLoginTestPassword})
				if err != nil {
					t.Fatal(err)
				}
				challenge = r.PreAuthChallenge
			} else {
				r, err := m.completeFederatedPrimaryAuthentication(t.Context(), totpLoginTestUserID, "Browser", method)
				if err != nil {
					t.Fatal(err)
				}
				challenge = r.PreAuthChallenge
			}
			if challenge == nil {
				t.Fatal("optional MFA bypassed")
			}
			if _, err := completeTOTPLogin(t, m, challenge.Token, "invalid"); err == nil {
				t.Fatal("invalid code accepted")
			}
			session, err := completeTOTPLogin(t, m, challenge.Token, setupTOTPCode(t, secret, now))
			if err != nil || session == nil || session.AssuranceLevel != AssuranceLevelMultiFactor || session.AuthenticationMethod != method {
				t.Fatalf("MFA completion: %#v %v", session, err)
			}
			if _, err := completeTOTPLogin(t, m, challenge.Token, setupTOTPCode(t, secret, now)); err == nil {
				t.Fatal("challenge replay accepted")
			}
		})
	}
}

func TestPasskeyEnrollmentRevokesOtherSessionsAtomically(t *testing.T) {
	for _, failAudit := range []bool{false, true} {
		t.Run(fmt.Sprint(failAudit), func(t *testing.T) {
			f := newPasskeyTestFixture(t, false)
			m := f.manager
			if _, err := m.db.Write().Exec(`INSERT INTO sessions (id,user_id,token_hash,auth_version,authentication_method,assurance_level,user_agent,authenticated_at,last_used_at,idle_expires_at,absolute_expires_at,created_at) SELECT 'other-session',user_id,?,auth_version,authentication_method,assurance_level,user_agent,authenticated_at,last_used_at,idle_expires_at,absolute_expires_at,created_at FROM sessions WHERE id=?`, hashToken("other-token"), f.session.ID); err != nil {
				t.Fatal(err)
			}
			start, err := m.StartPasskeyRegistration(t.Context(), f.session.Token, "https://gofer.example", "Laptop")
			if err != nil {
				t.Fatal(err)
			}
			if failAudit {
				if _, err := m.db.Write().Exec(`CREATE TRIGGER fail_passkey_audit BEFORE INSERT ON auth_events WHEN NEW.event_type='credential_changed' BEGIN SELECT RAISE(ABORT,'test audit failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			_, err = m.FinishPasskeyRegistration(t.Context(), start.Challenge.Token, f.session.Token, "https://gofer.example", []byte(`{}`), "Browser")
			if (err != nil) != failAudit {
				t.Fatalf("registration error: %v", err)
			}
			var revoked, version, keys int
			if err := m.db.Read().QueryRow(`SELECT revoked_at IS NOT NULL FROM sessions WHERE id='other-session'`).Scan(&revoked); err != nil {
				t.Fatal(err)
			}
			if err := m.db.Read().QueryRow(`SELECT auth_version FROM users WHERE id=?`, f.session.UserID).Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err := m.db.Read().QueryRow(`SELECT COUNT(*) FROM webauthn_credentials`).Scan(&keys); err != nil {
				t.Fatal(err)
			}
			if failAudit {
				if revoked != 0 || version != 1 || keys != 0 {
					t.Fatal("failed enrollment did not roll back")
				}
			} else {
				if revoked != 1 || version != 2 || keys != 1 {
					t.Fatal("enrollment did not revoke others atomically")
				}
				current, err := m.GetSessionByToken(t.Context(), f.session.Token)
				if err != nil || current == nil || current.AssuranceLevel != AssuranceLevelPhishingResistant {
					t.Fatal("retained session was not strengthened")
				}
			}
		})
	}
}

func TestOptionalMFARecoveryCompletesWithoutDowngrade(t *testing.T) {
	now := time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC)
	m, oldBatch := prepareRecoveryLoginManager(t, now, secureTokenGenerator{})
	if _, err := m.db.Write().Exec(`UPDATE users SET user_type='webmail',is_admin=0,mfa_required=0 WHERE id=?`, totpLoginTestUserID); err != nil {
		t.Fatal(err)
	}
	repair, err := startRecoveryRepair(t, m, recoveryLoginTestToken, oldBatch.Codes[0])
	if err != nil {
		t.Fatal(err)
	}
	_, draft, _, err := m.readRecoveryRepairDraft(t.Context(), repair.Token, totpLoginTestOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ConfirmRecoveryRepairTOTP(t.Context(), repair.Token, totpLoginTestOrigin, setupTOTPCode(t, draft.TOTPSecret, now)); err != nil {
		t.Fatal(err)
	}
	batch, err := m.GenerateRecoveryRepairCodes(t.Context(), repair.Token, totpLoginTestOrigin, false)
	if err != nil {
		t.Fatal(err)
	}
	session, err := m.CompleteRecoveryRepair(t.Context(), CompleteRecoveryRepairOptions{Token: repair.Token, Origin: totpLoginTestOrigin, BatchID: batch.RecoveryBatchID, Saved: true})
	if err != nil || session == nil || session.AssuranceLevel != AssuranceLevelMultiFactor {
		t.Fatalf("optional recovery completion: %#v %v", session, err)
	}
	login, err := m.AuthenticatePassword(t.Context(), PasswordLoginOptions{Identifier: "totp-person", Password: passwordLoginTestPassword})
	if err != nil || login == nil || login.Session != nil || login.PreAuthChallenge == nil {
		t.Fatal("recovery disabled future MFA")
	}
}

func TestClearingEnrollmentRequirementDoesNotCancelEnrolledMFAVerification(t *testing.T) {
	now := time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC)
	m, secret, _ := prepareTOTPLoginManager(t, now, secureTokenGenerator{})
	if _, err := m.db.Write().Exec(`UPDATE users SET user_type='webmail',is_admin=0,mfa_required=1 WHERE id=?`, totpLoginTestUserID); err != nil {
		t.Fatal(err)
	}
	login, err := m.AuthenticatePassword(t.Context(), PasswordLoginOptions{Identifier: "totp-person", Password: passwordLoginTestPassword})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Write().Exec(`UPDATE users SET mfa_required=0 WHERE id=?`, totpLoginTestUserID); err != nil {
		t.Fatal(err)
	}
	if challenge, err := m.GetActiveMFAChallenge(t.Context(), login.PreAuthChallenge.Token, totpLoginTestOrigin); err != nil || challenge == nil {
		t.Fatal("clearing enrollment requirement invalidated active verification")
	}
	session, err := completeTOTPLogin(t, m, login.PreAuthChallenge.Token, setupTOTPCode(t, secret, now))
	if err != nil || session == nil || session.AssuranceLevel != AssuranceLevelMultiFactor {
		t.Fatal("MFA completion failed after clearing enrollment requirement")
	}
}

func TestLegacySessionNeedsStrongStepUpBeforeRemovingOptionalMFA(t *testing.T) {
	now := time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC)
	m, _, secret, session := prepareMFAManagement(t, now, false)
	if _, err := m.db.Write().Exec(`UPDATE users SET user_type='webmail',is_admin=0,mfa_required=0 WHERE id=?; UPDATE sessions SET assurance_level='single_factor' WHERE id=?`, session.UserID, session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.DisableTOTP(t.Context(), session.Token, "Browser"); err == nil {
		t.Fatal("removed optional MFA without strong step-up")
	}
	if err := m.VerifySecurityTOTPStepUp(t.Context(), session.Token, setupTOTPCode(t, secret, now), "198.51.100.1", "Browser"); err != nil {
		t.Fatal(err)
	}
	current, err := m.GetSessionByToken(t.Context(), session.Token)
	if err != nil || current == nil || current.AssuranceLevel != AssuranceLevelMultiFactor {
		t.Fatal("strong step-up did not strengthen legacy session")
	}
	if _, err := m.DisableTOTP(t.Context(), session.Token, "Browser"); err != nil {
		t.Fatal(err)
	}
	policy, err := m.loadAuthenticationPolicy(t.Context(), m.db.Read(), session.UserID, 0)
	if err != nil || policy.RequiresMFA {
		t.Fatal("removing last optional factor did not restore primary-only login policy")
	}
}

func TestGoogleMFAConfirmationUsesVerifiedEmailWithoutAuditDisclosure(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	m, _, _ := prepareTOTPLoginManager(t, now, secureTokenGenerator{})
	if _, err := m.db.Write().Exec(`UPDATE users SET user_type='webmail',is_admin=0 WHERE id=?`, totpLoginTestUserID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.db.Write().Exec(`INSERT INTO auth_identities (id,user_id,provider,issuer,subject,email,email_verified,created_at,linked_at) VALUES ('google-confirmation',?,?,?,'selected-subject','stale@example.com',1,?,?)`, totpLoginTestUserID, googleIdentityProvider, googleLoginIssuer, now, now); err != nil {
		t.Fatal(err)
	}
	claims := &GoogleIDTokenClaims{Subject: "selected-subject", Email: "selected@example.com", EmailVerified: true}
	_, result, err := m.authenticateGoogleIdentity(t.Context(), claims, "Browser")
	if err != nil {
		t.Fatal(err)
	}
	if result.Session != nil || result.PreAuthChallenge == nil {
		t.Fatal("MFA was bypassed")
	}
	factors, err := m.GetMFAContinuationFactors(t.Context(), result.PreAuthChallenge.Token, m.config.BaseURL)
	if err != nil || factors.VerifiedEmail != claims.Email {
		t.Fatalf("selected verified email missing: %v", err)
	}
	var payload []byte
	if err := m.db.Read().QueryRow(`SELECT payload_ciphertext FROM auth_challenges WHERE id=?`, result.PreAuthChallenge.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), claims.Email) {
		t.Fatal("email persisted as plaintext in continuation")
	}
	var disclosures int
	if err := m.db.Read().QueryRow(`SELECT COUNT(*) FROM auth_events WHERE metadata_json LIKE ?`, "%"+claims.Email+"%").Scan(&disclosures); err != nil || disclosures != 0 {
		t.Fatal("email disclosed in audit metadata")
	}
	claims.EmailVerified = false
	if _, _, err := m.authenticateGoogleIdentity(t.Context(), claims, "Browser"); err == nil {
		t.Fatal("unverified email accepted")
	}
}
