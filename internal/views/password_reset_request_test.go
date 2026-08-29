package views

import (
	"bytes"
	"strings"
	"testing"
)

func TestPasswordResetRequestPageSeparatesRequestAndTokenEntry(t *testing.T) {
	var form bytes.Buffer
	if err := PasswordResetRequestPage(PasswordResetRequestData{}).Render(t.Context(), &form); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`action="/account/recover"`, `name="identifier"`, `autocomplete="username"`,
		"Request password reset token", "administrator must issue the private reset token",
		`href="/account/redeem"`, "I already have a reset token",
	} {
		if !strings.Contains(form.String(), want) {
			t.Fatalf("password reset request form missing %q: %q", want, form.String())
		}
	}
	if strings.Contains(form.String(), `name="password"`) || strings.Contains(form.String(), "email address") {
		t.Fatal("password reset request form asks for a password or email address")
	}

	var submitted bytes.Buffer
	if err := PasswordResetRequestPage(PasswordResetRequestData{Submitted: true}).Render(t.Context(), &submitted); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Request received", "same for every submitted username", `href="/account/redeem"`, "Enter reset token"} {
		if !strings.Contains(submitted.String(), want) {
			t.Fatalf("password reset confirmation missing %q: %q", want, submitted.String())
		}
	}
	if strings.Contains(submitted.String(), `name="identifier"`) {
		t.Fatal("password reset confirmation retained the submitted identifier field")
	}
}
