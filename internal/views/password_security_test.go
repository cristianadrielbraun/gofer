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
		LoginEmail:    `<owner@example.com>`,
	}).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecuritySettings.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		`data-local-login-identifiers`, `aria-label="Local sign-in identifiers"`,
		`data-local-login-username`, `data-local-login-email`,
		"Local sign-in", "Username", "Account email",
		`&lt;owner&amp;name&gt;`, `&lt;owner@example.com&gt;`,
		"separate from mailbox addresses and external sign-in identities",
		"An identifier is not a credential",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("local sign-in identifiers missing %q: %q", want, html)
		}
	}
	for _, forbidden := range []string{
		`<owner&name>`, `<owner@example.com>`, `name="username"`, `name="email"`,
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
	if strings.Count(output.String(), ">Not set</") != 2 {
		t.Fatalf("missing local identifiers should render two Not set values: %q", output.String())
	}
}

func TestPasswordSecuritySettingsRendersSessionHistoryWithoutInternalValues(t *testing.T) {
	var output bytes.Buffer
	if err := PasswordSecuritySettings(PasswordSecurityData{
		SessionsTruncated: true,
		Sessions: []SecuritySessionData{
			{
				Client: "Firefox on Linux", Authentication: "Password", Assurance: "Multi-factor",
				SignedInAt: "Aug 19, 2026 at 10:00 AM", LastActiveAt: "Aug 19, 2026 at 10:05 AM",
				Current: true, Active: true,
			},
			{
				Client: `<script>signed-out</script>`, Authentication: "Google", Assurance: "Single factor",
				SignedInAt: "Aug 18, 2026 at 9:00 AM", LastActiveAt: "Aug 18, 2026 at 9:30 AM",
				EndedAt: "Aug 18, 2026 at 9:31 AM",
			},
		},
	}).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecuritySettings.Render() session history error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		`data-security-sessions`, `aria-label="Current and recent sessions"`,
		"Firefox on Linux", "Password · Multi-factor", "Current",
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
		`<script>signed-out</script>`, "session-token", "session-id", `action="/settings/security/sessions`,
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("security session view exposed forbidden value %q", forbidden)
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
	if strings.Contains(output.String(), "Replace authenticator") {
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

func TestPasswordSecuritySettingsRequiresStepUpBeforeSensitiveFactorForms(t *testing.T) {
	csrf := strings.Repeat("c", 64)
	var output bytes.Buffer
	if err := PasswordSecuritySettings(PasswordSecurityData{
		HasPassword: true, HasTOTP: true, HasPasskey: true, RecoveryCodesRemaining: 10,
		Passkeys: []PasskeySecurityData{{ID: "passkey", Name: "Laptop"}},
		CSRFTokens: map[string]string{
			"/settings/security/step-up":                 csrf,
			"/settings/security/passkeys/step-up/start":  csrf,
			"/settings/security/passkeys/step-up/finish": csrf,
		},
	}).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecuritySettings.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		`data-security-step-up`, `action="/settings/security/step-up"`,
		`inputmode="numeric"`, `autocomplete="one-time-code"`,
		"unlock sensitive security changes for ten minutes",
		`data-passkey-authentication`, `data-start-path="/settings/security/passkeys/step-up/start"`,
		`data-finish-path="/settings/security/passkeys/step-up/finish"`, "Verify with a passkey",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("stale security view missing %q", want)
		}
	}
	for _, forbidden := range []string{
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
		"does not connect a Gmail mailbox", "does not", "mail, contacts, or calendars",
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
		"Microsoft sign-in does not connect an Outlook mailbox",
		"neither grants access to mail, contacts, or calendars",
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
		"Connect Company SSO sign-in", "grants no mailbox or provider-resource access",
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
