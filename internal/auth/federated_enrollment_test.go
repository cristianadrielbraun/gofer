package auth

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newGoogleEnrollmentTestManager(t *testing.T, now time.Time, tokens *deterministicTokenGenerator) *Manager {
	t.Helper()
	manager := newDeterministicManager(t, &fixedClock{now: now}, tokens)
	configureGoogleOAuthTest(manager)
	manager.config.GoogleLoginClient.RedirectURL = "https://gofer.example/auth/google/callback"
	manager.config.GoogleLoginClient.Scopes = []string{"openid", "email", "profile", "https://mail.google.com/"}
	return manager
}

func verifyGoogleEnrollmentClaims(manager *Manager, challenge *PreAuthChallenge, subject, email string) {
	manager.googleIDTokenVerifier = googleIDTokenVerifierFunc(func(_ context.Context, rawIDToken string) (*GoogleIDTokenClaims, error) {
		if rawIDToken != "signed-id-token" {
			return nil, errors.New("unexpected ID token")
		}
		return &GoogleIDTokenClaims{
			Subject: subject, Nonce: challenge.Nonce, Email: email, EmailVerified: true,
		}, nil
	})
}

func TestGoogleInvitationEnrollmentActivatesOnlyGoferIdentityAndSession(t *testing.T) {
	now := time.Date(2026, time.August, 15, 10, 0, 0, 0, time.UTC)
	manager := newGoogleEnrollmentTestManager(t, now, &deterministicTokenGenerator{
		ids:    []string{"google-challenge", "google-identity", "enrollment-event", "session-id"},
		tokens: []string{"state-token", "nonce-token", "session-token"},
	})
	insertRedemptionUser(t, manager, "invitee", UserStatusPending, now)
	insertRedemptionToken(t, manager, "invitation-id", "invitee", "private-invitation", EnrollmentTokenPurposeEnrollment, now.Add(time.Hour))

	start, err := manager.BeginGoogleEnrollment(t.Context(), " private-invitation ")
	if err != nil {
		t.Fatalf("BeginGoogleEnrollment() error = %v", err)
	}
	if start.Challenge.Purpose != ChallengePurposeFederatedEnrollment || start.Challenge.UserID != "invitee" || start.Challenge.SessionID != "" {
		t.Fatalf("Google enrollment challenge = %#v", start.Challenge)
	}
	authorizationURL, err := url.Parse(start.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	if got := authorizationURL.Query().Get("scope"); got != "openid email profile" || strings.Contains(got, "gmail") || strings.Contains(got, "contacts") || strings.Contains(got, "calendar") {
		t.Fatalf("Google enrollment scopes = %q", got)
	}
	var payload []byte
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT payload_ciphertext FROM auth_challenges WHERE id = 'google-challenge'`,
	).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{[]byte("private-invitation"), []byte(hashToken("private-invitation")), []byte("invitation-id")} {
		if bytes.Contains(payload, forbidden) {
			t.Fatalf("Google enrollment challenge exposed invitation material: %q", payload)
		}
	}

	verifyGoogleEnrollmentClaims(manager, start.Challenge, "google-subject", "different-google-address@example.com")
	result, err := manager.CompleteGoogleEnrollment(
		googleOAuthTestContext(t), start.Challenge.Token, "authorization-code", "Enrollment Browser/1.0",
	)
	if err != nil || result == nil || result.Session == nil || result.PreAuthChallenge != nil {
		t.Fatalf("CompleteGoogleEnrollment() = %#v, %v", result, err)
	}
	if result.Session.UserID != "invitee" || result.Session.AuthenticationMethod != AuthenticationMethodFederatedGoogle || result.Session.Token != "session-token" {
		t.Fatalf("Google enrollment session = %#v", result.Session)
	}

	var status UserStatus
	var username string
	var used, consumed int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT status, username FROM users WHERE id = 'invitee'`).Scan(&status, &username); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT used_at IS NOT NULL FROM user_enrollment_tokens WHERE id = 'invitation-id'`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT consumed_at IS NOT NULL FROM auth_challenges WHERE id = 'google-challenge'`).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if status != UserStatusActive || username != "invitee" || used != 1 || consumed != 1 {
		t.Fatalf("enrolled Gofer user = status:%q username:%q invitation-used:%d challenge-consumed:%d", status, username, used, consumed)
	}
	var identityUserID, identityEmail, identitySubject string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT user_id, email, subject FROM auth_identities WHERE id = 'google-identity'`,
	).Scan(&identityUserID, &identityEmail, &identitySubject); err != nil {
		t.Fatal(err)
	}
	if identityUserID != "invitee" || identityEmail != "different-google-address@example.com" || identitySubject != "google-subject" {
		t.Fatalf("enrolled Google identity = user:%q email:%q subject:%q", identityUserID, identityEmail, identitySubject)
	}
	for table, want := range map[string]int{
		"accounts": 0, "oauth_accounts": 0, "password_credentials": 0, "sessions": 1,
	} {
		var count int
		if err := manager.db.Read().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != want {
			t.Fatalf("%s rows = %d, want %d", table, count, want)
		}
	}
	var eventMetadata string
	if err := manager.db.Read().QueryRowContext(t.Context(), `
		SELECT metadata_json FROM auth_events WHERE id = 'enrollment-event'`,
	).Scan(&eventMetadata); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(eventMetadata, `"method":"federated_google"`) ||
		strings.Contains(eventMetadata, "private-invitation") || strings.Contains(eventMetadata, "google-subject") ||
		strings.Contains(eventMetadata, "different-google-address@example.com") {
		t.Fatalf("Google enrollment event metadata = %q", eventMetadata)
	}
}

func TestGoogleInvitationEnrollmentRejectsCredentialResetAndFactorlessMFAAccount(t *testing.T) {
	now := time.Date(2026, time.August, 15, 10, 30, 0, 0, time.UTC)
	manager := newGoogleEnrollmentTestManager(t, now, &deterministicTokenGenerator{})
	insertRedemptionUser(t, manager, "reset-user", UserStatusActive, now)
	insertRedemptionToken(t, manager, "reset-id", "reset-user", "reset-secret", EnrollmentTokenPurposeCredentialReset, now.Add(time.Hour))
	if start, err := manager.BeginGoogleEnrollment(t.Context(), "reset-secret"); start != nil || !errors.Is(err, ErrEnrollmentTokenInvalid) {
		t.Fatalf("credential-reset Google enrollment = %#v, %v", start, err)
	}

	insertRedemptionUser(t, manager, "pending-admin", UserStatusPending, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE users SET user_type = 'management' WHERE id = 'pending-admin'`); err != nil {
		t.Fatal(err)
	}
	insertRedemptionToken(t, manager, "admin-invite-id", "pending-admin", "admin-invite", EnrollmentTokenPurposeEnrollment, now.Add(time.Hour))
	if start, err := manager.BeginGoogleEnrollment(t.Context(), "admin-invite"); start != nil || !errors.Is(err, ErrEnrollmentTokenInvalid) {
		t.Fatalf("management-account Google enrollment = %#v, %v", start, err)
	}
	var challenges, identities, sessions int
	for table, target := range map[string]*int{"auth_challenges": &challenges, "auth_identities": &identities, "sessions": &sessions} {
		if err := manager.db.Read().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	var status UserStatus
	var usedAt sql.NullTime
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT status FROM users WHERE id = 'pending-admin'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT used_at FROM user_enrollment_tokens WHERE id = 'admin-invite-id'`).Scan(&usedAt); err != nil {
		t.Fatal(err)
	}
	if challenges != 0 || identities != 0 || sessions != 0 || status != UserStatusPending || usedAt.Valid {
		t.Fatalf("blocked administrator state = challenges:%d identities:%d sessions:%d status:%q used:%v", challenges, identities, sessions, status, usedAt.Valid)
	}
}

func TestGoogleInvitationEnrollmentContinuesToExistingMFA(t *testing.T) {
	now := time.Date(2026, time.August, 15, 11, 0, 0, 0, time.UTC)
	manager := newGoogleEnrollmentTestManager(t, now, &deterministicTokenGenerator{
		ids:    []string{"google-challenge", "google-identity", "enrollment-event", "mfa-challenge"},
		tokens: []string{"state-token", "nonce-token", "mfa-token"},
	})
	insertRedemptionUser(t, manager, "pending-admin", UserStatusPending, now)
	if _, err := manager.db.Write().ExecContext(t.Context(), `UPDATE users SET mfa_required = 1 WHERE id = 'pending-admin'`); err != nil {
		t.Fatal(err)
	}
	insertPolicyTestTOTP(t, manager, "pending-admin", now)
	insertRedemptionToken(t, manager, "admin-invite-id", "pending-admin", "admin-invite", EnrollmentTokenPurposeEnrollment, now.Add(time.Hour))

	start, err := manager.BeginGoogleEnrollment(t.Context(), "admin-invite")
	if err != nil {
		t.Fatal(err)
	}
	verifyGoogleEnrollmentClaims(manager, start.Challenge, "admin-google-subject", "admin-google@example.com")
	result, err := manager.CompleteGoogleEnrollment(googleOAuthTestContext(t), start.Challenge.Token, "authorization-code", "Admin Browser")
	if err != nil || result == nil || result.Session != nil || result.PreAuthChallenge == nil {
		t.Fatalf("MFA Google enrollment = %#v, %v", result, err)
	}
	if result.PreAuthChallenge.Purpose != ChallengePurposeMFA || result.PreAuthChallenge.UserID != "pending-admin" || result.PreAuthChallenge.Token != "mfa-token" {
		t.Fatalf("MFA continuation = %#v", result.PreAuthChallenge)
	}
	var status UserStatus
	var sessions, identities, used int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT status FROM users WHERE id = 'pending-admin'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions WHERE user_id = 'pending-admin'`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_identities WHERE user_id = 'pending-admin'`).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT used_at IS NOT NULL FROM user_enrollment_tokens WHERE id = 'admin-invite-id'`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if status != UserStatusActive || sessions != 0 || identities != 1 || used != 1 {
		t.Fatalf("MFA-enrolled state = status:%q sessions:%d identities:%d used:%d", status, sessions, identities, used)
	}
}

func TestGoogleInvitationEnrollmentRechecksInvitationAtCallback(t *testing.T) {
	now := time.Date(2026, time.August, 15, 11, 15, 0, 0, time.UTC)
	manager := newGoogleEnrollmentTestManager(t, now, &deterministicTokenGenerator{
		ids:    []string{"google-challenge", "google-identity", "enrollment-event", "session-id"},
		tokens: []string{"state-token", "nonce-token", "session-token"},
	})
	insertRedemptionUser(t, manager, "invitee", UserStatusPending, now)
	insertRedemptionToken(t, manager, "invitation-id", "invitee", "private-invitation", EnrollmentTokenPurposeEnrollment, now.Add(time.Hour))
	start, err := manager.BeginGoogleEnrollment(t.Context(), "private-invitation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.db.Write().ExecContext(t.Context(), `
		UPDATE user_enrollment_tokens SET revoked_at = ? WHERE id = 'invitation-id'`, now,
	); err != nil {
		t.Fatal(err)
	}
	verifyGoogleEnrollmentClaims(manager, start.Challenge, "google-subject", "invitee-google@example.com")
	result, err := manager.CompleteGoogleEnrollment(googleOAuthTestContext(t), start.Challenge.Token, "authorization-code", "Browser")
	if result != nil || FederatedLoginReason(err) != FederatedLoginFailureChallengeInvalid {
		t.Fatalf("revoked invitation callback = %#v, %v", result, err)
	}
	var status UserStatus
	var identities, sessions, consumed int
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT status FROM users WHERE id = 'invitee'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_identities WHERE user_id = 'invitee'`).Scan(&identities); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions WHERE user_id = 'invitee'`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT consumed_at IS NOT NULL FROM auth_challenges WHERE id = 'google-challenge'`).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if status != UserStatusPending || identities != 0 || sessions != 0 || consumed != 1 {
		t.Fatalf("revoked invitation state = status:%q identities:%d sessions:%d consumed:%d", status, identities, sessions, consumed)
	}
}

func TestGoogleInvitationEnrollmentConflictAndAuditFailureRollBackActivation(t *testing.T) {
	for _, test := range []struct {
		name         string
		prepare      func(*testing.T, *Manager, time.Time)
		wantConflict bool
	}{
		{
			name: "identity already belongs to another user", wantConflict: true,
			prepare: func(t *testing.T, manager *Manager, now time.Time) {
				insertActiveUser(t, manager, "existing-owner", false, now)
				if _, err := manager.db.Write().ExecContext(t.Context(), `
					INSERT INTO auth_identities (
						id, user_id, provider, issuer, subject, email, email_verified, created_at, linked_at
					) VALUES ('existing-identity', 'existing-owner', 'google', ?, 'shared-subject', 'owner@example.com', 1, ?, ?)`,
					googleLoginIssuer, now, now,
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "audit insert fails",
			prepare: func(t *testing.T, manager *Manager, _ time.Time) {
				if _, err := manager.db.Write().ExecContext(t.Context(), `
					CREATE TRIGGER reject_google_enrollment_audit
					BEFORE INSERT ON auth_events
					BEGIN
						SELECT RAISE(ABORT, 'forced audit failure');
					END`); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, time.August, 15, 11, 30, 0, 0, time.UTC)
			manager := newGoogleEnrollmentTestManager(t, now, &deterministicTokenGenerator{
				ids:    []string{"google-challenge", "new-identity", "enrollment-event", "session-id"},
				tokens: []string{"state-token", "nonce-token", "session-token"},
			})
			insertRedemptionUser(t, manager, "invitee", UserStatusPending, now)
			insertRedemptionToken(t, manager, "invitation-id", "invitee", "private-invitation", EnrollmentTokenPurposeEnrollment, now.Add(time.Hour))
			test.prepare(t, manager, now)

			start, err := manager.BeginGoogleEnrollment(t.Context(), "private-invitation")
			if err != nil {
				t.Fatal(err)
			}
			subject := "new-subject"
			if test.wantConflict {
				subject = "shared-subject"
			}
			verifyGoogleEnrollmentClaims(manager, start.Challenge, subject, "invitee-google@example.com")
			result, err := manager.CompleteGoogleEnrollment(googleOAuthTestContext(t), start.Challenge.Token, "authorization-code", "Browser")
			if result != nil || (test.wantConflict && !errors.Is(err, ErrFederatedIdentityConflict)) || (!test.wantConflict && err == nil) {
				t.Fatalf("failed Google enrollment = %#v, %v", result, err)
			}

			var status UserStatus
			var invitationUsed sql.NullTime
			var inviteeIdentities, inviteeSessions int
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT status FROM users WHERE id = 'invitee'`).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT used_at FROM user_enrollment_tokens WHERE id = 'invitation-id'`).Scan(&invitationUsed); err != nil {
				t.Fatal(err)
			}
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM auth_identities WHERE user_id = 'invitee'`).Scan(&inviteeIdentities); err != nil {
				t.Fatal(err)
			}
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sessions WHERE user_id = 'invitee'`).Scan(&inviteeSessions); err != nil {
				t.Fatal(err)
			}
			if status != UserStatusPending || invitationUsed.Valid || inviteeIdentities != 0 || inviteeSessions != 0 {
				t.Fatalf("rolled-back enrollment = status:%q used:%t identities:%d sessions:%d", status, invitationUsed.Valid, inviteeIdentities, inviteeSessions)
			}
			var challengeConsumed int
			if err := manager.db.Read().QueryRowContext(t.Context(), `SELECT consumed_at IS NOT NULL FROM auth_challenges WHERE id = 'google-challenge'`).Scan(&challengeConsumed); err != nil {
				t.Fatal(err)
			}
			if challengeConsumed != 1 {
				t.Fatalf("failed enrollment challenge consumed = %d", challengeConsumed)
			}
			if test.wantConflict {
				var success int
				var reason AuthEventReason
				var metadata string
				if err := manager.db.Read().QueryRowContext(t.Context(), `
					SELECT success, reason, metadata_json FROM auth_events
					WHERE id = 'enrollment-event'`,
				).Scan(&success, &reason, &metadata); err != nil {
					t.Fatal(err)
				}
				if success != 0 || reason != AuthEventReasonInvalidCredentials ||
					!strings.Contains(metadata, `"result":"identity_conflict"`) ||
					strings.Contains(metadata, "shared-subject") || strings.Contains(metadata, "invitee-google@example.com") {
					t.Fatalf("conflict audit = success:%d reason:%q metadata:%q", success, reason, metadata)
				}
			}
		})
	}
}
