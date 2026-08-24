package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/auth"
)

var administratorInvitationTokenPattern = regexp.MustCompile(`Invitation token: ([0-9a-f]{64})`)
var administratorInvitationRotateActionPattern = regexp.MustCompile(`action="(/admin/users/invitations/[A-Za-z0-9_-]{43}/rotate)"`)
var administratorInvitationRevokeActionPattern = regexp.MustCompile(`action="(/admin/users/invitations/[A-Za-z0-9_-]{43}/revoke)"`)

func TestAdminUsersViewDataSummarizesStateRoleAndCurrentUser(t *testing.T) {
	data := adminUsersViewData([]auth.AdministratorUserSummary{
		{ID: "admin", Username: "owner", Status: auth.UserStatusActive, UserType: auth.UserTypeManagement, IsAdmin: true},
		{ID: "pending", Username: "pending", Status: auth.UserStatusPending, UserType: auth.UserTypeWebmail},
		{ID: "disabled", Username: "disabled", Status: auth.UserStatusDisabled, UserType: auth.UserTypeManagement, IsAdmin: true},
	}, "admin", auth.InstanceMFAPolicyAdministrators)
	if data.Total != 3 || data.Active != 1 || data.Pending != 1 || data.Disabled != 1 || data.Administrators != 2 || len(data.Users) != 3 {
		t.Fatalf("adminUsersViewData() = %#v", data)
	}
	if !data.Users[0].Current || data.Users[0].Status != "Active" || data.Users[0].Role != "Management administrator" ||
		data.Users[1].Current || data.Users[1].Status != "Pending" || data.Users[1].Role != "Webmail user" ||
		data.Users[2].Status != "Disabled" || data.Users[2].Role != "Management administrator" {
		t.Fatalf("admin user rows = %#v", data.Users)
	}
	if data.Users[0].MFALabel != "Required" || data.Users[0].MFADetail != "Management policy" || data.Users[0].MFAPolicyPath != "" ||
		data.Users[1].MFALabel != "Optional" || data.Users[1].MFAPolicyPath != "/admin/users/pending/mfa-policy" ||
		data.Users[2].MFALabel != "Required" || data.Users[2].MFAPolicyPath != "" {
		t.Fatalf("admin user MFA policy rows = %#v", data.Users)
	}
}

func TestAdminUsersViewDataReflectsInstanceAndIndividualMFAPolicies(t *testing.T) {
	users := []auth.AdministratorUserSummary{
		{ID: "instance-user", Username: "instance", Status: auth.UserStatusActive, UserType: auth.UserTypeWebmail},
		{ID: "individual-user", Username: "individual", Status: auth.UserStatusActive, UserType: auth.UserTypeWebmail, MFARequired: true},
	}
	data := adminUsersViewData(users, "admin", auth.InstanceMFAPolicyAllUsers)
	if data.Users[0].MFALabel != "Required" || data.Users[0].MFADetail != "Instance policy" || data.Users[0].MFAPolicyPath != "" ||
		data.Users[1].MFALabel != "Required" || data.Users[1].MFADetail != "Individual policy" ||
		data.Users[1].MFAPolicyPath != "/admin/users/individual-user/mfa-policy" {
		t.Fatalf("instance and individual MFA rows = %#v", data.Users)
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
			id, username, username_normalized, name, avatar_url,
			status, auth_version, mfa_required, user_type, is_admin, created_at, updated_at
		) VALUES (
			'pending-user-id', 'pending-user', 'pending-user', 'private-profile-name',
			'private-avatar-url', 'pending', 7, 1, 'webmail', 0, ?, ?
		), (
			'disabled-admin-id', 'disabled-admin', 'disabled-admin', 'Disabled administrator', '',
			'disabled', 3, 1, 'management', 1, ?, ?
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
		`data-admin-navigation-label="Users"`, `aria-current`, "Loading section", "Application users", "Application identity and invitation state",
		currentUser.Username, "You", "pending-user", "disabled-admin",
		"pending-user-id", "disabled-admin-id",
		"Active", "Pending", "Disabled", "Management administrator", "Webmail user",
		"3 users", "Mailboxes, messages, contacts, credentials",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("administrator users page missing %q: %q", want, html)
		}
	}
	for _, forbidden := range []string{
		"private-profile-name", "private-avatar-url",
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

func TestAdministratorCanRequireAndClearIndividualUserMFA(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, username, username_normalized, name, status, auth_version,
			mfa_required, user_type, is_admin, created_at, updated_at
		) VALUES ('policy-target', 'policy-target', 'policy-target', 'Policy Target',
			'active', 1, 0, 'webmail', 0, ?, ?);
		INSERT INTO totp_credentials (
			id, user_id, encrypted_seed, key_version, algorithm, digits,
			period, issuer, enabled, created_at
		) VALUES ('policy-target-totp', 'policy-target', x'01', 1, 'SHA1', 6, 30, 'Gofer', 1, ?)`,
		now, now, now,
	); err != nil {
		t.Fatal(err)
	}
	weak, err := manager.CreateAuthenticatedSession(
		t.Context(), "policy-target", "Weak policy browser",
		auth.AuthenticationMethodPassword, auth.AssuranceLevelSingleFactor,
	)
	if err != nil {
		t.Fatal(err)
	}
	path := adminUserMFAPolicyPath("policy-target")
	pageRequest := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	pageRequest.AddCookie(sessionCookie)
	page := httptest.NewRecorder()
	stack.ServeHTTP(page, pageRequest)
	if page.Code != http.StatusOK {
		t.Fatalf("administrator users page = %d %q", page.Code, page.Body.String())
	}
	for _, want := range []string{"MFA policy", "Require MFA", `action="` + path + `"`, `name="required" value="true"`} {
		if !strings.Contains(page.Body.String(), want) {
			t.Fatalf("administrator user MFA control missing %q: %q", want, page.Body.String())
		}
	}
	withoutCSRF := postSecuritySettings(t, stack, path, url.Values{"required": {"true"}}, sessionCookie)
	if withoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("user MFA policy without CSRF = %d %q", withoutCSRF.Code, withoutCSRF.Body.String())
	}

	requireForm := url.Values{
		"required":             {"true"},
		auth.CSRFFormFieldName: {csrfProofFromForm(t, page.Body.String(), path)},
	}
	required := postSecuritySettings(t, stack, path, requireForm, sessionCookie)
	if required.Code != http.StatusSeeOther || !strings.HasPrefix(required.Header().Get("Location"), "/admin/users?") {
		t.Fatalf("require individual MFA = %d location:%q body:%q", required.Code, required.Header().Get("Location"), required.Body.String())
	}
	var storedRequired int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT mfa_required FROM users WHERE id = 'policy-target'`).Scan(&storedRequired); err != nil || storedRequired != 1 {
		t.Fatalf("required individual MFA = %d, %v", storedRequired, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), weak.Token); err != nil || stored == nil || stored.ID != weak.ID {
		t.Fatalf("weak target session after requiring MFA = %#v, %v", stored, err)
	}
	var events int
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_events
		WHERE event_type = ? AND subject_user_id = 'policy-target'`, auth.AuthEventSecurityPolicyChanged,
	).Scan(&events); err != nil || events != 1 {
		t.Fatalf("individual MFA policy events = %d, %v", events, err)
	}

	clearPageRequest := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	clearPageRequest.AddCookie(sessionCookie)
	clearPage := httptest.NewRecorder()
	stack.ServeHTTP(clearPage, clearPageRequest)
	if clearPage.Code != http.StatusOK || !strings.Contains(clearPage.Body.String(), "Clear individual policy") ||
		!strings.Contains(clearPage.Body.String(), `name="required" value="false"`) {
		t.Fatalf("clear individual MFA control = %d %q", clearPage.Code, clearPage.Body.String())
	}
	clearForm := url.Values{
		"required":             {"false"},
		auth.CSRFFormFieldName: {csrfProofFromForm(t, clearPage.Body.String(), path)},
	}
	cleared := postSecuritySettings(t, stack, path, clearForm, sessionCookie)
	if cleared.Code != http.StatusSeeOther {
		t.Fatalf("clear individual MFA = %d %q", cleared.Code, cleared.Body.String())
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT mfa_required FROM users WHERE id = 'policy-target'`).Scan(&storedRequired); err != nil || storedRequired != 0 {
		t.Fatalf("cleared individual MFA = %d, %v", storedRequired, err)
	}
	if stored, err := manager.GetSessionByToken(t.Context(), weak.Token); err != nil || stored == nil || stored.ID != weak.ID {
		t.Fatalf("preserved session after clearing = %#v, %v", stored, err)
	}
}

func TestAdministratorCanRequireMFAForFactorlessUser(t *testing.T) {
	_, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	now := time.Now().UTC()
	if _, err := db.Write().ExecContext(t.Context(), `
		INSERT INTO users (
			id, username, username_normalized, name, status, auth_version,
			mfa_required, user_type, is_admin, created_at, updated_at
		) VALUES ('factorless-target', 'factorless-target', 'factorless-target', 'Factorless Target',
			'active', 1, 0, 'webmail', 0, ?, ?)`, now, now,
	); err != nil {
		t.Fatal(err)
	}
	path := adminUserMFAPolicyPath("factorless-target")
	pageRequest := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	pageRequest.AddCookie(sessionCookie)
	page := httptest.NewRecorder()
	stack.ServeHTTP(page, pageRequest)
	if page.Code != http.StatusOK {
		t.Fatalf("administrator users page = %d %q", page.Code, page.Body.String())
	}
	form := url.Values{
		"required":             {"true"},
		auth.CSRFFormFieldName: {csrfProofFromForm(t, page.Body.String(), path)},
	}
	response := postSecuritySettings(t, stack, path, form, sessionCookie)
	if response.Code != http.StatusSeeOther || !strings.HasPrefix(response.Header().Get("Location"), "/admin/users?") {
		t.Fatalf("factorless individual MFA = %d %q", response.Code, response.Body.String())
	}
	var required, events int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT mfa_required FROM users WHERE id = 'factorless-target'`).Scan(&required); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM auth_events
		WHERE event_type = ? AND subject_user_id = 'factorless-target'`, auth.AuthEventSecurityPolicyChanged,
	).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if required != 1 || events != 1 {
		t.Fatalf("factorless policy state = required:%d events:%d", required, events)
	}
}

func TestAdministratorCanCreateAndRedeemSingleUseUserInvitation(t *testing.T) {
	_, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)

	pageRequest := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	pageRequest.AddCookie(sessionCookie)
	page := httptest.NewRecorder()
	stack.ServeHTTP(page, pageRequest)
	if page.Code != http.StatusOK {
		t.Fatalf("administrator users page = %d %q", page.Code, page.Body.String())
	}
	for _, want := range []string{
		"Invite user", `action="/admin/users/invitations"`, `name="name"`,
		`name="username"`, "does not create, connect, or authorize a mailbox",
	} {
		if !strings.Contains(page.Body.String(), want) {
			t.Fatalf("administrator invitation form missing %q: %q", want, page.Body.String())
		}
	}

	invitationForm := url.Values{
		"name":     {"Invited Person"},
		"username": {"invited.person"},
	}
	withoutCSRF := postSecuritySettings(t, stack, adminUserInvitationPath, invitationForm, sessionCookie)
	if withoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("invitation without CSRF = %d %q", withoutCSRF.Code, withoutCSRF.Body.String())
	}
	var before int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM users WHERE username_normalized = 'invited.person'`).Scan(&before); err != nil || before != 0 {
		t.Fatalf("user after rejected invitation = %d, %v", before, err)
	}

	invitationForm.Set(auth.CSRFFormFieldName, csrfProofFromForm(t, page.Body.String(), adminUserInvitationPath))
	created := postSecuritySettings(t, stack, adminUserInvitationPath, invitationForm, sessionCookie)
	if created.Code != http.StatusCreated {
		t.Fatalf("created invitation = %d %q", created.Code, created.Body.String())
	}
	if created.Header().Get("Location") != "" || created.Header().Get("Cache-Control") != "no-store" ||
		created.Header().Get("Referrer-Policy") != "no-referrer" || created.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("created invitation headers = %#v", created.Header())
	}
	html := created.Body.String()
	match := administratorInvitationTokenPattern.FindStringSubmatch(html)
	if len(match) != 2 {
		t.Fatalf("created invitation omitted one-time token: %q", html)
	}
	rawToken := match[1]
	for _, want := range []string{
		"Invitation created", "invited.person is pending enrollment",
		"https://gofer.example/account/redeem", "Copy invitation details",
		"token is deliberately not placed in the URL", "2 users",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("created invitation missing %q: %q", want, html)
		}
	}
	if strings.Contains(html, "/account/redeem?token=") || strings.Contains(html, sessionCookie.Value) {
		t.Fatal("created invitation exposed its token in a URL or exposed the session bearer")
	}

	var userID, username, name, status string
	var isAdmin, mfaRequired int
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT id, username, name, status, is_admin, mfa_required
		FROM users WHERE username_normalized = 'invited.person'`,
	).Scan(&userID, &username, &name, &status, &isAdmin, &mfaRequired); err != nil {
		t.Fatal(err)
	}
	if username != "invited.person" || name != "Invited Person" ||
		status != string(auth.UserStatusPending) || isAdmin != 0 || mfaRequired != 0 {
		t.Fatalf("created invited user = id:%q username:%q name:%q status:%q admin:%d mfa:%d",
			userID, username, name, status, isAdmin, mfaRequired)
	}
	var storedHash string
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT token_hash FROM user_enrollment_tokens
		WHERE user_id = ? AND purpose = 'enrollment' AND used_at IS NULL AND revoked_at IS NULL`, userID,
	).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(rawToken))
	if storedHash != hex.EncodeToString(digest[:]) || storedHash == rawToken {
		t.Fatalf("stored invitation hash = %q", storedHash)
	}
	for _, table := range []string{"accounts", "password_credentials", "webauthn_credentials", "totp_credentials", "recovery_codes", "auth_identities", "sessions"} {
		var count int
		if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table+` WHERE user_id = ?`, userID).Scan(&count); err != nil {
			t.Fatalf("count invited user %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("new invitation unexpectedly created %d %s rows", count, table)
		}
	}

	refreshedRequest := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	refreshedRequest.AddCookie(sessionCookie)
	refreshed := httptest.NewRecorder()
	stack.ServeHTTP(refreshed, refreshedRequest)
	if refreshed.Code != http.StatusOK || strings.Contains(refreshed.Body.String(), rawToken) {
		t.Fatalf("refreshed users page retained one-time token = %d %q", refreshed.Code, refreshed.Body.String())
	}

	password := "a reliable invited account passphrase"
	redeem := httptest.NewRequest(http.MethodPost, enrollmentRedemptionPath, strings.NewReader(url.Values{
		"token": {rawToken}, "new_password": {password}, "confirm_password": {password},
	}.Encode()))
	redeem.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	redeemed := httptest.NewRecorder()
	stack.ServeHTTP(redeemed, redeem)
	if redeemed.Code != http.StatusSeeOther || redeemed.Header().Get("Location") != enrollmentRedemptionCompletePath {
		t.Fatalf("redeem created invitation = %d %q body=%q", redeemed.Code, redeemed.Header().Get("Location"), redeemed.Body.String())
	}
	var activeStatus string
	var passwords, usedTokens int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT status FROM users WHERE id = ?`, userID).Scan(&activeStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM password_credentials WHERE user_id = ?`, userID).Scan(&passwords); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens WHERE user_id = ? AND used_at IS NOT NULL`, userID).Scan(&usedTokens); err != nil {
		t.Fatal(err)
	}
	if activeStatus != string(auth.UserStatusActive) || passwords != 1 || usedTokens != 1 {
		t.Fatalf("redeemed invited user = status:%q passwords:%d usedTokens:%d", activeStatus, passwords, usedTokens)
	}
	if replay := postSecuritySettings(t, stack, enrollmentRedemptionPath, url.Values{
		"token": {rawToken}, "new_password": {password}, "confirm_password": {password},
	}); replay.Code != http.StatusBadRequest {
		t.Fatalf("replayed invitation = %d %q", replay.Code, replay.Body.String())
	}
}

func TestAdministratorCanRotateAndRevokeInvitationWithoutExposingInternalTargets(t *testing.T) {
	_, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	pageRequest := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	pageRequest.AddCookie(sessionCookie)
	page := httptest.NewRecorder()
	stack.ServeHTTP(page, pageRequest)
	if page.Code != http.StatusOK {
		t.Fatalf("administrator users page = %d %q", page.Code, page.Body.String())
	}
	createForm := url.Values{
		"name": {"Lifecycle Person"}, "username": {"lifecycle.person"},
		auth.CSRFFormFieldName: {csrfProofFromForm(t, page.Body.String(), adminUserInvitationPath)},
	}
	created := postSecuritySettings(t, stack, adminUserInvitationPath, createForm, sessionCookie)
	if created.Code != http.StatusCreated {
		t.Fatalf("create lifecycle invitation = %d %q", created.Code, created.Body.String())
	}
	originalMatch := administratorInvitationTokenPattern.FindStringSubmatch(created.Body.String())
	rotateMatch := administratorInvitationRotateActionPattern.FindStringSubmatch(created.Body.String())
	revokeMatch := administratorInvitationRevokeActionPattern.FindStringSubmatch(created.Body.String())
	if len(originalMatch) != 2 || len(rotateMatch) != 2 || len(revokeMatch) != 2 {
		t.Fatalf("created invitation lifecycle controls missing: %q", created.Body.String())
	}
	originalToken := originalMatch[1]
	rotatePath := rotateMatch[1]
	var userID, originalTokenID, originalHash string
	if err := db.Read().QueryRowContext(t.Context(), `SELECT id FROM users WHERE username_normalized = 'lifecycle.person'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT id, token_hash FROM user_enrollment_tokens
		WHERE user_id = ? AND used_at IS NULL AND revoked_at IS NULL`, userID,
	).Scan(&originalTokenID, &originalHash); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{userID, originalTokenID, originalHash, originalToken} {
		if strings.Contains(rotatePath, forbidden) || strings.Contains(revokeMatch[1], forbidden) {
			t.Fatalf("invitation action path exposed internal target %q", forbidden)
		}
	}
	withoutCSRF := postSecuritySettings(t, stack, rotatePath, url.Values{}, sessionCookie)
	if withoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("invitation rotation without CSRF = %d %q", withoutCSRF.Code, withoutCSRF.Body.String())
	}
	rotateForm := url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, created.Body.String(), rotatePath)},
	}
	rotated := postSecuritySettings(t, stack, rotatePath, rotateForm, sessionCookie)
	if rotated.Code != http.StatusCreated {
		t.Fatalf("rotate invitation = %d %q", rotated.Code, rotated.Body.String())
	}
	rotatedHTML := rotated.Body.String()
	replacementMatch := administratorInvitationTokenPattern.FindStringSubmatch(rotatedHTML)
	if len(replacementMatch) != 2 || replacementMatch[1] == originalToken ||
		!strings.Contains(rotatedHTML, "Invitation rotated") ||
		!strings.Contains(rotatedHTML, "New invitation ready for lifecycle.person") {
		t.Fatalf("rotated invitation result = %q", rotatedHTML)
	}
	replacementToken := replacementMatch[1]
	var originalRevoked int
	var activeTokens int
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT revoked_at IS NOT NULL FROM user_enrollment_tokens WHERE id = ?`, originalTokenID,
	).Scan(&originalRevoked); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM user_enrollment_tokens
		WHERE user_id = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > CURRENT_TIMESTAMP`, userID,
	).Scan(&activeTokens); err != nil {
		t.Fatal(err)
	}
	if originalRevoked != 1 || activeTokens != 1 {
		t.Fatalf("rotated invitation database state = original revoked:%d active:%d", originalRevoked, activeTokens)
	}

	refreshRequest := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	refreshRequest.AddCookie(sessionCookie)
	refreshed := httptest.NewRecorder()
	stack.ServeHTTP(refreshed, refreshRequest)
	if refreshed.Code != http.StatusOK || strings.Contains(refreshed.Body.String(), replacementToken) {
		t.Fatalf("refreshed invitation page retained replacement token = %d %q", refreshed.Code, refreshed.Body.String())
	}
	revokeMatch = administratorInvitationRevokeActionPattern.FindStringSubmatch(refreshed.Body.String())
	if len(revokeMatch) != 2 {
		t.Fatalf("refreshed invitation page omitted revoke action: %q", refreshed.Body.String())
	}
	revokePath := revokeMatch[1]
	revokeForm := url.Values{
		auth.CSRFFormFieldName: {csrfProofFromForm(t, refreshed.Body.String(), revokePath)},
	}
	revoked := postSecuritySettings(t, stack, revokePath, revokeForm, sessionCookie)
	if revoked.Code != http.StatusSeeOther || revoked.Header().Get("Location") != "/admin/users?notice=Invitation+revoked." ||
		revoked.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("revoke invitation = %d location:%q headers:%#v body:%q",
			revoked.Code, revoked.Header().Get("Location"), revoked.Header(), revoked.Body.String())
	}
	resultRequest := httptest.NewRequest(http.MethodGet, revoked.Header().Get("Location"), nil)
	resultRequest.AddCookie(sessionCookie)
	result := httptest.NewRecorder()
	stack.ServeHTTP(result, resultRequest)
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), "Invitation revoked.") ||
		!strings.Contains(result.Body.String(), "Revoked") || !strings.Contains(result.Body.String(), "Issue invitation") ||
		strings.Contains(result.Body.String(), replacementToken) {
		t.Fatalf("revoked invitation page = %d %q", result.Code, result.Body.String())
	}
	password := "a reliable lifecycle account passphrase"
	for _, token := range []string{originalToken, replacementToken} {
		redeem := httptest.NewRequest(http.MethodPost, enrollmentRedemptionPath, strings.NewReader(url.Values{
			"token": {token}, "new_password": {password}, "confirm_password": {password},
		}.Encode()))
		redeem.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		recorder := httptest.NewRecorder()
		stack.ServeHTTP(recorder, redeem)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("revoked invitation token %q redemption = %d %q", token, recorder.Code, recorder.Body.String())
		}
	}
}

func TestAdministratorUserInvitationRequiresRecentStrongVerification(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	currentSession, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || currentSession == nil {
		t.Fatalf("load administrator session = %#v, %v", currentSession, err)
	}
	if _, err := db.Write().ExecContext(t.Context(), `UPDATE sessions SET step_up_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-11*time.Minute), currentSession.ID); err != nil {
		t.Fatal(err)
	}

	pageRequest := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	pageRequest.AddCookie(sessionCookie)
	page := httptest.NewRecorder()
	stack.ServeHTTP(page, pageRequest)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "Recent administrator verification required") ||
		!strings.Contains(page.Body.String(), `href="/settings/security"`) {
		t.Fatalf("stale administrator users page = %d %q", page.Code, page.Body.String())
	}
	form := url.Values{
		"name": {"Blocked Person"}, "username": {"blocked.person"},
		auth.CSRFFormFieldName: {csrfProofFromForm(t, page.Body.String(), adminUserInvitationPath)},
	}
	blocked := postSecuritySettings(t, stack, adminUserInvitationPath, form, sessionCookie)
	if blocked.Code != http.StatusForbidden || !strings.Contains(blocked.Body.String(), "Verify this administrator session") ||
		!strings.Contains(blocked.Body.String(), "Recent administrator verification required") {
		t.Fatalf("stale administrator invitation = %d %q", blocked.Code, blocked.Body.String())
	}
	var users, tokens int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM users WHERE username_normalized = 'blocked.person'`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens WHERE created_by = ?`, currentSession.UserID).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if users != 0 || tokens != 0 {
		t.Fatalf("stale administrator invitation mutated state = users:%d tokens:%d", users, tokens)
	}
}

func TestAdministratorUserInvitationReturnsSafeFieldErrorsWithoutMutation(t *testing.T) {
	manager, db, stack, sessionCookie, _ := completedSecuritySettingsStack(t)
	currentSession, err := manager.GetSessionByToken(t.Context(), sessionCookie.Value)
	if err != nil || currentSession == nil {
		t.Fatalf("load administrator session = %#v, %v", currentSession, err)
	}
	currentUser, err := manager.GetUserByID(t.Context(), currentSession.UserID)
	if err != nil || currentUser == nil {
		t.Fatalf("load administrator user = %#v, %v", currentUser, err)
	}

	pageRequest := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	pageRequest.AddCookie(sessionCookie)
	page := httptest.NewRecorder()
	stack.ServeHTTP(page, pageRequest)
	if page.Code != http.StatusOK {
		t.Fatalf("administrator users page = %d %q", page.Code, page.Body.String())
	}
	form := url.Values{
		"name": {`<Conflicting Person>`}, "username": {currentUser.Username},
		auth.CSRFFormFieldName: {csrfProofFromForm(t, page.Body.String(), adminUserInvitationPath)},
	}
	rejected := postSecuritySettings(t, stack, adminUserInvitationPath, form, sessionCookie)
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("colliding invitation = %d %q", rejected.Code, rejected.Body.String())
	}
	html := rejected.Body.String()
	for _, want := range []string{
		"Correct the highlighted invitation details.",
		"That username is already used by another Gofer user.",
		`value="&lt;Conflicting Person&gt;"`, `data-tui-dialog-open="true"`, `aria-invalid="true"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("colliding invitation response missing %q: %q", want, html)
		}
	}
	if strings.Contains(html, sessionCookie.Value) {
		t.Fatal("colliding invitation response exposed the session bearer")
	}
	var users, tokens int
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := db.Read().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM user_enrollment_tokens`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if users != 1 || tokens != 0 {
		t.Fatalf("colliding invitation mutated state = users:%d tokens:%d", users, tokens)
	}
}
