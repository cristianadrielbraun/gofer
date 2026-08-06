package views

import (
	"bytes"
	"strings"
	"testing"
)

func TestEnrollmentRedemptionPageIsAccessibleLocalAndEscapesErrors(t *testing.T) {
	var output bytes.Buffer
	message := `<script>alert("token")</script>`
	if err := EnrollmentRedemptionPage(EnrollmentRedemptionData{Message: message}).Render(t.Context(), &output); err != nil {
		t.Fatalf("EnrollmentRedemptionPage.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{
		`role="alert"`, `action="/account/redeem"`, `name="token"`,
		`autocomplete="one-time-code"`, `name="new_password"`,
		`name="confirm_password"`, `autocomplete="new-password"`,
		`aria-describedby="redemption-password-help"`, `&lt;script&gt;alert`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("enrollment redemption page missing %q", want)
		}
	}
	if strings.Contains(html, message) || strings.Contains(html, "fonts.googleapis.com") || strings.Contains(html, "fonts.gstatic.com") {
		t.Fatal("enrollment redemption page rendered unsafe or remote content")
	}
}

func TestEnrollmentRedemptionCompletionIsAccessibleAndLocal(t *testing.T) {
	var output bytes.Buffer
	if err := EnrollmentRedemptionCompletePage().Render(t.Context(), &output); err != nil {
		t.Fatalf("EnrollmentRedemptionCompletePage.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{`role="status"`, "Password set", `href="/login"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("enrollment redemption completion missing %q", want)
		}
	}
	if strings.Contains(html, "fonts.googleapis.com") || strings.Contains(html, "fonts.gstatic.com") {
		t.Fatal("enrollment redemption completion requested a remote font")
	}
}
