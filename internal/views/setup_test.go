package views

import (
	"bytes"
	"strings"
	"testing"
)

func TestSetupTokenPageIsAccessibleLocalAndEscapesErrors(t *testing.T) {
	var output bytes.Buffer
	message := `<script>alert("setup")</script>`
	if err := SetupTokenPage(SetupTokenData{Message: message}).Render(t.Context(), &output); err != nil {
		t.Fatalf("SetupTokenPage.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		`action="/setup"`, `name="token"`, `type="password"`,
		`autocomplete="one-time-code"`, `role="alert"`, `aria-live="polite"`,
		`aria-describedby="setup-token-error"`, `&lt;script&gt;alert`,
		"never included in the page URL", "browser storage",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("setup token page missing %q", want)
		}
	}
	if strings.Contains(html, message) || strings.Contains(html, "fonts.googleapis.com") || strings.Contains(html, "fonts.gstatic.com") {
		t.Fatal("setup token page rendered unsafe or remote content")
	}
}

func TestSetupOwnerPageIsAccessibleAndLocal(t *testing.T) {
	var output bytes.Buffer
	if err := SetupOwnerPage(SetupOwnerData{
		Kind: "existing", DraftSaved: true,
		Candidates: []SetupOwnerCandidateData{{
			ID: "person-id", Name: "Person <Owner>", Email: "person@example.com",
			Status: "active", MailboxCount: 2, LegacySessions: 1,
		}},
		Form: SetupOwnerFormData{
			Target: "existing:person-id", Name: "Person <Owner>", Username: "person", Email: "person@example.com",
			Errors: map[string]string{"username": `<script>alert("owner")</script>`},
		},
	}).Render(t.Context(), &output); err != nil {
		t.Fatalf("SetupOwnerPage.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		"Setup access verified", "Choose the Gofer owner", `action="/setup/owner"`,
		`name="owner_target"`, `value="existing:person-id"`, "2 mail accounts", "1 legacy sessions",
		`autocomplete="username"`, `autocomplete="email"`, `role="alert"`, `&lt;script&gt;alert`,
		"No user, role, credential, or owned data has been changed yet", `href="/setup/password"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("setup owner page missing %q", want)
		}
	}
	if strings.Contains(html, `<script>alert("owner")</script>`) || strings.Contains(html, "fonts.googleapis.com") || strings.Contains(html, "fonts.gstatic.com") {
		t.Fatal("setup owner page requested a remote font")
	}
}

func TestSetupPasswordPageIsAccessibleLocalAndSecretFree(t *testing.T) {
	var output bytes.Buffer
	message := `<script>alert("password")</script>`
	if err := SetupPasswordPage(SetupPasswordData{
		PasswordReady: true,
		Errors:        map[string]string{"password": message, "confirmation": "Passwords differ."},
	}).Render(t.Context(), &output); err != nil {
		t.Fatalf("SetupPasswordPage.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		"Choose the owner password", `action="/setup/password"`, `name="password"`,
		`name="password_confirmation"`, `autocomplete="new-password"`, `minlength="15"`, `maxlength="256"`,
		`aria-describedby="owner-password-help owner-password-error"`, `role="alert"`, `&lt;script&gt;alert`,
		"Owner password ready for final enrollment", "Only its Argon2id hash is inside the encrypted setup draft",
		`href="/setup/owner"`, `action="/setup/mfa"`, `value="start"`,
		"administrator MFA and recovery codes", "Passwords are never echoed",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("setup password page missing %q", want)
		}
	}
	if strings.Contains(html, message) || strings.Contains(html, `name="password" type="password" value=`) ||
		strings.Contains(html, `name="password_confirmation" type="password" value=`) ||
		strings.Contains(html, "fonts.googleapis.com") || strings.Contains(html, "fonts.gstatic.com") {
		t.Fatal("setup password page rendered a secret-bearing value or unsafe remote content")
	}
}

func TestSetupMFAPageIsAccessibleLocalAndEscapesErrors(t *testing.T) {
	var output bytes.Buffer
	message := `<script>alert("totp")</script>`
	if err := SetupMFAPage(SetupMFAData{
		QRCodeDataURL: "data:image/png;base64,ZmFrZS1wbmc=",
		ManualKey:     "ABCD EFGH IJKL MNOP",
		Algorithm:     "SHA1", Digits: 6, Period: 30,
		Errors: map[string]string{"code": message},
	}).Render(t.Context(), &output); err != nil {
		t.Fatalf("SetupMFAPage.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		"Secure the owner account", `src="data:image/png;base64,ZmFrZS1wbmc="`,
		`alt="QR code containing the Gofer authenticator setup key"`, "ABCD EFGH IJKL MNOP",
		`action="/setup/mfa"`, `name="action" value="confirm"`, `name="code" type="text"`,
		`inputmode="numeric"`, `autocomplete="one-time-code"`, `pattern="[0-9]{6}"`,
		`aria-describedby="setup-mfa-code-help setup-mfa-code-error"`, `role="alert"`, `&lt;script&gt;alert`,
		"SHA1", "6 digits", "30 seconds", `value="restart"`, `href="/setup/password"`,
		"never sent to a third-party QR service",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("setup MFA page missing %q", want)
		}
	}
	if strings.Contains(html, message) || strings.Contains(html, "fonts.googleapis.com") || strings.Contains(html, "fonts.gstatic.com") || strings.Contains(html, "https://") {
		t.Fatal("setup MFA page rendered unsafe or remote content")
	}
}

func TestSetupMFAReadyPageDoesNotRedisplayEnrollmentSecret(t *testing.T) {
	var output bytes.Buffer
	if err := SetupMFAPage(SetupMFAData{
		QRCodeDataURL: "data:image/png;base64,c2VjcmV0",
		ManualKey:     "MUST NOT BE RENDERED",
		Algorithm:     "SHA1", Digits: 6, Period: 30, TOTPReady: true,
	}).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, want := range []string{"Authenticator verified for final enrollment", "Next: recovery codes", "No authenticator, user, or session has been created yet", "Replace this authenticator setup"} {
		if !strings.Contains(html, want) {
			t.Fatalf("ready setup MFA page missing %q", want)
		}
	}
	if strings.Contains(html, "MUST NOT BE RENDERED") || strings.Contains(html, "data:image/png") || strings.Contains(html, `name="code"`) {
		t.Fatal("ready setup MFA page redisplayed enrollment material")
	}
}

func TestSetupRecoveryPageDisplaysPlaintextBatchOnceAndEscapesErrors(t *testing.T) {
	var output bytes.Buffer
	message := `<script>alert("recovery")</script>`
	codes := []string{
		"0123-4567-89AB-CDEF-GHJK-MNPQ",
		"RSTV-WXYZ-2345-6789-ABCD-EFGH",
	}
	if err := SetupRecoveryPage(SetupRecoveryData{
		BatchID: strings.Repeat("a", 64), Codes: codes, Generated: true,
		Errors: map[string]string{"saved": message},
	}).Render(t.Context(), &output); err != nil {
		t.Fatalf("SetupRecoveryPage.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		"Save the owner recovery codes", "These codes are shown only in this response",
		codes[0], codes[1], `aria-label="Owner recovery codes"`,
		`action="/setup/recovery"`, `name="action" value="acknowledge"`,
		`name="batch_id" value="` + strings.Repeat("a", 64) + `"`,
		`name="saved" type="checkbox" value="yes"`, `required`,
		`aria-describedby="setup-recovery-saved-help setup-recovery-saved-error"`,
		`role="alert"`, `&lt;script&gt;alert`, "Generate replacement codes", `href="/setup/mfa"`,
		"never logged, placed in URLs, stored in browser storage",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("setup recovery page missing %q", want)
		}
	}
	if strings.Contains(html, message) || strings.Contains(html, "fonts.googleapis.com") || strings.Contains(html, "fonts.gstatic.com") || strings.Contains(html, "https://") {
		t.Fatal("setup recovery page rendered unsafe or remote content")
	}
}

func TestSetupRecoveryPageDoesNotRedisplayGeneratedBatch(t *testing.T) {
	var output bytes.Buffer
	if err := SetupRecoveryPage(SetupRecoveryData{
		BatchID: strings.Repeat("b", 64), Codes: nil, Generated: true,
	}).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, want := range []string{
		"Recovery codes were already shown", "acknowledge it below", "generate a replacement",
		`name="batch_id" value="` + strings.Repeat("b", 64) + `"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("generated recovery state missing %q", want)
		}
	}
	if strings.Contains(html, "These codes are shown only in this response") || strings.Contains(html, `<code class=`) {
		t.Fatal("generated recovery state implied plaintext was redisplayed")
	}
}

func TestSetupRecoveryAcknowledgedPageHidesBatchAndCodes(t *testing.T) {
	var output bytes.Buffer
	if err := SetupRecoveryPage(SetupRecoveryData{
		BatchID: strings.Repeat("c", 64), Codes: []string{"MUST-NOT-BE-RENDERED"},
		Generated: true, Acknowledged: true,
	}).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, want := range []string{
		"Recovery codes acknowledged for final enrollment", "Only the code hashes remain",
		"Next: final setup review", "Replace recovery codes",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("acknowledged recovery page missing %q", want)
		}
	}
	if strings.Contains(html, "MUST-NOT-BE-RENDERED") || strings.Contains(html, strings.Repeat("c", 64)) || strings.Contains(html, `name="batch_id"`) {
		t.Fatal("acknowledged recovery page redisplayed batch material")
	}
}

func TestSetupReviewPageIsAccessibleLocalAndEscapesIdentity(t *testing.T) {
	var output bytes.Buffer
	message := `<script>alert("review")</script>`
	if err := SetupReviewPage(SetupReviewData{
		TopologyKind: "existing", Mode: "existing", TargetUserID: "existing-user",
		CurrentName: "Current Person", CurrentUsername: "current", CurrentEmail: "current@example.com",
		CurrentStatus: "disabled", CurrentIsAdmin: false,
		OwnerName: message, OwnerUsername: "owner", OwnerEmail: "owner@example.com",
		ExistingUserCount: 2, TotalMailboxCount: 3, TargetMailboxCount: 2,
		UnrevokedSessionCount: 4, TargetLegacySessions: 1,
		ExistingPasswordCredentials: 1, RetainedPasskeys: 2, ReplacedTOTPs: 1,
		RetainedIdentities: 1, ReplacedRecoveryCodes: 8, ClaimsExistingUser: true,
	}).Render(t.Context(), &output); err != nil {
		t.Fatalf("SetupReviewPage.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		"Review owner setup", `aria-labelledby="review-owner-heading"`,
		`aria-labelledby="review-security-heading"`, `aria-labelledby="review-migration-heading"`,
		"existing-user", "Current Person", "disabled", "standard user", `&lt;script&gt;alert`,
		"1 existing password credential(s) will be replaced",
		"1 active TOTP credential(s) will be replaced",
		"8 unused recovery code(s) will be replaced",
		"2 active passkey(s) and 1 app-login identity record(s) remain attached",
		"Claim the selected existing user in place", "3 mail account record(s) exist; 3 are attached to valid user IDs",
		"2 mail account(s) are already attached", "4 currently unrevoked session(s)",
		"1 legacy session record(s)", "Review only — nothing has been committed",
		`role="status"`, `aria-live="polite"`, `href="/setup/recovery"`,
		`method="post" action="/setup/review"`, `name="action" value="complete"`,
		"Ready to initialize mandatory authentication", "Complete setup and sign in",
		"If any database operation fails, none of those changes are kept",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("setup review page missing %q", want)
		}
	}
	if strings.Contains(html, message) || strings.Contains(html, `name="batch_id"`) ||
		strings.Contains(html, "fonts.googleapis.com") || strings.Contains(html, "fonts.gstatic.com") || strings.Contains(html, "https://") {
		t.Fatal("setup review page rendered unsafe, remote, or mutating content")
	}
}

func TestSetupReviewFreshAndBlockedImpactAreExplicit(t *testing.T) {
	var fresh bytes.Buffer
	if err := SetupReviewPage(SetupReviewData{
		TopologyKind: "fresh", Mode: "create", CreatesNewOwner: true,
		OwnerName: "Owner", OwnerUsername: "owner", OwnerEmail: "owner@example.com",
	}).Render(t.Context(), &fresh); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Create a new active owner and administrator", "stable user ID will be generated inside the final transaction",
		"Initialize a fresh owner without creating or claiming a synthetic default user",
	} {
		if !strings.Contains(fresh.String(), want) {
			t.Fatalf("fresh setup review missing %q", want)
		}
	}

	var blocked bytes.Buffer
	if err := SetupReviewPage(SetupReviewData{
		CreatesNewOwner: true, OwnerName: "Owner", OwnerUsername: "owner", OwnerEmail: "owner@example.com",
		UnassignedMailboxCount: 1, BlockedMessage: "One mailbox has no valid owner.",
	}).Render(t.Context(), &blocked); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Setup completion is blocked", "One mailbox has no valid owner", "1 mail account(s) have no valid user owner", "will not guess, delete, merge, or reassign",
	} {
		if !strings.Contains(blocked.String(), want) {
			t.Fatalf("blocked setup review missing %q", want)
		}
	}
	if strings.Contains(blocked.String(), `action="/setup/review"`) || strings.Contains(blocked.String(), "Complete setup and sign in") {
		t.Fatal("blocked setup review rendered the completion action")
	}

	var failed bytes.Buffer
	if err := SetupReviewPage(SetupReviewData{
		CreatesNewOwner: true, OwnerName: "Owner", OwnerUsername: "owner", OwnerEmail: "owner@example.com",
		CompletionError: `<script>retry</script>`,
	}).Render(t.Context(), &failed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(failed.String(), "Setup was not completed") || !strings.Contains(failed.String(), `&lt;script&gt;retry&lt;/script&gt;`) ||
		strings.Contains(failed.String(), `<script>retry</script>`) {
		t.Fatalf("completion error rendering = %q", failed.String())
	}
}

func TestSetupOwnerLegacyPageExplainsInPlaceClaim(t *testing.T) {
	var output bytes.Buffer
	if err := SetupOwnerPage(SetupOwnerData{
		Kind: "legacy_default", Form: SetupOwnerFormData{Target: "existing:default"},
	}).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, want := range []string{
		"Claim your existing Gofer data", `value="existing:default"`, "keep using user ID", "Nothing is copied or reassigned",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("legacy owner page missing %q", want)
		}
	}
}

func TestSetupOwnerBlockedPageHasLocalRepairGuidanceAndNoForm(t *testing.T) {
	var output bytes.Buffer
	if err := SetupOwnerPage(SetupOwnerData{BlockedMessage: "Ambiguous users."}).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	if !strings.Contains(html, "Owner setup needs local repair") || !strings.Contains(html, "gofer auth users list") ||
		strings.Contains(html, `action="/setup/owner"`) {
		t.Fatalf("blocked setup owner page = %q", html)
	}
}
