package views

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

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
			RedemptionURL: "https://gofer.example/account/redeem",
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
		"Gofer stores only a hash of the token", "https://gofer.example/account/redeem",
		"Copy invitation details", "deliberately not placed in the URL",
		`private-token&lt;/textarea&gt;&lt;script&gt;alert(&#34;token&#34;)&lt;/script&gt;`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("administrator invitation view missing %q: %s", want, html)
		}
	}
	for _, forbidden := range []string{
		`<script>alert("token")</script>`,
		`https://gofer.example/account/redeem?token=`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("administrator invitation view exposed forbidden value %q", forbidden)
		}
	}
}

func TestAdminUsersPageDisablesInvitationUntilRecentVerification(t *testing.T) {
	data := AdminUsersData{StepUpRequired: true, InvitationCSRFToken: strings.Repeat("b", 64)}
	var out bytes.Buffer
	if err := AdminUsersPage(data).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminUsersPage.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		"Recent administrator verification required", `href="/settings/security"`,
		"manage invitations and individual MFA policies for the next ten minutes", " disabled",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("stale administrator invitation view missing %q: %s", want, html)
		}
	}
}

func TestAdminLabelsPageRendersOutlookGraphDiagnostics(t *testing.T) {
	status := models.LabelAdminStatus{
		Accounts: []models.LabelAccountSyncStatus{{
			AccountID:       "acc_outlook",
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
	for _, want := range []string{"Outlook Graph parity", "Graph IDs", "IMAP rows", "Parity delta", "Needs repair", "Backfillable", "No Graph folder"} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered admin labels page missing %q: %s", want, html)
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

	var out bytes.Buffer
	if err := AdminSecurityPage(data).Render(context.Background(), &out); err != nil {
		t.Fatalf("AdminSecurityPage.Render() error = %v", err)
	}
	html := out.String()
	for _, want := range []string{
		`role="alert"`,
		"Recent administrator verification required",
		`href="/settings/security"`,
		"unlock these changes for ten minutes",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("step-up-required admin security page missing %q: %s", want, html)
		}
	}
	if got := strings.Count(html, ` disabled>`); got != 4 {
		t.Fatalf("admin security disabled submit button count = %d, want 4", got)
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
