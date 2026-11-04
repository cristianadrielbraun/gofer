package i18n

import (
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/branding"
)

func TestMessagesCarryTheInstallationName(t *testing.T) {
	t.Cleanup(func() { branding.Set(branding.Settings{}) })
	if got := Translate(English, "page.title", "page", "Mail"); got != "Gofer - Mail" {
		t.Fatalf("default title = %q", got)
	}
	_, before := Script(English)
	branding.Set(branding.Settings{Name: "Acme Mail"})
	if got := Translate(English, "page.title", "page", "Mail"); got != "Acme Mail - Mail" {
		t.Fatalf("branded title = %q", got)
	}
	body, after := Script(English)
	if after == before || strings.Contains(string(body), `"js.page.title":"Gofer`) {
		t.Fatalf("browser catalog kept the old name (version %s → %s)", before, after)
	}
}
