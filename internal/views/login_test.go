package views

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func TestLoginPageUsesAccessibleLocalPasswordForm(t *testing.T) {
	var output bytes.Buffer
	if err := LoginPage(LoginPageData{ErrorMessage: "Unable to sign in with those credentials.", Identifier: `person"@example.com`, MethodsShown: true}).Render(t.Context(), &output); err != nil {
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
		`href="/account/recover"`, "Forgot password?",
		"Have an invitation?", `href="/account/enroll"`, "Set up your account",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("login page missing %q", required)
		}
	}
	if strings.Contains(html, `person"@example.com`) || !strings.Contains(html, `person&#34;@example.com`) {
		t.Fatalf("login identifier was not safely escaped: %q", html)
	}
	if loadsRemoteResource(html) || strings.Contains(html, "/auth/google") || strings.Contains(html, "/auth/microsoft") {
		t.Fatalf("local-only login page contains external/provider content: %q", html)
	}
}

// The page starts at the username: the password is closed and left out of the
// form, and the button continues.
func TestLoginPageStartsAtTheUsername(t *testing.T) {
	var output bytes.Buffer
	if err := LoginPage(LoginPageData{}).Render(t.Context(), &output); err != nil {
		t.Fatalf("LoginPage().Render() error = %v", err)
	}
	html := output.String()
	for _, required := range []string{
		`data-login-reveal="password" data-open="false"`, `data-login-reveal="passkey" data-open="false"`,
		`data-login-submit-label="continue" data-shown="true"`, `src="/assets/js/login.js"`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("login page missing %q", required)
		}
	}
	passwordInput := html[strings.Index(html, `id="login-password"`):]
	if passwordInput = passwordInput[:strings.Index(passwordInput, ">")]; !strings.Contains(passwordInput, "disabled") {
		t.Fatalf("the closed password is sent: %s", passwordInput)
	}
}

func TestLoginPageShowsGoogleOnlyWhenConfigured(t *testing.T) {
	var output bytes.Buffer
	if err := LoginPage(LoginPageData{ShowGoogle: true}).Render(t.Context(), &output); err != nil {
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
	if err := LoginPage(LoginPageData{ShowMicrosoft: true}).Render(t.Context(), &output); err != nil {
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
	if err := LoginPage(LoginPageData{OIDCName: "Company SSO"}).Render(t.Context(), &output); err != nil {
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
	if err := LoginMFAContinuationPage("That code is invalid.", LoginMFAFactors{HasTOTP: true}).Render(t.Context(), &output); err != nil {
		t.Fatalf("LoginMFAContinuationPage().Render() error = %v", err)
	}
	html := output.String()
	for _, required := range []string{
		"Complete additional verification", `method="post"`, `action="/login/mfa"`,
		`name="code"`, `inputmode="numeric"`, `autocomplete="one-time-code"`,
		`pattern="[0-9]{6}"`, `maxlength="6"`, `role="alert"`,
		`aria-describedby="login-mfa-error"`, "Verify and sign in", "/login/mfa/recovery",
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("MFA continuation page missing %q: %q", required, html)
		}
	}
	if loadsRemoteResource(html) {
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
	if loadsRemoteResource(recovery.String()) {
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
	if loadsRemoteResource(mfa.String()) {
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
	if loadsRemoteResource(codes.String()) {
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
		"Finishing setup signs out other sessions",
	} {
		if !strings.Contains(enrollment.String(), required) {
			t.Fatalf("required MFA enrollment page missing %q: %q", required, enrollment.String())
		}
	}
	if loadsRemoteResource(enrollment.String()) {
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
		"Finish setup and sign in", "No new session or authenticator exists",
	} {
		if !strings.Contains(codes.String(), required) {
			t.Fatalf("required MFA recovery page missing %q: %q", required, codes.String())
		}
	}
	if loadsRemoteResource(codes.String()) {
		t.Fatalf("required MFA recovery page loads a remote resource: %q", codes.String())
	}
}

func TestLoginMFAConfirmationEscapesProviderAndKeepsVerificationRequired(t *testing.T) {
	{
		var output bytes.Buffer
		err := LoginMFAContinuationPage("", LoginMFAFactors{
			PrimarySignIn: "<script>provider</script> sign-in successful.",
			HasTOTP:       true,
		}).Render(t.Context(), &output)
		if err != nil {
			t.Fatal(err)
		}
		html := output.String()
		if strings.Contains(html, "<script>provider</script>") || !strings.Contains(html, "&lt;script&gt;provider&lt;/script&gt;") {
			t.Fatal("provider confirmation was not escaped")
		}
		if !strings.Contains(html, "Enter your authenticator app code to finish signing in.") || strings.Contains(html, "Your password was accepted") {
			t.Fatal("confirmation misrepresents completed authentication")
		}
	}
}

// remoteResource matches HTML that makes the browser fetch from another
// site: a script, image, frame or stylesheet, or a CSS url(). Plain links,
// such as the version's link to GitHub, fetch nothing until followed.
var remoteResource = regexp.MustCompile(`(?i)\bsrc(set)?="https?://|<link\b[^>]*\bhref="https?://|url\(['"]?https?://`)

func loadsRemoteResource(html string) bool {
	return remoteResource.MatchString(html)
}

func TestLoadsRemoteResourceCatchesFetchesButNotLinks(t *testing.T) {
	for _, html := range []string{
		`<script src="https://cdn.example/x.js"></script>`,
		`<img srcset="https://cdn.example/a.png 2x">`,
		`<link rel="stylesheet" href="https://fonts.example/css">`,
		`<div style="background:url('https://cdn.example/bg.png')"></div>`,
	} {
		if !loadsRemoteResource(html) {
			t.Fatalf("missed a remote fetch: %s", html)
		}
	}
	if loadsRemoteResource(`<a href="https://github.com/cristianadrielbraun/gofer/releases/tag/v1.0.0">Gofer v1.0.0</a>`) {
		t.Fatal("a plain link counted as a remote fetch")
	}
}
