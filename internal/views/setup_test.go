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
		`href="/setup/owner"`, "administrator MFA and recovery codes", "Passwords are never echoed",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("setup password page missing %q", want)
		}
	}
	if strings.Contains(html, message) || strings.Contains(html, `value="`) || strings.Contains(html, "fonts.googleapis.com") || strings.Contains(html, "fonts.gstatic.com") {
		t.Fatal("setup password page rendered a secret-bearing value or unsafe remote content")
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
