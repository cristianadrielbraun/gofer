package views

import (
	"bytes"
	"context"
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
		Notice: "Retention saved.", Error: "Example error.",
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
		"Retention saved.", "Example error.",
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
			{ID: "current-id", Username: "owner", Status: "Active", Role: "Administrator", Current: true},
			{ID: "pending-id", Username: `<script>pending</script>`, Status: "Pending", Role: "User"},
			{ID: "disabled-id", Username: "disabled", Status: "Disabled", Role: "Administrator"},
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

func TestAdminUsersPageRendersProtectedInvitationLifecycleActions(t *testing.T) {
	expiresAt := time.Date(2026, time.August, 21, 12, 30, 0, 0, time.Local)
	reference := strings.Repeat("a", 43)
	rotatePath := "/admin/users/invitations/" + reference + "/rotate"
	revokePath := "/admin/users/invitations/" + reference + "/revoke"
	data := AdminUsersData{Users: []AdminUserData{{
		ID: "pending-internal-id", Username: "pending.user",
		Status: "Pending", Role: "Webmail user", InvitationState: "active",
		InvitationExpiresAt:  &expiresAt,
		InvitationRevokePath: revokePath, InvitationRevokeCSRFToken: strings.Repeat("b", 64),
		InvitationRotatePath: rotatePath, InvitationRotateCSRFToken: strings.Repeat("c", 64),
	}}}
	var out bytes.Buffer
	if err := AdminUsersPage(data).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminUsersPage.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		"Invitation", "Active", "Expires Aug 21, 12:30", "Revoke", "Rotate",
		`action="` + revokePath + `"`, `action="` + rotatePath + `"`,
		strings.Repeat("b", 64), strings.Repeat("c", 64),
		"will stop working immediately", "Gofer will show the replacement token only once",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("administrator invitation lifecycle view missing %q: %s", want, html)
		}
	}
	if strings.Contains(html, "private-token-hash") {
		t.Fatal("administrator invitation lifecycle view exposed a token hash")
	}
}

func TestAdminUsersPageRendersProtectedIndividualMFAPolicyActions(t *testing.T) {
	csrf := strings.Repeat("d", 64)
	data := AdminUsersData{Users: []AdminUserData{
		{
			ID: "optional-user", Username: "optional.user", Status: "Active", Role: "Webmail user",
			MFALabel: "Optional", MFADetail: "User choice",
			MFAPolicyPath: "/admin/users/optional-user/mfa-policy", MFAPolicyCSRFToken: csrf,
		},
		{
			ID: "required-user", Username: "required.user", Status: "Active", Role: "Webmail user",
			MFALabel: "Required", MFADetail: "Individual policy", MFARequired: true,
			MFAPolicyPath: "/admin/users/required-user/mfa-policy", MFAPolicyCSRFToken: csrf,
		},
	}}
	var out bytes.Buffer
	if err := AdminUsersPage(data).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminUsersPage.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		"MFA policy", "Require MFA for optional.user?", "Clear the individual MFA requirement?",
		`action="/admin/users/optional-user/mfa-policy"`, `action="/admin/users/required-user/mfa-policy"`,
		`name="required" value="true"`, `name="required" value="false"`, csrf,
		"Existing sessions and authenticators are unchanged", "require enrollment after their next primary sign-in",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("administrator individual MFA view missing %q: %s", want, html)
		}
	}
	for _, forbidden := range []string{"totp-secret", "passkey-credential", "recovery-code"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("administrator individual MFA view exposed forbidden value %q", forbidden)
		}
	}
}

func TestAdminUsersPageRendersProtectedUserStatusActions(t *testing.T) {
	activeCSRF := strings.Repeat("e", 64)
	disabledCSRF := strings.Repeat("f", 64)
	data := AdminUsersData{Users: []AdminUserData{
		{
			ID: "active-user", Username: "active.user", Status: "Active", Role: "Webmail user",
			StatusPath: "/admin/users/active-user/status", StatusCSRFToken: activeCSRF,
		},
		{
			ID: "disabled-user", Username: "disabled.user", Status: "Disabled", Role: "Webmail user",
			StatusPath: "/admin/users/disabled-user/status", StatusCSRFToken: disabledCSRF,
		},
	}}
	var out bytes.Buffer
	if err := AdminUsersPage(data).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminUsersPage.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		"Disable active.user?", "Enable disabled.user?", "Disable user", "Enable user",
		`action="/admin/users/active-user/status"`, `action="/admin/users/disabled-user/status"`,
		`name="status" value="disabled"`, `name="status" value="active"`, activeCSRF, disabledCSRF,
		"signed out everywhere immediately", "mail, accounts, credentials, and settings remain stored",
		"revoked sessions stay revoked and the user must sign in again",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("administrator user status view missing %q: %s", want, html)
		}
	}
}

func TestAdminUsersPageDisablesUserStatusActionsUntilRecentVerification(t *testing.T) {
	data := AdminUsersData{StepUpRequired: true, Users: []AdminUserData{{
		ID: "active-user", Username: "active.user", Status: "Active", Role: "Webmail user",
		StatusPath: "/admin/users/active-user/status", StatusCSRFToken: strings.Repeat("a", 64),
	}}}
	var out bytes.Buffer
	if err := AdminUsersPage(data).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminUsersPage.Render() error = %v", err)
	}
	html := out.String()
	trigger := `data-tui-dialog-target="admin-user-status-active-user"`
	index := strings.Index(html, trigger)
	if index < 0 {
		t.Fatalf("administrator user status trigger missing: %s", html)
	}
	end := index + 1200
	if end > len(html) {
		end = len(html)
	}
	if !strings.Contains(html[index:end], " disabled") {
		t.Fatalf("stale administrator status action was enabled: %s", html[index:end])
	}
}

func TestAdminUsersPageRendersProtectedCredentialResetActionAndOneTimeResult(t *testing.T) {
	csrfToken := strings.Repeat("c", 64)
	expiresAt := time.Date(2026, time.August, 29, 14, 30, 0, 0, time.Local)
	data := AdminUsersData{
		Users: []AdminUserData{{
			ID: "webmail-user", Username: "webmail.user", Status: "Active", Role: "Webmail user",
			CredentialResetPath: "/admin/users/webmail-user/credential-reset", CredentialResetCSRFToken: csrfToken,
		}},
		CredentialReset: &AdminUserCredentialResetData{
			Username: "webmail.user", RedemptionURL: "https://gofer.example/account/redeem",
			Token: "private-reset-token", ExpiresAt: expiresAt,
		},
	}
	var out bytes.Buffer
	if err := AdminUsersPage(data).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminUsersPage.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		"Reset password", "Generate a password-reset token for webmail.user?",
		`action="/admin/users/webmail-user/credential-reset"`, csrfToken,
		"does not change the password or sign the user out yet", "Generate reset token",
		"Password-reset token created", "Reset token ready for webmail.user",
		"Gofer password reset for webmail.user", "https://gofer.example/account/redeem",
		"Reset token: private-reset-token", "Gofer stores only a hash of the token",
		"Successful redemption signs the user out everywhere, preserves MFA, and does not enable a disabled account",
		"I saved the reset token",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("administrator credential-reset view missing %q: %s", want, html)
		}
	}
	if strings.Contains(html, `https://gofer.example/account/redeem?token=`) {
		t.Fatalf("administrator credential-reset result put token in URL: %s", html)
	}
}

func TestAdminUsersPageHighlightsRequestedPasswordReset(t *testing.T) {
	requestedAt := time.Date(2026, time.August, 29, 14, 15, 0, 0, time.Local)
	data := AdminUsersData{Users: []AdminUserData{{
		ID: "requested-user", Username: "requested.user", Status: "Disabled", Role: "Webmail user",
		CredentialResetPath:      "/admin/users/requested-user/credential-reset",
		CredentialResetCSRFToken: strings.Repeat("r", 64), PasswordResetRequestedAt: &requestedAt,
	}}}
	var out bytes.Buffer
	if err := AdminUsersPage(data).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	for _, want := range []string{
		"Password reset requested", "Requested Aug 29, 14:15", "Issue reset token",
		"Issue a password-reset token for requested.user?",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("requested password reset view missing %q: %s", want, html)
		}
	}
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

func TestAdminUsersPageDisablesInvitationUntilRecentVerification(t *testing.T) {
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
		"Recent administrator verification required", `data-admin-security-verification`,
		`data-tui-dialog-target="admin-security-verification-dialog"`,
		`action="/settings/security/step-up"`, `name="return_to" value="/admin/users"`,
		"manage invitations, password resets, user access, deletion, and individual MFA policies for the next ten minutes", " disabled",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("stale administrator invitation view missing %q: %s", want, html)
		}
	}
	if strings.Contains(html, `href="/admin/account/security"`) {
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

func TestAdminSecurityPageRequiresRecentVerificationForMutations(t *testing.T) {
	data := models.MailSecurityAdminData{
		StepUpRequired: true,
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
		`role="alert"`,
		"Recent administrator verification required",
		`data-passkey-authentication`,
		`data-success-redirect="/admin/security"`,
		`data-start-csrf="` + strings.Repeat("d", 64) + `"`,
		"unlock these changes for ten minutes",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("step-up-required admin security page missing %q: %s", want, html)
		}
	}
	if got := strings.Count(html, ` disabled>`); got != 4 {
		t.Fatalf("admin security disabled submit button count = %d, want 4", got)
	}
	if strings.Contains(html, `href="/settings/security"`) {
		t.Fatal("stale mail-security view still sends verification to webmail settings")
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

func TestAdminSecurityRetentionDialogVisibilityAndFeedback(t *testing.T) {
	for _, test := range []struct {
		name, notice, message string
		open                  bool
	}{
		{name: "closed by default"},
		{name: "saved", notice: "Retention saved.", open: true},
		{name: "invalid", message: "Enter a valid retention period.", open: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out strings.Builder
			data := AdminSecurityActivityData{ManagedMode: true, RetentionDays: 180, RetentionMinimumDays: 1, RetentionMaximumDays: 365, Notice: test.notice, Error: test.message}
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
			body := html[start:end]
			for _, value := range []string{`action="/admin/activity/retention"`, test.notice, test.message} {
				if value != "" && !strings.Contains(body, value) {
					t.Fatalf("dialog omitted %q", value)
				}
			}
			if strings.Contains(html[:start]+html[end:], `action="/admin/activity/retention"`) || strings.Contains(html, `onsubmit="return confirm`) {
				t.Fatal("retention form is not confined to the dialog")
			}
		})
	}
}
