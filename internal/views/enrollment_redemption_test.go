package views

import (
	"bytes"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func TestInvitationEnrollmentPageIsPurposeSpecificAndEscapesErrors(t *testing.T) {
	var output bytes.Buffer
	message := `<script>alert("token")</script>`
	if err := InvitationEnrollmentPage(InvitationEnrollmentData{
		Message: message, GoogleLoginAvailable: true,
	}).Render(t.Context(), &output); err != nil {
		t.Fatalf("InvitationEnrollmentPage.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		`role="alert"`, `action="/account/enroll"`, "Invitation token",
		`name="token"`, `autocomplete="one-time-code"`, `name="new_password"`,
		`name="confirm_password"`, `autocomplete="new-password"`,
		`aria-describedby="enrollment-password-help"`, `&lt;script&gt;alert`,
		`formaction="/account/enroll/google"`, `formnovalidate`,
		`aria-describedby="enrollment-error enrollment-token-help"`,
		"Required for both options below.", "Choose how to sign in",
		"Accept invitation with password", "Accept invitation with Google",
		"This uses the invitation token entered above.", "does not connect your Gmail mailbox",
		`href="/login"`, "Back to sign in",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("invitation enrollment page missing %q", want)
		}
	}
	for _, forbidden := range []string{message, "/account/redeem", "reset token", "fonts.googleapis.com", "fonts.gstatic.com"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("invitation enrollment page contains %q", forbidden)
		}
	}
}

func TestCredentialRedemptionPageContainsNoInvitationOrFederatedEnrollment(t *testing.T) {
	var output bytes.Buffer
	message := `<script>alert("reset")</script>`
	if err := CredentialRedemptionPage(CredentialRedemptionData{Message: message}).Render(t.Context(), &output); err != nil {
		t.Fatalf("CredentialRedemptionPage.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		`role="alert"`, `action="/account/redeem"`, "Reset token",
		`name="token"`, `name="new_password"`, `name="confirm_password"`,
		`aria-describedby="redemption-password-help"`, `&lt;script&gt;alert`, "Reset password",
		`href="/login"`, "Back to sign in",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("credential redemption page missing %q", want)
		}
	}
	for _, forbidden := range []string{
		message, "/account/enroll", "Invitation", "Google", "Gmail", "fonts.googleapis.com", "fonts.gstatic.com",
	} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("credential redemption page contains %q", forbidden)
		}
	}
}

func TestPasswordTokenCompletionPagesArePurposeSpecificAndLocal(t *testing.T) {
	tests := []struct {
		name      string
		component templ.Component
		want      string
	}{
		{name: "invitation", component: InvitationEnrollmentCompletePage(), want: "Invitation accepted"},
		{name: "reset", component: CredentialRedemptionCompletePage(), want: "Password reset"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := test.component.Render(t.Context(), &output); err != nil {
				t.Fatalf("completion page render error = %v", err)
			}
			html := output.String()
			for _, want := range []string{`role="status"`, test.want, `href="/login"`, "Back to sign in"} {
				if !strings.Contains(html, want) {
					t.Fatalf("completion page missing %q", want)
				}
			}
			if strings.Contains(html, "fonts.googleapis.com") || strings.Contains(html, "fonts.gstatic.com") {
				t.Fatal("completion page requested a remote font")
			}
		})
	}
}
