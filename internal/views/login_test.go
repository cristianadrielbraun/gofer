package views

import (
	"bytes"
	"strings"
	"testing"
)

func TestLoginPageUsesAccessibleLocalPasswordForm(t *testing.T) {
	var output bytes.Buffer
	if err := LoginPage(false, "Unable to sign in with those credentials.", `person"@example.com`).Render(t.Context(), &output); err != nil {
		t.Fatalf("LoginPage().Render() error = %v", err)
	}
	html := output.String()
	for _, required := range []string{
		`method="post"`, `action="/login"`, `name="identifier"`,
		`autocomplete="username"`, `name="password"`,
		`autocomplete="current-password"`, `role="alert"`,
		`aria-describedby="login-error"`, `type="submit"`,
	} {
		if !strings.Contains(html, required) {
			t.Fatalf("login page missing %q", required)
		}
	}
	if strings.Contains(html, `person"@example.com`) || !strings.Contains(html, `person&#34;@example.com`) {
		t.Fatalf("login identifier was not safely escaped: %q", html)
	}
	if strings.Contains(html, "https://") || strings.Contains(html, "/auth/google") {
		t.Fatalf("local-only login page contains external/provider content: %q", html)
	}
}

func TestLoginPageShowsGoogleOnlyWhenConfigured(t *testing.T) {
	var output bytes.Buffer
	if err := LoginPage(true, "", "").Render(t.Context(), &output); err != nil {
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

func TestLoginMFAContinuationPageUsesOnlyLocalResources(t *testing.T) {
	var output bytes.Buffer
	if err := LoginMFAContinuationPage().Render(t.Context(), &output); err != nil {
		t.Fatalf("LoginMFAContinuationPage().Render() error = %v", err)
	}
	html := output.String()
	if !strings.Contains(html, "Additional verification required") || strings.Contains(html, "https://") {
		t.Fatalf("MFA continuation page = %q", html)
	}
}
