package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

func TestAdminUsersViewDataSummarizesStateRoleAndCurrentUser(t *testing.T) {
	data := adminUsersViewData([]auth.AdministratorUserSummary{
		{ID: "admin", Username: "owner", Email: "owner@example.com", Status: auth.UserStatusActive, IsAdmin: true},
		{ID: "pending", Email: "pending@example.com", Status: auth.UserStatusPending},
		{ID: "disabled", Username: "disabled", Email: "disabled@example.com", Status: auth.UserStatusDisabled, IsAdmin: true},
	}, "admin")
	if data.Total != 3 || data.Active != 1 || data.Pending != 1 || data.Disabled != 1 || data.Administrators != 2 || len(data.Users) != 3 {
		t.Fatalf("adminUsersViewData() = %#v", data)
	}
	if !data.Users[0].Current || data.Users[0].Status != "Active" || data.Users[0].Role != "Administrator" ||
		data.Users[1].Current || data.Users[1].Status != "Pending" || data.Users[1].Role != "User" ||
		data.Users[2].Status != "Disabled" || data.Users[2].Role != "Administrator" {
		t.Fatalf("admin user rows = %#v", data.Users)
	}
}

func TestAdminUsersPageListsOnlyAuthenticationProfileMetadata(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	currentSession, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || currentSession == nil {
		t.Fatalf("load administrator session = %#v, %v", currentSession, err)
	}
	currentUser, err := manager.GetUserByID(t.Context(), currentSession.UserID)
	if err != nil || currentUser == nil {
		t.Fatalf("load administrator user = %#v, %v", currentUser, err)
	}
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, email, email_normalized, username, username_normalized, name, avatar_url,
			status, auth_version, mfa_required, is_admin, created_at, updated_at
		) VALUES (
			'pending-user-id', 'pending@example.com', 'pending@example.com',
			'<script>pending-user</script>', 'pending-user', 'private-profile-name',
			'private-avatar-url', 'pending', 7, 1, 0, ?, ?
		), (
			'disabled-admin-id', 'disabled@example.com', 'disabled@example.com',
			'disabled-admin', 'disabled-admin', 'Disabled administrator', '',
			'disabled', 3, 1, 1, ?, ?
		);
		INSERT INTO password_credentials (user_id, password_hash)
		VALUES ('pending-user-id', 'private-password-hash');
		INSERT INTO auth_identities (
			id, user_id, provider, issuer, subject, email, email_verified
		) VALUES (
			'private-identity-id', 'pending-user-id', 'google',
			'https://accounts.google.com', 'private-provider-subject',
			'private-provider-email@example.com', 1
		)`, now, now, now, now); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	request.AddCookie(sessionCookie)
	recorder := httptest.NewRecorder()
	stack.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("administrator users page = %d %q", recorder.Code, recorder.Body.String())
	}
	html := recorder.Body.String()
	for _, want := range []string{
		`data-admin-users`, `href="/admin/users"`, `data-admin-navigation-link`, `data-admin-navigation-loading`,
		`data-admin-navigation-label="Users"`, `aria-current`, "Loading section", "Application users", "Read only",
		currentUser.Email, "You", "pending@example.com", "disabled@example.com",
		`&lt;script&gt;pending-user&lt;/script&gt;`, "pending-user-id", "disabled-admin-id",
		"Active", "Pending", "Disabled", "Administrator", "User",
		"3 users", "Mailboxes, messages, contacts, credentials",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("administrator users page missing %q: %q", want, html)
		}
	}
	for _, forbidden := range []string{
		`<script>pending-user</script>`, "private-profile-name", "private-avatar-url",
		"private-password-hash", "private-identity-id", "private-provider-subject",
		"private-provider-email@example.com", sessionCookie.Value,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("administrator users page exposed forbidden value %q", forbidden)
		}
	}
	if recorder.Header().Get("Cache-Control") != "no-store" ||
		recorder.Header().Get("Referrer-Policy") != "no-referrer" ||
		recorder.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("administrator users headers = %#v", recorder.Header())
	}

	partialRequest := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	partialRequest.Header.Set("HX-Request", "true")
	partialRequest.AddCookie(sessionCookie)
	partial := httptest.NewRecorder()
	stack.ServeHTTP(partial, partialRequest)
	if partial.Code != http.StatusOK || !strings.Contains(partial.Body.String(), `id="main-content"`) ||
		!strings.Contains(partial.Body.String(), `data-admin-users`) || strings.Contains(partial.Body.String(), "<!DOCTYPE html>") {
		t.Fatalf("administrator users partial = %d %q", partial.Code, partial.Body.String())
	}
}
