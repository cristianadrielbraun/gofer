package auth

import (
	"strings"
	"testing"
	"time"
)

func TestAdministratorUserPageFiltersSortsPagesAndCounts(t *testing.T) {
	now := time.Date(2026, time.November, 10, 15, 0, 0, 0, time.UTC)
	manager := newDeterministicManager(t, &fixedClock{now: now}, secureTokenGenerator{})
	insertActiveUser(t, manager, "head-admin", true, now)
	for _, id := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
		insertActiveUser(t, manager, id, false, now)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := manager.db.Write().ExecContext(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO totp_credentials (id, user_id, encrypted_seed, key_version, algorithm, digits, period, issuer, enabled, created_at)
		VALUES ('head-admin-app', 'head-admin', x'01', 1, 'SHA1', 6, 30, 'Gofer', 1, ?)`, now)
	// alpha must set up 2FA; bravo asked for a reset and signed in last; charlie
	// is disabled and being deleted; delta's invitation expired; echo's is
	// still good. alpha's display name is theirs: the search doesn't read it.
	exec(`UPDATE users SET name = 'Zulu Alpha', mfa_required = 1, created_at = ?, last_login_at = ? WHERE id = 'alpha'`, now.Add(-5*time.Hour), now.Add(-2*time.Hour))
	exec(`UPDATE users SET password_reset_requested_at = ?, created_at = ?, last_login_at = ? WHERE id = 'bravo'`, now, now.Add(-4*time.Hour), now.Add(-time.Hour))
	exec(`INSERT INTO users (id, username, username_normalized, name, status, user_type, is_admin, created_at, updated_at) VALUES ('percent', 'sales-100', 'sales-100', '', 'disabled', 'webmail', 0, ?, ?)`, now.Add(-10*time.Hour), now)
	exec(`UPDATE users SET status = 'disabled', deletion_pending = 1, created_at = ? WHERE id = 'charlie'`, now.Add(-3*time.Hour))
	exec(`UPDATE users SET status = 'pending', created_at = ? WHERE id = 'delta'`, now.Add(-2*time.Hour))
	exec(`UPDATE users SET status = 'pending', created_at = ? WHERE id = 'echo'`, now.Add(-time.Hour))
	exec(`INSERT INTO user_enrollment_tokens (id, user_id, created_by, token_hash, purpose, created_at, expires_at)
		VALUES ('delta-token', 'delta', 'head-admin', 'delta-token-hash', 'enrollment', ?, ?),
		       ('echo-token', 'echo', 'head-admin', 'echo-token-hash', 'enrollment', ?, ?)`,
		now.Add(-48*time.Hour), now.Add(-24*time.Hour), now, now.Add(24*time.Hour))
	session := insertEnrollmentStepUpSession(t, manager, "head-admin", now, now)

	list := func(query AdministratorUserQuery) *AdministratorUserPage {
		t.Helper()
		page, err := manager.ListAdministratorUserPage(t.Context(), "head-admin", session, query)
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	ids := func(page *AdministratorUserPage) string {
		names := make([]string, 0, len(page.Users))
		for _, user := range page.Users {
			names = append(names, user.ID)
		}
		return strings.Join(names, ",")
	}

	everyone := list(AdministratorUserQuery{})
	want := AdministratorUserCounts{Total: 7, Active: 3, Pending: 2, Disabled: 2, Administrators: 1, ResetRequested: 1, MFASetup: 1, InvitationExpired: 1, Deleting: 1}
	if everyone.Counts != want || everyone.Matching != 7 || ids(everyone) != "alpha,bravo,charlie,delta,echo,head-admin,percent" {
		t.Fatalf("everyone = %s, matching %d, counts %+v", ids(everyone), everyone.Matching, everyone.Counts)
	}
	if everyone.Users[0].LastLoginAt == nil || !everyone.Users[0].CreatedAt.Equal(now.Add(-5*time.Hour)) ||
		everyone.Users[3].InvitationState != AdministratorUserInvitationExpired || everyone.Users[3].InvitationActionReference == "" ||
		everyone.Users[4].InvitationState != AdministratorUserInvitationActive {
		t.Fatalf("listed people = %+v", everyone.Users)
	}

	for _, test := range []struct {
		name  string
		query AdministratorUserQuery
		want  string
	}{
		{"not display names", AdministratorUserQuery{Search: "zulu"}, ""},
		{"username", AdministratorUserQuery{Search: " ECH "}, "echo"},
		{"a percent sign is just a percent sign", AdministratorUserQuery{Search: "100%"}, ""},
		{"digits", AdministratorUserQuery{Search: "100"}, "percent"},
		{"an underscore is just an underscore", AdministratorUserQuery{Search: "_"}, ""},
		{"status", AdministratorUserQuery{Status: UserStatusPending}, "delta,echo"},
		{"disabled", AdministratorUserQuery{Status: UserStatusDisabled}, "charlie,percent"},
		{"reset requested", AdministratorUserQuery{Attention: AdministratorUserAttentionResetRequested}, "bravo"},
		{"2FA still to set up", AdministratorUserQuery{Attention: AdministratorUserAttentionMFASetup}, "alpha"},
		{"invitation expired", AdministratorUserQuery{Attention: AdministratorUserAttentionInvitationExpired}, "delta"},
		{"being deleted", AdministratorUserQuery{Attention: AdministratorUserAttentionDeleting}, "charlie"},
		{"both at once", AdministratorUserQuery{Status: UserStatusActive, Attention: AdministratorUserAttentionDeleting}, ""},
		{"newest first", AdministratorUserQuery{Sort: AdministratorUserSortNewest}, "head-admin,echo,delta,charlie,bravo,alpha,percent"},
		{"last sign-in first, never last", AdministratorUserQuery{Sort: AdministratorUserSortLastSignIn}, "bravo,alpha,charlie,delta,echo,head-admin,percent"},
		{"by status", AdministratorUserQuery{Sort: AdministratorUserSortStatus}, "alpha,bravo,head-admin,delta,echo,charlie,percent"},
		{"a page", AdministratorUserQuery{Limit: 2, Offset: 2}, "charlie,delta"},
		{"past the end", AdministratorUserQuery{Limit: 2, Offset: 10}, ""},
	} {
		page := list(test.query)
		if got := ids(page); got != test.want {
			t.Errorf("%s: %s, want %s", test.name, got, test.want)
		}
		if page.Counts != want {
			t.Errorf("%s: the counts followed the filter: %+v", test.name, page.Counts)
		}
	}
	if page := list(AdministratorUserQuery{Status: UserStatusPending, Limit: 1}); page.Matching != 2 || len(page.Users) != 1 {
		t.Fatalf("matching counts every page: %d, %d", page.Matching, len(page.Users))
	}
	if _, err := manager.ListAdministratorUserPage(t.Context(), "head-admin", session, AdministratorUserQuery{Sort: "random"}); err == nil {
		t.Fatal("an unknown order was accepted")
	}
	if _, err := manager.ListAdministratorUserPage(t.Context(), "alpha", "", AdministratorUserQuery{}); err == nil {
		t.Fatal("someone who isn't an administrator listed people")
	}
}
