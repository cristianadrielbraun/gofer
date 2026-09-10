package views

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/models"
)

func TestSettingsLayoutLoadsPasskeyControllersForHTMXSecurityNavigation(t *testing.T) {
	var output bytes.Buffer
	if err := SettingsLayout(nil, models.SyncSettings{}, "accounts", nil, nil).Render(context.Background(), &output); err != nil {
		t.Fatalf("SettingsLayout.Render() error = %v", err)
	}
	html := output.String()
	for _, script := range []string{
		`src="/assets/js/passkey-registration.js"`,
		`src="/assets/js/passkey-authentication.js"`,
	} {
		if !strings.Contains(html, script) {
			t.Fatalf("settings layout missing passkey controller %q", script)
		}
	}
}

func TestPasswordSecurityLayoutIsLocalAccessibleAndEscapesMessages(t *testing.T) {
	var output bytes.Buffer
	data := PasswordSecurityData{
		HasPassword:    true,
		Message:        `<script>alert("credential")</script>`,
		MessageIsError: true,
		CSRFToken:      strings.Repeat("a", 64),
	}
	if err := PasswordSecurityLayout(nil, data).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecurityLayout.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		`role="alert"`,
		`src="/assets/js/popover.min.js?`,
		`src="/assets/js/htmx.min.js"`,
		`src="/assets/js/app.js"`,
		`src="/assets/js/settings.js"`,
		`id="choose-account-type-dialog"`,
		`id="edit-account-container"`,
		`action="/settings/security/password"`,
		`name="_csrf" value="` + data.CSRFToken + `"`,
		`autocomplete="current-password"`,
		`autocomplete="new-password"`,
		`aria-describedby="new-password-help"`,
		`&lt;script&gt;alert`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("password security layout missing %q", want)
		}
	}
	if strings.Contains(html, `<script>alert("credential")</script>`) {
		t.Fatal("password security layout rendered an unescaped message")
	}
	if strings.Contains(html, "fonts.googleapis.com") || strings.Contains(html, "fonts.gstatic.com") {
		t.Fatal("password security layout requested a remote font")
	}
}

func TestPasswordSecuritySettingsDoesNotOfferChangeWithoutCredential(t *testing.T) {
	var output bytes.Buffer
	if err := PasswordSecuritySettings(PasswordSecurityData{}).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecuritySettings.Render() error = %v", err)
	}
	html := output.String()
	if !strings.Contains(html, "does not have a local password") || strings.Contains(html, `action="/settings/security/password"`) || strings.Contains(html, `name="current_password"`) {
		t.Fatalf("missing-credential password settings = %q", html)
	}
}

func TestPasswordSecuritySettingsRendersLocalLoginIdentifiersReadOnlyAndEscaped(t *testing.T) {
	var output bytes.Buffer
	if err := PasswordSecuritySettings(PasswordSecurityData{
		LoginUsername: `<owner&name>`,
	}).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecuritySettings.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		`data-local-login-identifiers`, `aria-label="Local sign-in identifier"`,
		`data-local-login-username`,
		"Local sign-in", "Username",
		`&lt;owner&amp;name&gt;`,
		"separate from mailbox addresses and external sign-in identities",
		"A username is not a credential",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("local sign-in identifiers missing %q: %q", want, html)
		}
	}
	for _, forbidden := range []string{
		`<owner&name>`, `name="username"`, `name="email"`,
		`username_normalized`, `email_normalized`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("local sign-in identifiers exposed editable or unescaped value %q", forbidden)
		}
	}

	output.Reset()
	if err := PasswordSecuritySettings(PasswordSecurityData{}).Render(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	if strings.Count(output.String(), ">Not set</") != 1 {
		t.Fatalf("missing local identifier should render one Not set value: %q", output.String())
	}
}

func TestPasswordSecuritySettingsRendersSessionHistoryWithoutInternalValues(t *testing.T) {
	const revokePath = "/settings/security/sessions/opaque-reference/revoke"
	const revokeOthersPath = "/settings/security/sessions/revoke-others"
	csrf := strings.Repeat("r", 64)
	var output bytes.Buffer
	if err := PasswordSecuritySettings(PasswordSecurityData{
		SessionsTruncated:      true,
		CanRevokeOtherSessions: true,
		Sessions: []SecuritySessionData{
			{
				Client: "Firefox on Linux", Authentication: "Password", Assurance: "Multi-factor",
				SignedInAt: "Aug 19, 2026 at 10:00 AM", LastActiveAt: "Aug 19, 2026 at 10:05 AM",
				Current: true, Active: true,
			},
			{
				Client: "Chrome on macOS", Authentication: "Passkey", Assurance: "Phishing-resistant",
				SignedInAt: "Aug 19, 2026 at 9:00 AM", LastActiveAt: "Aug 19, 2026 at 9:05 AM",
				Active: true, RevokePath: revokePath,
			},
			{
				Client: `<script>signed-out</script>`, Authentication: "Google", Assurance: "Single factor",
				SignedInAt: "Aug 18, 2026 at 9:00 AM", LastActiveAt: "Aug 18, 2026 at 9:30 AM",
				EndedAt: "Aug 18, 2026 at 9:31 AM",
			},
		},
		CSRFTokens: map[string]string{revokePath: csrf, revokeOthersPath: csrf},
	}).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecuritySettings.Render() session history error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		`data-security-sessions`, `aria-label="Current and recent sessions"`,
		`data-tui-dialog-target="security-sessions-revoke-others"`,
		`id="security-sessions-revoke-others"`,
		`data-tui-dialog-target="security-session-revoke-1"`,
		`id="security-session-revoke-1"`,
		"Firefox on Linux", "Password · Multi-factor", "Current",
		"Chrome on macOS", "Passkey · Phishing-resistant", "Active",
		`action="` + revokePath + `"`, ">Sign out</button>",
		`action="` + revokeOthersPath + `"`, "Sign out all other sessions",
		`name="_csrf" value="` + csrf + `"`,
		`&lt;script&gt;signed-out&lt;/script&gt;`, "Google · Single factor", "Signed out",
		"Signed in Aug 19, 2026 at 10:00 AM", "Last active Aug 19, 2026 at 10:05 AM",
		"Signed out Aug 18, 2026 at 9:31 AM", "Showing the 50 most relevant sessions",
		"Session tokens and internal identifiers are never shown.",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("security session view missing %q: %q", want, html)
		}
	}
	for _, forbidden := range []string{
		`<script>signed-out</script>`, "session-token", "session-id", "target-session-id",
		`id="security-session-revoke-0"`, `id="security-session-revoke-2"`,
		`onsubmit="return confirm(&#39;Sign out`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("security session view exposed forbidden value %q", forbidden)
		}
	}
}

func TestPasswordSecuritySettingsRendersCompactLazySecurityActivityCard(t *testing.T) {
	var output bytes.Buffer
	if err := PasswordSecuritySettings(PasswordSecurityData{
		SecurityEventCount: 42,
	}).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecuritySettings.Render() compact security activity error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		`data-security-events`, "Your security activity", "42 events", "View activity",
		`data-tui-dialog-target="security-activity-dialog"`,
		`hx-get="/settings/security/activity?page=1"`,
		`hx-target="#security-activity-dialog-body"`, `hx-swap="innerHTML"`,
		`id="security-activity-dialog"`, `id="security-activity-dialog-body"`,
		"Loading security activity…",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("compact security activity view missing %q: %q", want, html)
		}
	}
	for _, forbidden := range []string{
		"Signed in", "A verified security request was completed.", "Chrome on Linux",
		"audit-event-id", "actor-user-id", "subject-user-id",
		"session-id", "source-hash", "request-id", "metadata_json", "provider-subject",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("compact security activity view exposed forbidden value %q", forbidden)
		}
	}
}

func TestSecurityActivityDialogPageRendersPaginationWithoutAuditInternals(t *testing.T) {
	var output bytes.Buffer
	if err := SecurityActivityDialogPage(SecurityActivityPageData{
		TotalEvents: 42, Page: 2, TotalPages: 3, FirstEvent: 21, LastEvent: 40,
		PreviousPage: 1, NextPage: 3, HasPrevious: true, HasNext: true,
		Events: []SecurityEventData{
			{
				Title: "Signed in", Detail: "A verified security request was completed.",
				OccurredAt: "Aug 19, 2026 at 6:00 PM", Client: "Chrome on Linux",
				Status: "Completed", Successful: true,
			},
			{
				Title: `<script>failed</script>`, Detail: `Rejected <private> detail`,
				OccurredAt: "Aug 19, 2026 at 5:59 PM", Client: `<unknown client>`,
				Status: "Failed", Successful: false,
			},
		},
	}).Render(context.Background(), &output); err != nil {
		t.Fatalf("SecurityActivityDialogPage.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		`data-security-activity-page`, `aria-label="Security activity events"`,
		"Signed in", "A verified security request was completed.", "Aug 19, 2026 at 6:00 PM",
		"From Chrome on Linux", ">Completed</span>",
		`&lt;script&gt;failed&lt;/script&gt;`, `Rejected &lt;private&gt; detail`,
		`From &lt;unknown client&gt;`, ">Failed</span>", "Showing 21–40 of 42", "Page 2 of 3",
		`hx-get="/settings/security/activity?page=1"`,
		`hx-get="/settings/security/activity?page=3"`,
		"Raw audit metadata and internal identifiers are not displayed",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("security activity dialog page missing %q: %q", want, html)
		}
	}
	for _, forbidden := range []string{
		`<script>failed</script>`, "audit-event-id", "actor-user-id", "subject-user-id",
		"session-id", "source-hash", "request-id", "metadata_json", "provider-subject",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("security activity dialog page exposed forbidden value %q", forbidden)
		}
	}
}

func TestPasswordSecuritySettingsRendersAccessibleFactorManagementAndEscapesSecrets(t *testing.T) {
	csrf := strings.Repeat("b", 64)
	data := PasswordSecurityData{
		HasPassword: true, HasTOTP: true, RecoveryCodesRemaining: 7, StepUpFresh: true,
		TOTPManagement: &TOTPManagementData{
			QRCodeDataURL: "data:image/png;base64,cG5n", ManualKey: `<new-authenticator-key>`,
			Algorithm: "SHA1", Digits: 6, Period: 30, IsReplacement: true,
		},
		RecoveryReplacementPending: true,
		RecoveryBatchID:            "batch-id",
		RecoveryCodes:              []string{"SAFE-CODE", `<script>recovery</script>`},
		CSRFTokens: map[string]string{
			"/settings/security/totp/confirm":      csrf,
			"/settings/security/totp/start":        csrf,
			"/settings/security/recovery/complete": csrf,
			"/settings/security/recovery/start":    csrf,
			"/settings/security/management/cancel": csrf,
		},
	}
	var output bytes.Buffer
	if err := PasswordSecuritySettings(data).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecuritySettings.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		`data-totp-management`,
		`alt="QR code containing the replacement Gofer authenticator key"`,
		`aria-label="Replacement authenticator manual setup key"`,
		`action="/settings/security/totp/confirm"`,
		`autocomplete="one-time-code"`,
		`data-recovery-code-replacement`,
		`aria-label="New recovery codes"`,
		`action="/settings/security/recovery/complete"`,
		`name="saved" value="yes"`,
		`name="_csrf" value="` + csrf + `"`,
		`&lt;new-authenticator-key&gt;`,
		`&lt;script&gt;recovery&lt;/script&gt;`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("factor management view missing %q", want)
		}
	}
	if strings.Contains(html, `<new-authenticator-key>`) || strings.Contains(html, `<script>recovery</script>`) {
		t.Fatal("factor management view rendered unescaped credential material")
	}
}

func TestPasswordSecuritySettingsRendersFirstTimeTOTPEnrollmentWithoutReplacementClaims(t *testing.T) {
	csrf := strings.Repeat("d", 64)
	var output bytes.Buffer
	if err := PasswordSecuritySettings(PasswordSecurityData{
		HasPassword: true, StepUpFresh: true,
		CSRFTokens: map[string]string{"/settings/security/totp/start": csrf},
	}).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecuritySettings.Render() setup action error = %v", err)
	}
	for _, want := range []string{
		"Not enrolled", "Set up authenticator", `action="/settings/security/totp/start"`,
		`name="_csrf" value="` + csrf + `"`,
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("first-time TOTP setup action missing %q: %q", want, output.String())
		}
	}
	if strings.Contains(output.String(), "Replace TOTP authenticator app") {
		t.Fatal("first-time TOTP setup action rendered replacement copy")
	}

	output.Reset()
	if err := PasswordSecuritySettings(PasswordSecurityData{
		HasPassword: true, StepUpFresh: true,
		TOTPManagement: &TOTPManagementData{
			QRCodeDataURL: "data:image/png;base64,cG5n", ManualKey: `<first-authenticator-key>`,
			Algorithm: "SHA1", Digits: 6, Period: 30,
		},
		CSRFTokens: map[string]string{
			"/settings/security/totp/confirm":      csrf,
			"/settings/security/totp/start":        csrf,
			"/settings/security/management/cancel": csrf,
		},
	}).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecuritySettings.Render() enrollment error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		`data-totp-management`, "Verify the new authenticator",
		`alt="QR code containing the new Gofer authenticator key"`,
		`aria-label="New authenticator manual setup key"`,
		"authenticator is not enabled until the code is verified", "Enable authenticator",
		`&lt;first-authenticator-key&gt;`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("first-time TOTP enrollment missing %q: %q", want, html)
		}
	}
	for _, forbidden := range []string{
		"Verify the replacement authenticator", "current authenticator remains active", `<first-authenticator-key>`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("first-time TOTP enrollment rendered forbidden value %q", forbidden)
		}
	}
}

func TestPasswordSecurityVerificationRendersOnlyAvailableStepUpMethods(t *testing.T) {
	csrf := strings.Repeat("c", 64)
	var output bytes.Buffer
	if err := PasswordSecurityVerification(PasswordSecurityVerificationData{
		HasTOTP: true, HasPasskey: true,
		CSRFTokens: map[string]string{
			"/settings/security/step-up":                 csrf,
			"/settings/security/passkeys/step-up/start":  csrf,
			"/settings/security/passkeys/step-up/finish": csrf,
		},
	}).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecurityVerification.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		`data-security-step-up`, `action="/settings/security/step-up"`,
		`inputmode="numeric"`, `autocomplete="one-time-code"`,
		"has not loaded your security details yet",
		`data-passkey-authentication`, `data-start-path="/settings/security/passkeys/step-up/start"`,
		`data-finish-path="/settings/security/passkeys/step-up/finish"`, "Verify with a passkey",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("stale security view missing %q", want)
		}
	}
	for _, forbidden := range []string{
		`data-password-security-settings`,
		`data-local-login-identifiers`,
		`data-federated-identity-settings`,
		`data-passkey-security-settings`,
		`data-security-sessions`,
		`data-security-events`,
		`action="/settings/security/totp/start"`,
		`action="/settings/security/totp/disable"`,
		`action="/settings/security/recovery/start"`,
		`action="/settings/security/recovery/revoke"`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("stale security view exposed sensitive action %q", forbidden)
		}
	}
}

func TestPasswordSecurityVerificationRequiresSignInAgainWithoutStepUpFactor(t *testing.T) {
	csrf := strings.Repeat("s", 64)
	var output bytes.Buffer
	if err := PasswordSecurityVerification(PasswordSecurityVerificationData{
		CSRFTokens: map[string]string{"/auth/logout": csrf},
	}).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecurityVerification.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		"Sign in again to securely load", `action="/auth/logout"`,
		`name="_csrf" value="` + csrf + `"`, ">Sign in again</button>",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("verification fallback missing %q: %q", want, html)
		}
	}
	for _, forbidden := range []string{
		`action="/settings/security/step-up"`, `data-passkey-authentication`,
		`data-password-security-settings`, `data-local-login-identifiers`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("verification fallback exposed %q", forbidden)
		}
	}
}

func TestPasswordSecuritySettingsRequiresStrongStepUpForMFAPasswordChange(t *testing.T) {
	var requiredOutput bytes.Buffer
	if err := PasswordSecuritySettings(PasswordSecurityData{
		HasPassword: true,
		HasTOTP:     true,
		RequiresMFA: true,
	}).Render(context.Background(), &requiredOutput); err != nil {
		t.Fatalf("PasswordSecuritySettings.Render(required MFA) error = %v", err)
	}
	requiredHTML := requiredOutput.String()
	for _, want := range []string{
		"current password alone is not sufficient verification",
		"Verify this session above before changing the password",
	} {
		if !strings.Contains(requiredHTML, want) {
			t.Fatalf("MFA-required stale security view missing %q", want)
		}
	}
	if strings.Contains(requiredHTML, `action="/settings/security/password"`) || strings.Contains(requiredHTML, `name="current_password"`) {
		t.Fatal("MFA-required stale security view exposed the password-change form")
	}

	var normalOutput bytes.Buffer
	if err := PasswordSecuritySettings(PasswordSecurityData{
		HasPassword: true,
	}).Render(context.Background(), &normalOutput); err != nil {
		t.Fatalf("PasswordSecuritySettings.Render(normal account) error = %v", err)
	}
	normalHTML := normalOutput.String()
	if !strings.Contains(normalHTML, `action="/settings/security/password"`) || !strings.Contains(normalHTML, `name="current_password"`) {
		t.Fatal("normal password account should still be able to verify with its current password")
	}
}

func TestPasswordSecuritySettingsRendersConnectedGoogleIdentityWithoutMailboxConfusion(t *testing.T) {
	const linkPath = "/settings/security/identities/google/link"
	const unlinkPath = "/settings/security/identities/google/identity-id/unlink"
	csrf := strings.Repeat("d", 64)
	var output bytes.Buffer
	if err := PasswordSecuritySettings(PasswordSecurityData{
		GoogleLoginAvailable: true,
		StepUpFresh:          true,
		FederatedIdentities: []FederatedIdentityData{{
			ID: "identity-id", Provider: "google", Email: `<person&family@gmail.example>`,
			LinkedAt: "Aug 15, 2026", LastUsedAt: "Aug 16, 2026",
			CanUnlink: true, UnlinkPath: unlinkPath,
		}},
		CSRFTokens: map[string]string{linkPath: csrf, unlinkPath: csrf},
	}).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecuritySettings.Render(Google identity) error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		`data-federated-identity-settings`, "1 connected", "Google · connected Aug 15, 2026",
		"last used Aug 16, 2026", `action="` + linkPath + `"`,
		`action="` + unlinkPath + `"`, "Disconnect", "removes only this identity",
		`name="_csrf" value="` + csrf + `"`, `&lt;person&amp;family@gmail.example&gt;`,
		"For Gofer sign-in only", "does not connect a Gmail or Outlook mailbox", "mail, contacts, calendars",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("Google identity settings missing %q", want)
		}
	}
	if strings.Contains(html, `<person&family@gmail.example>`) {
		t.Fatal("Google identity settings rendered an unescaped provider email")
	}

	output.Reset()
	if err := PasswordSecuritySettings(PasswordSecurityData{
		GoogleLoginAvailable: true,
		FederatedIdentities: []FederatedIdentityData{{
			ID: "identity-id", Provider: "google", Email: "person@example.com",
			LinkedAt: "Aug 15, 2026", CanUnlink: true, UnlinkPath: unlinkPath,
		}},
		CSRFTokens: map[string]string{linkPath: csrf, unlinkPath: csrf},
	}).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecuritySettings.Render(stale Google identity) error = %v", err)
	}
	if strings.Contains(output.String(), `action="`+linkPath+`"`) ||
		strings.Contains(output.String(), `action="`+unlinkPath+`"`) ||
		!strings.Contains(output.String(), "sign in again first") ||
		!strings.Contains(output.String(), "Verify this session before disconnecting") {
		t.Fatalf("stale Google identity settings = %q", output.String())
	}

	output.Reset()
	if err := PasswordSecuritySettings(PasswordSecurityData{
		GoogleLoginAvailable: true,
		StepUpFresh:          true,
		FederatedIdentities: []FederatedIdentityData{{
			ID: "identity-id", Provider: "google", Email: "person@example.com",
			LinkedAt: "Aug 15, 2026", UnlinkPath: unlinkPath,
			UnlinkReason: "Add another usable sign-in method before disconnecting this identity.",
		}},
		CSRFTokens: map[string]string{linkPath: csrf, unlinkPath: csrf},
	}).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecuritySettings.Render(protected Google identity) error = %v", err)
	}
	if strings.Contains(output.String(), `action="`+unlinkPath+`"`) ||
		!strings.Contains(output.String(), "Add another usable sign-in method") {
		t.Fatalf("protected Google identity settings = %q", output.String())
	}
}

func TestPasswordSecuritySettingsUsesOneGenericVerificationNoticeForApplicationSignIn(t *testing.T) {
	var output bytes.Buffer
	if err := PasswordSecuritySettings(PasswordSecurityData{
		GoogleLoginAvailable:    true,
		MicrosoftLoginAvailable: true,
		OIDCLoginAvailable:      true,
		OIDCLoginName:           "Company SSO",
	}).Render(context.Background(), &output); err != nil {
		t.Fatal(err)
	}

	html := output.String()
	const notice = "Verify this session above before connecting another sign-in method. If no verification form is shown, sign in again first."
	if strings.Count(html, notice) != 1 {
		t.Fatalf("generic application sign-in verification notice count = %d, want 1: %q", strings.Count(html, notice), html)
	}
	for _, providerNotice := range []string{
		"before connecting a Google identity",
		"before connecting a Microsoft identity",
		"before connecting a Company SSO identity",
	} {
		if strings.Contains(html, providerNotice) {
			t.Fatalf("application sign-in settings rendered provider-specific notice %q: %q", providerNotice, html)
		}
	}
}

func TestPasswordSecuritySettingsRendersGoogleAndMicrosoftActionsSideBySideWithIcons(t *testing.T) {
	const googlePath = "/settings/security/identities/google/link"
	const microsoftPath = "/settings/security/identities/microsoft/link"
	csrf := strings.Repeat("p", 64)
	var output bytes.Buffer
	if err := PasswordSecuritySettings(PasswordSecurityData{
		GoogleLoginAvailable:    true,
		MicrosoftLoginAvailable: true,
		StepUpFresh:             true,
		CSRFTokens: map[string]string{
			googlePath:    csrf,
			microsoftPath: csrf,
		},
	}).Render(context.Background(), &output); err != nil {
		t.Fatal(err)
	}

	html := output.String()
	for _, want := range []string{
		`data-application-sign-in-actions`, `class="grid gap-3 sm:grid-cols-2"`,
		`action="` + googlePath + `"`, "Connect Google sign-in", `<title>Gmail</title>`,
		`action="` + microsoftPath + `"`, "Connect Microsoft sign-in", `viewBox="0 0 14 14"`,
		`class="size-4 shrink-0"`, `name="_csrf" value="` + csrf + `"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("provider sign-in actions missing %q: %q", want, html)
		}
	}
}

func TestPasswordSecuritySettingsRendersMicrosoftIdentityWithoutOutlookMailboxConfusion(t *testing.T) {
	const linkPath = "/settings/security/identities/microsoft/link"
	const unlinkPath = "/settings/security/identities/microsoft/identity-id/unlink"
	csrf := strings.Repeat("e", 64)
	var output bytes.Buffer
	if err := PasswordSecuritySettings(PasswordSecurityData{
		MicrosoftLoginAvailable: true,
		StepUpFresh:             true,
		FederatedIdentities: []FederatedIdentityData{{
			ID: "identity-id", Provider: "microsoft", Email: "person@microsoft.example",
			LinkedAt: "Aug 18, 2026", CanUnlink: true, UnlinkPath: unlinkPath,
		}},
		CSRFTokens: map[string]string{linkPath: csrf, unlinkPath: csrf},
	}).Render(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, want := range []string{
		"Microsoft · connected Aug 18, 2026", "person@microsoft.example",
		`action="` + linkPath + `"`, `action="` + unlinkPath + `"`,
		"For Gofer sign-in only", "does not connect a Gmail or Outlook mailbox",
		"grant Gofer access to mail, contacts, calendars",
		`name="_csrf" value="` + csrf + `"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("Microsoft identity settings missing %q: %q", want, html)
		}
	}
	if strings.Contains(html, "Mail.Read") || strings.Contains(html, "graph.microsoft.com") {
		t.Fatal("Microsoft application sign-in UI exposed mailbox authorization")
	}
}

func TestPasswordSecuritySettingsRendersConfiguredOIDCIdentityWithoutMailboxConfusion(t *testing.T) {
	const linkPath = "/settings/security/identities/oidc/link"
	const unlinkPath = "/settings/security/identities/oidc/identity-id/unlink"
	csrf := strings.Repeat("f", 64)
	var output bytes.Buffer
	if err := PasswordSecuritySettings(PasswordSecurityData{
		OIDCLoginAvailable: true,
		OIDCLoginName:      "Company SSO",
		StepUpFresh:        true,
		FederatedIdentities: []FederatedIdentityData{{
			ID: "identity-id", Provider: "oidc", Email: "person@identity.example",
			LinkedAt: "Aug 18, 2026", CanUnlink: true, UnlinkPath: unlinkPath,
		}},
		CSRFTokens: map[string]string{linkPath: csrf, unlinkPath: csrf},
	}).Render(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, want := range []string{
		"Company SSO · connected Aug 18, 2026", "person@identity.example",
		`action="` + linkPath + `"`, `action="` + unlinkPath + `"`,
		"Connect Company SSO sign-in", "For Gofer sign-in only", "other provider resources",
		`name="_csrf" value="` + csrf + `"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("OIDC identity settings missing %q: %q", want, html)
		}
	}
	if strings.Contains(html, "offline_access") || strings.Contains(html, "Mail.Read") {
		t.Fatal("OIDC application sign-in UI exposed provider resource authorization")
	}
}

func TestSecurityActionConfirmationDialogKeepsSubmissionInsideConfirmation(t *testing.T) {
	var output bytes.Buffer
	if err := securityActionConfirmationDialog("session-confirmation", "/settings/security/sessions/reference/revoke", "csrf-value", "Sign out", "Sign out <private-client>?").Render(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	start := strings.Index(html, "<dialog")
	end := strings.Index(html, "</dialog>")
	if start < 0 || end <= start {
		t.Fatal("missing templui confirmation dialog")
	}
	confirmation := html[start:end]
	for _, want := range []string{
		`method="post" action="/settings/security/sessions/reference/revoke"`,
		`name="_csrf" value="csrf-value"`,
		`type="submit"`, `data-tui-dialog-close`, `>Cancel</button>`,
		`Sign out &lt;private-client&gt;?`,
	} {
		if !strings.Contains(confirmation, want) {
			t.Errorf("confirmation missing %q", want)
		}
	}
	if strings.Contains(html[:start], `type="submit"`) || strings.Contains(html, "confirm(") || strings.Contains(html, "<private-client>") {
		t.Fatal("dialog allows unconfirmed submission, uses native confirmation, or exposes unescaped client text")
	}
}
