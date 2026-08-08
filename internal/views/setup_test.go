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
	if err := SetupOwnerPage().Render(t.Context(), &output); err != nil {
		t.Fatalf("SetupOwnerPage.Render() error = %v", err)
	}
	html := output.String()
	for _, want := range []string{`role="status"`, "Setup access verified", "short-lived access", "has not been consumed"} {
		if !strings.Contains(html, want) {
			t.Fatalf("setup owner page missing %q", want)
		}
	}
	if strings.Contains(html, "fonts.googleapis.com") || strings.Contains(html, "fonts.gstatic.com") {
		t.Fatal("setup owner page requested a remote font")
	}
}
