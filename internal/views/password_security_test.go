package views

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

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
