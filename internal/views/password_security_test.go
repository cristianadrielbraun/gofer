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

func TestPasswordSecuritySettingsRendersAccessibleFactorManagementAndEscapesSecrets(t *testing.T) {
	csrf := strings.Repeat("b", 64)
	data := PasswordSecurityData{
		HasPassword: true, HasTOTP: true, RecoveryCodesRemaining: 7, StepUpFresh: true,
		TOTPReplacement: &TOTPReplacementData{
			QRCodeDataURL: "data:image/png;base64,cG5n", ManualKey: `<new-authenticator-key>`,
			Algorithm: "SHA1", Digits: 6, Period: 30,
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
		`data-totp-replacement`,
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
	csrf := strings.Repeat("d", 64)
	var output bytes.Buffer
	if err := PasswordSecuritySettings(PasswordSecurityData{
		GoogleLoginAvailable: true,
		StepUpFresh:          true,
		FederatedIdentities: []FederatedIdentityData{{
			Provider: "google", Email: `<person&family@gmail.example>`,
			LinkedAt: "Aug 15, 2026", LastUsedAt: "Aug 16, 2026",
		}},
		CSRFTokens: map[string]string{linkPath: csrf},
	}).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecuritySettings.Render(Google identity) error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		`data-federated-identity-settings`, "1 connected", "Google · connected Aug 15, 2026",
		"last used Aug 16, 2026", `action="` + linkPath + `"`,
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
		CSRFTokens:           map[string]string{linkPath: csrf},
	}).Render(context.Background(), &output); err != nil {
		t.Fatalf("PasswordSecuritySettings.Render(stale Google identity) error = %v", err)
	}
	if strings.Contains(output.String(), `action="`+linkPath+`"`) || !strings.Contains(output.String(), "sign in again first") {
		t.Fatalf("stale Google identity settings = %q", output.String())
	}
}
