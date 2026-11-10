package auth

import (
	"fmt"
	"testing"
	"time"
)

// The plan's target: 10,000 people, any filter, under 100 ms a page.
func BenchmarkAdministratorUserPage(b *testing.B) {
	now := time.Date(2026, time.November, 10, 15, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(b, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(b, manager, "head-admin", true, now)
	tx, err := manager.db.Write().BeginTx(b.Context(), nil)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 10000; i++ {
		id := fmt.Sprintf("person-%05d", i)
		status := []string{"active", "active", "active", "pending", "disabled"}[i%5]
		if _, err := tx.ExecContext(b.Context(), `INSERT INTO users (id, username, username_normalized, name, status, mfa_required, user_type, is_admin, created_at, updated_at, last_login_at)
			VALUES (?, ?, ?, ?, ?, ?, 'webmail', 0, ?, ?, ?)`, id, id, id, fmt.Sprintf("Sample Person %d", i), status, i%7 == 0,
			now.Add(-time.Duration(i)*time.Minute), now, now.Add(-time.Duration(i%300)*time.Hour)); err != nil {
			b.Fatal(err)
		}
		if status == "pending" {
			if _, err := tx.ExecContext(b.Context(), `INSERT INTO user_enrollment_tokens (id, user_id, created_by, token_hash, purpose, created_at, expires_at)
				VALUES (?, ?, 'head-admin', ?, 'enrollment', ?, ?)`, id+"-token", id, id+"-hash", now.Add(-48*time.Hour), now.Add(time.Duration(i%3-1)*24*time.Hour)); err != nil {
				b.Fatal(err)
			}
		}
		if i%3 == 0 {
			if _, err := tx.ExecContext(b.Context(), `INSERT INTO totp_credentials (id, user_id, encrypted_seed, key_version, algorithm, digits, period, issuer, enabled, created_at)
				VALUES (?, ?, x'01', 1, 'SHA1', 6, 30, 'Gofer', 1, ?)`, id+"-app", id, now); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	session := insertEnrollmentStepUpSession(b, manager, "head-admin", now, now)
	for _, test := range []struct {
		name  string
		query AdministratorUserQuery
	}{
		{"first page", AdministratorUserQuery{}},
		{"last page", AdministratorUserQuery{Offset: 9950}},
		{"search", AdministratorUserQuery{Search: "person-12"}},
		{"2FA still to set up", AdministratorUserQuery{Attention: AdministratorUserAttentionMFASetup}},
		{"expired invitations by last sign-in", AdministratorUserQuery{Attention: AdministratorUserAttentionInvitationExpired, Sort: AdministratorUserSortLastSignIn}},
	} {
		b.Run(test.name, func(b *testing.B) {
			for b.Loop() {
				if _, err := manager.ListAdministratorUserPage(b.Context(), "head-admin", session, test.query); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
