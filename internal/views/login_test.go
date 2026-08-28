package views

import (
	"bytes"
	"strings"
	"testing"
)

func TestLoginPageUsesAccessibleLocalPasswordForm(t *testing.T) {
	var output bytes.Buffer
	if err := LoginPage(false, false, "", "Unable to sign in with those credentials.", `person"@example.com`).Render(t.Context(), &output); err != nil {
		t.Fatalf("LoginPage().Render() error = %v", err)
	}
	html := output.String()
	for _, required := range []string{
		`method="post"`, `action="/login"`, `name="identifier"`,
		`autocomplete="username"`, `name="password"`,
		`autocomplete="current-password"`, `role="alert"`,
		`aria-describedby="login-error"`, `type="submit"`,
		`data-passkey-authentication`, `data-start-path="/login/passkey/start"`,
		`data-finish-path="/login/passkey/finish"`, `data-identifier-source="#login-identifier"`,
		`src="/assets/js/passkey-authentication.js"`, "Sign in with a passkey",
		`href="/account/redeem"`, "Forgot password?",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("login page missing %q", required)
		}
	}
	if strings.Contains(html, `person"@example.com`) || !strings.Contains(html, `person&#34;@example.com`) {
		t.Fatalf("login identifier was not safely escaped: %q", html)
	}
	if strings.Contains(html, "https://") || strings.Contains(html, "/auth/google") || strings.Contains(html, "/auth/microsoft") {
		t.Fatalf("local-only login page contains external/provider content: %q", html)
	}
}

func TestLoginPageShowsGoogleOnlyWhenConfigured(t *testing.T) {
	var output bytes.Buffer
	if err := LoginPage(true, false, "", "", "").Render(t.Context(), &output); err != nil {
		t.Fatalf("LoginPage().Render() error = %v", err)
	}
	html := output.String()
	if !strings.Contains(html, `href="/auth/google"`) || !strings.Contains(html, "Continue with Google") {
		t.Fatalf("configured Google option missing: %q", html)
	}
	if strings.Contains(html, "fonts.googleapis.com") || strings.Contains(html, "fonts.gstatic.com") {
		t.Fatalf("login page loads remote fonts: %q", html)
	}
}

func TestLoginPageShowsMicrosoftOnlyWhenConfigured(t *testing.T) {
	var output bytes.Buffer
	if err := LoginPage(false, true, "", "", "").Render(t.Context(), &output); err != nil {
		t.Fatalf("LoginPage().Render() error = %v", err)
	}
	html := output.String()
	if !strings.Contains(html, `href="/auth/microsoft"`) || !strings.Contains(html, "Continue with Microsoft") {
		t.Fatalf("configured Microsoft option missing: %q", html)
	}
	if strings.Contains(html, `href="/auth/google"`) || strings.Contains(html, "graph.microsoft.com") {
		t.Fatalf("Microsoft sign-in option crossed provider/resource boundary: %q", html)
	}
}

func TestLoginPageShowsConfiguredOIDCProviderWithoutMailboxAccess(t *testing.T) {
	var output bytes.Buffer
	if err := LoginPage(false, false, "Company SSO", "", "").Render(t.Context(), &output); err != nil {
		t.Fatalf("LoginPage().Render() error = %v", err)
	}
	html := output.String()
	if !strings.Contains(html, `href="/auth/oidc"`) || !strings.Contains(html, "Continue with Company SSO") {
		t.Fatalf("configured OIDC option missing: %q", html)
	}
	if strings.Contains(html, `href="/auth/google"`) || strings.Contains(html, `href="/auth/microsoft"`) ||
		strings.Contains(html, "offline_access") {
		t.Fatalf("OIDC sign-in option crossed provider/resource boundary: %q", html)
	}
}

func TestLoginMFAContinuationPageUsesOnlyLocalResources(t *testing.T) {
	var output bytes.Buffer
	if err := LoginMFAContinuationPage("That code is invalid.").Render(t.Context(), &output); err != nil {
		t.Fatalf("LoginMFAContinuationPage().Render() error = %v", err)
	}
	html := output.String()
	for _, required := range []string{
		"Enter your authenticator code", `method="post"`, `action="/login/mfa"`,
		`name="code"`, `inputmode="numeric"`, `autocomplete="one-time-code"`,
		`pattern="[0-9]{6}"`, `maxlength="6"`, `role="alert"`,
		`aria-describedby="login-mfa-error"`, "Verify and sign in", "/login/mfa/recovery",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("MFA continuation page missing %q: %q", required, html)
		}
	}
	if strings.Contains(html, "https://") {
		t.Fatalf("MFA continuation page loads a remote resource: %q", html)
	}
}

func TestRecoveryLoginAndRepairPagesAreAccessibleLocalAndExplicit(t *testing.T) {
	var recovery bytes.Buffer
	if err := RecoveryCodeLoginPage("That code is invalid.").Render(t.Context(), &recovery); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"Use a recovery code", `action="/login/mfa/recovery"`, `name="code"`,
		`autocomplete="one-time-code"`, `role="alert"`, "required authenticator repair",
	} {
		if !strings.Contains(recovery.String(), required) {
			t.Fatalf("recovery-code page missing %q: %q", required, recovery.String())
		}
	}
	if strings.Contains(recovery.String(), "https://") {
		t.Fatalf("recovery-code page loads a remote resource: %q", recovery.String())
	}

	var mfa bytes.Buffer
	if err := RecoveryRepairMFAPage(RecoveryRepairMFAData{
		QRCodeDataURL: "data:image/png;base64,cXItZGF0YQ==", ManualKey: "ABCD EFGH",
		Algorithm: "SHA1", Digits: 6, Period: 30, ErrorMessage: "Try again.",
	}).Render(t.Context(), &mfa); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"Replace your authenticator", "Gofer has not created a session",
		`src="data:image/png;base64,cXItZGF0YQ=="`, "ABCD EFGH",
		`action="/login/recovery/mfa"`, `value="confirm"`, `value="restart"`,
		`role="alert"`, "stay active until repair completes",
	} {
		if !strings.Contains(mfa.String(), required) {
			t.Fatalf("recovery-repair MFA page missing %q: %q", required, mfa.String())
		}
	}
	if strings.Contains(mfa.String(), "https://") {
		t.Fatalf("recovery-repair MFA page loads a remote resource: %q", mfa.String())
	}

	var codes bytes.Buffer
	if err := RecoveryRepairCodesPage(RecoveryRepairCodesData{
		BatchID: "batch-id", Codes: []string{"AAAA-BBBB-CCCC-DDDD-EEEE-FFFF"}, Generated: true,
	}).Render(t.Context(), &codes); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"Save fresh recovery codes", "AAAA-BBBB-CCCC-DDDD-EEEE-FFFF",
		`aria-label="New recovery codes"`, `action="/login/recovery/codes"`,
		`name="batch_id" value="batch-id"`, `name="saved"`,
		"Complete repair and sign in", "No session exists until repair completes",
	} {
		if !strings.Contains(codes.String(), required) {
			t.Fatalf("recovery-repair codes page missing %q: %q", required, codes.String())
		}
	}
	if strings.Contains(codes.String(), "https://") {
		t.Fatalf("recovery-repair codes page loads a remote resource: %q", codes.String())
	}
}

func TestRequiredMFAEnrollmentPagesRemainSessionlessAndLocal(t *testing.T) {
	var enrollment bytes.Buffer
	if err := MFAEnrollmentPage(MFAEnrollmentData{
		QRCodeDataURL: "data:image/png;base64,cXItZGF0YQ==", ManualKey: "ABCD EFGH",
		Algorithm: "SHA1", Digits: 6, Period: 30, ErrorMessage: "Try again.",
	}).Render(t.Context(), &enrollment); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"Set up an authenticator", "Gofer has not created a session",
		`src="data:image/png;base64,cXItZGF0YQ=="`, "ABCD EFGH",
		`action="/login/mfa/enroll"`, `value="confirm"`, `value="restart"`,
		"Existing sessions and authenticators are unchanged",
	} {
		if !strings.Contains(enrollment.String(), required) {
			t.Fatalf("required MFA enrollment page missing %q: %q", required, enrollment.String())
		}
	}
	if strings.Contains(enrollment.String(), "https://") {
		t.Fatalf("required MFA enrollment page loads a remote resource: %q", enrollment.String())
	}

	var codes bytes.Buffer
	if err := MFAEnrollmentCodesPage(MFAEnrollmentCodesData{
		BatchID: "batch-id", Codes: []string{"AAAA-BBBB-CCCC-DDDD-EEEE-FFFF"}, Generated: true,
	}).Render(t.Context(), &codes); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"Save your recovery codes", "AAAA-BBBB-CCCC-DDDD-EEEE-FFFF",
		`action="/login/mfa/enroll/codes"`, `name="batch_id" value="batch-id"`,
		"Complete MFA setup and sign in", "No new session or authenticator exists",
	} {
		if !strings.Contains(codes.String(), required) {
			t.Fatalf("required MFA recovery page missing %q: %q", required, codes.String())
		}
	}
	if strings.Contains(codes.String(), "https://") {
		t.Fatalf("required MFA recovery page loads a remote resource: %q", codes.String())
	}
}
