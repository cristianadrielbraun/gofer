package views

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestAdminSecurityActivityPageRendersSanitizedProjectionAndLockState(t *testing.T) {
	var out strings.Builder
	data := AdminSecurityActivityData{
		ManagedMode: true,
		Events: []AdminSecurityActivityEventData{{
			Title: "Sign-in attempt failed", Detail: "The submitted verification was not accepted.",
			OccurredAt: "Aug 31, 2026 at 4:00 PM", Client: "<unsafe-client>", Status: "Failed",
			Actor: "System", Subject: "target-user", Successful: false,
		}},
		TotalEvents: 51, Page: 1, TotalPages: 2, FirstEvent: 1, LastEvent: 50,
		NextPage: 2, HasNext: true, RetentionDays: 180,
		RetentionMinimumDays: 1, RetentionMaximumDays: 365, RetentionCSRFToken: "csrf-proof",
	}
	if err := AdminSecurityActivityPage(data).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		`aria-label="Instance security activity events"`, "Sign-in attempt failed",
		"System", "target-user", `&lt;unsafe-client&gt;`, `href="/admin/activity?page=2"`,
		`data-admin-security-activity-scroll`, `class="min-h-0 flex-1 overflow-y-auto"`,
		`data-admin-security-retention`, "Security activity settings", "180 days",
		`action="/admin/activity/retention"`, `name="_csrf" value="csrf-proof"`,
		`min="1"`, `max="365"`, `value="180"`, "Reducing this period permanently removes audit history",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("administrator security activity view omitted %q", want)
		}
	}
	if strings.Contains(html, "<unsafe-client>") {
		t.Fatal("administrator security activity rendered an unescaped client label")
	}

	out.Reset()
	data.Filter = "sessions"
	if err := AdminSecurityActivityPage(data).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	filteredHTML := out.String()
	for _, want := range []string{
		"51 matching events", `href="/admin/activity?filter=sessions"`,
		`href="/admin/activity?filter=sessions&amp;page=2"`,
	} {
		if !strings.Contains(filteredHTML, want) {
			t.Fatalf("filtered administrator security activity view omitted %q", want)
		}
	}
	if !strings.Contains(filteredHTML, `href="/admin/activity?filter=sessions" data-admin-navigation-link`) ||
		!strings.Contains(filteredHTML, `aria-current="page"`) {
		t.Fatalf("filtered administrator security activity did not mark the selected filter: %q", filteredHTML)
	}

	out.Reset()
	if err := AdminSecurityActivityPage(AdminSecurityActivityData{
		ManagedMode: true, Filter: "recovery",
	}).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No security activity matches this filter.") {
		t.Fatalf("empty filtered administrator security activity view = %q", out.String())
	}

	out.Reset()
	if err := AdminSecurityActivityPage(AdminSecurityActivityData{
		ManagedMode: true, StepUpRequired: true,
	}).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if html := out.String(); !strings.Contains(html, `data-admin-security-activity-locked`) ||
		strings.Contains(html, "Instance events") || strings.Contains(html, `data-admin-security-retention`) {
		t.Fatalf("locked administrator security activity view = %q", html)
	}
}

func TestAdminUsersPageRendersStatesRolesAndEscapesProfileMetadata(t *testing.T) {
	data := AdminUsersData{
		Total: 3, Active: 1, Pending: 1, Disabled: 1, Administrators: 2,
		Users: []AdminUserData{
			{ID: "current-id", Username: "owner", Status: "active", Role: "Administrator", Current: true},
			{ID: "pending-id", Username: `<script>pending</script>`, Status: "pending", Role: "User"},
			{ID: "disabled-id", Username: "disabled", Status: "disabled", Role: "Administrator"},
		},
	}
	var out bytes.Buffer
	if err := AdminUsersPage(data).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminUsersPage.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		`data-admin-users`, "3 users", "Application users", "Application identity and invitation state",
		"owner", "You", "Active", "Pending", "Disabled",
		"Administrator", "User", "current-id", "pending-id", "disabled-id",
		`&lt;script&gt;pending&lt;/script&gt;`,
		"Mailboxes, messages, contacts, credentials",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("administrator users view missing %q: %s", want, html)
		}
	}
	for _, forbidden := range []string{`<script>pending</script>`, "private-password-hash", "private-provider-subject"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("administrator users view exposed forbidden value %q", forbidden)
		}
	}
}

// disabledAttribute finds a disabled attribute, not Tailwind's disabled:
// variants in class names.
var disabledAttribute = regexp.MustCompile(`\sdisabled[\s>]`)

// renderAdminUsersTestPage renders the Users page for data.
func renderAdminUsersTestPage(t *testing.T, data AdminUsersData) string {
	t.Helper()
	var out bytes.Buffer
	if err := AdminUsersPage(data).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminUsersPage.Render() error = %v", err)
	}
	return out.String()
}

// renderAdminUserTestDialog renders one action's dialog for user, as htmx
// receives it when the action is picked.
func renderAdminUserTestDialog(t *testing.T, action AdminUserAction, user AdminUserData, csrf string) string {
	t.Helper()
	var out bytes.Buffer
	if err := AdminUserActionDialog(AdminUserDialog{Action: action, User: user, CSRFToken: csrf}).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminUserActionDialog(%s).Render() error = %v", action, err)
	}
	html := out.String()
	if !strings.Contains(html, `data-tui-dialog-initial-open="true"`) {
		t.Fatalf("%s dialog does not open when it arrives: %s", action, html)
	}
	return html
}

func requireAdminUsersHTML(t *testing.T, what, html string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(html, want) {
			t.Fatalf("%s missing %q: %s", what, want, html)
		}
	}
}

func TestAdminUsersPageMenusFetchEachDialogWhenPicked(t *testing.T) {
	expiresAt := time.Date(2026, time.August, 21, 12, 30, 0, 0, time.Local)
	reference := strings.Repeat("a", 43)
	pending := AdminUserData{
		ID: "pending-internal-id", Username: "pending.user", Status: "pending", Role: "webmail_user",
		InvitationState: "active", InvitationExpiresAt: &expiresAt,
		InvitationRevokePath: "/admin/users/invitations/" + reference + "/revoke",
		InvitationRotatePath: "/admin/users/invitations/" + reference + "/rotate",
	}
	active := AdminUserData{
		ID: "active-user", Username: "active.user", Status: "active", Role: "webmail_user",
		HasPassword: true, MFALabel: "optional", MFADetail: "user_choice",
		MFAPolicyPath: "/admin/users/active-user/mfa-policy", StatusPath: "/admin/users/active-user/status",
		CredentialResetPath: "/admin/users/active-user/credential-reset",
		RequiredChangePath:  "/admin/users/active-user/require-password-change",
	}
	html := renderAdminUsersTestPage(t, AdminUsersData{InvitationCSRFToken: strings.Repeat("a", 64), Users: []AdminUserData{pending, active}})
	requireAdminUsersHTML(t, "users page", html,
		`id="admin-user-dialog"`, `aria-label="Actions for pending.user"`, `aria-label="Actions for active.user"`,
		"Revoke invitation", "Rotate invitation", "Generate password-reset token",
		"Require password change at next login", "Require two-factor", "Expires Aug 21, 12:30",
		`hx-target="#admin-user-dialog"`, `hx-swap="innerHTML"`,
	)
	for _, path := range []string{
		pending.InvitationRevokePath, pending.InvitationRotatePath, active.MFAPolicyPath,
		active.StatusPath, active.CredentialResetPath, active.RequiredChangePath,
	} {
		requireAdminUsersHTML(t, "users page", html, `href="`+path+`"`, `hx-get="`+path+`"`)
		if strings.Contains(html, `action="`+path+`"`) {
			t.Fatalf("users page rendered the form for %s before it was picked", path)
		}
	}
	if count := strings.Count(html, `name="_csrf"`); count != 1 {
		t.Fatalf("users page made %d CSRF proofs; only the invitation form needs one", count)
	}
	if strings.Contains(html, "private-token-hash") {
		t.Fatal("users page exposed a token hash")
	}
}

func TestAdminUserInvitationDialogsCarryTheirOwnProof(t *testing.T) {
	reference := strings.Repeat("a", 43)
	user := AdminUserData{
		ID: "pending-internal-id", Username: "pending.user", Status: "pending", Role: "webmail_user",
		InvitationState:      "active",
		InvitationRevokePath: "/admin/users/invitations/" + reference + "/revoke",
		InvitationRotatePath: "/admin/users/invitations/" + reference + "/rotate",
	}
	revoke := renderAdminUserTestDialog(t, AdminUserActionInvitationRevoke, user, strings.Repeat("b", 64))
	requireAdminUsersHTML(t, "revoke dialog", revoke,
		`action="`+user.InvitationRevokePath+`"`, strings.Repeat("b", 64), "Revoke invitation?", "will stop working immediately",
	)
	rotate := renderAdminUserTestDialog(t, AdminUserActionInvitationRotate, user, strings.Repeat("c", 64))
	requireAdminUsersHTML(t, "rotate dialog", rotate,
		`action="`+user.InvitationRotatePath+`"`, strings.Repeat("c", 64), "Rotate invitation?",
		"Gofer will show the replacement token only once", "Rotate and show token",
	)

	user.InvitationState = "expired"
	if user.Path(AdminUserActionInvitationRevoke) != "" {
		t.Fatal("an expired invitation offered revocation")
	}
	page := renderAdminUsersTestPage(t, AdminUsersData{Users: []AdminUserData{user}})
	if strings.Contains(page, "Revoke invitation") || !strings.Contains(page, "Issue invitation") {
		t.Fatalf("expired invitation menu = %s", page)
	}
	issue := renderAdminUserTestDialog(t, AdminUserActionInvitationRotate, user, strings.Repeat("d", 64))
	requireAdminUsersHTML(t, "issue dialog", issue,
		`action="`+user.InvitationRotatePath+`"`, strings.Repeat("d", 64),
		"Issue a new invitation for pending.user?", "Gofer will show it only once", "Issue and show token",
	)
}

func TestAdminUserMFAPolicyDialogsRequireOrClear(t *testing.T) {
	csrf := strings.Repeat("d", 64)
	optional := AdminUserData{
		ID: "optional-user", Username: "optional.user", Status: "active", Role: "webmail_user",
		MFALabel: "optional", MFADetail: "user_choice", MFAPolicyPath: "/admin/users/optional-user/mfa-policy",
	}
	required := AdminUserData{
		ID: "required-user", Username: "required.user", Status: "active", Role: "webmail_user",
		MFALabel: "required", MFADetail: "individual_policy", MFARequired: true,
		MFAPolicyPath: "/admin/users/required-user/mfa-policy",
	}
	page := renderAdminUsersTestPage(t, AdminUsersData{Users: []AdminUserData{optional, required}})
	requireAdminUsersHTML(t, "users page", page, "Require two-factor", "Change two-factor requirement")
	html := renderAdminUserTestDialog(t, AdminUserActionMFAPolicy, optional, csrf) +
		renderAdminUserTestDialog(t, AdminUserActionMFAPolicy, required, csrf)
	requireAdminUsersHTML(t, "MFA dialogs", html,
		"Require two-factor sign-in for optional.user?", "Two-factor sign-in for required.user", "Remove requirement",
		`action="/admin/users/optional-user/mfa-policy"`, `action="/admin/users/required-user/mfa-policy"`,
		`name="required" value="true"`, `name="required" value="false"`, csrf,
		`name="passkey" value="true"`, `name="passkey" value="false"`,
		"A passkey is enough", "Always ask for the authenticator app code",
		"Choose what counts as the second step", "sets up the authenticator app at their next sign-in",
	)
}

func TestAdminUserStatusDialogsDisableOrEnable(t *testing.T) {
	activeCSRF := strings.Repeat("e", 64)
	disabledCSRF := strings.Repeat("f", 64)
	active := AdminUserData{ID: "active-user", Username: "active.user", Status: "active", Role: "webmail_user", StatusPath: "/admin/users/active-user/status"}
	disabled := AdminUserData{ID: "disabled-user", Username: "disabled.user", Status: "disabled", Role: "webmail_user", StatusPath: "/admin/users/disabled-user/status"}
	page := renderAdminUsersTestPage(t, AdminUsersData{Users: []AdminUserData{active, disabled}})
	requireAdminUsersHTML(t, "users page", page, `href="/admin/users/active-user/status"`, `href="/admin/users/disabled-user/status"`)
	html := renderAdminUserTestDialog(t, AdminUserActionStatus, active, activeCSRF) +
		renderAdminUserTestDialog(t, AdminUserActionStatus, disabled, disabledCSRF)
	requireAdminUsersHTML(t, "status dialogs", html,
		"Disable active.user?", "Enable disabled.user?", "Disable user", "Enable user",
		`action="/admin/users/active-user/status"`, `action="/admin/users/disabled-user/status"`,
		`name="status" value="disabled"`, `name="status" value="active"`, activeCSRF, disabledCSRF,
		"signed out everywhere immediately", "mail, accounts, credentials, and settings remain stored",
		"revoked sessions stay revoked and the user must sign in again",
	)
}

func TestAdminUsersPageAsksForVerificationWhenAChangeIsSent(t *testing.T) {
	user := AdminUserData{
		ID: "active-user", Username: "active.user", Status: "active", Role: "webmail_user",
		StatusPath: "/admin/users/active-user/status", CredentialResetPath: "/admin/users/active-user/credential-reset",
	}
	verification := AdminSecurityVerificationData{
		Required: true, HasTOTP: true, ReturnTo: "/admin/users",
		TOTPPath: "/settings/security/step-up", TOTPCSRFToken: strings.Repeat("c", 64),
	}
	var out bytes.Buffer
	if err := AdminUsersPage(AdminUsersData{StepUpRequired: true, InvitationCSRFToken: strings.Repeat("b", 64), Users: []AdminUserData{user}}, verification).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	requireAdminUsersHTML(t, "stale users page", html,
		`hx-get="/admin/users/active-user/status"`, `id="admin-security-verification-dialog"`,
		`data-tui-dialog-open="false"`, `action="/settings/security/step-up"`,
		// Fetched dialogs don't bring a second verification to this page.
		`hx-headers="{&#34;Gofer-Verification-On-Page&#34;:&#34;true&#34;}"`,
		`action="/admin/users/invitations" class="space-y-4" data-step-up`,
	)
	for _, unwanted := range []string{"Recent administrator verification required", "Verify now"} {
		if strings.Contains(html, unwanted) {
			t.Fatalf("stale users page still waits for verification up front (%q): %s", unwanted, html)
		}
	}
	if disabledAttribute.MatchString(html) {
		t.Fatalf("stale users page disabled a change: %s", html)
	}
	for _, action := range []AdminUserAction{AdminUserActionStatus, AdminUserActionCredentialReset} {
		dialog := renderAdminUserTestDialog(t, action, user, strings.Repeat("d", 64))
		requireAdminUsersHTML(t, string(action)+" dialog", dialog, `action="`+user.Path(action)+`" data-step-up`)
		if strings.Contains(dialog, "admin-security-verification-dialog") {
			t.Fatalf("%s dialog brought verification nobody asked for", action)
		}
	}
	var fetched bytes.Buffer
	if err := AdminUserActionDialog(AdminUserDialog{Action: AdminUserActionStatus, User: user, CSRFToken: strings.Repeat("e", 64)}, verification).Render(context.Background(), &fetched); err != nil {
		t.Fatal(err)
	}
	requireAdminUsersHTML(t, "dialog fetched by a page without verification", fetched.String(),
		`id="admin-user-status-active-user"`, `id="admin-security-verification-dialog"`, `data-admin-security-totp`,
	)
}

func TestAdminUsersPageOpensTheDialogItWasAskedFor(t *testing.T) {
	user := AdminUserData{ID: "active-user", Username: "active.user", Status: "active", Role: "webmail_user", StatusPath: "/admin/users/active-user/status"}
	page := renderAdminUsersTestPage(t, AdminUsersData{
		Users:  []AdminUserData{user},
		Dialog: &AdminUserDialog{Action: AdminUserActionStatus, User: user, CSRFToken: strings.Repeat("s", 64)},
	})
	slot := strings.Index(page, `id="admin-user-dialog"`)
	dialog := strings.Index(page, `id="admin-user-status-active-user"`)
	if slot < 0 || dialog < slot {
		t.Fatalf("asked-for dialog is not in the dialog slot: %s", page)
	}
	requireAdminUsersHTML(t, "page with dialog", page, `data-tui-dialog-initial-open="true"`, strings.Repeat("s", 64), `action="/admin/users/active-user/status"`)
}

func TestAdminUsersPageRendersProtectedCredentialResetActionAndOneTimeResult(t *testing.T) {
	csrfToken := strings.Repeat("c", 64)
	expiresAt := time.Date(2026, time.August, 29, 14, 30, 0, 0, time.Local)
	user := AdminUserData{
		ID: "webmail-user", Username: "webmail.user", Status: "active", Role: "webmail_user",
		CredentialResetPath: "/admin/users/webmail-user/credential-reset",
	}
	html := renderAdminUsersTestPage(t, AdminUsersData{
		Users: []AdminUserData{user},
		CredentialReset: &AdminUserCredentialResetData{
			Username: "webmail.user", RedemptionURL: "https://gofer.example/account/redeem",
			Token: "private-reset-token", ExpiresAt: expiresAt,
		},
	})
	requireAdminUsersHTML(t, "credential-reset result", html,
		"Generate password-reset token", "Password-reset token created", "Reset token ready for webmail.user",
		"Gofer password reset for webmail.user", "https://gofer.example/account/redeem",
		"Reset token: private-reset-token", "Gofer stores only a hash of the token",
		"Successful redemption signs the user out everywhere, keeps their authenticator app (TOTP), and does not enable a disabled account",
		"I saved the reset token",
	)
	if strings.Contains(html, `https://gofer.example/account/redeem?token=`) {
		t.Fatalf("administrator credential-reset result put token in URL: %s", html)
	}
	dialog := renderAdminUserTestDialog(t, AdminUserActionCredentialReset, user, csrfToken)
	requireAdminUsersHTML(t, "credential-reset dialog", dialog,
		"Generate a password-reset token for webmail.user?", `action="/admin/users/webmail-user/credential-reset"`, csrfToken,
		"does not change the password or sign the user out yet", "Generate reset token",
	)
}

func TestAdminUsersPageHighlightsRequestedPasswordReset(t *testing.T) {
	requestedAt := time.Date(2026, time.August, 29, 14, 15, 0, 0, time.Local)
	user := AdminUserData{
		ID: "requested-user", Username: "requested.user", Status: "disabled", Role: "webmail_user",
		CredentialResetPath: "/admin/users/requested-user/credential-reset", PasswordResetRequestedAt: &requestedAt,
	}
	page := renderAdminUsersTestPage(t, AdminUsersData{Users: []AdminUserData{user}})
	requireAdminUsersHTML(t, "requested password reset", page, "Password reset requested", "Requested Aug 29, 2:15 PM", "Generate password-reset token")
	dialog := renderAdminUserTestDialog(t, AdminUserActionCredentialReset, user, strings.Repeat("r", 64))
	requireAdminUsersHTML(t, "requested password reset dialog", dialog, "Issue a password-reset token for requested.user?")
}

func TestAdminUsersPageShowsWhoStillHasToSetUpTwoFactor(t *testing.T) {
	waiting := AdminUserData{ID: "waiting-user", Username: "waiting.user", Status: "active", Role: "webmail_user", MFALabel: "required", MFADetail: "individual_policy", MFASetupPending: true}
	ready := AdminUserData{ID: "ready-user", Username: "ready.user", Status: "active", Role: "webmail_user", MFALabel: "required", MFADetail: "individual_policy"}
	page := renderAdminUsersTestPage(t, AdminUsersData{Users: []AdminUserData{waiting, ready}})
	if strings.Count(page, "data-admin-user-mfa-setup-pending") != 1 || !strings.Contains(page, "Not set up yet") {
		t.Fatalf("the 2FA column doesn't single out who still has to set it up: %q", page)
	}
}

func TestAdminUsersPageOffersPasswordChangeOnlyWhileNotRequired(t *testing.T) {
	user := AdminUserData{
		ID: "forced-user", Username: "forced.user", Status: "active", Role: "webmail_user",
		HasPassword: true, PasswordChangeRequired: true,
		CredentialResetPath: "/admin/users/forced-user/credential-reset",
		RequiredChangePath:  "/admin/users/forced-user/require-password-change",
	}
	page := renderAdminUsersTestPage(t, AdminUsersData{Users: []AdminUserData{user}})
	requireAdminUsersHTML(t, "users page", page, "Password change required", "Require password change at next login")
	if strings.Contains(page, `href="/admin/users/forced-user/require-password-change"`) {
		t.Fatal("a password change that is already required was offered again")
	}
	dialog := renderAdminUserTestDialog(t, AdminUserActionRequiredChange, user, strings.Repeat("p", 64))
	requireAdminUsersHTML(t, "required change dialog", dialog,
		"Require a password change for forced.user?", `action="/admin/users/forced-user/require-password-change"`, strings.Repeat("p", 64),
	)
}

func TestAdminUserDeletionDialogAsksForTheUsername(t *testing.T) {
	user := AdminUserData{
		ID: "gone-user", Username: "gone.user", Status: "disabled", Role: "webmail_user",
		StatusPath: "/admin/users/gone-user/status", DeletionPath: "/admin/users/gone-user/delete",
	}
	page := renderAdminUsersTestPage(t, AdminUsersData{Users: []AdminUserData{user}})
	requireAdminUsersHTML(t, "users page", page, "Delete permanently", `role="separator"`, `href="/admin/users/gone-user/delete"`)
	dialog := renderAdminUserTestDialog(t, AdminUserActionDeletion, user, strings.Repeat("g", 64))
	requireAdminUsersHTML(t, "deletion dialog", dialog,
		"Permanently delete gone.user?", `name="confirmation"`, `action="/admin/users/gone-user/delete"`,
		strings.Repeat("g", 64), "Delete user and local data",
	)
}

func TestAdminUsersPageRendersProtectedInvitationFormAndOneTimeResult(t *testing.T) {
	expiresAt := time.Date(2026, time.August, 21, 12, 30, 0, 0, time.Local)
	data := AdminUsersData{
		InvitationCSRFToken: strings.Repeat("a", 64),
		InvitationForm: AdminUserInvitationFormData{
			Name: `<Admin & helper>`, Username: "invalid username",
			FieldErrors: map[string]string{
				"username": `Username <already> exists`,
			},
		},
		Invitation: &AdminUserInvitationData{
			Name: "Invited Person", Username: "invited.person",
			RedemptionURL: "https://gofer.example/account/enroll",
			Token:         `private-token</textarea><script>alert("token")</script>`,
			ExpiresAt:     expiresAt,
		},
	}
	var out bytes.Buffer
	if err := AdminUsersPage(data).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminUsersPage.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		`data-tui-dialog-target="admin-user-invitation-dialog"`, "Invite user",
		`action="/admin/users/invitations"`, `name="_csrf"`, strings.Repeat("a", 64),
		`name="name"`, `name="username"`, `aria-invalid="true"`,
		`&lt;Admin &amp; helper&gt;`, `Username &lt;already&gt; exists`,
		"does not create, connect, or authorize a mailbox", "Invitation created",
		`data-tui-dialog-disable-click-away="true"`, `data-tui-dialog-disable-esc="true"`,
		"Gofer stores only a hash of the token", "https://gofer.example/account/enroll",
		"Copy invitation details", "deliberately not placed in the URL",
		`private-token&lt;/textarea&gt;&lt;script&gt;alert(&#34;token&#34;)&lt;/script&gt;`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("administrator invitation view missing %q: %s", want, html)
		}
	}
	for _, forbidden := range []string{
		`<script>alert("token")</script>`,
		`https://gofer.example/account/enroll?token=`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("administrator invitation view exposed forbidden value %q", forbidden)
		}
	}
}

func TestAdminUsersPageKeepsTheInvitationOpenToAStaleSession(t *testing.T) {
	data := AdminUsersData{StepUpRequired: true, InvitationCSRFToken: strings.Repeat("b", 64)}
	verification := AdminSecurityVerificationData{
		Required:      true,
		HasTOTP:       true,
		ReturnTo:      "/admin/users",
		TOTPPath:      "/settings/security/step-up",
		TOTPCSRFToken: strings.Repeat("c", 64),
	}
	var out bytes.Buffer
	if err := AdminUsersPage(data, verification).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminUsersPage.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		`data-admin-security-verification`, `data-tui-dialog-show-modal="false"`, `id="admin-security-totp-code"`,
		`autocomplete="one-time-code"`, `aria-label="Authenticator app code"`,
		`action="/settings/security/step-up"`, `name="return_to" value="/admin/users"`,
		`data-tui-dialog-target="admin-user-invitation-dialog"`, `data-step-up`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("stale administrator invitation view missing %q: %s", want, html)
		}
	}
	if disabledAttribute.MatchString(html) {
		t.Fatal("stale administrator invitation view disabled the invitation")
	}
	if strings.Contains(html, `href="/admin/account"`) {
		t.Fatal("stale administrator invitation view still sends verification to account security")
	}
}

func TestAdminLabelsPageRendersOutlookGraphDiagnostics(t *testing.T) {
	status := models.LabelAdminStatus{
		Accounts: []models.LabelAccountSyncStatus{{
			AccountID:       "acc_outlook",
			OwnerUsername:   "mail-user",
			AccountName:     "Outlook",
			AccountEmail:    "user@example.com",
			AccountProvider: "outlook",
			LabelProvider:   "outlook_category",
			TotalMessages:   3,
			OutlookGraph: &models.OutlookGraphDiagnostics{
				GraphBackedMessages:              1,
				IMAPBackedMessages:               2,
				MessageParityDelta:               -1,
				MessagesMissingGraphID:           2,
				MissingGraphIDWithInternetID:     1,
				MissingGraphIDWithoutInternetID:  1,
				MissingGraphIDWithoutGraphFolder: 1,
				LocalFolders:                     2,
				GraphBackedFolders:               1,
				FoldersMissingGraphID:            1,
			},
		}},
	}

	var out bytes.Buffer
	if err := AdminLabelsPage(status).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminLabelsPage.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{"selected webmail scope", "Owned by mail-user", "Outlook Graph parity", "Graph IDs", "IMAP rows", "Parity delta", "Needs repair", "Backfillable", "No Graph folder"} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered admin labels page missing %q: %s", want, html)
		}
	}
}

func TestAdminContactsPageExplainsInstanceScopeAndOwners(t *testing.T) {
	status := models.ContactAdminStatus{
		Synced: 7,
		AccountSync: []models.ContactSyncStatus{{
			AccountID: "mail-account", OwnerUsername: "mail-user", AccountName: "Mail", AccountEmail: "mail@example.com", Provider: "gmail",
		}},
		RecentEvents: []models.ContactActivityEvent{{
			Type: "observed_contact_added", Username: "mail-user", Email: "sender@example.com", Message: "Observed contact added",
		}},
	}

	var out bytes.Buffer
	if err := AdminContactsPage(status).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminContactsPage.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		"selected webmail scope", "Provider-synced contacts", "Force instance backfill", "Owned by mail-user", "User / contact", "sender@example.com",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered admin contacts page missing %q: %s", want, html)
		}
	}
}

func TestAdminSecurityPageShowsExceptionsAndWarnings(t *testing.T) {
	data := models.MailSecurityAdminData{Exceptions: []models.MailSecurityException{
		{ID: "http", Kind: models.MailSecurityExceptionHTTPDiscovery, Host: "lab.example.test", CreatedBy: "admin"},
		{ID: "imap", Kind: models.MailSecurityExceptionPlaintextTransport, Protocol: "imap", Host: "mail.test", Port: 1143, CreatedBy: "admin", Accounts: []models.MailSecurityExceptionAccount{{ID: "account", Email: "user@example.com"}}},
		{ID: "private", Kind: models.MailSecurityExceptionPrivateTarget, Protocol: "http", Host: "127.0.0.1", Port: 8080, CreatedBy: "admin"},
	}}

	var out bytes.Buffer
	if err := AdminSecurityPage(data).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminSecurityPage.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{"Mail security", "lab.example.test", "IMAP mail.test:1143", "HTTP 127.0.0.1:8080", "user@example.com", "OAuth tokens are never allowed", "Only approve endpoints you control"} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered admin security page missing %q: %s", want, html)
		}
	}
}

func TestAdminSecurityPageAsksForVerificationWhenAChangeIsSent(t *testing.T) {
	data := models.MailSecurityAdminData{
		StepUpRequired: true, VerifiesOnSubmit: true,
		Exceptions: []models.MailSecurityException{{
			ID: "private", Kind: models.MailSecurityExceptionPrivateTarget,
			Protocol: "http", Host: "127.0.0.1", Port: 8080,
		}},
	}
	verification := AdminSecurityVerificationData{
		Required:               true,
		HasPasskey:             true,
		ReturnTo:               "/admin/security",
		PasskeyStartPath:       "/settings/security/passkeys/step-up/start",
		PasskeyFinishPath:      "/settings/security/passkeys/step-up/finish",
		PasskeyStartCSRFToken:  strings.Repeat("d", 64),
		PasskeyFinishCSRFToken: strings.Repeat("e", 64),
	}

	var out bytes.Buffer
	if err := AdminSecurityPage(data, verification).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminSecurityPage.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		`id="admin-security-verification-dialog"`,
		`data-passkey-authentication`,
		`data-success-redirect="/admin/security"`,
		`data-start-csrf="` + strings.Repeat("d", 64) + `"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("step-up-required admin security page missing %q: %s", want, html)
		}
	}
	if strings.Contains(html, `role="alert"`) || strings.Contains(html, "Recent administrator verification required") {
		t.Fatal("admin security page still asks for verification up front")
	}
	if got := strings.Count(html, ` disabled>`); got != 0 {
		t.Fatalf("admin security page disabled %d changes; it asks when one is sent", got)
	}
	// The exception forms and the proxy form each ask for verification.
	if got := strings.Count(html, `method="post" data-step-up`); got < 4 {
		t.Fatalf("admin security page marked %d changes as needing verification", got)
	}
	if strings.Contains(html, `href="/settings/security"`) {
		t.Fatal("stale mail-security view still sends verification to webmail settings")
	}

	// The webmail's own settings page has no such script: its changes wait,
	// disabled, until the owner verifies.
	data.VerifiesOnSubmit = false
	out.Reset()
	if err := MailSecuritySettingsCard(data).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), ` disabled>`); got == 0 {
		t.Fatal("webmail mail security settings enabled changes before verification")
	}
}

func TestAdminMailOperationsPageRendersSMTPBaseline(t *testing.T) {
	status := models.MailOperationsAdminStatus{}
	status.Health.SMTPProfile = models.MailSMTPAdminProfile{
		Samples: 2, Successes: 1, Failures: 1, Connections: 2, Messages: 2,
		AvgConnectAuthMs: 1250, AvgDataMs: 240, AvgTotalMs: 1490, AvgQueueWaitMs: 3200,
	}

	var out bytes.Buffer
	if err := AdminMailOperationsPage(status).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminMailOperationsPage.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{"Queue health", "SMTP baseline", "Process lifetime", "Avg connect + auth", "1.2 s", "240 ms"} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered admin operations page missing %q: %s", want, html)
		}
	}
}

func TestAdminOperationalPagesRenderSelectedWebmailScope(t *testing.T) {
	scope := models.AdminWebmailScope{
		SelectedUserID:   "mail-user",
		SelectedUsername: "mail.user",
		Users: []models.AdminWebmailUserOption{
			{ID: "mail-user", Username: "mail.user", Status: "active"},
			{ID: "disabled-user", Username: "disabled.user", Status: "disabled"},
		},
	}

	pages := []struct {
		name   string
		render func(*bytes.Buffer) error
	}{
		{name: "avatars", render: func(out *bytes.Buffer) error {
			return AdminPage(models.AvatarStatus{Scope: scope}, "senders").Render(context.Background(), out)
		}},
		{name: "contacts", render: func(out *bytes.Buffer) error {
			return AdminContactsPage(models.ContactAdminStatus{Scope: scope}).Render(context.Background(), out)
		}},
		{name: "labels", render: func(out *bytes.Buffer) error {
			return AdminLabelsPage(models.LabelAdminStatus{Scope: scope}).Render(context.Background(), out)
		}},
		{name: "operations", render: func(out *bytes.Buffer) error {
			return AdminMailOperationsPage(models.MailOperationsAdminStatus{Scope: scope}).Render(context.Background(), out)
		}},
	}
	for _, page := range pages {
		t.Run(page.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := page.render(&out); err != nil {
				t.Fatalf("render error = %v", err)
			}
			html := out.String()
			for _, want := range []string{`name="user_id"`, `data-admin-webmail-scope`, "mail.user", "disabled.user", "Metrics include every mailbox account owned by mail.user."} {
				if !strings.Contains(html, want) {
					t.Fatalf("rendered page missing %q: %s", want, html)
				}
			}
		})
	}

	var operations bytes.Buffer
	if err := AdminMailOperationsPage(models.MailOperationsAdminStatus{Scope: scope}).Render(context.Background(), &operations); err != nil {
		t.Fatalf("render selected operations: %v", err)
	}
	if strings.Contains(operations.String(), "Retention cleanup") || strings.Contains(operations.String(), "SMTP baseline") {
		t.Fatalf("selected-user operations page rendered process-wide metrics: %s", operations.String())
	}
}

func TestAdminSecurityRetentionDialogVisibility(t *testing.T) {
	// Results show as toasts; the dialog reopens only to correct a refused value.
	for _, test := range []struct {
		name string
		open bool
	}{
		{name: "closed by default"},
		{name: "reopened after a refused value", open: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out strings.Builder
			data := AdminSecurityActivityData{ManagedMode: true, RetentionDays: 180, RetentionMinimumDays: 1, RetentionMaximumDays: 365, RetentionOpen: test.open}
			if err := AdminSecurityActivityPage(data).Render(context.Background(), &out); err != nil {
				t.Fatal(err)
			}
			html := out.String()
			if !strings.Contains(html, `data-tui-dialog-target="admin-security-retention-dialog"`) {
				t.Fatal("settings trigger is missing")
			}
			wantOpen := `data-tui-dialog-initial-open="false"`
			if test.open {
				wantOpen = `data-tui-dialog-initial-open="true"`
			}
			if !strings.Contains(html, wantOpen) {
				t.Fatalf("missing dialog state %s", wantOpen)
			}
			start := strings.Index(html, "<dialog")
			end := strings.Index(html, "</dialog>")
			if start < 0 || end < start {
				t.Fatal("retention dialog is missing")
			}
			if !strings.Contains(html[start:end], `action="/admin/activity/retention"`) {
				t.Fatal("dialog omitted the retention form")
			}
			if strings.Contains(html[:start]+html[end:], `action="/admin/activity/retention"`) || strings.Contains(html, `onsubmit="return confirm`) {
				t.Fatal("retention form is not confined to the dialog")
			}
		})
	}
}

func TestAdminUsersQueryAddresses(t *testing.T) {
	for _, test := range []struct {
		query AdminUsersQuery
		page  int
		want  string
	}{
		{AdminUsersQuery{}, 1, "/admin/users"},
		{AdminUsersQuery{}, 3, "/admin/users?page=3"},
		{AdminUsersQuery{Search: "a b&c", Status: "pending", Attention: "mfa_setup", Sort: "newest"}, 2, "/admin/users?attention=mfa_setup&page=2&q=a+b%26c&sort=newest&status=pending"},
	} {
		if got := test.query.Path(test.page); got != test.want {
			t.Errorf("Path(%d) of %+v = %q, want %q", test.page, test.query, got, test.want)
		}
	}
	if (AdminUsersQuery{Sort: "newest", Page: 2}).Narrowed() || !(AdminUsersQuery{Status: "active"}).Narrowed() {
		t.Fatal("Narrowed counts order or paging as narrowing")
	}
	if got := adminUserActionFromList("/admin/users/sample/status", "/admin/users?q=x"); got != "/admin/users/sample/status?list=%2Fadmin%2Fusers%3Fq%3Dx" {
		t.Fatalf("action from a list = %q", got)
	}
	if got := adminUserActionFromList("/admin/users/sample/status", "/admin/users"); got != "/admin/users/sample/status" {
		t.Fatalf("action from the whole list = %q", got)
	}
}
